//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

const defaultShellTimeoutSec = 600

func init() { commands["shell"] = cmdShell }

func parseShellArgs(args []string) (idx int, runID string, timeoutSec int, cmd []string, err error) {
	before := args
	for i, a := range args {
		if a == "--" {
			before = args[:i]
			cmd = args[i+1:]
			break
		}
	}

	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	runFlag := fs.String("run", "", "run id (default: latest run)")
	timeoutFlag := fs.Int("timeout", defaultShellTimeoutSec, "non-interactive command timeout in seconds (0 disables; ignored for interactive shells)")
	idx, err = parseIndexArgs(fs, before, "branchvm shell <i> [--run RUNID] [--timeout 600] [-- CMD ARGS...]")
	if err != nil {
		return 0, "", 0, nil, err
	}
	if *timeoutFlag < 0 {
		return 0, "", 0, nil, fmt.Errorf("--timeout must be >= 0, got %d", *timeoutFlag)
	}
	return idx, *runFlag, *timeoutFlag, cmd, nil
}

func cmdShell(ctx context.Context, args []string) error {
	idx, runID, timeoutSec, cmd, err := parseShellArgs(args)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	sigs := []os.Signal{syscall.SIGTERM, syscall.SIGHUP}
	if len(cmd) > 0 {
		sigs = append(sigs, os.Interrupt)
	}
	sigCtx, stop := signal.NotifyContext(ctx, sigs...)
	defer stop()

	code, err := pool.Shell(sigCtx, pool.ShellOptions{
		Index:      idx,
		RunID:      runID,
		Cmd:        cmd,
		TimeoutSec: timeoutSec,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
	})
	if err != nil {
		if code == 124 {
			fmt.Fprintf(os.Stderr, "branchvm shell: %v\n", err)
			return exitCodeError(124)
		}
		return err
	}
	if code != 0 {
		return exitCodeError(code)
	}
	return nil
}
