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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
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
	cred, home, err := lookupUserCred(pgUser)
	if err != nil {
		return nil, "", err
	}
	a.credMu.Lock()
	a.pgCred = cred
	a.pgHome = home
	a.credMu.Unlock()
	return cred, home, nil
}

func pgEnv(home string) []string {
	return []string{basePATH, "HOME=" + home, "LANG=C.UTF-8", "PGHOST=" + pgRunDir, "PGPORT=" + pgPort}
}

func discoverPostgres(imageEnv []string) (binDir, dataDir, confFile string, err error) {
	binDir, err = findPgBinDir()
	if err != nil {
		return "", "", "", err
	}
	dataDir, confFile = findPgDataAndConf(imageEnv)
	return binDir, dataDir, confFile, nil
}

func findPgBinDir() (string, error) {
	matches, _ := filepath.Glob("/usr/lib/postgresql/*/bin")
	sort.Slice(matches, func(i, j int) bool {
		return pgVerFromPath(matches[i]) > pgVerFromPath(matches[j])
	})
	for _, dir := range matches {
		if fi, err := os.Stat(filepath.Join(dir, "postgres")); err == nil && !fi.IsDir() {
			return dir, nil
		}
	}
	if p, err := exec.LookPath("postgres"); err == nil {
		return filepath.Dir(p), nil
	}
	return "", errors.New("postgres binary not found in /usr/lib/postgresql/*/bin or PATH")
}

func pgVerFromPath(p string) int {
	parts := strings.Split(filepath.ToSlash(p), "/")
	if len(parts) < 5 {
		return 0
	}
	n, _ := strconv.Atoi(parts[4])
	return n
}

func extractEnvVal(env []string, key string) string {
	prefix := key + "="
	var val string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			val = v
		}
	}
	return val
}

func findPgDataAndConf(imageEnv []string) (dataDir, confFile string) {
	matches, _ := filepath.Glob("/var/lib/postgresql/*/main/PG_VERSION")
	sort.Slice(matches, func(i, j int) bool {
		return pgVerFromPath(matches[i]) > pgVerFromPath(matches[j])
	})
	for _, m := range matches {
		dd := filepath.Dir(m)
		ver := filepath.Base(filepath.Dir(dd))
		cf := filepath.Join("/etc/postgresql", ver, "main", "postgresql.conf")
		if _, err := os.Stat(cf); err == nil {
			return dd, cf
		}
		return dd, ""
	}
	if envData := extractEnvVal(imageEnv, "PGDATA"); envData != "" {
		return envData, ""
	}
	if envData := os.Getenv("PGDATA"); envData != "" {
		return envData, ""
	}
	return "/var/lib/postgresql/data", ""
}

func ensurePgInitialized(binDir, dataDir string, cred *syscall.Credential, env []string, procs *procTable) error {
	if _, err := os.Stat(filepath.Join(dataDir, "PG_VERSION")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dataDir), 0o755); err != nil {
		return fmt.Errorf("create pgdata parent %s: %w", filepath.Dir(dataDir), err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create pgdata %s: %w", dataDir, err)
	}
	if err := os.Chown(dataDir, int(cred.Uid), int(cred.Gid)); err != nil {
		return fmt.Errorf("chown pgdata %s: %w", dataDir, err)
	}
	initdb := filepath.Join(binDir, "initdb")
	cmd := exec.Command(initdb, "-D", dataDir, "--auth=trust", "--encoding=UTF8", "-U", pgUser)
	cmd.Dir = "/"
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := procs.run(cmd); err != nil {
		return fmt.Errorf("initdb %s: %w: %s", dataDir, err, strings.TrimSpace(stderr.String()))
	}
	confPath := filepath.Join(dataDir, "postgresql.conf")
	if f, err := os.OpenFile(confPath, os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.WriteString("\nlisten_addresses = '127.0.0.1'\nport = 5432\nssl = off\nunix_socket_directories = '/run/postgresql'\n")
		_ = f.Close()
	}
	hba := "local all postgres trust\nlocal all all trust\nhost all all 127.0.0.1/32 trust\nhost all all ::1/128 trust\n"
	_ = os.WriteFile(filepath.Join(dataDir, "pg_hba.conf"), []byte(hba), 0o600)
	_ = os.Chown(filepath.Join(dataDir, "pg_hba.conf"), int(cred.Uid), int(cred.Gid))
	return nil
}

func ensurePgTrustHBA(confFile, dataDir string, cred *syscall.Credential) error {
	hbaDir := dataDir
	if confFile != "" {
		hbaDir = filepath.Dir(confFile)
	}
	hbaPath := filepath.Join(hbaDir, "pg_hba.conf")
	raw, err := os.ReadFile(hbaPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", hbaPath, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[3] == "127.0.0.1/32" && fields[4] == "trust" {
			return nil
		}
	}
	const trustRules = "local all postgres trust\nlocal all all trust\nhost all all 127.0.0.1/32 trust\nhost all all ::1/128 trust\n"
	if err := os.WriteFile(hbaPath, []byte(trustRules+string(raw)), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", hbaPath, err)
	}
	if err := os.Chmod(hbaPath, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", hbaPath, err)
	}
	if cred != nil {
		if err := os.Chown(hbaPath, int(cred.Uid), int(cred.Gid)); err != nil {
			return fmt.Errorf("chown %s: %w", hbaPath, err)
		}
	}
	return nil
}

func preparePgDirs(dataDir string, cred *syscall.Credential) error {
	if err := os.MkdirAll(pgRunDir, 0o775); err != nil {
		return fmt.Errorf("create %s: %w", pgRunDir, err)
	}
	if err := os.Chmod(pgRunDir, fs.ModeSetgid|0o775); err != nil {
		return fmt.Errorf("chmod %s: %w", pgRunDir, err)
	}
	if err := os.Chown(pgRunDir, int(cred.Uid), int(cred.Gid)); err != nil {
		return fmt.Errorf("chown %s: %w", pgRunDir, err)
	}
	pidFile := filepath.Join(dataDir, "postmaster.pid")
	if err := os.Remove(pidFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale %s: %w", pidFile, err)
	}
	return nil
}

func (a *agent) runPostgres(svcName string) {
	cred, home, err := a.postgresCred()
	if err != nil {
		a.fail("postgres", err)
		return
	}
	binDir, dataDir, confFile, err := discoverPostgres(a.imageEnv)
	if err != nil {
		a.fail("postgres", err)
		return
	}
	env := a.mergeEnv(pgEnv(home))
	if err := ensurePgInitialized(binDir, dataDir, cred, env, a.procs); err != nil {
		a.fail("postgres", err)
		return
	}
	if err := ensurePgTrustHBA(confFile, dataDir, cred); err != nil {
		a.fail("postgres", err)
		return
	}
	if err := preparePgDirs(dataDir, cred); err != nil {
		a.fail("postgres", err)
		return
	}
	a.credMu.Lock()
	a.pgHome = home
	a.pgBinDir = binDir
	a.pgDataDir = dataDir
	a.pgConfFile = confFile
	a.credMu.Unlock()

	args := []string{"-D", dataDir, "-c", "listen_addresses=" + pgHost, "-c", "port=" + pgPort, "-c", "unix_socket_directories=" + pgRunDir}
	if confFile != "" {
		args = append(args, "-c", "config_file="+confFile)
	}
	cmd := exec.Command(filepath.Join(binDir, "postgres"), args...)
	cmd.Dir = "/"
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logOut, logOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred, Setsid: true}
	if err := a.procs.start(cmd); err != nil {
		a.fail("postgres", err)
		return
	}
	a.pgAlive.Store(true)
	log.Printf("execd: postgres started pid %d (data %s)", cmd.Process.Pid, dataDir)

	go func() {
		err := a.procs.wait(cmd)
		a.pgAlive.Store(false)
		a.pgReady.Store(false)
		a.markServiceDead(svcName)
		if err == nil {
			err = errors.New("exit status 0")
		}
		a.fail("postgres", fmt.Errorf("postmaster exited: %w", err))
	}()

	if err := a.waitPgReady(binDir, cred, home); err != nil {
		a.fail("postgres", err)
		return
	}
	if a.pgAlive.Load() {
		a.pgReady.Store(true)
		a.setServiceReady(svcName, true)
		log.Printf("execd: postgres ready on %s:%s", pgHost, pgPort)
	}
}

func (a *agent) waitPgReady(binDir string, cred *syscall.Credential, home string) error {
	deadline := time.Now().Add(pgReadyTimeout)
	for time.Now().Before(deadline) {
		if !a.pgAlive.Load() {
			return errors.New("postmaster exited before becoming ready")
		}
		if a.pgIsReady(binDir, cred, home) {
			return nil
		}
		time.Sleep(pgPollInterval)
	}
	return fmt.Errorf("pg_isready did not succeed within %s", pgReadyTimeout)
}

func (a *agent) pgIsReady(binDir string, cred *syscall.Credential, home string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), pgProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "pg_isready"), "-q", "-h", pgHost, "-p", pgPort)
	cmd.Env = a.mergeEnv(pgEnv(home))
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	return a.procs.run(cmd) == nil
}

func (a *agent) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	errs := a.checkpointServices(r.Context())
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
	cred, binDir, home := a.pgCred, a.pgBinDir, a.pgHome
	a.credMu.Unlock()
	if cred == nil || binDir == "" {
		return errors.New("checkpoint: postgres credentials unknown")
	}
	ctx, cancel := context.WithTimeout(parent, checkpointWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "psql"), "-X", "-q", "-A", "-t",
		"-v", "ON_ERROR_STOP=1", "-h", pgRunDir, "-p", pgPort, "-U", pgUser, "-d", "postgres",
		"-c", "CHECKPOINT", "-c", terminateIdle)
	cmd.Env = a.mergeEnv(pgEnv(home))
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
	a.credMu.Lock()
	dataDir := a.pgDataDir
	a.credMu.Unlock()
	paths := append([]string{}, identityFiles...)
	if !a.pgAlive.Load() && dataDir != "" {
		paths = append(paths, filepath.Join(dataDir, "postmaster.pid"))
	}
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	return errs
}
