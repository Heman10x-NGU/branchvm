//go:build linux

// Command execd is PID 1 inside a branchvm guest: it mounts the kernel
// filesystems, configures the network, supervises configured services,
// reaps orphans, and serves the host control API.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

const (
	execdVersion      = "0.2.1"
	guestAddr         = "172.16.0.2"
	guestCIDR         = guestAddr + "/30"
	gatewayAddr       = "172.16.0.1"
	listenAddr        = guestAddr + ":8080"
	guestHostname     = "branchvm"
	defaultPATH       = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	basePATH          = "PATH=" + defaultPATH
	imageEnvPath      = "/etc/branchvm/image-env.json"
	servicesSpecPath  = "/etc/branchvm/services.json"
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
	start         time.Time
	procs         *procTable
	degraded      atomic.Bool
	pgAlive       atomic.Bool
	pgReady       atomic.Bool
	presetsReady  atomic.Bool
	servicesReady atomic.Bool

	imageEnv []string
	services []types.ServiceSpec

	svcMu         sync.Mutex
	svcState      map[string]bool
	svcDead       map[string]bool
	customStarted bool

	credMu     sync.Mutex
	pgCred     *syscall.Credential
	pgHome     string
	pgBinDir   string
	pgDataDir  string
	pgConfFile string

	wsMu    sync.Mutex
	wsDir   string
	baseSHA string
}

func main() {
	a := &agent{
		start:    time.Now(),
		procs:    newProcTable(),
		svcState: map[string]bool{},
		svcDead:  map[string]bool{},
	}
	a.imageEnv = loadImageEnv(imageEnvPath)
	a.services = loadServiceSpecs(servicesSpecPath, func(err error) {
		a.fail("services-config", err)
	})

	_ = os.Setenv("PATH", mergePATH(extractPATH(a.imageEnv), defaultPATH))

	if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT, ""); err != nil {
		log.Printf("execd: remount / rw: %v", err)
	}
	a.mountAll()
	initLogging()
	// Catch rather than ignore so execve resets child dispositions to SIG_DFL.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for range sigCh {
		}
	}()
	go a.procs.reapLoop()

	if h, _ := os.Hostname(); h == "" || h == "(none)" {
		if err := syscall.Sethostname([]byte(guestHostname)); err != nil {
			log.Printf("execd: sethostname: %v", err)
		}
	}
	if err := a.setupNetwork(); err != nil {
		a.fail("network", err)
	}
	go a.runServices()
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

func loadImageEnv(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("execd: read %s: %v", path, err)
		}
		return nil
	}
	var env []string
	if err := json.Unmarshal(raw, &env); err != nil {
		log.Printf("execd: parse %s: %v", path, err)
		return nil
	}
	return env
}

func (a *agent) mergeEnv(extra ...[]string) []string {
	layers := make([][]string, 0, 2+len(extra))
	layers = append(layers, baseEnv(), a.imageEnv)
	layers = append(layers, extra...)
	return mergeEnv(layers...)
}

// mergeEnv merges KEY=VALUE layers so later layers override earlier ones while
// preserving standard system directories inside PATH.
func mergeEnv(layers ...[]string) []string {
	order := []string{}
	vals := map[string]string{}
	for _, layer := range layers {
		for _, item := range layer {
			k, v, ok := strings.Cut(item, "=")
			if !ok || k == "" {
				continue
			}
			if k == "PATH" {
				v = mergePATH(v, vals[k])
			}
			if _, exists := vals[k]; !exists {
				order = append(order, k)
			}
			vals[k] = v
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+vals[k])
	}
	return out
}

func extractPATH(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return ""
}

func mergePATH(primary, fallback string) string {
	seen := map[string]bool{}
	var parts []string
	add := func(raw string) {
		for _, p := range strings.Split(raw, ":") {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			parts = append(parts, p)
		}
	}
	add(primary)
	add(fallback)
	add(defaultPATH)
	return strings.Join(parts, ":")
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
