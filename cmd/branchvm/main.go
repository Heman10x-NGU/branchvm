// Command branchvm forks a booted, service-backed dev VM into N Firecracker
// branches, runs a command or coding agent in each, inspects diffs/logs/shells,
// and promotes one winning diff back to the host.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const usage = `usage: branchvm <command> [flags]

commands:
  init     [--repo DIR] [--image IMG] [--mem 1024] [--cpus 2] [--warm CMD] [--warm-timeout 1800] [--rootfs-size MB] [--execd PATH]
           boot the golden VM (from base rootfs, Docker image, or branchvm.toml), sync repo, snapshot
  run      --branches N [--repo DIR] [--mem MB] [--timeout 600] [--pager mmap|fadvise] -- CMD ARGS...
           restore N branches, sync deltas, run CMD in each; VMs stay up for inspection and apply
  logs     <i> [--run RUNID]
           print saved stdout and stderr for branch i
  diff     <i> [--run RUNID] [--stat]
           print the diff from branch i (live from the VM while running)
  shell    <i> [--run RUNID] [--timeout 600] [-- CMD ARGS...]
           open an interactive PTY shell (or run CMD) inside live branch i
  forward  <i> <guest_port>[:<host_port>] [--run RUNID]
           forward host 127.0.0.1:<host_port> to guest 127.0.0.1:<guest_port> on branch i
  apply    <i> [--run RUNID] [--repo DIR]
           apply branch i's diff to the repo, then tear the whole run down
  gc       [--run RUNID] [--all]
           reap crashed runs; --run discards one run, --all discards all runs
  verify   [--repo examples/auth-demo]
           fire the 6 live isolation attacks and print PASS/FAIL
  bench    [--repo examples/auth-demo] [--iters 10] [--out results/] [--fast] [--max-clones 20]
           measure boot, pager, and RAM density; writes raw.jsonl and BENCHMARKS.md
`

// command is registered by the Linux-only cmd_*.go files. On other systems
// the table stays empty and main explains that branchvm needs Linux.
type command func(ctx context.Context, args []string) error

var commands = map[string]command{}

type exitCodeError int

func (e exitCodeError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

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
		var codeErr exitCodeError
		if errors.As(err, &codeErr) {
			os.Exit(int(codeErr))
		}
		fmt.Fprintf(os.Stderr, "branchvm %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("must run as root (use sudo -E to keep ANTHROPIC_API_KEY, OPENAI_API_KEY, and OPENROUTER_API_KEY)")
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

// parseIndexArgs parses flags appearing either before or after a single
// branch index positional argument.
func parseIndexArgs(fs *flag.FlagSet, args []string, cmdUsage string) (int, error) {
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() == 0 {
		return 0, fmt.Errorf("missing branch index: %s", cmdUsage)
	}
	idxArg := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	idx, err := strconv.Atoi(idxArg)
	if err != nil || idx < 1 {
		return 0, fmt.Errorf("branch index %q must be a positive integer", idxArg)
	}
	return idx, nil
}
