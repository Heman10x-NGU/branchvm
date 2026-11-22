//go:build linux

package main

import (
	"context"
	"flag"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["logs"] = cmdLogs }

func cmdLogs(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	runID := fs.String("run", "", "run id (default: latest run)")
	idx, err := parseIndexArgs(fs, args, "branchvm logs <i> [--run RUNID]")
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	return pool.Logs(pool.LogsOptions{Index: idx, RunID: *runID, Out: os.Stdout})
}
