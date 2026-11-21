// Command branchvm forks a booted, Postgres-backed dev VM into N
// Firecracker branches, runs a command in each, and promotes one diff.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const usage = `usage: branchvm <command> [flags]

commands:
  init     [--repo DIR] [--mem 1024] [--cpus 2]
           boot the golden VM, sync the repo, write the golden snapshot
  run      --branches N [--repo DIR] [--mem 1024] [--timeout 600] -- CMD ARGS...
           restore N branches and run CMD in each; VMs stay up for apply
  apply    <i> [--run RUNID] [--repo DIR]
           apply branch i's diff to the repo, then tear the whole run down
  gc       [--run RUNID]
           reap resources of crashed runs; --run discards one run, live VMs included
  verify   [--repo examples/auth-demo]
           fire the 6 live isolation attacks and print PASS/FAIL
`

// command is registered by the Linux-only cmd_*.go files. On other systems
// the table stays empty and main explains that branchvm needs Linux.
type command func(ctx context.Context, args []string) error

var commands = map[string]command{}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if len(commands) == 0 {
		fmt.Fprintln(os.Stderr, "branchvm: requires Linux with KVM; run it inside the fc-lab VM")
		os.Exit(1)
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "branchvm: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err := run(context.Background(), os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "branchvm %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("must run as root (use sudo -E to keep ANTHROPIC_API_KEY and OPENAI_API_KEY)")
	}
	return nil
}

// absRepo resolves a --repo value, defaulting to the current directory.
func absRepo(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve repo %q: %w", dir, err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("repo %s is not a directory", abs)
	}
	return abs, nil
}
