//go:build linux

package main

import (
	"context"
	"flag"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["diff"] = cmdDiff }

func cmdDiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	runID := fs.String("run", "", "run id (default: latest run)")
	stat := fs.Bool("stat", false, "show diffstat summary instead of full patch")
	idx, err := parseIndexArgs(fs, args, "branchvm diff <i> [--run RUNID] [--stat]")
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	return pool.Diff(ctx, pool.DiffOptions{Index: idx, RunID: *runID, Stat: *stat, Out: os.Stdout})
}
