//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["gc"] = cmdGC }

func cmdGC(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	runID := fs.String("run", "", "discard this run completely, live VMs included")
	all := fs.Bool("all", false, "reap crashed runs and discard all live runs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all && *runID != "" {
		return errors.New("cannot specify both --all and --run")
	}
	if err := requireRoot(); err != nil {
		return err
	}
	if *all {
		return pool.GCAll(os.Stdout)
	}
	return pool.GC(os.Stdout, *runID)
}
