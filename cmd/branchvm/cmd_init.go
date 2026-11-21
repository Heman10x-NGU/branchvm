//go:build linux

package main

import (
	"context"
	"flag"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["init"] = cmdInit }

func cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := fs.String("repo", "", "git repo to sync into the golden VM (default: current dir)")
	mem := fs.Int64("mem", 1024, "guest memory in MiB")
	cpus := fs.Int("cpus", 2, "guest vCPUs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	dir, err := absRepo(*repo)
	if err != nil {
		return err
	}
	return pool.Init(ctx, pool.InitOptions{Repo: dir, MemMB: *mem, VCPUs: *cpus, Out: os.Stdout})
}
