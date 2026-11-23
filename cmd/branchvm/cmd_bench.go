//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/cgroup"
	"github.com/Heman10x-NGU/branchvm/pkg/netiso"
	"github.com/Heman10x-NGU/branchvm/pkg/pool"
	"github.com/Heman10x-NGU/branchvm/pkg/storage"
	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

func init() { commands["bench"] = cmdBench }

const (
	benchBudget = 15 * time.Minute
	settleDelay = 2 * time.Second
	reportFile  = "BENCHMARKS.md"
	rawFile     = "raw.jsonl"
	dropCmd     = "sync && echo 3 > /proc/sys/vm/drop_caches"

	modeColdBoot = "cold-boot"
	modeRestore  = "snapshot-restore"
	modeRun3     = "run-3-branches"
	pagerMmap    = "mmap"
	pagerFadvise = "fadvise"
	pagerUffd    = "uffd"
)

var densitySizes = []int{1, 3, 5, 10, 20}

var pagers = []struct {
	name string
	mode types.RestoreMode
}{{pagerMmap, types.FileRestore}, {pagerFadvise, pool.PrefaultRestore}, {pagerUffd, types.UffdRestore}}

type benchConfig struct {
	repo      string
	iters     int
	outDir    string
	memMB     int64
	fast      bool
	maxClones int
}

type bench struct {
	cfg          benchConfig
	bcfg         pool.BranchConfig
	out          io.Writer
	raw          *os.File
	enc          *json.Encoder
	recs         []types.BenchRecord
	runBase      int64
	runSeq       int64
	ws           storage.WorkingSet
	densityIters int
	densityNote  string
}

func cmdBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	repo := fs.String("repo", "examples/auth-demo", "git repo packed and synced in the 3-branch run")
	iters := fs.Int("iters", 10, "samples per mode and cache condition")
	outDir := fs.String("out", "results", "directory for raw.jsonl")
	mem := fs.Int64("mem", 1024, "guest MiB for cold boots and cgroup limits; match init --mem")
	fast := fs.Bool("fast", false, "one density iteration per clone count")
	maxClones := fs.Int("max-clones", 20, "largest clone count in the density table")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *iters < 1 || *maxClones < 1 || *mem < 128 {
		return fmt.Errorf("need --iters >= 1, --max-clones >= 1, --mem >= 128")
	}
	if err := requireRoot(); err != nil {
		return err
	}
	dir, err := absRepo(*repo)
	if err != nil {
		return err
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	if err := pool.BenchPreflight(os.Stdout); err != nil {
		return err
	}
	meta, hasMeta, err := pool.LoadGoldenMeta()
	if err != nil {
		return err
	}
	if !explicit["mem"] && hasMeta && meta.MemMB >= 128 {
		*mem = meta.MemMB
	}
	if explicit["mem"] && hasMeta && meta.MemMB > 0 && *mem < meta.MemMB {
		return fmt.Errorf("bench: --mem %d MiB cannot be smaller than golden snapshot memory %d MiB", *mem, meta.MemMB)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", *outDir, err)
	}
	raw, err := os.Create(filepath.Join(*outDir, rawFile))
	if err != nil {
		return fmt.Errorf("create raw samples file: %w", err)
	}
	defer raw.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cgroup.InstallCleanupTrap(cancel)
	b := &bench{
		cfg:  benchConfig{repo: dir, iters: *iters, outDir: *outDir, memMB: *mem, fast: *fast, maxClones: *maxClones},
		bcfg: pool.BranchConfigFromMeta(meta),
		out:  os.Stdout, raw: raw, enc: json.NewEncoder(raw), runBase: time.Now().Unix(),
	}
	return b.run(ctx)
}

func (b *bench) run(ctx context.Context) error {
	start := time.Now()
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{{"boot", b.bootTable}, {"working set", b.recordWS}, {"pager", b.pagerTable}, {"density", b.densityTable}}
	for _, s := range steps {
		fmt.Fprintf(b.out, "== %s\n", s.name)
		if err := s.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w (partial samples in %s)", s.name, err, b.raw.Name())
		}
	}
	if err := b.raw.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", b.raw.Name(), err)
	}
	report := b.report(readHostInfo(), time.Since(start))
	if err := os.WriteFile(reportFile, []byte(report), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", reportFile, err)
	}
	fmt.Fprintf(b.out, "wrote %d samples to %s and the report to %s in %s\n",
		len(b.recs), b.raw.Name(), reportFile, time.Since(start).Round(time.Second))
	return nil
}

// sampleSpec describes one sample. pre runs inside the timed window after
// the cache drop, after runs per branch once it serves, and measure reads
// the live branches before teardown.
type sampleSpec struct {
	rec     types.BenchRecord
	mode    types.RestoreMode
	pre     func() error
	after   func(context.Context, *pool.Branch) error
	measure func(*types.BenchRecord, []*pool.Branch)
	// launchLimit bounds concurrent restores; 0 means all at once. The
	// density table staggers restores because it measures steady-state
	// RAM, and N cold restores at once stall a small host for minutes.
	launchLimit int
}

func (b *bench) sample(ctx context.Context, s sampleSpec) (types.BenchRecord, error) {
	rec := s.rec
	if rec.ColdCache {
		if err := dropCaches(); err != nil {
			return rec, err
		}
	}
	run, err := b.newRun(rec.Clones)
	if err != nil {
		return rec, err
	}
	start := time.Now()
	var brs []*pool.Branch
	var lerr error
	if s.pre != nil {
		lerr = s.pre()
	}
	if lerr == nil {
		brs, lerr = b.launchN(ctx, run, rec.Clones, s.mode, s.launchLimit, s.after)
	}
	rec.TotalMs = time.Since(start).Milliseconds()
	if lerr == nil {
		fillCommon(&rec, brs)
		if s.measure != nil {
			s.measure(&rec, brs)
		}
	}
	if err := errors.Join(lerr, b.finish(run, brs)); err != nil {
		return rec, fmt.Errorf("%s/%s/%s cold=%t sample %d: %w", rec.Table, rec.Mode, rec.Pager, rec.ColdCache, rec.Iteration, err)
	}
	return rec, nil
}

func fillCommon(rec *types.BenchRecord, brs []*pool.Branch) {
	rec.HostRAMMB = 0
	for _, br := range brs {
		rec.RestoreMs = max(rec.RestoreMs, br.RestoreMs)
		rec.FirstQueryMs = max(rec.FirstQueryMs, br.FirstQueryMs)
		if mb := br.MemMB(); mb > 0 {
			rec.HostRAMMB += mb
		}
	}
	rec.PerCloneMB = float64(rec.HostRAMMB) / float64(len(brs))
}

// benchRun persists run state on every change, like a normal run, so a
// crashed bench is reaped by `branchvm gc`.
type benchRun struct {
	id  string
	mu  sync.Mutex
	st  types.RunState
	err error
}

func (b *bench) newRun(n int) (*benchRun, error) {
	b.runSeq++
	id := fmt.Sprintf("b%06x", (b.runBase+b.runSeq)&0xffffff)
	st := types.RunState{RunID: id, StartedAt: time.Now().UTC(), Branches: make([]types.BranchState, n)}
	for i := range st.Branches {
		st.Branches[i] = types.BranchState{Index: i + 1, Status: pool.StatusPending}
	}
	if err := cgroup.Save(pool.RunsDir, st); err != nil {
		return nil, fmt.Errorf("save bench run state: %w", err)
	}
	return &benchRun{id: id, st: st}, nil
}

func (r *benchRun) record(bs types.BranchState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.Branches[bs.Index-1] = bs
	if err := cgroup.Save(pool.RunsDir, r.st); err != nil && r.err == nil {
		r.err = fmt.Errorf("save run state: %w", err)
	}
}

func (b *bench) launchN(ctx context.Context, run *benchRun, n int, mode types.RestoreMode, limit int, after func(context.Context, *pool.Branch) error) ([]*pool.Branch, error) {
	brs, errs := make([]*pool.Branch, n), make([]error, n)
	if limit <= 0 {
		limit = n
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			br, err := pool.LaunchBranchConfigured(ctx, run.id, i, b.cfg.memMB, mode, b.bcfg, run.record)
			brs[i-1] = br
			if err == nil && after != nil {
				err = after(ctx, br)
			}
			errs[i-1] = err
		}(i)
	}
	wg.Wait()
	run.mu.Lock()
	errs = append(errs, run.err)
	run.mu.Unlock()
	return brs, errors.Join(errs...)
}

// finish tears every branch down, removes the run state, and fails on any
// leftover netns, veth, cgroup, clone dir, firecracker, or gateway.
func (b *bench) finish(run *benchRun, brs []*pool.Branch) error {
	var errs []error
	for _, br := range brs {
		if br != nil {
			errs = append(errs, br.Teardown())
		}
	}
	if err := os.Remove(filepath.Join(pool.RunsDir, run.id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove run state %s: %w", run.id, err))
	}
	addrs := make([]string, len(run.st.Branches))
	for i := range addrs {
		addrs[i] = fmt.Sprintf("%s:%d", netiso.HostIP(i+1), netiso.GatewayPort)
	}
	rep, err := pool.ScanLeaks(addrs)
	switch {
	case err != nil:
		errs = append(errs, err)
	case !rep.Clean():
		errs = append(errs, fmt.Errorf("leak after run %s: %s", run.id, rep.Summary()))
	}
	return errors.Join(errs...)
}

func (b *bench) emit(rec types.BenchRecord) error {
	rec.Timestamp = time.Now().UTC()
	if err := b.enc.Encode(rec); err != nil {
		return fmt.Errorf("write %s: %w", b.raw.Name(), err)
	}
	b.recs = append(b.recs, rec)
	fmt.Fprintf(b.out, "%-7s %-16s %-7s n=%-2d it=%-2d cold=%-5t restore=%5dms first_query=%5dms total=%5dms ram=%5dMiB faults=%d bytes=%d\n",
		rec.Table, rec.Mode, rec.Pager, rec.Clones, rec.Iteration, rec.ColdCache,
		rec.RestoreMs, rec.FirstQueryMs, rec.TotalMs, rec.HostRAMMB, rec.PageFaults, rec.BytesFaulted)
	return nil
}

func (b *bench) bootTable(ctx context.Context) error {
	modes := []struct {
		name   string
		mode   types.RestoreMode
		clones int
	}{{modeColdBoot, types.ColdRestore, 1}, {modeRestore, types.FileRestore, 1}, {modeRun3, types.FileRestore, 3}}
	for it := 1; it <= b.cfg.iters; it++ {
		for _, m := range modes {
			for _, cold := range []bool{true, false} {
				s := sampleSpec{
					rec:  types.BenchRecord{Table: "boot", Mode: m.name, Iteration: it, Clones: m.clones, ColdCache: cold},
					mode: m.mode,
				}
				if m.name == modeRun3 {
					var body []byte
					var ctype string
					s.pre = func() error {
						tgz, err := storage.PackWorkspace(b.cfg.repo)
						if err != nil {
							return fmt.Errorf("pack workspace %s: %w", b.cfg.repo, err)
						}
						body, ctype, err = storage.EncodeSyncMultipart(types.SyncMeta{Mode: "full"}, tgz)
						if err != nil {
							return fmt.Errorf("encode workspace sync: %w", err)
						}
						return nil
					}
					s.after = func(ctx context.Context, br *pool.Branch) error { return br.SyncMultipart(ctx, ctype, body) }
				}
				if err := b.sampleEmit(ctx, s); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (b *bench) sampleEmit(ctx context.Context, s sampleSpec) error {
	rec, err := b.sample(ctx, s)
	if err != nil {
		return err
	}
	return b.emit(rec)
}

// recordWS re-records golden/ws.bin as the pages one cold File restore
// touches up to its first query, replacing the init-time approximation.
func (b *bench) recordWS(ctx context.Context) error {
	var wsErr error
	_, err := b.sample(ctx, sampleSpec{
		rec:  types.BenchRecord{Table: "ws", Clones: 1, ColdCache: true},
		mode: types.FileRestore,
		measure: func(*types.BenchRecord, []*pool.Branch) {
			b.ws, wsErr = storage.RecordWorkingSet(pool.GoldenMemPath(), pool.WorkingSetPath())
		},
	})
	if err = errors.Join(err, wsErr); err != nil {
		return err
	}
	fmt.Fprintf(b.out, "working set: %d of %d pages (%d MiB) -> %s\n", b.ws.Resident, b.ws.Pages, b.ws.ResidentBytes()>>20, pool.WorkingSetPath())
	return nil
}

func (b *bench) pagerTable(ctx context.Context) error {
	for it := 1; it <= b.cfg.iters; it++ {
		for _, p := range pagers {
			for _, cold := range []bool{true, false} {
				s := sampleSpec{
					rec:     types.BenchRecord{Table: "pager", Mode: modeRestore, Pager: p.name, Iteration: it, Clones: 1, ColdCache: cold},
					mode:    p.mode,
					measure: measurePager(p.name),
				}
				if err := b.sampleEmit(ctx, s); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// measurePager reads uffd faults and bytes copied from the pager, and for
// the File paths the firecracker process's major fault count plus the
// bytes fadvise advised.
func measurePager(name string) func(*types.BenchRecord, []*pool.Branch) {
	return func(rec *types.BenchRecord, brs []*pool.Branch) {
		br := brs[0]
		if name == pagerUffd {
			rec.PageFaults, rec.BytesFaulted = br.PagerStats()
			return
		}
		rec.PageFaults = majorFaults(br.State("").FirecrackerPID)
		rec.BytesFaulted = br.PrefaultBytes
	}
}

func measureDensity(rec *types.BenchRecord, brs []*pool.Branch) {
	time.Sleep(settleDelay)
	fillCommon(rec, brs)
}

func (b *bench) densityTable(ctx context.Context) error {
	// N concurrent cold restores stampede one disk; the cap must not
	// masquerade as a result, so give the stampede room and report the
	// measured times.
	old := pool.RestoreTimeout
	pool.RestoreTimeout = 3 * time.Minute
	defer func() { pool.RestoreTimeout = old }()
	var sizes []int
	for _, n := range densitySizes {
		if n <= b.cfg.maxClones {
			sizes = append(sizes, n)
		}
	}
	iters := b.cfg.iters
	for it := 1; it <= iters; it++ {
		t0 := time.Now()
		for _, n := range sizes {
			s := sampleSpec{
				rec:         types.BenchRecord{Table: "density", Mode: modeRestore, Iteration: it, Clones: n, ColdCache: true},
				mode:        types.FileRestore,
				measure:     measureDensity,
				launchLimit: 3,
			}
			if err := b.sampleEmit(ctx, s); err != nil {
				return err
			}
		}
		if it == 1 && iters > 1 {
			switch est := time.Since(t0) * time.Duration(iters); {
			case b.cfg.fast:
				iters, b.densityNote = 1, "1 iteration per clone count (--fast)"
			case est > benchBudget:
				iters, b.densityNote = 1, fmt.Sprintf("1 iteration per clone count: %d iterations were estimated at %s, over the %s budget", b.cfg.iters, est.Round(time.Second), benchBudget)
			}
		}
	}
	b.densityIters = iters
	if b.densityNote == "" {
		b.densityNote = fmt.Sprintf("%d iterations per clone count", iters)
	}
	b.densityNote += "; restores staggered 3 in flight, since the table measures steady-state RAM and N simultaneous cold restores stall a small host"
	return nil
}

func dropCaches() error {
	syscall.Sync()
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o644); err != nil {
		return fmt.Errorf("drop page caches: %w", err)
	}
	return nil
}

// majorFaults reads majflt (field 12) from /proc/<pid>/stat; 0 if unreadable.
func majorFaults(pid int) int64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(raw[bytes.LastIndexByte(raw, ')')+1:]))
	if len(f) < 10 {
		return 0
	}
	v, _ := strconv.ParseInt(f[9], 10, 64)
	return v
}

// Aggregation and report.

type stat struct {
	n        int
	med, p95 float64
}

func summarize(xs []float64) stat {
	if len(xs) == 0 {
		return stat{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	med := s[len(s)/2]
	if len(s)%2 == 0 {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return stat{n: len(s), med: med, p95: s[int(math.Ceil(0.95*float64(len(s))))-1]}
}

func (b *bench) stat(table, mode, pager string, clones int, cold bool, f func(types.BenchRecord) float64) stat {
	var xs []float64
	for _, r := range b.recs {
		if r.Table == table && r.Mode == mode && r.Pager == pager && r.Clones == clones && r.ColdCache == cold {
			xs = append(xs, f(r))
		}
	}
	return summarize(xs)
}

var (
	fRestore = func(r types.BenchRecord) float64 { return float64(r.RestoreMs) }
	fFirst   = func(r types.BenchRecord) float64 { return float64(r.FirstQueryMs) }
	fTotal   = func(r types.BenchRecord) float64 { return float64(r.TotalMs) }
	fRAM     = func(r types.BenchRecord) float64 { return float64(r.HostRAMMB) }
	fPer     = func(r types.BenchRecord) float64 { return r.PerCloneMB }
	fFaults  = func(r types.BenchRecord) float64 { return float64(r.PageFaults) }
	fBytes   = func(r types.BenchRecord) float64 { return float64(r.BytesFaulted) / (1 << 20) }
)

func cell(s stat) string {
	if s.n == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f / %.0f", s.med, s.p95)
}

func cacheName(cold bool) string {
	if cold {
		return "cold"
	}
	return "warm"
}

type hostInfo struct{ cpu, mem, kernel, firecracker string }

func readHostInfo() hostInfo {
	h := hostInfo{cpu: "unknown", mem: "unknown", kernel: runtime.GOARCH, firecracker: "unknown"}
	if raw, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		h.cpu = cpuModel(string(raw))
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
				h.mem = strings.TrimSpace(v)
			}
		}
	}
	if raw, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h.kernel = strings.TrimSpace(string(raw)) + " " + runtime.GOARCH
	}
	if out, err := exec.Command("firecracker", "--version").Output(); err == nil {
		h.firecracker, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
	}
	return h
}

// cpuModel prefers "model name"; arm64 kernels only list implementer and
// part codes, so those stand in.
func cpuModel(cpuinfo string) string {
	fields := map[string]string{}
	cpus := 0
	sc := bufio.NewScanner(strings.NewReader(cpuinfo))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "processor" {
			cpus++
		}
		if _, seen := fields[k]; !seen {
			fields[k] = v
		}
	}
	if m := fields["model name"]; m != "" {
		return fmt.Sprintf("%d x %s", cpus, m)
	}
	return fmt.Sprintf("%d x %s CPU (implementer %s, part %s)", cpus, runtime.GOARCH, fields["CPU implementer"], fields["CPU part"])
}

func (b *bench) report(h hostInfo, took time.Duration) string {
	var w strings.Builder
	fmt.Fprintf(&w, "# Benchmarks\n\nGenerated by `branchvm bench` on %s in %s. Raw samples: `%s` (%d lines, one `types.BenchRecord` each). Cells with two numbers are median / p95 across the samples counted in the Samples column.\n\n",
		time.Now().UTC().Format("2006-01-02 15:04 MST"), took.Round(time.Second), filepath.Join(b.cfg.outDir, rawFile), len(b.recs))
	b.bootSection(&w)
	b.pagerSection(&w)
	b.densitySection(&w)
	b.methodology(&w, h)
	b.reading(&w)
	return w.String()
}

func (b *bench) bootSection(w io.Writer) {
	fmt.Fprint(w, "## Boot and restore\n\n| Mode | Cache | Samples | Restore ms | First query ms | Total ms | Host RAM MiB |\n|---|---|---|---|---|---|---|\n")
	for _, m := range []struct {
		mode   string
		clones int
	}{{modeColdBoot, 1}, {modeRestore, 1}, {modeRun3, 3}} {
		for _, cold := range []bool{true, false} {
			r := b.stat("boot", m.mode, "", m.clones, cold, fRestore)
			fmt.Fprintf(w, "| %s | %s | %d | %s | %s | %s | %s |\n", m.mode, cacheName(cold), r.n, cell(r),
				cell(b.stat("boot", m.mode, "", m.clones, cold, fFirst)),
				cell(b.stat("boot", m.mode, "", m.clones, cold, fTotal)),
				cell(b.stat("boot", m.mode, "", m.clones, cold, fRAM)))
		}
	}
	fmt.Fprint(w, "\n`cold-boot` boots the guest kernel on a reflink clone of the golden rootfs, the path `init` takes. `snapshot-restore` loads the golden snapshot with the File memory backend. `run-3-branches` packs the workspace, restores three branches concurrently, and syncs the workspace into each; it stops before the user command.\n\n")
}

func (b *bench) pagerSection(w io.Writer) {
	fmt.Fprint(w, "## Pager strategies\n\n| Pager | Cache | Samples | First query ms | Restore ms | Faults (median) | MiB (median) |\n|---|---|---|---|---|---|---|\n")
	for _, p := range pagers {
		for _, cold := range []bool{true, false} {
			f := b.stat("pager", modeRestore, p.name, 1, cold, fFirst)
			fmt.Fprintf(w, "| %s | %s | %d | %s | %s | %.0f | %.1f |\n", p.name, cacheName(cold), f.n, cell(f),
				cell(b.stat("pager", modeRestore, p.name, 1, cold, fRestore)),
				b.stat("pager", modeRestore, p.name, 1, cold, fFaults).med,
				b.stat("pager", modeRestore, p.name, 1, cold, fBytes).med)
		}
	}
	fmt.Fprint(w, "\n`mmap` is a plain File restore: Firecracker maps the mem file privately and the kernel pages it in on fault. `fadvise` runs `posix_fadvise(POSIX_FADV_WILLNEED)` over the recorded working set first. `uffd` hands guest memory to a userfaultfd pager in the branchvm process, which resolves each fault with one `UFFDIO_COPY` of one page. Faults: pages the uffd pager served, or major faults of the firecracker process for mmap and fadvise. MiB: bytes copied for uffd, bytes advised for fadvise.\n\n")
}

func (b *bench) densitySection(w io.Writer) {
	fmt.Fprint(w, "## Density\n\n| Clones | Samples | Host RAM MiB | Per clone MiB | Private copies MiB | All serving ms |\n|---|---|---|---|---|---|\n")
	for _, n := range densitySizes {
		ram := b.stat("density", modeRestore, "", n, true, fRAM)
		if ram.n == 0 {
			continue
		}
		per := b.stat("density", modeRestore, "", n, true, fPer)
		fmt.Fprintf(w, "| %d | %d | %s | %.1f / %.1f | %d | %s |\n", n, ram.n, cell(ram), per.med, per.p95,
			int64(n)*b.cfg.memMB, cell(b.stat("density", modeRestore, "", n, true, fTotal)))
	}
	fmt.Fprintf(w, "\nHost RAM sums `memory.current` over the branch cgroups %s after every branch serves. Private copies is clones x %d MiB, the guest memory size, for comparison with a design that copies guest memory per clone.\n\n", settleDelay, b.cfg.memMB)
}

func (b *bench) methodology(w io.Writer, h hostInfo) {
	fmt.Fprintf(w, "## Methodology\n\n")
	for _, line := range []string{
		fmt.Sprintf("Host: %s, MemTotal %s, kernel %s, page size %d, %s.", h.cpu, h.mem, h.kernel, os.Getpagesize(), h.firecracker),
		fmt.Sprintf("Cold cache: before every cold sample the bench runs the equivalent of `%s` (sync(2), then writes 3 to /proc/sys/vm/drop_caches). A warm sample repeats the same mode immediately, without dropping.", dropCmd),
		fmt.Sprintf("Iterations: %d per mode and cache condition in the boot and pager tables; density: %s. Every density sample is cold, so golden mem file pages are charged to the live branch cgroups that read them instead of to an earlier, deleted cgroup.", b.cfg.iters, b.densityNote),
		"Restore ms: from the Firecracker API socket accepting connections to the guest agent answering /health with ok (cold boot: ok and pg_ready).",
		"First query ms: from just before pager setup and the snapshot load call (cold boot: from the API socket) to the first successful `pg_isready` inside the guest through the agent. The probe retries every 500 ms while Postgres refuses connections. Pager setup (fadvise calls, uffd socket and mmap) is inside this window.",
		"Total ms: the whole sample from netns creation to every branch serving, including cgroup, reflink clone, gateway, and firecracker start; for run-3-branches also the workspace pack and sync.",
		"Host RAM: `memory.current` of each branch cgroup. A page cache page of the shared golden mem file is charged once, to the first cgroup that read it, so the sum counts shared pages once and private copy-on-write pages per branch.",
		fmt.Sprintf("Working set: re-recorded before the pager table. Caches dropped, one File restore served its first query, then mincore(2) over the mem file: %d of %d pages, %d MiB of %d MiB. `init` also records ws.bin right after writing the snapshot, but freshly written pages are all cached, so that copy covers nearly the whole file.",
			b.ws.Resident, b.ws.Pages, b.ws.ResidentBytes()>>20, (b.ws.Pages*b.ws.PageSize)>>20),
		"Teardown: after every sample the bench removes every branch resource and scans for leftover netns, veths, cgroups, clone dirs, firecracker processes, and listening gateways. Any leftover aborts the bench.",
	} {
		fmt.Fprintf(w, "- %s\n", line)
	}
	fmt.Fprintln(w)
}

func pct(a, base float64) float64 {
	if base == 0 {
		return 0
	}
	return (a - base) / base * 100
}

func (b *bench) reading(w io.Writer) {
	med := func(table, mode, pager string, clones int, cold bool, f func(types.BenchRecord) float64) float64 {
		return b.stat(table, mode, pager, clones, cold, f).med
	}
	coldBoot := med("boot", modeColdBoot, "", 1, true, fRestore)
	restoreCold := med("boot", modeRestore, "", 1, true, fRestore)
	restoreWarm := med("boot", modeRestore, "", 1, false, fRestore)
	fmt.Fprintf(w, "## Reading the numbers\n\n")
	fmt.Fprintf(w, "A snapshot restore reached a serving guest in %.0f ms median with cold caches and %.0f ms warm, against %.0f ms for a cold boot of the same rootfs (%.1fx). End to end, with netns, cgroup, reflink clone, gateway, and firecracker start, one cold branch took %.0f ms. Three branches with a workspace sync were all ready in %.0f ms cold and %.0f ms warm.",
		restoreCold, restoreWarm, coldBoot, coldBoot/math.Max(restoreCold, 1),
		med("boot", modeRestore, "", 1, true, fTotal), med("boot", modeRun3, "", 3, true, fTotal), med("boot", modeRun3, "", 3, false, fTotal))
	if n := b.largestDensity(); n > 0 {
		fmt.Fprintf(w, " At %d clones the branch cgroups held %.0f MiB in total, %.1f MiB per clone, where private copies of guest memory would take %d MiB.",
			n, med("density", modeRestore, "", n, true, fRAM), med("density", modeRestore, "", n, true, fPer), int64(n)*b.cfg.memMB)
	}
	fmt.Fprint(w, "\n\n")

	first := map[string][2]float64{}
	for _, p := range pagers {
		first[p.name] = [2]float64{med("pager", modeRestore, p.name, 1, true, fFirst), med("pager", modeRestore, p.name, 1, false, fFirst)}
	}
	mm, fa, uf := first[pagerMmap], first[pagerFadvise], first[pagerUffd]
	fmt.Fprintf(w, "With cold caches the first query returned after %.0f ms on mmap, %.0f ms on fadvise (%.1f MiB advised), and %.0f ms on uffd (%.0f faults, %.1f MiB copied); %s was fastest. With a warm page cache: %.0f ms mmap, %.0f ms fadvise, %.0f ms uffd.",
		mm[0], fa[0], med("pager", modeRestore, pagerFadvise, 1, true, fBytes), uf[0],
		med("pager", modeRestore, pagerUffd, 1, true, fFaults), med("pager", modeRestore, pagerUffd, 1, true, fBytes),
		fastest(first, 0), mm[1], fa[1], uf[1])
	if fa[0] < mm[0] {
		fmt.Fprintf(w, " Prefaulting the recorded working set cut the cold first query by %.0f ms against plain mmap.", mm[0]-fa[0])
	} else {
		fmt.Fprintf(w, " Prefaulting did not beat plain mmap cold in this run (%.0f vs %.0f ms).", fa[0], mm[0])
	}
	if uf[1] > mm[1] {
		fmt.Fprintf(w, " UFFD loses on a warm cache, by %.0f ms (%+.0f%%): every first touch of a guest page is a fault round trip to the pager plus a page copy, while the mmap path maps pages already in the page cache with no copy. On a warm local SSD that overhead is pure cost; a pager only pays off where reading the mem file is slower than the round trip, such as remote or compressed snapshot storage, which this bench does not measure.",
			uf[1]-mm[1], pct(uf[1], mm[1]))
	} else {
		fmt.Fprintf(w, " UFFD did not lose to mmap on a warm cache in this run (%.0f vs %.0f ms).", uf[1], mm[1])
	}
	fmt.Fprintln(w)
}

func (b *bench) largestDensity() int {
	best := 0
	for _, r := range b.recs {
		if r.Table == "density" && r.Clones > best {
			best = r.Clones
		}
	}
	return best
}

func fastest(first map[string][2]float64, i int) string {
	best := ""
	for _, p := range pagers {
		if best == "" || first[p.name][i] < first[best][i] {
			best = p.name
		}
	}
	return best
}
