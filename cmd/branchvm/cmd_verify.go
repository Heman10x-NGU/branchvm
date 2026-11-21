//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["verify"] = cmdVerify }

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	repo := fs.String("repo", "examples/auth-demo", "git repo to sync into the verify branches")
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
	ok, err := pool.Verify(ctx, pool.VerifyOptions{Repo: dir, Out: os.Stdout})
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("not every attack was blocked")
	}
	return nil
}
