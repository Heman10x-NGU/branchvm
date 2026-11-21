package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := types.RunState{RunID: "r1", StartedAt: time.Unix(100, 0).UTC(),
		Branches: []types.BranchState{{Index: 0, FirecrackerPID: 42, NetNS: "bvm-r1-0"}}}
	if err := Save(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, types.RunState{RunID: "r2"}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "r1")
	if err != nil || got.RunID != "r1" || got.Branches[0].FirecrackerPID != 42 || !got.StartedAt.Equal(st.StartedAt) {
		t.Fatalf("load: %+v %v", got, err)
	}
	all, err := List(dir)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %d %v", len(all), err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
	if err := Save(dir, types.RunState{RunID: "../x"}); err == nil {
		t.Error("expected invalid run id error")
	}
}

func TestListMissingDir(t *testing.T) {
	runs, err := List(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(runs) != 0 {
		t.Fatalf("%v %v", runs, err)
	}
}

type fakeOps struct {
	alive   map[int]bool
	removed []string
	killed  []int
}

func (f *fakeOps) ProcessAlive(pid int) bool { return f.alive[pid] }
func (f *fakeOps) KillPID(pid int) error     { f.killed = append(f.killed, pid); return nil }
func (f *fakeOps) RemoveNetNS(n string) error {
	f.removed = append(f.removed, "ns:"+n)
	return nil
}
func (f *fakeOps) RemoveVeth(n string) error { f.removed = append(f.removed, "veth:"+n); return nil }
func (f *fakeOps) RemoveCgroup(p string) error {
	f.removed = append(f.removed, "cg:"+p)
	return nil
}
func (f *fakeOps) RemoveCloneDir(p string) error {
	f.removed = append(f.removed, "clone:"+p)
	return nil
}

func branch(i, pid, gw int) types.BranchState {
	return types.BranchState{Index: i, FirecrackerPID: pid, GatewayPID: gw, NetNS: "ns", VethHost: "v", CgroupPath: "cg", CloneDir: "c"}
}

func TestGC(t *testing.T) {
	dir := t.TempDir()
	// dead: all branches dead -> file deleted. mixed: one alive, one dead.
	must(t, Save(dir, types.RunState{RunID: "dead", Branches: []types.BranchState{branch(0, 10, 11), branch(1, 0, 0)}}))
	must(t, Save(dir, types.RunState{RunID: "mixed", Branches: []types.BranchState{branch(0, 20, 0), branch(1, 21, 0)}}))
	ops := &fakeOps{alive: map[int]bool{20: true, 11: true}}
	rep, err := GC(dir, GCOptions{Ops: ops})
	if err != nil {
		t.Fatal(err)
	}
	if rep.BranchesReaped != 3 || rep.BranchesAlive != 1 || rep.RunsDeleted != 1 ||
		rep.NetNS != 3 || rep.Veths != 3 || rep.Cgroups != 3 || rep.CloneDirs != 3 || rep.PIDsKilled != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if len(ops.killed) != 1 || ops.killed[0] != 11 {
		t.Fatalf("killed: %v", ops.killed)
	}
	if _, err := Load(dir, "dead"); err == nil {
		t.Error("dead run file should be deleted")
	}
	m, err := Load(dir, "mixed")
	if err != nil || len(m.Branches) != 1 || m.Branches[0].FirecrackerPID != 20 {
		t.Fatalf("mixed: %+v %v", m, err)
	}
	if _, err := GC(dir, GCOptions{}); err == nil {
		t.Error("nil ops should error")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCageLinux(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("cage test requires root")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("cgroup2 not mounted at /sys/fs/cgroup")
	}
	c, err := New("test", 0, 64<<20, "200000 100000", 32)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(c.Path, "pids.max"))
	if err != nil || strings.TrimSpace(string(b)) != "32" {
		t.Fatalf("pids.max %q %v", b, err)
	}
	if _, err := c.CurrentMemMB(); err != nil {
		t.Fatal(err)
	}
	must(t, c.Destroy())
	must(t, c.Destroy())
	if _, err := os.Stat(c.Path); !os.IsNotExist(err) {
		t.Fatal("cgroup dir remains")
	}
}
