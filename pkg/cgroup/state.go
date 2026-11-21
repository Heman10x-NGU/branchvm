// Package cgroup holds the cgroups v2 cage for branch VMs and the
// crash-safe run state used to garbage-collect leaked resources.
package cgroup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

// Save writes st atomically to dir/<run-id>.json via temp file, fsync, rename.
func Save(dir string, st types.RunState) error {
	if st.RunID == "" || strings.ContainsAny(st.RunID, `/\`) {
		return fmt.Errorf("save state: invalid run id %q", st.RunID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	f, err := os.CreateTemp(dir, ".tmp-"+st.RunID+"-*")
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("save state write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("save state fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("save state close: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, st.RunID+".json")); err != nil {
		return fmt.Errorf("save state rename: %w", err)
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Load reads the state of one run.
func Load(dir, runID string) (types.RunState, error) {
	var st types.RunState
	data, err := os.ReadFile(filepath.Join(dir, runID+".json"))
	if err != nil {
		return st, fmt.Errorf("load state %s: %w", runID, err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("load state %s: %w", runID, err)
	}
	return st, nil
}

// List returns every saved run in dir. A missing dir yields no runs.
func List(dir string) ([]types.RunState, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list state: %w", err)
	}
	var out []types.RunState
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		st, err := Load(dir, strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// GCOps abstracts host side effects so GC is testable with fakes.
type GCOps interface {
	ProcessAlive(pid int) bool
	KillPID(pid int) error
	RemoveNetNS(name string) error
	RemoveVeth(name string) error
	RemoveCgroup(path string) error
	RemoveCloneDir(path string) error
}

// GCOptions configures GC.
type GCOptions struct {
	Ops GCOps
}

// GCReport counts what GC reaped.
type GCReport struct {
	BranchesReaped int
	BranchesAlive  int
	NetNS          int
	Veths          int
	Cgroups        int
	CloneDirs      int
	PIDsKilled     int
	RunsDeleted    int
	Errors         []string
}

// GC reaps resources of branches whose Firecracker process is gone. Runs
// with no surviving branches have their state file deleted; partially
// reaped runs are rewritten with only the surviving branches.
func GC(dir string, opts GCOptions) (GCReport, error) {
	var rep GCReport
	if opts.Ops == nil {
		return rep, errors.New("gc: nil ops")
	}
	runs, err := List(dir)
	if err != nil {
		return rep, fmt.Errorf("gc: %w", err)
	}
	for _, run := range runs {
		var keep []types.BranchState
		for _, b := range run.Branches {
			if b.FirecrackerPID > 0 && opts.Ops.ProcessAlive(b.FirecrackerPID) {
				rep.BranchesAlive++
				keep = append(keep, b)
				continue
			}
			if reapBranch(opts.Ops, b, &rep) {
				rep.BranchesReaped++
			} else {
				keep = append(keep, b)
			}
		}
		if len(keep) == 0 {
			if err := os.Remove(filepath.Join(dir, run.RunID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				rep.Errors = append(rep.Errors, err.Error())
				continue
			}
			rep.RunsDeleted++
		} else if len(keep) != len(run.Branches) {
			next := types.RunState{RunID: run.RunID, StartedAt: run.StartedAt, Branches: keep}
			if err := Save(dir, next); err != nil {
				rep.Errors = append(rep.Errors, err.Error())
			}
		}
	}
	return rep, nil
}

// reapBranch removes all resources of b; it returns false if any removal failed.
func reapBranch(ops GCOps, b types.BranchState, rep *GCReport) bool {
	ok := true
	note := func(err error, counter *int) {
		if err != nil {
			ok = false
			rep.Errors = append(rep.Errors, fmt.Sprintf("branch %d: %v", b.Index, err))
			return
		}
		*counter++
	}
	for _, pid := range []int{b.GatewayPID, b.PagerPID} {
		if pid > 0 && ops.ProcessAlive(pid) {
			note(ops.KillPID(pid), &rep.PIDsKilled)
		}
	}
	if b.NetNS != "" {
		note(ops.RemoveNetNS(b.NetNS), &rep.NetNS)
	}
	if b.VethHost != "" {
		note(ops.RemoveVeth(b.VethHost), &rep.Veths)
	}
	if b.CgroupPath != "" {
		note(ops.RemoveCgroup(b.CgroupPath), &rep.Cgroups)
	}
	if b.CloneDir != "" {
		note(ops.RemoveCloneDir(b.CloneDir), &rep.CloneDirs)
	}
	return ok
}

// InstallCleanupTrap calls cancel once on the first SIGINT or SIGTERM.
func InstallCleanupTrap(cancel func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	var once sync.Once
	go func() {
		<-ch
		once.Do(cancel)
	}()
}
