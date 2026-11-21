//go:build linux

package main

import (
	"context"
	"flag"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["gc"] = cmdGC }

func cmdGC(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	runID := fs.String("run", "", "discard this run completely, live VMs included")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	return pool.GC(os.Stdout, *runID)
}
