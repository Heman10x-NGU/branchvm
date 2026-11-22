//go:build linux

package main

import (
	"context"
	"flag"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["apply"] = cmdApply }

func cmdApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	runID := fs.String("run", "", "run id (default: latest run)")
	repo := fs.String("repo", "", "git repo to apply the patch to (default: current dir)")
	idx, err := parseIndexArgs(fs, args, "branchvm apply <i> [--run RUNID] [--repo DIR]")
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
	return pool.Apply(ctx, pool.ApplyOptions{Index: idx, RunID: *runID, Repo: dir, Out: os.Stdout})
}
