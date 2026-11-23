//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

func init() { commands["run"] = cmdRun }

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	branches := fs.Int("branches", 0, "number of branch VMs")
	repo := fs.String("repo", "", "git repo to sync into each branch (default: current dir)")
	mem := fs.Int64("mem", 1024, "guest memory in MiB (default: inherited from init; cgroup adds 256 MiB headroom)")
	timeout := fs.Int("timeout", 600, "per-branch command timeout in seconds")
	pager := fs.String("pager", "mmap", "snapshot memory pager: mmap or fadvise")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("missing command: branchvm run --branches N -- CMD ARGS...")
	}
	mode, err := parsePager(*pager)
	if err != nil {
		return err
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

	return pool.Run(ctx, pool.RunOptions{
		Repo:        dir,
		Branches:    *branches,
		MemMB:       *mem,
		MemExplicit: explicit["mem"],
		TimeoutSec:  *timeout,
		RestoreMode: mode,
		Cmd:         fs.Args(),
		Out:         os.Stdout,
	})
}

func parsePager(name string) (types.RestoreMode, error) {
	switch name {
	case "", "mmap", "file":
		return types.FileRestore, nil
	case "fadvise", "prefault":
		return pool.PrefaultRestore, nil
	case "uffd":
		return "", errors.New("uffd pager runs in-process and is only supported in `branchvm bench`; use mmap or fadvise")
	default:
		return "", fmt.Errorf("unknown --pager %q (want mmap or fadvise)", name)
	}
}
