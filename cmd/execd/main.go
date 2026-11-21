//go:build linux

// Command execd is PID 1 inside a branchvm guest: it mounts the kernel
// filesystems, configures the network, supervises Postgres, reaps orphans,
// and serves the host control API.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	guestAddr         = "172.16.0.2"
	guestCIDR         = guestAddr + "/30"
	gatewayAddr       = "172.16.0.1"
	listenAddr        = guestAddr + ":8080"
	guestHostname     = "branchvm"
	basePATH          = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	retryDelay        = time.Second
	readHeaderTimeout = 10 * time.Second
)

type mountSpec struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

// /dev/shm is required by Postgres (posix dynamic shared memory) and /run
// is a tmpfs so pid files and sockets never persist across cold boots.
var mounts = []mountSpec{
	{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755"},
	{"devpts", "/dev/pts", "devpts", syscall.MS_NOSUID | syscall.MS_NOEXEC, "gid=5,mode=0620,ptmxmode=0666"},
	{"tmpfs", "/dev/shm", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777"},
	{"tmpfs", "/run", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=0755"},
	{"tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777"},
}

var logOut = os.Stdout

type agent struct {
	start    time.Time
	procs    *procTable
	degraded atomic.Bool
	pgAlive  atomic.Bool
	pgReady  atomic.Bool

	credMu sync.Mutex
	pgCred *syscall.Credential

	wsMu    sync.Mutex
	baseSHA string
}

func main() {
	a := &agent{start: time.Now(), procs: newProcTable()}
	_ = os.Setenv("PATH", strings.TrimPrefix(basePATH, "PATH="))

	if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT, ""); err != nil {
		log.Printf("execd: remount / rw: %v", err)
	}
	a.mountAll()
	initLogging()
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go a.procs.reapLoop()

	if h, _ := os.Hostname(); h == "" || h == "(none)" {
		if err := syscall.Sethostname([]byte(guestHostname)); err != nil {
			log.Printf("execd: sethostname: %v", err)
		}
	}
	if err := a.setupNetwork(); err != nil {
		a.fail("network", err)
	}
	go a.runPostgres()
	a.serve()
}

func (a *agent) fail(subsystem string, err error) {
	a.degraded.Store(true)
	log.Printf("execd: %s: %v", subsystem, err)
}

func (a *agent) mountAll() {
	for _, m := range mounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			a.fail("mount", fmt.Errorf("mkdir %s: %w", m.target, err))
			continue
		}
		err := syscall.Mount(m.source, m.target, m.fstype, m.flags, m.data)
		if err != nil && !errors.Is(err, syscall.EBUSY) {
			a.fail("mount", fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err))
		}
	}
}

func initLogging() {
	if f, err := os.OpenFile("/dev/console", os.O_WRONLY|syscall.O_NOCTTY, 0); err == nil {
		logOut = f
	}
	log.SetOutput(logOut)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

func baseEnv() []string {
	return []string{basePATH, "HOME=/root", "LANG=C.UTF-8"}
}

func (a *agent) setupNetwork() error {
	steps := [][]string{
		{"link", "set", "lo", "up"},
		{"link", "set", "eth0", "up"},
		{"addr", "replace", guestCIDR, "dev", "eth0"},
		{"route", "replace", "default", "via", gatewayAddr},
	}
	for _, args := range steps {
		cmd := exec.Command("ip", args...)
		cmd.Env = baseEnv()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := a.procs.run(cmd); err != nil {
			return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}

func (a *agent) serve() {
	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: readHeaderTimeout}
	for {
		ln, err := net.Listen("tcp", listenAddr)
		if err != nil {
			log.Printf("execd: listen %s: %v", listenAddr, err)
			time.Sleep(retryDelay)
			continue
		}
		log.Printf("execd: serving on %s", listenAddr)
		log.Printf("execd: http server stopped: %v", srv.Serve(ln))
		time.Sleep(retryDelay)
	}
}

// procTable tracks children started through os/exec. A blanket
// wait4(-1) would steal their exit statuses and make cmd.Wait fail with
// ECHILD, so the reaper only collects zombie children nobody owns.
type procTable struct {
	mu    sync.Mutex
	owned map[int]struct{}
}

func newProcTable() *procTable {
	return &procTable{owned: make(map[int]struct{})}
}

func (p *procTable) start(cmd *exec.Cmd) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	p.owned[cmd.Process.Pid] = struct{}{}
	return nil
}

func (p *procTable) wait(cmd *exec.Cmd) error {
	err := cmd.Wait()
	p.mu.Lock()
	delete(p.owned, cmd.Process.Pid)
	p.mu.Unlock()
	return err
}

func (p *procTable) run(cmd *exec.Cmd) error {
	if err := p.start(cmd); err != nil {
		return err
	}
	return p.wait(cmd)
}

func (p *procTable) reapLoop() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	for range ch {
		p.reapOrphans()
	}
}

func (p *procTable) reapOrphans() {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		log.Printf("execd: reaper: read /proc: %v", err)
		return
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if _, ok := p.owned[pid]; ok || !isZombieChild(pid, self) {
			continue
		}
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); err != nil && !errors.Is(err, syscall.ECHILD) {
			log.Printf("execd: reaper: wait4 %d: %v", pid, err)
		}
	}
}

func isZombieChild(pid, parent int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return false
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 2 || fields[0] != "Z" {
		return false
	}
	ppid, err := strconv.Atoi(fields[1])
	return err == nil && ppid == parent
}
