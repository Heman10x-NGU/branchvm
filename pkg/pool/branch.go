//go:build linux

package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/cgroup"
	"github.com/Heman10x-NGU/branchvm/pkg/fcclient"
	"github.com/Heman10x-NGU/branchvm/pkg/gateway"
	"github.com/Heman10x-NGU/branchvm/pkg/netiso"
	"github.com/Heman10x-NGU/branchvm/pkg/storage"
	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

const (
	uffdSockName  = "uffd.sock"
	coldVCPUs     = 2
	sockName      = "firecracker.sock"
	logName       = "firecracker.log"
	guestMAC      = "AA:FC:00:00:00:01"
	tapMAC        = "06:00:ac:10:00:01"
	socketTimeout = 5 * time.Second
	killTimeout   = 5 * time.Second
	maxGuestBody  = 256 << 20
)

// RestoreTimeout caps how long a restored guest may take to serve /health.
// Many concurrent cold restores stampede one disk, so the bench raises it;
// the measured first-query time, not this cap, is the reported number.
var RestoreTimeout = 30 * time.Second

// BranchConfig holds per-snapshot runtime environment, service, and gateway
// provider settings passed to restored branch VMs.
type BranchConfig struct {
	Services  []types.ServiceSpec
	Env       []string
	Providers []types.ProviderSpec
}

func defaultBranchConfig() BranchConfig {
	return BranchConfig{
		Services: []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}},
	}
}

func branchConfigFromMeta(meta types.GoldenMeta) BranchConfig {
	svcs := meta.Services
	if svcs == nil {
		svcs = []types.ServiceSpec{}
	}
	return BranchConfig{
		Services:  svcs,
		Env:       meta.Env,
		Providers: meta.Providers,
	}
}

func hasPostgres(services []types.ServiceSpec) bool {
	for _, s := range services {
		if s.Preset == "postgres" || s.Name == "postgres" {
			return true
		}
	}
	return false
}

// vm is one Firecracker process running inside a branch netns.
type vm struct {
	cmd  *exec.Cmd
	fc   *fcclient.Client
	done chan struct{}
}

// startVM launches firecracker in netns ns with cwd dir, so the relative
// drive path "rootfs.ext4" resolves to dir/rootfs.ext4.
func startVM(ns, dir string) (*vm, error) {
	sock := filepath.Join(dir, sockName)
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", sock, err)
	}
	logPath := filepath.Join(dir, logName)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open firecracker log: %w", err)
	}
	cmd := netiso.RunInNS(ns, []string{"firecracker", "--api-sock", sock})
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = logf, logf
	// A new session keeps a later terminal Ctrl-C away from VMs left running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("start firecracker in %s: %w", ns, err)
	}
	v := &vm{cmd: cmd, fc: fcclient.New(sock), done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		logf.Close()
		close(v.done)
	}()
	if err := v.waitSocket(sock); err != nil {
		v.kill()
		return nil, fmt.Errorf("firecracker in %s (log %s): %w", ns, logPath, err)
	}
	return v, nil
}

func (v *vm) waitSocket(sock string) error {
	deadline := time.Now().Add(socketTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-v.done:
			return errors.New("exited before its API socket appeared")
		default:
		}
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return fmt.Errorf("API socket %s not ready after %s", sock, socketTimeout)
}

func (v *vm) pid() int { return v.cmd.Process.Pid }

func (v *vm) kill() {
	_ = v.cmd.Process.Kill()
	select {
	case <-v.done:
	case <-time.After(killTimeout):
	}
}

// bootCold configures and starts a fresh microVM. No balloon device is ever
// attached: File-backed restores must keep every guest page mapped.
func (v *vm) bootCold(memMB int64, cpus int, tap string) error {
	steps := []struct {
		name string
		call func() error
	}{
		{"machine-config", func() error {
			return v.fc.PutMachineConfig(fcclient.MachineConfig{VcpuCount: cpus, MemSizeMib: memMB, Smt: false})
		}},
		{"boot-source", func() error {
			return v.fc.PutBootSource(fcclient.BootSource{KernelImagePath: KernelPath(), BootArgs: bootArgs()})
		}},
		{"drive", func() error {
			return v.fc.PutDrive(fcclient.Drive{DriveID: "rootfs", PathOnHost: RootfsName, IsRootDevice: true})
		}},
		{"network-interface", func() error {
			return v.fc.PutNetworkInterface(fcclient.NetworkInterface{IfaceID: "eth0", HostDevName: tap, GuestMAC: guestMAC})
		}},
		{"start", v.fc.StartInstance},
	}
	for _, s := range steps {
		if err := s.call(); err != nil {
			return fmt.Errorf("cold boot %s: %w", s.name, err)
		}
	}
	return nil
}

// pinTapMAC gives every tap the same MAC. Restored guests keep the golden
// guest's ARP entry for their gateway, so a random tap MAC would make the
// netns drop every reply frame until that entry expires.
func pinTapMAC(ns string) error {
	out, err := exec.Command("ip", "-n", ns, "link", "set", "dev", netiso.TapName, "address", tapMAC).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pin tap MAC in %s: %w: %s", ns, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// guest talks to execd inside one VM through the netns DNAT address.
type guest struct{ base string }

var guestHTTP = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

func newGuest(apiAddr string) *guest { return &guest{base: "http://" + apiAddr} }

func (g *guest) do(ctx context.Context, method, path, ctype string, body io.Reader, out any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("guest %s %s: %w", method, path, err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := guestHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("guest %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGuestBody))
	if err != nil {
		return nil, fmt.Errorf("guest %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("guest %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("guest %s %s: decode: %w", method, path, err)
		}
	}
	return raw, nil
}

func withTimeout(ctx context.Context, d time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return fn(ctx)
}

func (g *guest) health(ctx context.Context) (types.HealthResponse, error) {
	var h types.HealthResponse
	err := withTimeout(ctx, time.Second, func(ctx context.Context) error {
		_, err := g.do(ctx, http.MethodGet, "/health", "", nil, &h)
		return err
	})
	return h, err
}

// waitReady polls /health until execd and all configured services report ready.
func (g *guest) waitReady(ctx context.Context, timeout, interval time.Duration, progress func(time.Duration, error)) error {
	start := time.Now()
	for {
		h, err := g.health(ctx)
		if err == nil && h.OK && (h.ServicesReady || (h.PgReady && len(h.Services) == 0)) {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("ok=%t services_ready=%t pg_ready=%t", h.OK, h.ServicesReady, h.PgReady)
		}
		if progress != nil {
			progress(time.Since(start), err)
		}
		if time.Since(start) > timeout {
			return fmt.Errorf("guest %s not ready after %s: %w", g.base, timeout, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for guest %s: %w", g.base, ctx.Err())
		case <-time.After(interval):
		}
	}
}

func (g *guest) entropy(ctx context.Context) (types.EntropyResponse, error) {
	var e types.EntropyResponse
	err := withTimeout(ctx, 10*time.Second, func(ctx context.Context) error {
		_, err := g.do(ctx, http.MethodGet, "/entropy", "", nil, &e)
		return err
	})
	return e, err
}

func (g *guest) sync(ctx context.Context, tgz []byte) error {
	return g.syncPayload(ctx, "application/gzip", tgz)
}

func (g *guest) syncPayload(ctx context.Context, contentType string, payload []byte) error {
	return withTimeout(ctx, 5*time.Minute, func(ctx context.Context) error {
		_, err := g.do(ctx, http.MethodPost, "/sync", contentType, bytes.NewReader(payload), nil)
		return err
	})
}

func (g *guest) exec(ctx context.Context, req types.ExecRequest) (types.ExecResponse, error) {
	var res types.ExecResponse
	body, err := json.Marshal(req)
	if err != nil {
		return res, fmt.Errorf("encode exec request: %w", err)
	}
	limit := time.Duration(max(req.TimeoutSec, 600)+30) * time.Second
	err = withTimeout(ctx, limit, func(ctx context.Context) error {
		_, err := g.do(ctx, http.MethodPost, "/exec", "application/json", bytes.NewReader(body), &res)
		return err
	})
	return res, err
}

func (g *guest) diff(ctx context.Context) ([]byte, error) {
	var patch []byte
	err := withTimeout(ctx, 2*time.Minute, func(ctx context.Context) error {
		var err error
		patch, err = g.do(ctx, http.MethodGet, "/diff", "", nil, nil)
		return err
	})
	return patch, err
}

func (g *guest) checkpoint(ctx context.Context) error {
	return withTimeout(ctx, 2*time.Minute, func(ctx context.Context) error {
		_, err := g.do(ctx, http.MethodPost, "/checkpoint", "application/json", nil, nil)
		return err
	})
}

// Branch is one restored clone with all of its host resources.
type Branch struct {
	RunID     string
	Index     int
	Dir       string
	Net       *netiso.Net
	Cage      *cgroup.Cage
	VM        *vm
	Guest     *guest
	RestoreMs int64
	cfg       BranchConfig
	gwCancel  context.CancelFunc
	gwDone    chan error

	// Bench seam. Mode empty means FileRestore. FirstQueryMs runs from
	// pager setup and LoadSnapshot (or cold boot start) to the first
	// successful service readiness round trip.
	Mode          types.RestoreMode
	FirstQueryMs  int64
	PrefaultBytes int64
	memMB         int64
	pager         *storage.UffdServer
	pagerErr      chan error
}

// Sync uploads a legacy gzip tarball to the branch's /workspace.
func (b *Branch) Sync(ctx context.Context, tgz []byte) error { return b.Guest.sync(ctx, tgz) }

// SyncMultipart uploads a multipart delta/full sync payload to /sync.
func (b *Branch) SyncMultipart(ctx context.Context, contentType string, body []byte) error {
	return b.Guest.syncPayload(ctx, contentType, body)
}

// PagerStats reports faults served and bytes copied by the uffd pager.
func (b *Branch) PagerStats() (faults, bytes int64) {
	if b.pager == nil {
		return 0, 0
	}
	return b.pager.Stats()
}

// DummyKey is the per-branch key the guest presents to its gateway.
func DummyKey(i int) string { return fmt.Sprintf("bvm_dummy_%d", i) }

// State names every resource deterministically so a crash at any point
// leaves enough on disk for GC to reap it.
func (b *Branch) State(status string) types.BranchState {
	st := types.BranchState{
		Index:      b.Index,
		NetNS:      netiso.NetNSName(b.RunID, b.Index),
		VethHost:   netiso.VethHostName(b.RunID, b.Index),
		TapName:    netiso.TapName,
		CgroupPath: filepath.Join(cgroup.Root, fmt.Sprintf("%s-%d", b.RunID, b.Index)),
		CloneDir:   b.Dir,
		Status:     status,
	}
	if b.VM != nil {
		st.FirecrackerPID = b.VM.pid()
	}
	return st
}

// Env is the environment injected into every command run in the branch.
func (b *Branch) Env() []string {
	gw := "http://" + b.Net.GatewayAddr
	key := DummyKey(b.Index)
	env := make([]string, 0, len(b.cfg.Env)+16+len(b.cfg.Providers)*2)
	env = append(env, b.cfg.Env...)
	env = append(env,
		"HOME=/root",
		fmt.Sprintf("BRANCH_INDEX=%d", b.Index),
		"ANTHROPIC_BASE_URL="+gw+"/"+gateway.ProviderAnthropic,
		"OPENAI_BASE_URL="+gw+"/"+gateway.ProviderOpenAI+"/v1",
		"OPENROUTER_BASE_URL="+gw+"/"+gateway.ProviderOpenRouter+"/v1",
		"ANTHROPIC_API_KEY="+key,
		"ANTHROPIC_AUTH_TOKEN="+key,
		"OPENAI_API_KEY="+key,
		"CODEX_API_KEY="+key,
		"OPENROUTER_API_KEY="+key,
		"IS_SANDBOX=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_AUTOUPDATER=1",
	)
	for _, p := range b.cfg.Providers {
		if p.BaseURLEnv != "" {
			env = append(env, p.BaseURLEnv+"="+gw+"/"+p.Name)
		}
		if p.KeyEnv != "" {
			env = append(env, p.KeyEnv+"="+key)
		}
	}
	return env
}

// Exec runs argv in the branch with the branch environment.
func (b *Branch) Exec(ctx context.Context, argv []string, timeoutSec int) (types.ExecResponse, error) {
	return b.Guest.exec(ctx, types.ExecRequest{Cmd: argv, Env: b.Env(), TimeoutSec: timeoutSec})
}

// MemMB reads the cage's memory.current, or -1 when unavailable.
func (b *Branch) MemMB() int64 {
	if b.Cage == nil {
		return -1
	}
	mb, err := b.Cage.CurrentMemMB()
	if err != nil {
		return -1
	}
	return mb
}

// LaunchBranch builds branch i of runID and restores the golden snapshot
// into it using default BranchConfig. The bench drives all restore modes here.
func LaunchBranch(ctx context.Context, runID string, i int, memMB int64, mode types.RestoreMode, record func(types.BranchState)) (*Branch, error) {
	return LaunchBranchConfigured(ctx, runID, i, memMB, mode, defaultBranchConfig(), record)
}

// LaunchBranchConfigured builds branch i with an explicit BranchConfig.
func LaunchBranchConfigured(ctx context.Context, runID string, i int, memMB int64, mode types.RestoreMode, cfg BranchConfig, record func(types.BranchState)) (*Branch, error) {
	if i < 1 || i > maxBranches {
		return nil, fmt.Errorf("branch index %d outside 1..%d", i, maxBranches)
	}
	b := &Branch{RunID: runID, Index: i, Dir: cloneDir(runID, i), Mode: mode, cfg: cfg}
	record(b.State(StatusStarting))
	if err := b.launch(ctx, memMB, record); err != nil {
		if terr := b.Teardown(); terr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup: %w", terr))
		}
		return nil, fmt.Errorf("branch %d: %w", i, err)
	}
	return b, nil
}

func (b *Branch) launch(ctx context.Context, memMB int64, record func(types.BranchState)) error {
	n, err := netiso.Setup(b.RunID, b.Index)
	if err != nil {
		return fmt.Errorf("network: %w", err)
	}
	b.Net = n
	if err := pinTapMAC(n.NetNSName); err != nil {
		return err
	}
	cage, err := cgroup.New(b.RunID, b.Index, (memMB+headroomMB)<<20, cpuMax, pidsMax)
	if err != nil {
		return fmt.Errorf("cgroup: %w", err)
	}
	b.Cage = cage
	b.memMB = memMB
	if err := cloneReflink(goldenRootfs(), filepath.Join(b.Dir, RootfsName)); err != nil {
		return err
	}
	if err := b.startGateway(); err != nil {
		return err
	}
	v, err := startVM(n.NetNSName, b.Dir)
	if err != nil {
		return err
	}
	b.VM = v
	record(b.State(StatusStarting))
	return b.restore(ctx)
}

// restore measures restore_ms from API socket ready to /health OK.
func (b *Branch) restore(ctx context.Context) error {
	ready := time.Now()
	if err := b.Cage.AddPID(b.VM.pid()); err != nil {
		return fmt.Errorf("cgroup add pid: %w", err)
	}
	if b.Mode == types.ColdRestore {
		return b.bootColdReady(ctx, ready)
	}
	load := time.Now()
	mem, err := b.memBackend(ctx)
	if err != nil {
		return err
	}
	if err := b.VM.fc.LoadSnapshot(snapshotFile(), mem, true); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}
	if err := b.awaitPager(); err != nil {
		return err
	}
	b.Guest = newGuest(b.Net.GuestAPIAddr)
	if err := b.Guest.waitReady(ctx, RestoreTimeout, 2*time.Millisecond, nil); err != nil {
		return err
	}
	b.RestoreMs = time.Since(ready).Milliseconds()
	if hasPostgres(b.cfg.Services) {
		if err := b.waitPostgres(ctx, pgReadyTimeout()); err != nil {
			return err
		}
	}
	b.FirstQueryMs = time.Since(load).Milliseconds()
	return nil
}

// bootColdReady boots the clone's kernel from scratch and waits for ready.
func (b *Branch) bootColdReady(ctx context.Context, start time.Time) error {
	if err := b.VM.bootCold(b.memMB, coldVCPUs, b.Net.TapName); err != nil {
		return err
	}
	b.Guest = newGuest(b.Net.GuestAPIAddr)
	if err := b.Guest.waitReady(ctx, goldenBootTimeout, 20*time.Millisecond, nil); err != nil {
		return fmt.Errorf("cold boot: %w", err)
	}
	b.RestoreMs = time.Since(start).Milliseconds()
	if hasPostgres(b.cfg.Services) {
		if err := b.waitPostgres(ctx, pgReadyTimeout()); err != nil {
			return err
		}
	}
	b.FirstQueryMs = time.Since(start).Milliseconds()
	return nil
}

// memBackend prepares guest memory for b.Mode.
func (b *Branch) memBackend(ctx context.Context) (fcclient.MemBackend, error) {
	file := fcclient.MemBackend{BackendType: fcclient.BackendFile, BackendPath: memFile()}
	switch b.Mode {
	case "", types.FileRestore:
		return file, nil
	case PrefaultRestore:
		if _, err := os.Stat(wsFile()); err != nil {
			return file, nil
		}
		n, err := storage.Prefault(memFile(), wsFile())
		if err != nil {
			return file, fmt.Errorf("prefault: %w", err)
		}
		b.PrefaultBytes = n
		return file, nil
	case types.UffdRestore:
		srv, err := storage.NewUffdServer(memFile(), filepath.Join(b.Dir, uffdSockName))
		if err != nil {
			return file, err
		}
		b.pager, b.pagerErr = srv, make(chan error, 1)
		go func() { b.pagerErr <- srv.Serve(ctx) }()
		return fcclient.MemBackend{BackendType: fcclient.BackendUffd, BackendPath: filepath.Join(b.Dir, uffdSockName)}, nil
	}
	return file, fmt.Errorf("unknown restore mode %q", b.Mode)
}

func (b *Branch) awaitPager() error {
	if b.pager == nil {
		return nil
	}
	select {
	case err := <-b.pagerErr:
		if err != nil {
			return fmt.Errorf("uffd pager handshake: %w", err)
		}
		return nil
	case <-time.After(socketTimeout):
		return fmt.Errorf("uffd pager: no handshake from firecracker after %s", socketTimeout)
	}
}

func pgReadyTimeout() time.Duration {
	if RestoreTimeout > 60*time.Second {
		return RestoreTimeout
	}
	return 60 * time.Second
}

func pgReadyCmd() []string {
	return []string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-t", "2"}
}

func (b *Branch) waitPostgres(ctx context.Context, timeout time.Duration) error {
	start := time.Now()
	for {
		res, err := b.Guest.exec(ctx, types.ExecRequest{
			Cmd: pgReadyCmd(), TimeoutSec: 5,
		})
		if err == nil && res.ExitCode == 0 {
			return nil
		}
		if time.Since(start) > timeout {
			return fmt.Errorf("branch %d: postgres not accepting connections after %s", b.Index, timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("branch %d: wait for postgres: %w", b.Index, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (b *Branch) startGateway() error {
	var extras []gateway.ProviderRoute
	for _, p := range b.cfg.Providers {
		extras = append(extras, gateway.ProviderRoute{
			Name:       p.Name,
			Upstream:   p.Upstream,
			Key:        os.Getenv(p.APIKeyEnv),
			AuthHeader: p.AuthHeader,
		})
	}
	gw, err := gateway.New(gateway.Config{
		ListenAddr:     b.Net.GatewayAddr,
		BranchKey:      DummyKey(b.Index),
		AnthropicKey:   os.Getenv("ANTHROPIC_API_KEY"),
		OpenAIKey:      os.Getenv("OPENAI_API_KEY"),
		OpenRouterKey:  os.Getenv("OPENROUTER_API_KEY"),
		ExtraProviders: extras,
	})
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.gwCancel, b.gwDone = cancel, make(chan error, 1)
	go func() { b.gwDone <- gw.Serve(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", b.Net.GatewayAddr, 200*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("gateway %s not listening", b.Net.GatewayAddr)
}

func (b *Branch) stopGateway() {
	if b.gwCancel == nil {
		return
	}
	b.gwCancel()
	select {
	case <-b.gwDone:
	case <-time.After(killTimeout):
	}
	b.gwCancel = nil
}

// Teardown stops the gateway and VM, then removes every host resource.
func (b *Branch) Teardown() error {
	b.stopGateway()
	if b.VM != nil {
		b.VM.kill()
	}
	var perr error
	if b.pager != nil {
		perr = b.pager.Close()
		b.pager = nil
	}
	return errors.Join(perr, teardownState(b.State("")))
}

func teardownState(b types.BranchState) error {
	ops := cgroup.HostOps{}
	if isFirecracker(b.FirecrackerPID) {
		if err := ops.KillPID(b.FirecrackerPID); err != nil {
			return fmt.Errorf("teardown branch %d: %w", b.Index, err)
		}
		waitGone(b.FirecrackerPID, killTimeout)
	}
	var errs []error
	if b.CgroupPath != "" {
		errs = append(errs, ops.RemoveCgroup(b.CgroupPath))
	}
	errs = append(errs, netiso.DeleteNetNS(b.NetNS), netiso.DeleteLink(b.VethHost))
	if b.CloneDir != "" {
		errs = append(errs, ops.RemoveCloneDir(b.CloneDir))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("teardown branch %d: %w", b.Index, err)
	}
	return nil
}

func procInfo(pid int) (comm string, state byte, ok bool) {
	if pid <= 0 {
		return "", 0, false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, false
	}
	open, end := bytes.IndexByte(raw, '('), bytes.LastIndexByte(raw, ')')
	if open < 0 || end < open || end+2 >= len(raw) {
		return "", 0, false
	}
	return string(raw[open+1 : end]), raw[end+2], true
}

func isFirecracker(pid int) bool {
	comm, state, ok := procInfo(pid)
	return ok && comm == "firecracker" && state != 'Z'
}

func waitGone(pid int, d time.Duration) {
	deadline := time.Now().Add(d)
	for isFirecracker(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}
