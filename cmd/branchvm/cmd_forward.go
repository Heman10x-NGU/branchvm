//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
)

func init() { commands["forward"] = cmdForward }

func cmdForward(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forward", flag.ContinueOnError)
	runID := fs.String("run", "", "run id (default: latest run)")
	const specUsage = "branchvm forward <i> <guest_port>[:<host_port>] [--run RUNID]"

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("missing arguments: %s", specUsage)
	}
	idxArg, portSpec := fs.Arg(0), fs.Arg(1)
	if err := fs.Parse(fs.Args()[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}

	idx, err := strconv.Atoi(idxArg)
	if err != nil || idx < 1 {
		return fmt.Errorf("branch index %q must be a positive integer", idxArg)
	}
	guestPort, hostPort, err := parsePortSpec(portSpec)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	return pool.Forward(ctx, pool.ForwardOptions{
		Index:     idx,
		RunID:     *runID,
		GuestPort: guestPort,
		HostPort:  hostPort,
		Out:       os.Stdout,
	})
}

func parsePortSpec(spec string) (guestPort, hostPort int, err error) {
	gStr, hStr, hasColon := strings.Cut(spec, ":")
	guestPort, err = strconv.Atoi(gStr)
	if err != nil || guestPort < 1 || guestPort > 65535 {
		return 0, 0, fmt.Errorf("invalid guest port in %q", spec)
	}
	if !hasColon || hStr == "" {
		return guestPort, guestPort, nil
	}
	hostPort, err = strconv.Atoi(hStr)
	if err != nil || hostPort < 0 || hostPort > 65535 {
		return 0, 0, fmt.Errorf("invalid host port in %q", spec)
	}
	return guestPort, hostPort, nil
}
