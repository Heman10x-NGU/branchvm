//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	pgBin          = "/usr/lib/postgresql/16/bin"
	pgData         = "/var/lib/postgresql/16/main"
	pgConf         = "/etc/postgresql/16/main/postgresql.conf"
	pgPIDFile      = pgData + "/postmaster.pid"
	pgRunDir       = "/run/postgresql"
	pgUser         = "postgres"
	pgHost         = "127.0.0.1"
	pgPort         = "5432"
	pgReadyTimeout = 90 * time.Second
	pgPollInterval = 250 * time.Millisecond
	pgProbeTimeout = 5 * time.Second
	checkpointWait = time.Minute
	terminateIdle  = "SELECT pg_terminate_backend(pid) FROM pg_stat_activity " +
		"WHERE pid <> pg_backend_pid() AND state = 'idle'"
)

var identityFiles = []string{"/etc/machine-id", "/var/lib/systemd/random-seed"}

func (a *agent) postgresCred() (*syscall.Credential, string, error) {
	u, err := user.Lookup(pgUser)
	if err != nil {
		return nil, "", fmt.Errorf("lookup user %s: %w", pgUser, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, "", fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, "", fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}
	a.credMu.Lock()
	a.pgCred = cred
	a.credMu.Unlock()
	return cred, u.HomeDir, nil
}

func pgEnv(home string) []string {
	return []string{basePATH, "HOME=" + home, "LANG=C.UTF-8", "PGHOST=" + pgRunDir, "PGPORT=" + pgPort}
}

func preparePgDirs(cred *syscall.Credential) error {
	if err := os.MkdirAll(pgRunDir, 0o775); err != nil {
		return fmt.Errorf("create %s: %w", pgRunDir, err)
	}
	if err := os.Chmod(pgRunDir, fs.ModeSetgid|0o775); err != nil {
		return fmt.Errorf("chmod %s: %w", pgRunDir, err)
	}
	if err := os.Chown(pgRunDir, int(cred.Uid), int(cred.Gid)); err != nil {
		return fmt.Errorf("chown %s: %w", pgRunDir, err)
	}
	if err := os.Remove(pgPIDFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale %s: %w", pgPIDFile, err)
	}
	return nil
}

func (a *agent) runPostgres() {
	cred, home, err := a.postgresCred()
	if err != nil {
		a.fail("postgres", err)
		return
	}
	if err := preparePgDirs(cred); err != nil {
		a.fail("postgres", err)
		return
	}
	cmd := exec.Command(pgBin+"/postgres", "-D", pgData,
		"-c", "config_file="+pgConf,
		"-c", "listen_addresses="+pgHost,
		"-c", "port="+pgPort)
	cmd.Dir = "/"
	cmd.Env = pgEnv(home)
	cmd.Stdout, cmd.Stderr = logOut, logOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred, Setsid: true}
	if err := a.procs.start(cmd); err != nil {
		a.fail("postgres", err)
		return
	}
	a.pgAlive.Store(true)
	log.Printf("execd: postgres started pid %d", cmd.Process.Pid)

	go func() {
		err := a.procs.wait(cmd)
		a.pgAlive.Store(false)
		a.pgReady.Store(false)
		if err == nil {
			err = errors.New("exit status 0")
		}
		a.fail("postgres", fmt.Errorf("postmaster exited: %w", err))
	}()

	if err := a.waitPgReady(cred, home); err != nil {
		a.fail("postgres", err)
		return
	}
	if a.pgAlive.Load() {
		a.pgReady.Store(true)
		log.Printf("execd: postgres ready on %s:%s", pgHost, pgPort)
	}
}

func (a *agent) waitPgReady(cred *syscall.Credential, home string) error {
	deadline := time.Now().Add(pgReadyTimeout)
	for time.Now().Before(deadline) {
		if !a.pgAlive.Load() {
			return errors.New("postmaster exited before becoming ready")
		}
		if a.pgIsReady(cred, home) {
			return nil
		}
		time.Sleep(pgPollInterval)
	}
	return fmt.Errorf("pg_isready did not succeed within %s", pgReadyTimeout)
}

func (a *agent) pgIsReady(cred *syscall.Credential, home string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), pgProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, pgBin+"/pg_isready", "-q", "-h", pgHost, "-p", pgPort)
	cmd.Env = pgEnv(home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	return a.procs.run(cmd) == nil
}

func (a *agent) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	var errs []error
	if err := a.checkpointPostgres(r.Context()); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, a.removeIdentityArtifacts()...)
	syscall.Sync()
	if len(errs) > 0 {
		httpError(w, http.StatusInternalServerError, errors.Join(errs...))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *agent) checkpointPostgres(parent context.Context) error {
	if !a.pgAlive.Load() {
		return errors.New("checkpoint: postgres is not running")
	}
	a.credMu.Lock()
	cred := a.pgCred
	a.credMu.Unlock()
	if cred == nil {
		return errors.New("checkpoint: postgres credentials unknown")
	}
	ctx, cancel := context.WithTimeout(parent, checkpointWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, pgBin+"/psql", "-X", "-q", "-A", "-t",
		"-v", "ON_ERROR_STOP=1", "-h", pgRunDir, "-p", pgPort, "-U", pgUser, "-d", "postgres",
		"-c", "CHECKPOINT", "-c", terminateIdle)
	cmd.Env = pgEnv("/var/lib/postgresql")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := a.procs.run(cmd); err != nil {
		return fmt.Errorf("checkpoint: psql: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// postmaster.pid is only removed when Postgres is down: a running
// postmaster rechecks its lock file and shuts itself down if it vanishes.
func (a *agent) removeIdentityArtifacts() []error {
	paths := identityFiles
	if !a.pgAlive.Load() {
		paths = append(append([]string{}, identityFiles...), pgPIDFile)
	}
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	return errs
}
