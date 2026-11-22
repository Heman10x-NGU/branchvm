//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

const (
	defaultServiceTimeout = 60 * time.Second
	servicePollInterval   = 250 * time.Millisecond
)

func loadServiceSpecs(path string, onErr func(error)) []types.ServiceSpec {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}}
	}
	if err != nil {
		if onErr != nil {
			onErr(fmt.Errorf("read %s: %w", path, err))
		}
		return nil
	}
	specs, err := parseServiceSpecs(raw)
	if err != nil {
		if onErr != nil {
			onErr(fmt.Errorf("parse %s: %w", path, err))
		}
		return nil
	}
	return specs
}

func parseServiceSpecs(raw []byte) ([]types.ServiceSpec, error) {
	var specs []types.ServiceSpec
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil, err
	}
	if specs == nil {
		specs = []types.ServiceSpec{}
	}
	return specs, nil
}

func (a *agent) runServices() {
	if len(a.services) == 0 {
		a.pgReady.Store(true)
		a.servicesReady.Store(true)
		log.Printf("execd: stateless guest (0 background services)")
		return
	}

	hasPG := false
	a.svcMu.Lock()
	for _, s := range a.services {
		a.svcState[s.Name] = false
		if s.Preset == "postgres" {
			hasPG = true
		}
	}
	a.svcMu.Unlock()

	if !hasPG {
		a.pgReady.Store(true)
	}

	for _, svc := range a.services {
		svc := svc
		switch svc.Preset {
		case "postgres":
			go a.runPostgres(svc.Name)
		case "redis":
			go a.runRedis(svc)
		default:
			go a.runCustomService(svc)
		}
	}
}

func (a *agent) setServiceReady(name string, ready bool) {
	a.svcMu.Lock()
	defer a.svcMu.Unlock()
	if a.svcState == nil {
		a.svcState = map[string]bool{}
	}
	if ready && a.svcDead[name] {
		return
	}
	a.svcState[name] = ready
	a.recomputeServicesReadyLocked()
}

func (a *agent) markServiceDead(name string) {
	a.svcMu.Lock()
	defer a.svcMu.Unlock()
	if a.svcDead == nil {
		a.svcDead = map[string]bool{}
	}
	if a.svcState == nil {
		a.svcState = map[string]bool{}
	}
	a.svcDead[name] = true
	a.svcState[name] = false
	a.recomputeServicesReadyLocked()
}

func (a *agent) recomputeServicesReadyLocked() {
	all := len(a.svcState) > 0
	for _, ok := range a.svcState {
		if !ok {
			all = false
			break
		}
	}
	a.servicesReady.Store(all)
}

func (a *agent) serviceSnapshot() map[string]bool {
	a.svcMu.Lock()
	defer a.svcMu.Unlock()
	if len(a.svcState) == 0 {
		return nil
	}
	out := make(map[string]bool, len(a.svcState))
	for k, v := range a.svcState {
		out[k] = v
	}
	return out
}

func (a *agent) runRedis(svc types.ServiceSpec) {
	s := svc
	s.Cmd = []string{"redis-server", "--bind", "127.0.0.1", "--port", "6379", "--save", "", "--appendonly", "no"}
	if s.ReadyTCP == "" {
		s.ReadyTCP = "127.0.0.1:6379"
	}
	if s.ReadyTimeoutSec <= 0 {
		s.ReadyTimeoutSec = 30
	}
	a.runCustomService(s)
}

func (a *agent) runCustomService(svc types.ServiceSpec) {
	if len(svc.Cmd) == 0 {
		a.fail(svc.Name, errors.New("empty service cmd"))
		return
	}
	cmd := exec.Command(svc.Cmd[0], svc.Cmd[1:]...)
	cmd.Dir = workspaceDir
	cmd.Env = a.mergeEnv(svc.Env)
	cmd.Stdout, cmd.Stderr = logOut, logOut
	sys := &syscall.SysProcAttr{Setsid: true}
	if svc.User != "" {
		cred, home, err := lookupUserCred(svc.User)
		if err != nil {
			a.fail(svc.Name, err)
			return
		}
		sys.Credential = cred
		cmd.Env = a.mergeEnv([]string{"HOME=" + home}, svc.Env)
	}
	cmd.SysProcAttr = sys

	if err := a.procs.start(cmd); err != nil {
		a.fail(svc.Name, err)
		return
	}
	log.Printf("execd: service %s started pid %d", svc.Name, cmd.Process.Pid)

	alive := make(chan struct{})
	go func() {
		err := a.procs.wait(cmd)
		close(alive)
		a.markServiceDead(svc.Name)
		if err == nil {
			err = errors.New("exit status 0")
		}
		a.fail(svc.Name, fmt.Errorf("exited: %w", err))
	}()

	timeout := defaultServiceTimeout
	if svc.ReadyTimeoutSec > 0 {
		timeout = time.Duration(svc.ReadyTimeoutSec) * time.Second
	}
	if err := a.waitServiceReady(svc, alive, timeout); err != nil {
		a.fail(svc.Name, err)
		return
	}
	select {
	case <-alive:
		return
	default:
		a.setServiceReady(svc.Name, true)
		log.Printf("execd: service %s ready", svc.Name)
	}
}

func (a *agent) waitServiceReady(svc types.ServiceSpec, exited <-chan struct{}, timeout time.Duration) error {
	if svc.ReadyTCP == "" && len(svc.ReadyCmd) == 0 {
		select {
		case <-exited:
			return errors.New("exited immediately after start")
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return errors.New("exited before becoming ready")
		default:
		}
		if svc.ReadyTCP != "" {
			if c, err := net.DialTimeout("tcp", svc.ReadyTCP, 500*time.Millisecond); err == nil {
				c.Close()
				return nil
			}
		} else if len(svc.ReadyCmd) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			probe := exec.CommandContext(ctx, svc.ReadyCmd[0], svc.ReadyCmd[1:]...)
			probe.Dir = workspaceDir
			probe.Env = a.mergeEnv(svc.Env)
			err := a.procs.run(probe)
			cancel()
			if err == nil {
				return nil
			}
		}
		time.Sleep(servicePollInterval)
	}
	return fmt.Errorf("not ready within %s", timeout)
}

func (a *agent) checkpointServices(parent context.Context) []error {
	var errs []error
	for _, svc := range a.services {
		if svc.Preset == "postgres" {
			if err := a.checkpointPostgres(parent); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if len(svc.CheckpointCmd) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(parent, checkpointWait)
		cmd := exec.CommandContext(ctx, svc.CheckpointCmd[0], svc.CheckpointCmd[1:]...)
		cmd.Dir = workspaceDir
		cmd.Env = a.mergeEnv(svc.Env)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := a.procs.run(cmd)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("checkpoint %s: %w: %s", svc.Name, err, strings.TrimSpace(stderr.String())))
		}
	}
	return errs
}

func lookupUserCred(name string) (*syscall.Credential, string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, "", fmt.Errorf("lookup user %s: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, "", fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, "", fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, u.HomeDir, nil
}
