//go:build linux

// Package pool orchestrates branchvm: the golden snapshot, N restored
// branch VMs, promotion of one branch's diff, teardown, and verification.
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/cgroup"
	"github.com/Heman10x-NGU/branchvm/pkg/netiso"
	"github.com/Heman10x-NGU/branchvm/pkg/storage"
	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

// Host layout.
const (
	BaseDir    = "/var/lib/branchvm"
	AssetsDir  = BaseDir + "/assets"
	BaseRootfs = BaseDir + "/base/rootfs.ext4"
	PoolImage  = storage.DefaultPoolImage
	PoolMount  = storage.DefaultPoolMount
	GoldenDir  = PoolMount + "/golden"
	ClonesDir  = PoolMount + "/clones"
	RunsDir    = BaseDir + "/runs"
	RootfsName = "rootfs.ext4"
)

// Branch statuses recorded in run state and shown in the live table.
const (
	StatusPending  = "pending"
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusPass     = "pass"
	StatusFail     = "fail"
	StatusError    = "error"
)

const (
	poolSizeGB        = 20
	goldenRun         = "golden"
	databaseName      = "authdemo"
	headroomMB        = 256
	cpuMax            = "200000 100000"
	pidsMax           = 2048
	maxBranches       = 254
	goldenBootTimeout = 120 * time.Second
	pgPort            = 5432
)

// KernelPath is the guest kernel: an uncompressed Image on arm64, vmlinux elsewhere.
func KernelPath() string {
	if runtime.GOARCH == "arm64" {
		return filepath.Join(AssetsDir, "Image")
	}
	return filepath.Join(AssetsDir, "vmlinux")
}

func bootArgs() string {
	args := "console=ttyS0 reboot=k panic=1 pci=off"
	if runtime.GOARCH == "arm64" {
		args += " keep_bootcon"
	}
	return args
}

func goldenRootfs() string { return filepath.Join(GoldenDir, RootfsName) }
func snapshotFile() string { return filepath.Join(GoldenDir, "snapshot") }
func memFile() string      { return filepath.Join(GoldenDir, "mem") }
func wsFile() string       { return filepath.Join(GoldenDir, "ws.bin") }

// PrefaultRestore is a File restore preceded by posix_fadvise(WILLNEED)
// over the pages recorded in golden/ws.bin.
const PrefaultRestore types.RestoreMode = "prefault-file"

// GoldenMemPath and WorkingSetPath expose the golden mem file and its
// recorded working set to the bench.
func GoldenMemPath() string  { return memFile() }
func WorkingSetPath() string { return wsFile() }

// BenchPreflight runs the checks Init and Run make before touching VMs:
// assets present, pool mounted, crashed runs reaped, no live run, and a
// complete golden snapshot.
func BenchPreflight(out io.Writer) error {
	if err := checkPrereqs(); err != nil {
		return err
	}
	if err := prepareHost(out); err != nil {
		return err
	}
	return requireGolden()
}
func cloneDir(runID string, i int) string {
	return filepath.Join(ClonesDir, fmt.Sprintf("%s-%d", runID, i))
}
func patchPath(runID string, i int) string {
	return filepath.Join(RunsDir, fmt.Sprintf("%s-branch-%d.patch", runID, i))
}
func outputPath(runID string, i int) string {
	return filepath.Join(RunsDir, fmt.Sprintf("%s-branch-%d.out", runID, i))
}

func cloneReflink(src, dst string) error {
	if err := storage.CloneRootfs(src, dst); err != nil {
		return fmt.Errorf("reflink clone %s: %w", dst, err)
	}
	return nil
}

// newRunID is 7 chars so netiso's 15-byte veth names never truncate it.
func newRunID() string {
	for {
		id := fmt.Sprintf("r%06x", time.Now().Unix()&0xffffff)
		if _, err := os.Stat(filepath.Join(RunsDir, id+".json")); errors.Is(err, os.ErrNotExist) {
			return id
		}
		time.Sleep(time.Second)
	}
}

// InitOptions configures Init.
type InitOptions struct {
	Repo    string
	MemMB   int64
	VCPUs   int
	WarmCmd string
	Out     io.Writer
}

// Init brings up the XFS pool, cold boots the golden VM from a copy of the
// base rootfs, syncs the repo, checkpoints, and writes the golden snapshot.
func Init(ctx context.Context, opt InitOptions) error {
	if opt.MemMB < 128 || opt.VCPUs < 1 || opt.VCPUs > 32 {
		return fmt.Errorf("init: need --mem >= 128 and 1 <= --cpus <= 32, got %d MiB, %d cpus", opt.MemMB, opt.VCPUs)
	}
	if err := checkPrereqs(); err != nil {
		return err
	}
	if err := prepareHost(opt.Out); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cgroup.InstallCleanupTrap(cancel)
	start := time.Now()
	if err := prepareGoldenDisk(opt.Out); err != nil {
		return err
	}
	if err := bootGolden(ctx, opt); err != nil {
		return err
	}
	fmt.Fprintf(opt.Out, "golden snapshot ready in %s\n", time.Since(start).Round(time.Millisecond))
	for _, p := range []string{goldenRootfs(), snapshotFile(), memFile()} {
		fmt.Fprintln(opt.Out, "  "+sizeLine(p))
	}
	return nil
}

func checkPrereqs() error {
	for _, p := range []struct{ path, hint string }{
		{BaseRootfs, "run scripts/build-rootfs.sh first"},
		{KernelPath(), "run scripts/fetch-assets.sh first"},
	} {
		if _, err := os.Stat(p.path); err != nil {
			return fmt.Errorf("%s: %w (%s)", p.path, err, p.hint)
		}
	}
	if _, err := exec.LookPath("firecracker"); err != nil {
		return fmt.Errorf("firecracker not in PATH (run scripts/fetch-assets.sh first): %w", err)
	}
	return nil
}

// prepareHost mounts the pool, reaps crashed runs, and refuses to continue
// while another run still owns live VMs (branch IPs are per index).
func prepareHost(out io.Writer) error {
	if err := storage.EnsurePool(PoolImage, PoolMount, poolSizeGB); err != nil {
		return fmt.Errorf("xfs pool: %w", err)
	}
	rep, err := cgroup.GC(RunsDir, cgroup.GCOptions{Ops: cgroup.HostOps{}})
	if err != nil {
		return fmt.Errorf("gc: %w", err)
	}
	if rep.BranchesReaped > 0 || len(rep.Errors) > 0 {
		printGCReport(out, rep)
	}
	return refuseLiveRuns()
}

func refuseLiveRuns() error {
	runs, err := cgroup.List(RunsDir)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	for _, r := range runs {
		for _, b := range r.Branches {
			if isFirecracker(b.FirecrackerPID) {
				return fmt.Errorf("run %s still has live VMs (branch %d, pid %d): promote one with `branchvm apply <i> --run %s` or discard it with `branchvm gc --run %s`",
					r.RunID, b.Index, b.FirecrackerPID, r.RunID, r.RunID)
			}
		}
	}
	return nil
}

// prepareGoldenDisk copies the base rootfs into the pool. The base usually
// lives outside the XFS pool, where reflink cannot reach, so a sparse copy
// is the fallback; golden to clone copies are always reflinks.
func prepareGoldenDisk(out io.Writer) error {
	if err := os.MkdirAll(GoldenDir, 0o755); err != nil {
		return fmt.Errorf("create golden dir: %w", err)
	}
	dst := goldenRootfs()
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old golden rootfs: %w", err)
	}
	if err := storage.CloneRootfs(BaseRootfs, dst); err == nil {
		fmt.Fprintf(out, "golden rootfs: reflink of %s\n", BaseRootfs)
		return nil
	}
	if b, err := exec.Command("cp", "--sparse=always", BaseRootfs, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("copy base rootfs to %s: %w: %s", dst, err, strings.TrimSpace(string(b)))
	}
	fmt.Fprintf(out, "golden rootfs: sparse copy of %s (base is outside the XFS pool)\n", BaseRootfs)
	return nil
}

func bootGolden(ctx context.Context, opt InitOptions) (err error) {
	_ = netiso.Teardown(goldenRun, 0)
	n, err := netiso.Setup(goldenRun, 0)
	if err != nil {
		return fmt.Errorf("golden network: %w", err)
	}
	defer func() { err = errors.Join(err, netiso.Teardown(goldenRun, 0)) }()
	if err := pinTapMAC(n.NetNSName); err != nil {
		return err
	}
	v, err := startVM(n.NetNSName, GoldenDir)
	if err != nil {
		return err
	}
	st := types.RunState{RunID: goldenRun, StartedAt: time.Now().UTC(), Branches: []types.BranchState{{
		FirecrackerPID: v.pid(), NetNS: n.NetNSName, VethHost: n.VethHost, TapName: n.TapName, Status: goldenRun,
	}}}
	defer func() {
		v.kill()
		err = errors.Join(err, removeState(goldenRun))
	}()
	if err := cgroup.Save(RunsDir, st); err != nil {
		return fmt.Errorf("save golden state: %w", err)
	}
	if err := v.bootCold(opt.MemMB, opt.VCPUs, n.TapName); err != nil {
		return err
	}
	return snapshotGolden(ctx, opt, v, newGuest(n.GuestAPIAddr))
}

func snapshotGolden(ctx context.Context, opt InitOptions, v *vm, g *guest) error {
	fmt.Fprintf(opt.Out, "booting golden VM (%s, %d MiB, %d vCPUs), waiting for Postgres\n", KernelPath(), opt.MemMB, opt.VCPUs)
	last := time.Duration(0)
	progress := func(elapsed time.Duration, cause error) {
		if elapsed-last >= 5*time.Second {
			last = elapsed
			fmt.Fprintf(opt.Out, "  %2.0fs: not ready yet (%v)\n", elapsed.Seconds(), cause)
		}
	}
	if err := g.waitReady(ctx, goldenBootTimeout, 500*time.Millisecond, progress); err != nil {
		return fmt.Errorf("golden boot: %w", err)
	}
	tgz, err := storage.PackWorkspace(opt.Repo)
	if err != nil {
		return fmt.Errorf("pack workspace %s: %w", opt.Repo, err)
	}
	if err := g.sync(ctx, tgz); err != nil {
		return fmt.Errorf("golden sync (%d bytes): %w", len(tgz), err)
	}
	if err := ensureDatabase(ctx, g); err != nil {
		return err
	}
	if err := warmGolden(ctx, opt, g); err != nil {
		return err
	}
	if err := g.checkpoint(ctx); err != nil {
		return fmt.Errorf("golden checkpoint: %w", err)
	}
	if err := v.fc.PauseVM(); err != nil {
		return fmt.Errorf("pause golden VM: %w", err)
	}
	if err := writeSnapshot(v); err != nil {
		return err
	}
	recordWorkingSet(opt.Out)
	return nil
}

// recordWorkingSet saves golden/ws.bin from the page cache residency of
// the mem file Firecracker just wrote. Freshly written pages are all
// cached, so this approximates the whole file until `branchvm bench`
// re-records it from a cold restore. It is optional, so failure warns.
func recordWorkingSet(out io.Writer) {
	ws, err := storage.RecordWorkingSet(memFile(), wsFile())
	if err != nil {
		_ = os.Remove(wsFile())
		fmt.Fprintf(out, "warning: working set not recorded: %v\n", err)
		return
	}
	fmt.Fprintf(out, "working set: %d of %d pages resident (%d MiB) in %s\n", ws.Resident, ws.Pages, ws.ResidentBytes()>>20, wsFile())
}

// warmGolden runs an optional command in the golden /workspace before the
// checkpoint. Clones inherit its side effects (a hot Go build cache, warmed
// package data) through the CoW disk, so per-branch commands skip cold work
// that a 1 GiB guest pays for dearly.
func warmGolden(ctx context.Context, opt InitOptions, g *guest) error {
	if opt.WarmCmd == "" {
		return nil
	}
	fmt.Fprintf(opt.Out, "warming golden workspace: %s\n", opt.WarmCmd)
	start := time.Now()
	res, err := g.exec(ctx, types.ExecRequest{
		Cmd:        []string{"sh", "-c", opt.WarmCmd},
		Env:        []string{"HOME=/root", "GOTOOLCHAIN=local"},
		TimeoutSec: 900,
	})
	if err != nil {
		return fmt.Errorf("warm command: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("warm command: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	fmt.Fprintf(opt.Out, "warm done in %.1fs\n", time.Since(start).Seconds())
	return nil
}

func ensureDatabase(ctx context.Context, g *guest) error {
	script := fmt.Sprintf(`psql -h 127.0.0.1 -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='%[1]s'" | grep -q 1 || createdb -h 127.0.0.1 -U postgres %[1]s`, databaseName)
	res, err := g.exec(ctx, types.ExecRequest{Cmd: []string{"sh", "-c", script}, TimeoutSec: 60})
	if err != nil {
		return fmt.Errorf("create database %s: %w", databaseName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("create database %s: exit %d: %s", databaseName, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// writeSnapshot writes to temp names and renames, so a re-init gives the
// files fresh inodes instead of rewriting a mem file a clone has mapped.
func writeSnapshot(v *vm) error {
	snapTmp, memTmp := snapshotFile()+".tmp", memFile()+".tmp"
	for _, p := range []string{snapTmp, memTmp} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	if err := v.fc.CreateSnapshot(snapTmp, memTmp); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	if err := os.Rename(memTmp, memFile()); err != nil {
		return fmt.Errorf("install mem file: %w", err)
	}
	if err := os.Rename(snapTmp, snapshotFile()); err != nil {
		return fmt.Errorf("install snapshot file: %w", err)
	}
	return nil
}

func sizeLine(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("%s: %v", path, err)
	}
	alloc := int64(-1)
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		alloc = st.Blocks * 512
	}
	return fmt.Sprintf("%-42s %6d MiB apparent %6d MiB allocated", path, fi.Size()>>20, alloc>>20)
}

func requireGolden() error {
	for _, p := range []string{snapshotFile(), memFile(), goldenRootfs()} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("golden snapshot incomplete (%s): run `branchvm init` first: %w", p, err)
		}
	}
	return nil
}

// RunOptions configures Run.
type RunOptions struct {
	Repo       string
	Branches   int
	MemMB      int64
	TimeoutSec int
	Cmd        []string
	Out        io.Writer
}

// Run restores N branches concurrently, runs Cmd in each, stores each diff,
// and leaves the VMs running for apply. Ctrl-C tears everything down.
func Run(ctx context.Context, opt RunOptions) error {
	if opt.Branches < 1 || opt.Branches > maxBranches || len(opt.Cmd) == 0 || opt.MemMB < 128 || opt.TimeoutSec < 1 {
		return fmt.Errorf("run: need 1 <= --branches <= %d, --mem >= 128, --timeout >= 1, and a command after --", maxBranches)
	}
	if err := prepareHost(opt.Out); err != nil {
		return err
	}
	if err := requireGolden(); err != nil {
		return err
	}
	tgz, err := storage.PackWorkspace(opt.Repo)
	if err != nil {
		return fmt.Errorf("pack workspace %s: %w", opt.Repo, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cgroup.InstallCleanupTrap(cancel)
	rec := newRecorder(newRunID(), opt.Branches, opt.Out)
	fmt.Fprintf(opt.Out, "run %s: %d branches, cmd %q, workspace %d KiB\n", rec.runID(), opt.Branches, opt.Cmd, len(tgz)>>10)
	tab := newTable(opt.Out, opt.Branches)
	branches := make([]*Branch, opt.Branches)
	var wg sync.WaitGroup
	for i := 1; i <= opt.Branches; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			branches[i-1] = runBranch(ctx, rec, tab, opt, tgz, i)
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil {
		terr := teardownRun(rec.snapshot(), branches)
		return errors.Join(fmt.Errorf("interrupted: run %s torn down", rec.runID()), terr)
	}
	printRunNext(opt.Out, rec.runID(), branches)
	return nil
}

func runBranch(ctx context.Context, rec *recorder, tab *table, opt RunOptions, tgz []byte, i int) *Branch {
	tab.update(i, func(r *row) { r.status = StatusStarting })
	b, err := launchBranch(ctx, rec.runID(), i, opt.MemMB, rec.set)
	if err != nil {
		rec.setStatus(i, StatusError)
		tab.update(i, func(r *row) { r.status, r.note = StatusError, err.Error() })
		return nil
	}
	rec.set(b.State(StatusRunning))
	tab.update(i, func(r *row) { r.status, r.restoreMs, r.ramMB = StatusRunning, b.RestoreMs, b.MemMB() })
	status, note := execBranch(ctx, b, opt, tgz)
	rec.set(b.State(status))
	tab.update(i, func(r *row) { r.status, r.note, r.ramMB = status, note, b.MemMB() })
	return b
}

func execBranch(ctx context.Context, b *Branch, opt RunOptions, tgz []byte) (string, string) {
	if err := b.Guest.sync(ctx, tgz); err != nil {
		return StatusError, err.Error()
	}
	res, err := b.Exec(ctx, opt.Cmd, opt.TimeoutSec)
	if err != nil {
		return StatusError, err.Error()
	}
	status := StatusPass
	if res.ExitCode != 0 {
		status = StatusFail
	}
	note := fmt.Sprintf("exit %d in %dms", res.ExitCode, res.DurationMs)
	out := fmt.Sprintf("$ %s\n--- stdout\n%s\n--- stderr\n%s\n", strings.Join(opt.Cmd, " "), res.Stdout, res.Stderr)
	if err := os.WriteFile(outputPath(b.RunID, b.Index), []byte(out), 0o644); err != nil {
		note += ", save output: " + err.Error()
	}
	if last := lastLine(res.Stdout); last != "" {
		note += ": " + last
	}
	patch, err := b.Guest.diff(ctx)
	if err == nil {
		err = os.WriteFile(patchPath(b.RunID, b.Index), patch, 0o644)
	}
	if err != nil {
		return status, note + ", diff: " + err.Error()
	}
	return status, fmt.Sprintf("%s (diff %d bytes)", note, len(patch))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func printRunNext(out io.Writer, runID string, branches []*Branch) {
	live := 0
	for _, b := range branches {
		if b != nil {
			live++
		}
	}
	prefix := filepath.Join(RunsDir, runID+"-branch-<i>")
	fmt.Fprintf(out, "\nrun %s: %d VMs left running; patches %s.patch, output %s.out\n", runID, live, prefix, prefix)
	fmt.Fprintf(out, "  promote one: branchvm apply <i> --run %s   (from the repo dir, or pass --repo)\n", runID)
	fmt.Fprintf(out, "  discard all: branchvm gc --run %s\n", runID)
}

// recorder persists run state after every change for crash safety.
type recorder struct {
	mu   sync.Mutex
	st   types.RunState
	warn io.Writer
}

func newRecorder(runID string, n int, warn io.Writer) *recorder {
	st := types.RunState{RunID: runID, StartedAt: time.Now().UTC(), Branches: make([]types.BranchState, n)}
	for i := range st.Branches {
		st.Branches[i] = types.BranchState{Index: i + 1, Status: StatusPending}
	}
	r := &recorder{st: st, warn: warn}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveLocked()
	return r
}

func (r *recorder) runID() string { return r.st.RunID }

func (r *recorder) set(bs types.BranchState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.Branches[bs.Index-1] = bs
	r.saveLocked()
}

func (r *recorder) setStatus(i int, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.Branches[i-1].Status = status
	r.saveLocked()
}

func (r *recorder) saveLocked() {
	if err := cgroup.Save(RunsDir, r.st); err != nil {
		fmt.Fprintf(r.warn, "warning: save run state: %v\n", err)
	}
}

func (r *recorder) snapshot() types.RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.st
	st.Branches = append([]types.BranchState(nil), r.st.Branches...)
	return st
}

// table prints the live branch table: redrawn in place on a terminal,
// one log line per change otherwise.
type row struct {
	restoreMs, ramMB int64
	status, note     string
}

type table struct {
	mu    sync.Mutex
	out   io.Writer
	tty   bool
	rows  []row
	drawn bool
}

const tableHeader = "BRANCH RESTORE_MS PG_PORT HOST_RAM_MB STATUS   NOTE"

func newTable(out io.Writer, n int) *table {
	t := &table{out: out, rows: make([]row, n)}
	if f, ok := out.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			t.tty = true
		}
	}
	if !t.tty {
		fmt.Fprintln(out, tableHeader)
	}
	return t
}

func (t *table) update(i int, fn func(*row)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(&t.rows[i-1])
	if !t.tty {
		fmt.Fprintln(t.out, t.line(i))
		return
	}
	if t.drawn {
		fmt.Fprintf(t.out, "\033[%dA", len(t.rows)+1)
	}
	fmt.Fprintf(t.out, "\033[2K%s\n", tableHeader)
	for j := range t.rows {
		fmt.Fprintf(t.out, "\033[2K%s\n", t.line(j+1))
	}
	t.drawn = true
}

func (t *table) line(i int) string {
	r := t.rows[i-1]
	num := func(v int64) string {
		if v <= 0 {
			return "-"
		}
		return strconv.FormatInt(v, 10)
	}
	note := r.note
	if t.tty && len(note) > 70 {
		note = note[:67] + "..."
	}
	return fmt.Sprintf("%-6d %-10s %-7d %-11s %-8s %s", i, num(r.restoreMs), pgPort, num(r.ramMB), r.status, note)
}

// ApplyOptions configures Apply.
type ApplyOptions struct {
	Index int
	RunID string
	Repo  string
	Out   io.Writer
}

// Apply applies one branch's diff to the host repo, then tears down the
// whole run and scans for leaks.
func Apply(ctx context.Context, opt ApplyOptions) error {
	st, err := resolveRun(opt.RunID)
	if err != nil {
		return err
	}
	var bs *types.BranchState
	for i := range st.Branches {
		if st.Branches[i].Index == opt.Index {
			bs = &st.Branches[i]
		}
	}
	if bs == nil {
		return fmt.Errorf("run %s has no branch %d", st.RunID, opt.Index)
	}
	patch, err := loadPatch(ctx, st.RunID, *bs)
	if err != nil {
		return err
	}
	if err := storage.ApplyPatch(opt.Repo, patch); err != nil {
		return fmt.Errorf("apply branch %d of run %s to %s (run left intact): %w", opt.Index, st.RunID, opt.Repo, err)
	}
	fmt.Fprintf(opt.Out, "applied branch %d of run %s to %s (%d byte patch)\n", opt.Index, st.RunID, opt.Repo, len(patch))
	kept := patchPath(st.RunID, opt.Index) + ".applied"
	if err := os.WriteFile(kept, patch, 0o644); err != nil {
		return fmt.Errorf("keep applied patch: %w", err)
	}
	if err := teardownRun(st, nil); err != nil {
		return fmt.Errorf("teardown run %s: %w", st.RunID, err)
	}
	if err := removeRunFiles(st.RunID); err != nil {
		return err
	}
	fmt.Fprintf(opt.Out, "run %s torn down; applied patch kept at %s\n", st.RunID, kept)
	return reportLeaks(opt.Out, gatewayAddrs(st))
}

func resolveRun(runID string) (types.RunState, error) {
	if runID != "" {
		return cgroup.Load(RunsDir, runID)
	}
	runs, err := cgroup.List(RunsDir)
	if err != nil {
		return types.RunState{}, fmt.Errorf("list runs: %w", err)
	}
	var best *types.RunState
	for i := range runs {
		if runs[i].RunID != goldenRun && (best == nil || runs[i].StartedAt.After(best.StartedAt)) {
			best = &runs[i]
		}
	}
	if best == nil {
		return types.RunState{}, fmt.Errorf("no runs in %s", RunsDir)
	}
	return *best, nil
}

func loadPatch(ctx context.Context, runID string, bs types.BranchState) ([]byte, error) {
	patch, err := os.ReadFile(patchPath(runID, bs.Index))
	if err == nil {
		return patch, nil
	}
	if !errors.Is(err, os.ErrNotExist) || !isFirecracker(bs.FirecrackerPID) {
		return nil, fmt.Errorf("read patch for branch %d: %w", bs.Index, err)
	}
	g := newGuest(fmt.Sprintf("%s:%d", netiso.NSVethIP(bs.Index), netiso.GuestAPIPort))
	patch, err = g.diff(ctx)
	if err != nil {
		return nil, fmt.Errorf("live diff for branch %d: %w", bs.Index, err)
	}
	return patch, nil
}

// teardownRun stops in-process gateways and VMs, removes every branch's
// resources, and deletes the run state file.
func teardownRun(st types.RunState, live []*Branch) error {
	for _, b := range live {
		if b != nil {
			b.stopGateway()
			b.VM.kill()
		}
	}
	var errs []error
	for _, bs := range st.Branches {
		errs = append(errs, teardownState(bs))
	}
	errs = append(errs, removeState(st.RunID))
	return errors.Join(errs...)
}

func removeState(runID string) error {
	if err := os.Remove(filepath.Join(RunsDir, runID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove run state %s: %w", runID, err)
	}
	return nil
}

func removeRunFiles(runID string) error {
	for _, pattern := range []string{runID + "-branch-*.patch", runID + "-branch-*.out"} {
		matches, err := filepath.Glob(filepath.Join(RunsDir, pattern))
		if err != nil {
			return fmt.Errorf("glob %s: %w", pattern, err)
		}
		for _, m := range matches {
			if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove %s: %w", m, err)
			}
		}
	}
	return nil
}

// GC reaps crashed runs. With runID set it discards that run, live VMs included.
func GC(out io.Writer, runID string) error {
	if runID != "" {
		st, err := cgroup.Load(RunsDir, runID)
		if err != nil {
			return err
		}
		if err := teardownRun(st, nil); err != nil {
			return fmt.Errorf("discard run %s: %w", runID, err)
		}
		if err := removeRunFiles(runID); err != nil {
			return err
		}
		fmt.Fprintf(out, "run %s discarded\n", runID)
		return reportLeaks(out, gatewayAddrs(st))
	}
	rep, err := cgroup.GC(RunsDir, cgroup.GCOptions{Ops: cgroup.HostOps{}})
	if err != nil {
		return fmt.Errorf("gc: %w", err)
	}
	printGCReport(out, rep)
	if len(rep.Errors) > 0 {
		return fmt.Errorf("gc: %d errors", len(rep.Errors))
	}
	return nil
}

func printGCReport(out io.Writer, r cgroup.GCReport) {
	fmt.Fprintf(out, "gc: reaped %d branches (%d alive), netns %d, veths %d, cgroups %d, clone dirs %d, pids killed %d, runs deleted %d\n",
		r.BranchesReaped, r.BranchesAlive, r.NetNS, r.Veths, r.Cgroups, r.CloneDirs, r.PIDsKilled, r.RunsDeleted)
	for _, e := range r.Errors {
		fmt.Fprintf(out, "gc error: %s\n", e)
	}
}

func gatewayAddrs(st types.RunState) []string {
	addrs := make([]string, 0, len(st.Branches))
	for _, b := range st.Branches {
		addrs = append(addrs, fmt.Sprintf("%s:%d", netiso.HostIP(b.Index), netiso.GatewayPort))
	}
	return addrs
}
