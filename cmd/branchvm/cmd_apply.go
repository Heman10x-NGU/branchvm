//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["apply"] = cmdApply }

func cmdApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	runID := fs.String("run", "", "run id (default: latest run)")
	repo := fs.String("repo", "", "git repo to apply the patch to (default: current dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("missing branch index: branchvm apply <i> [--run RUNID]")
	}
	// Flags may also follow the index: apply 2 --run r1a2b3c.
	idxArg := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	idx, err := strconv.Atoi(idxArg)
	if err != nil || idx < 1 {
		return fmt.Errorf("branch index %q must be a positive integer", idxArg)
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
