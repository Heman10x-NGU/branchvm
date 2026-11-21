//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["run"] = cmdRun }

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	branches := fs.Int("branches", 0, "number of branch VMs")
	repo := fs.String("repo", "", "git repo to sync into each branch (default: current dir)")
	mem := fs.Int64("mem", 1024, "guest memory in MiB; the cgroup limit adds 256 MiB headroom")
	timeout := fs.Int("timeout", 600, "per-branch command timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("missing command: branchvm run --branches N -- CMD ARGS...")
	}
	if err := requireRoot(); err != nil {
		return err
	}
	dir, err := absRepo(*repo)
	if err != nil {
		return err
	}
	return pool.Run(ctx, pool.RunOptions{
		Repo: dir, Branches: *branches, MemMB: *mem, TimeoutSec: *timeout, Cmd: fs.Args(), Out: os.Stdout,
	})
}
