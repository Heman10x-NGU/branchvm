//go:build linux

// Package pool orchestrates branchvm: the golden snapshot, N restored
// branch VMs, branch inspection (diff, shell, logs, forward), promotion
// of one branch's diff, teardown, and verification.
package pool

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/Heman10x-NGU/branchvm/pkg/ptyclient"
	"github.com/Heman10x-NGU/branchvm/pkg/rootfs"
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
	headroomMB        = 256
	cpuMax            = "200000 100000"
	pidsMax           = 2048
	maxBranches       = 254
	goldenBootTimeout = 120 * time.Second
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

func goldenRootfs() string   { return filepath.Join(GoldenDir, RootfsName) }
func snapshotFile() string   { return filepath.Join(GoldenDir, "snapshot") }
func memFile() string        { return filepath.Join(GoldenDir, "mem") }
func wsFile() string         { return filepath.Join(GoldenDir, "ws.bin") }
func goldenMetaFile() string { return filepath.Join(GoldenDir, "meta.json") }

// PrefaultRestore is a File restore preceded by posix_fadvise(WILLNEED)
// over the pages recorded in golden/ws.bin.
const PrefaultRestore types.RestoreMode = "prefault-file"

// GoldenMemPath and WorkingSetPath expose the golden mem file and its
// recorded working set to the bench.
func GoldenMemPath() string  { return memFile() }
func WorkingSetPath() string { return wsFile() }

// BenchPreflight runs the checks Init and Run make before touching VMs.
func BenchPreflight(out io.Writer) error {
	if err := checkPrereqs(""); err != nil {
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
	Repo         string
	Image        string
	Dockerfile   string
	ExecdPath    string
	RootfsSizeMB int64
	MemMB        int64
	VCPUs        int
	WarmCmd      string
	Database     string
	Services     []types.ServiceSpec
	Env          []string
	Providers    []types.ProviderSpec
	Out          io.Writer
}

// Init builds or selects the rootfs, brings up the XFS pool, cold boots
// the golden VM, syncs the repo, checkpoints, writes the snapshot, and saves
// golden/meta.json.
func Init(ctx context.Context, opt InitOptions) error {
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.MemMB < 128 || opt.VCPUs < 1 || opt.VCPUs > 32 {
		return fmt.Errorf("init: need --mem >= 128 and 1 <= --cpus <= 32, got %d MiB, %d cpus", opt.MemMB, opt.VCPUs)
	}
	if opt.Services == nil {
		opt.Services = []types.ServiceSpec{}
	}

	mode := ""
	if opt.Image != "" || opt.Dockerfile != "" {
		mode = "oci"
	}
	if err := checkPrereqs(mode); err != nil {
		return err
	}
	if err := prepareHost(opt.Out); err != nil {
		return err
	}

	sourceRootfs := BaseRootfs
	var imageDigest string
	if mode == "oci" {
		res, err := rootfs.BuildFromImage(ctx, rootfs.BuildOptions{
			Image:        opt.Image,
			Dockerfile:   opt.Dockerfile,
			RepoDir:      opt.Repo,
			ExecdPath:    opt.ExecdPath,
			Services:     opt.Services,
			RootfsSizeMB: opt.RootfsSizeMB,
			Out:          opt.Out,
		})
		if err != nil {
			return fmt.Errorf("build rootfs from container image: %w", err)
		}
		sourceRootfs = res.RootfsPath
		imageDigest = res.ImageDigest
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cgroup.InstallCleanupTrap(cancel)
	start := time.Now()
	if err := prepareGoldenDisk(sourceRootfs, opt.Out); err != nil {
		return err
	}
	hostSHA, goldenFiles, err := storage.WorkspaceManifest(opt.Repo)
	if err != nil {
		return fmt.Errorf("manifest %s: %w", opt.Repo, err)
	}
	if err := bootGolden(ctx, opt); err != nil {
		return err
	}
	meta := types.GoldenMeta{
		RepoPath:    filepath.Clean(opt.Repo),
		HostSHA:     hostSHA,
		Image:       opt.Image,
		Dockerfile:  opt.Dockerfile,
		ImageDigest: imageDigest,
		MemMB:       opt.MemMB,
		VCPUs:       opt.VCPUs,
		Database:    opt.Database,
		Services:    opt.Services,
		Env:         opt.Env,
		Providers:   opt.Providers,
		GoldenFiles: goldenFiles,
	}
	if err := saveGoldenMeta(meta); err != nil {
		return err
	}
	fmt.Fprintf(opt.Out, "golden snapshot ready in %s (%d tracked/dirty files)\n",
		time.Since(start).Round(time.Millisecond), len(goldenFiles))
	for _, p := range []string{goldenRootfs(), snapshotFile(), memFile()} {
		fmt.Fprintln(opt.Out, "  "+sizeLine(p))
	}
	return nil
}

func checkPrereqs(mode string) error {
	checks := []struct{ path, hint string }{
		{KernelPath(), "run scripts/fetch-assets.sh first"},
	}
	if mode != "oci" {
		checks = append(checks, struct{ path, hint string }{BaseRootfs, "run scripts/build-rootfs.sh or set image/dockerfile in branchvm.toml"})
	}
	for _, p := range checks {
		if _, err := os.Stat(p.path); err != nil {
			return fmt.Errorf("%s: %w (%s)", p.path, err, p.hint)
		}
	}
	if _, err := exec.LookPath("firecracker"); err != nil {
		return fmt.Errorf("firecracker not in PATH (run scripts/fetch-assets.sh first): %w", err)
	}
	return nil
}

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
				return fmt.Errorf("run %s still has live VMs (branch %d, pid %d): inspect with `branchvm diff <i> --run %s`, promote with `branchvm apply <i> --run %s`, or discard with `branchvm gc --run %s`",
					r.RunID, b.Index, b.FirecrackerPID, r.RunID, r.RunID, r.RunID)
			}
		}
	}
	return nil
}

func clearGoldenSnapshotFiles(dir string) error {
	for _, name := range []string{"snapshot", "mem", "ws.bin", "meta.json"} {
		p := filepath.Join(dir, name)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale golden artifact %s: %w", p, err)
		}
	}
	return nil
}

func prepareGoldenDisk(sourceRootfs string, out io.Writer) error {
	if err := os.MkdirAll(GoldenDir, 0o755); err != nil {
		return fmt.Errorf("create golden dir: %w", err)
	}
	if err := clearGoldenSnapshotFiles(GoldenDir); err != nil {
		return err
	}
	dst := goldenRootfs()
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old golden rootfs: %w", err)
	}
	if err := storage.CloneRootfs(sourceRootfs, dst); err == nil {
		fmt.Fprintf(out, "golden rootfs: reflink of %s\n", sourceRootfs)
		return nil
	}
	if b, err := exec.Command("cp", "--sparse=always", sourceRootfs, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("copy base rootfs to %s: %w: %s", dst, err, strings.TrimSpace(string(b)))
	}
	fmt.Fprintf(out, "golden rootfs: sparse copy of %s\n", sourceRootfs)
	return nil
}

func saveGoldenMeta(meta types.GoldenMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode golden meta: %w", err)
	}
	tmp := goldenMetaFile() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write golden meta: %w", err)
	}
	if err := os.Rename(tmp, goldenMetaFile()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install golden meta: %w", err)
	}
	return nil
}

func loadGoldenMeta() (types.GoldenMeta, bool, error) {
	raw, err := os.ReadFile(goldenMetaFile())
	if errors.Is(err, os.ErrNotExist) {
		return types.GoldenMeta{
			MemMB:    1024,
			VCPUs:    2,
			Services: []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}},
		}, false, nil
	}
	if err != nil {
		return types.GoldenMeta{}, false, fmt.Errorf("read %s: %w", goldenMetaFile(), err)
	}
	var meta types.GoldenMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return types.GoldenMeta{}, false, fmt.Errorf("parse %s: %w", goldenMetaFile(), err)
	}
	if meta.Services == nil {
		meta.Services = []types.ServiceSpec{}
	}
	return meta, true, nil
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
	fmt.Fprintf(opt.Out, "booting golden VM (%s, %d MiB, %d vCPUs), waiting for services\n", KernelPath(), opt.MemMB, opt.VCPUs)
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
	body, ctype, err := storage.EncodeSyncMultipart(types.SyncMeta{Mode: "full"}, tgz)
	if err != nil {
		return fmt.Errorf("encode golden sync: %w", err)
	}
	if err := g.syncPayload(ctx, ctype, body); err != nil {
		return fmt.Errorf("golden sync (%d bytes): %w", len(body), err)
	}
	if opt.Database != "" && hasPostgres(opt.Services) {
		if err := ensureDatabase(ctx, g, opt.Database); err != nil {
			return err
		}
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

func recordWorkingSet(out io.Writer) {
	ws, err := storage.RecordWorkingSet(memFile(), wsFile())
	if err != nil {
		_ = os.Remove(wsFile())
		fmt.Fprintf(out, "warning: working set not recorded: %v\n", err)
		return
	}
	fmt.Fprintf(out, "working set: %d of %d pages resident (%d MiB) in %s\n", ws.Resident, ws.Pages, ws.ResidentBytes()>>20, wsFile())
}

func warmGolden(ctx context.Context, opt InitOptions, g *guest) error {
	if opt.WarmCmd == "" {
		return nil
	}
	fmt.Fprintf(opt.Out, "warming golden workspace: %s\n", opt.WarmCmd)
	start := time.Now()
	env := append([]string(nil), opt.Env...)
	env = append(env, "HOME=/root", "GOTOOLCHAIN=local")
	res, err := g.exec(ctx, types.ExecRequest{
		Cmd:        []string{"sh", "-c", opt.WarmCmd},
		Env:        env,
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

func ensureDatabase(ctx context.Context, g *guest, dbName string) error {
	script := fmt.Sprintf(`psql -h 127.0.0.1 -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='%[1]s'" | grep -q 1 || createdb -h 127.0.0.1 -U postgres %[1]s`, dbName)
	res, err := g.exec(ctx, types.ExecRequest{Cmd: []string{"sh", "-c", script}, TimeoutSec: 60})
	if err != nil {
		return fmt.Errorf("create database %s: %w", dbName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("create database %s: exit %d: %s", dbName, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

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
	Repo        string
	Branches    int
	MemMB       int64
	MemExplicit bool
	TimeoutSec  int
	RestoreMode types.RestoreMode
	Cmd         []string
	Out         io.Writer
}

type syncUpload struct {
	skip        bool
	contentType string
	body        []byte
	desc        string
}

func buildRunSync(repoDir string, meta types.GoldenMeta, hasMeta bool) (syncUpload, error) {
	if hasMeta && meta.GoldenFiles != nil {
		_, curFiles, err := storage.WorkspaceManifest(repoDir)
		if err != nil {
			return syncUpload{}, fmt.Errorf("manifest %s: %w", repoDir, err)
		}
		changed, deleted := storage.DiffManifests(meta.GoldenFiles, curFiles)
		if len(changed) == 0 && len(deleted) == 0 {
			return syncUpload{skip: true, desc: "delta: 0 changed, 0 deleted (sync skipped)"}, nil
		}
		var archive []byte
		if len(changed) > 0 {
			archive, err = storage.PackFiles(repoDir, changed)
			if err != nil {
				return syncUpload{}, fmt.Errorf("pack delta %s: %w", repoDir, err)
			}
		}
		body, ctype, err := storage.EncodeSyncMultipart(types.SyncMeta{Mode: "delta", Delete: deleted}, archive)
		if err != nil {
			return syncUpload{}, err
		}
		return syncUpload{
			contentType: ctype,
			body:        body,
			desc:        fmt.Sprintf("delta: %d changed, %d deleted, %d KiB", len(changed), len(deleted), len(body)>>10),
		}, nil
	}
	tgz, err := storage.PackWorkspace(repoDir)
	if err != nil {
		return syncUpload{}, fmt.Errorf("pack workspace %s: %w", repoDir, err)
	}
	body, ctype, err := storage.EncodeSyncMultipart(types.SyncMeta{Mode: "full"}, tgz)
	if err != nil {
		return syncUpload{}, err
	}
	return syncUpload{
		contentType: ctype,
		body:        body,
		desc:        fmt.Sprintf("full workspace %d KiB", len(body)>>10),
	}, nil
}

// Run restores N branches concurrently, syncs delta changes, runs Cmd in
// each, stores each diff and log, and leaves the VMs running for inspection
// and apply. Ctrl-C tears everything down.
func Run(ctx context.Context, opt RunOptions) error {
	if opt.Branches < 1 || opt.Branches > maxBranches || len(opt.Cmd) == 0 || opt.TimeoutSec < 1 {
		return fmt.Errorf("run: need 1 <= --branches <= %d, --timeout >= 1, and a command after --", maxBranches)
	}
	if err := prepareHost(opt.Out); err != nil {
		return err
	}
	if err := requireGolden(); err != nil {
		return err
	}
	meta, hasMeta, err := loadGoldenMeta()
	if err != nil {
		return err
	}
	if hasMeta && meta.RepoPath != "" && filepath.Clean(meta.RepoPath) != filepath.Clean(opt.Repo) {
		return fmt.Errorf("golden snapshot belongs to %s, not %s: run `branchvm init` in %s first",
			meta.RepoPath, opt.Repo, opt.Repo)
	}
	if !opt.MemExplicit && meta.MemMB >= 128 {
		opt.MemMB = meta.MemMB
	}
	if opt.MemMB < 128 {
		return fmt.Errorf("run: --mem must be >= 128, got %d", opt.MemMB)
	}

	syncData, err := buildRunSync(opt.Repo, meta, hasMeta)
	if err != nil {
		return err
	}
	bcfg := branchConfigFromMeta(meta)
	portLabel := "-"
	if hasPostgres(bcfg.Services) {
		portLabel = "5432"
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cgroup.InstallCleanupTrap(cancel)
	rec := newRecorder(newRunID(), opt.Branches, opt.Out)
	fmt.Fprintf(opt.Out, "run %s: %d branches, mem %d MiB, cmd %q, %s\n",
		rec.runID(), opt.Branches, opt.MemMB, opt.Cmd, syncData.desc)
	tab := newTable(opt.Out, opt.Branches, portLabel)
	branches := make([]*Branch, opt.Branches)
	var wg sync.WaitGroup
	for i := 1; i <= opt.Branches; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			branches[i-1] = runBranch(ctx, rec, tab, opt, bcfg, syncData, i)
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil {
		terr := teardownRun(rec.snapshot(), branches)
		return errors.Join(fmt.Errorf("interrupted: run %s torn down", rec.runID()), terr)
	}
	snap := rec.snapshot()
	printRunNext(opt.Out, rec.runID(), branches)
	if countPassed(snap.Branches) == 0 {
		return fmt.Errorf("run %s: 0 of %d branches passed", rec.runID(), len(snap.Branches))
	}
	return nil
}

func countPassed(branches []types.BranchState) int {
	n := 0
	for _, b := range branches {
		if b.Status == StatusPass {
			n++
		}
	}
	return n
}

func runBranch(ctx context.Context, rec *recorder, tab *table, opt RunOptions, bcfg BranchConfig, syncData syncUpload, i int) *Branch {
	tab.update(i, func(r *row) { r.status = StatusStarting })
	mode := opt.RestoreMode
	if mode == "" {
		mode = types.FileRestore
	}
	b, err := LaunchBranchConfigured(ctx, rec.runID(), i, opt.MemMB, mode, bcfg, rec.set)
	if err != nil {
		rec.setStatus(i, StatusError)
		tab.update(i, func(r *row) { r.status, r.note = StatusError, err.Error() })
		return nil
	}
	rec.set(b.State(StatusRunning))
	tab.update(i, func(r *row) { r.status, r.restoreMs, r.ramMB = StatusRunning, b.RestoreMs, b.MemMB() })
	status, note := execBranch(ctx, b, opt, syncData)
	rec.set(b.State(status))
	tab.update(i, func(r *row) { r.status, r.note, r.ramMB = status, note, b.MemMB() })
	return b
}

func execBranch(ctx context.Context, b *Branch, opt RunOptions, syncData syncUpload) (string, string) {
	if !syncData.skip {
		if err := b.SyncMultipart(ctx, syncData.contentType, syncData.body); err != nil {
			return StatusError, err.Error()
		}
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
	fmt.Fprintf(out, "  read logs:     branchvm logs <i> --run %s\n", runID)
	fmt.Fprintf(out, "  inspect diff:  branchvm diff <i> --run %s\n", runID)
	fmt.Fprintf(out, "  shell inside:  branchvm shell <i> --run %s\n", runID)
	fmt.Fprintf(out, "  forward port:  branchvm forward <i> <port> --run %s\n", runID)
	fmt.Fprintf(out, "  promote one:   branchvm apply <i> --run %s\n", runID)
	fmt.Fprintf(out, "  discard all:   branchvm gc --all\n")
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

type row struct {
	restoreMs, ramMB int64
	status, note     string
}

type table struct {
	mu    sync.Mutex
	out   io.Writer
	tty   bool
	port  string
	rows  []row
	drawn bool
}

const tableHeader = "BRANCH RESTORE_MS PORT    HOST_RAM_MB STATUS   NOTE"

func newTable(out io.Writer, n int, portLabel string) *table {
	t := &table{out: out, port: portLabel, rows: make([]row, n)}
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
	return fmt.Sprintf("%-6d %-10s %-7s %-11s %-8s %s", i, num(r.restoreMs), t.port, num(r.ramMB), r.status, note)
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
	st, bs, err := resolveBranch(opt.RunID, opt.Index)
	if err != nil {
		return err
	}
	patch, err := loadPatch(ctx, st.RunID, bs)
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

// DiffOptions configures Diff.
type DiffOptions struct {
	Index int
	RunID string
	Stat  bool
	Out   io.Writer
}

// Diff prints the diff of branch Index (live from the VM when running).
func Diff(ctx context.Context, opt DiffOptions) error {
	st, bs, err := resolveBranch(opt.RunID, opt.Index)
	if err != nil {
		return err
	}
	patch, err := loadPatch(ctx, st.RunID, bs)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(patch)) == 0 {
		fmt.Fprintf(opt.Out, "(no changes on branch %d of run %s)\n", opt.Index, st.RunID)
		return nil
	}
	if opt.Stat {
		return writeDiffStat(ctx, patch, opt.Out)
	}
	_, err = opt.Out.Write(patch)
	return err
}

func writeDiffStat(ctx context.Context, patch []byte, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "git", "apply", "--stat", "--summary")
	// Run outside any git worktree so git apply does not drop patch paths
	// when branchvm is invoked from a subdirectory of a parent repo.
	cmd.Dir = os.TempDir()
	cmd.Stdin = bytes.NewReader(patch)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("diff stat: %w", err)
	}
	return nil
}

// LogsOptions configures Logs.
type LogsOptions struct {
	Index int
	RunID string
	Out   io.Writer
}

// Logs prints the saved command stdout and stderr for branch Index.
func Logs(opt LogsOptions) error {
	st, bs, err := resolveBranch(opt.RunID, opt.Index)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(outputPath(st.RunID, bs.Index))
	if err != nil {
		return fmt.Errorf("read logs for branch %d of run %s: %w", bs.Index, st.RunID, err)
	}
	_, err = opt.Out.Write(raw)
	return err
}

// ShellOptions configures Shell.
type ShellOptions struct {
	Index      int
	RunID      string
	Cmd        []string
	TimeoutSec int
	Stdin      *os.File
	Stdout     io.Writer
}

func shellContext(ctx context.Context, cmd []string, timeoutSec int) (context.Context, context.CancelFunc) {
	if len(cmd) > 0 && timeoutSec > 0 {
		return context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	}
	return ctx, func() {}
}

// Shell opens an interactive PTY shell (or runs Cmd) inside a live branch VM
// with a temporary host LLM gateway attached for the session lifetime.
func Shell(ctx context.Context, opt ShellOptions) (int, error) {
	if opt.TimeoutSec < 0 {
		return 1, fmt.Errorf("shell: --timeout must be >= 0, got %d", opt.TimeoutSec)
	}
	st, bs, err := resolveBranch(opt.RunID, opt.Index)
	if err != nil {
		return 1, err
	}
	if !isFirecracker(bs.FirecrackerPID) {
		return 1, fmt.Errorf("branch %d of run %s is not running", opt.Index, st.RunID)
	}
	meta, _, err := loadGoldenMeta()
	if err != nil {
		return 1, err
	}

	b := &Branch{
		RunID: st.RunID,
		Index: bs.Index,
		Net: &netiso.Net{
			GatewayAddr:  fmt.Sprintf("%s:%d", netiso.HostIP(bs.Index), netiso.GatewayPort),
			GuestAPIAddr: fmt.Sprintf("%s:%d", netiso.NSVethIP(bs.Index), netiso.GuestAPIPort),
		},
		cfg: branchConfigFromMeta(meta),
	}
	if err := b.startGateway(); err != nil {
		return 1, err
	}
	defer b.stopGateway()

	shellCtx, cancel := shellContext(ctx, opt.Cmd, opt.TimeoutSec)
	defer cancel()

	env := b.Env()
	if term := os.Getenv("TERM"); term != "" {
		env = append(env, "TERM="+term)
	}
	code, err := ptyclient.Attach(shellCtx, b.Net.GuestAPIAddr, types.PTYRequest{
		Cmd: opt.Cmd,
		Env: env,
	}, opt.Stdin, opt.Stdout)
	if errors.Is(shellCtx.Err(), context.DeadlineExceeded) {
		return 124, fmt.Errorf("branch %d: shell command timed out after %ds", opt.Index, opt.TimeoutSec)
	}
	return code, err
}

// ForwardOptions configures Forward.
type ForwardOptions struct {
	Index     int
	RunID     string
	GuestPort int
	HostPort  int
	Out       io.Writer
}

// Forward exposes guest localhost:GuestPort on host 127.0.0.1:HostPort until canceled.
func Forward(ctx context.Context, opt ForwardOptions) error {
	st, bs, err := resolveBranch(opt.RunID, opt.Index)
	if err != nil {
		return err
	}
	if !isFirecracker(bs.FirecrackerPID) {
		return fmt.Errorf("branch %d of run %s is not running", opt.Index, st.RunID)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cgroup.InstallCleanupTrap(cancel)

	apiAddr := fmt.Sprintf("%s:%d", netiso.NSVethIP(bs.Index), netiso.GuestAPIPort)
	return ptyclient.Forward(ctx, apiAddr, opt.GuestPort, opt.HostPort, func(actual int) {
		fmt.Fprintf(opt.Out, "forwarding 127.0.0.1:%d -> branch %d (run %s) 127.0.0.1:%d (Ctrl-C to stop)\n",
			actual, bs.Index, st.RunID, opt.GuestPort)
	})
}

func resolveBranch(runID string, idx int) (types.RunState, types.BranchState, error) {
	st, err := resolveRun(runID)
	if err != nil {
		return types.RunState{}, types.BranchState{}, err
	}
	for _, b := range st.Branches {
		if b.Index == idx {
			return st, b, nil
		}
	}
	return types.RunState{}, types.BranchState{}, fmt.Errorf("run %s has no branch %d", st.RunID, idx)
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

// loadPatch queries the live guest /diff first so edits made inside
// `branchvm shell` are always reflected in `branchvm diff` and `apply`.
func loadPatch(ctx context.Context, runID string, bs types.BranchState) ([]byte, error) {
	if isFirecracker(bs.FirecrackerPID) {
		g := newGuest(fmt.Sprintf("%s:%d", netiso.NSVethIP(bs.Index), netiso.GuestAPIPort))
		patch, err := g.diff(ctx)
		if err != nil {
			return nil, fmt.Errorf("live diff for branch %d: %w", bs.Index, err)
		}
		_ = os.WriteFile(patchPath(runID, bs.Index), patch, 0o644)
		return patch, nil
	}
	patch, err := os.ReadFile(patchPath(runID, bs.Index))
	if err != nil {
		return nil, fmt.Errorf("read patch for branch %d: %w", bs.Index, err)
	}
	return patch, nil
}

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

// GCAll reaps dead runs and tears down all active runs cleanly.
func GCAll(out io.Writer) error {
	if err := GC(out, ""); err != nil {
		return err
	}
	runs, err := cgroup.List(RunsDir)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	for _, r := range runs {
		if r.RunID == goldenRun {
			continue
		}
		if err := GC(out, r.RunID); err != nil {
			return err
		}
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
