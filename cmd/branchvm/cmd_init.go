//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/config"
	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["init"] = cmdInit }

func cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := fs.String("repo", "", "git repo to sync into the golden VM (default: current dir)")
	image := fs.String("image", "", "Docker image to build guest rootfs.ext4 from (overrides branchvm.toml)")
	execd := fs.String("execd", "", "path to static linux execd binary (default: auto-detect)")
	rootfsSize := fs.Int64("rootfs-size", 0, "override rootfs.ext4 size in MiB when building from --image")
	mem := fs.Int64("mem", 1024, "guest memory in MiB")
	cpus := fs.Int("cpus", 2, "guest vCPUs")
	warm := fs.String("warm", "", "command to run in the golden /workspace before the checkpoint")
	warmTimeout := fs.Int("warm-timeout", 1800, "warm command timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *warmTimeout < 0 {
		return fmt.Errorf("--warm-timeout must be >= 0, got %d", *warmTimeout)
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

	cfg, found, err := config.Load(dir)
	if err != nil {
		return err
	}
	if found {
		fmt.Fprintf(os.Stdout, "loaded %s/%s\n", dir, config.FileName)
	}

	dockerfile := cfg.Dockerfile
	if explicit["image"] {
		dockerfile = ""
	} else {
		*image = cfg.Image
	}
	if !explicit["rootfs-size"] {
		*rootfsSize = cfg.RootfsSizeMB
	}
	if !explicit["mem"] {
		*mem = cfg.MemMB
	}
	if !explicit["cpus"] {
		*cpus = cfg.VCPUs
	}
	if !explicit["warm"] {
		*warm = cfg.Warm
	}
	if !explicit["warm-timeout"] {
		*warmTimeout = cfg.WarmTimeoutSec
	}

	return pool.Init(ctx, pool.InitOptions{
		Repo:           dir,
		Image:          *image,
		Dockerfile:     dockerfile,
		ExecdPath:      *execd,
		RootfsSizeMB:   *rootfsSize,
		MemMB:          *mem,
		VCPUs:          *cpus,
		WarmCmd:        *warm,
		WarmTimeoutSec: *warmTimeout,
		Database:       cfg.Database,
		Services:       cfg.Services,
		Env:            cfg.Env,
		Providers:      cfg.Providers,
		Out:            os.Stdout,
	})
}
