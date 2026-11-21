// The cage targets Linux cgroups v2. It compiles on any unix so the
// portable test file can reference it; the cage test is root-gated.
package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/netiso"
)

// Root is the parent cgroup for every branch.
const Root = "/sys/fs/cgroup/branchvm"

// Cage is one branch cgroup.
type Cage struct {
	Path string
}

// New creates Root/<run>-<i> and writes memory.max, cpu.max, pids.max.
func New(run string, i int, memMaxBytes int64, cpuMax string, pidsMax int) (*Cage, error) {
	if err := os.MkdirAll(Root, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup root: %w", err)
	}
	// Delegate controllers to children; ignore if already enabled.
	_ = os.WriteFile(filepath.Join(Root, "cgroup.subtree_control"), []byte("+memory +cpu +pids"), 0o644)
	c := &Cage{Path: filepath.Join(Root, fmt.Sprintf("%s-%d", run, i))}
	if err := os.Mkdir(c.Path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("cgroup mkdir: %w", err)
	}
	limits := map[string]string{
		"memory.max": strconv.FormatInt(memMaxBytes, 10),
		"cpu.max":    cpuMax,
		"pids.max":   strconv.Itoa(pidsMax),
	}
	for k, v := range limits {
		if err := c.write(k, v); err != nil {
			_ = c.Destroy()
			return nil, err
		}
	}
	return c, nil
}

func (c *Cage) write(file, val string) error {
	if err := os.WriteFile(filepath.Join(c.Path, file), []byte(val), 0o644); err != nil {
		return fmt.Errorf("cgroup write %s=%q: %w", file, val, err)
	}
	return nil
}

// AddPID moves pid into the cage.
func (c *Cage) AddPID(pid int) error { return c.write("cgroup.procs", strconv.Itoa(pid)) }

// CurrentMemMB returns memory.current in MiB.
func (c *Cage) CurrentMemMB() (int64, error) {
	b, err := os.ReadFile(filepath.Join(c.Path, "memory.current"))
	if err != nil {
		return 0, fmt.Errorf("cgroup memory.current: %w", err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cgroup memory.current: %w", err)
	}
	return n >> 20, nil
}

// Destroy kills remaining processes and removes the cgroup. Idempotent.
func (c *Cage) Destroy() error { return destroyPath(c.Path) }

func destroyPath(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o644) != nil {
		for _, pid := range procs(path) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	var err error
	for try := 0; try < 50; try++ {
		if err = os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		for _, pid := range procs(path) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("cgroup rmdir %s: %w", path, err)
}

func procs(path string) []int {
	b, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// HostOps is the real GCOps implementation.
type HostOps struct{}

// ProcessAlive reports whether pid exists (signal 0).
func (HostOps) ProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// KillPID sends SIGKILL; an already-gone process is not an error.
func (HostOps) KillPID(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}

// RemoveNetNS deletes a netns.
func (HostOps) RemoveNetNS(name string) error { return netiso.DeleteNetNS(name) }

// RemoveVeth deletes a host link.
func (HostOps) RemoveVeth(name string) error { return netiso.DeleteLink(name) }

// RemoveCgroup kills and removes a cgroup dir.
func (HostOps) RemoveCgroup(path string) error { return destroyPath(path) }

// RemoveCloneDir removes a clone directory tree.
func (HostOps) RemoveCloneDir(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove clone dir: %w", err)
	}
	return nil
}
