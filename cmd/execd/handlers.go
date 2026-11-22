//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

const (
	workspaceDir       = "/workspace"
	baseSHAFile        = workspaceDir + "/.bvm_base_sha"
	gitExcludeFile     = workspaceDir + "/.git/info/exclude"
	baseSHAExclude     = "/.bvm_base_sha"
	maxSyncBytes       = 2 << 30
	maxJSONBytes       = 1 << 20
	maxOutputBytes     = 8 << 20
	entropyBytes       = 32
	defaultExecTimeout = 10 * time.Minute
	execWaitDelay      = 2 * time.Second
	startFailExitCode  = 127
	timeoutExitCode    = 124
)

var gitEnv = []string{
	"GIT_AUTHOR_NAME=Branchvm", "GIT_AUTHOR_EMAIL=bvm@local",
	"GIT_COMMITTER_NAME=Branchvm", "GIT_COMMITTER_EMAIL=bvm@local",
}

type syncResponse struct {
	OK      bool   `json:"ok"`
	BaseSHA string `json:"base_sha"`
}

func (a *agent) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("GET /entropy", handleEntropy)
	mux.HandleFunc("POST /sync", a.handleSync)
	mux.HandleFunc("POST /exec", a.handleExec)
	mux.HandleFunc("GET /diff", a.handleDiff)
	mux.HandleFunc("POST /checkpoint", a.handleCheckpoint)
	mux.HandleFunc("POST /pty", a.handlePTY)
	mux.HandleFunc("POST /tcp", a.handleTCP)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("execd: write response: %v", err)
	}
}

func httpError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}

func (a *agent) handleHealth(w http.ResponseWriter, _ *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, types.HealthResponse{
		OK:            !a.degraded.Load(),
		PgReady:       a.pgReady.Load(),
		ServicesReady: a.servicesReady.Load(),
		Services:      a.serviceSnapshot(),
		UptimeSec:     int64(time.Since(a.start).Seconds()),
		Hostname:      host,
	})
}

func handleEntropy(w http.ResponseWriter, _ *http.Request) {
	buf := make([]byte, entropyBytes)
	f, err := os.Open("/dev/urandom")
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("open urandom: %w", err))
		return
	}
	defer f.Close()
	if _, err := io.ReadFull(f, buf); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("read urandom: %w", err))
		return
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("read boot_id: %w", err))
		return
	}
	machineID, _ := os.ReadFile("/etc/machine-id")
	writeJSON(w, http.StatusOK, types.EntropyResponse{
		Hex:       hex.EncodeToString(buf),
		MachineID: strings.TrimSpace(string(machineID)),
		BootID:    strings.TrimSpace(string(bootID)),
	})
}

func (a *agent) handleSync(w http.ResponseWriter, r *http.Request) {
	a.wsMu.Lock()
	defer a.wsMu.Unlock()
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("create workspace: %w", err))
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxSyncBytes)
	if err := a.applySyncRequest(r.Header.Get("Content-Type"), body, workspaceDir); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	sha, err := a.commitBase()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, syncResponse{OK: true, BaseSHA: sha})
}

func (a *agent) applySyncRequest(contentType string, body io.Reader, root string) error {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return extractTarGz(body, root)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return errors.New("multipart sync missing boundary")
	}

	mr := multipart.NewReader(body, boundary)
	var meta types.SyncMeta
	var hasMeta bool
	var spooledArchive string
	defer func() {
		if spooledArchive != "" {
			_ = os.Remove(spooledArchive)
		}
	}()

	applyMeta := func() error {
		if meta.Mode == "full" {
			if err := a.cleanTrackedFiles(root); err != nil {
				return err
			}
		}
		return deleteWorkspacePaths(root, meta.Delete)
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read sync multipart: %w", err)
		}
		switch part.FormName() {
		case "meta":
			raw, err := io.ReadAll(io.LimitReader(part, maxJSONBytes))
			_ = part.Close()
			if err != nil {
				return fmt.Errorf("read sync meta: %w", err)
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				return fmt.Errorf("decode sync meta: %w", err)
			}
			hasMeta = true
		case "archive":
			if hasMeta {
				if err := applyMeta(); err != nil {
					_ = part.Close()
					return err
				}
				hasMeta = false
				err := extractTarGzIfNonEmpty(part, root)
				_ = part.Close()
				if err != nil {
					return err
				}
			} else {
				tmp, err := os.CreateTemp("", "bvm-sync-*.tgz")
				if err != nil {
					_ = part.Close()
					return fmt.Errorf("spool sync archive: %w", err)
				}
				spooledArchive = tmp.Name()
				_, cerr := io.Copy(tmp, part)
				closeErr := tmp.Close()
				_ = part.Close()
				if err := errors.Join(cerr, closeErr); err != nil {
					return fmt.Errorf("write spooled archive: %w", err)
				}
			}
		default:
			_ = part.Close()
		}
	}

	if hasMeta {
		if err := applyMeta(); err != nil {
			return err
		}
	}
	if spooledArchive != "" {
		f, err := os.Open(spooledArchive)
		if err != nil {
			return fmt.Errorf("open spooled archive: %w", err)
		}
		defer f.Close()
		if err := extractTarGzIfNonEmpty(f, root); err != nil {
			return err
		}
	}
	return nil
}

func extractTarGzIfNonEmpty(r io.Reader, root string) error {
	var head [1]byte
	n, err := io.ReadFull(r, head[:])
	if errors.Is(err, io.EOF) || (err == nil && n == 0) {
		return nil
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("peek archive: %w", err)
	}
	return extractTarGz(io.MultiReader(bytes.NewReader(head[:n]), r), root)
}

// cleanTrackedFiles removes previously git-tracked files before a full sync so
// stale files disappear without wiping git-ignored caches such as node_modules.
func (a *agent) cleanTrackedFiles(root string) error {
	if _, err := os.Stat(filepath.Join(root, ".git")); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	out, err := gitInDir(a.procs, a.mergeEnv(gitEnv), root, "ls-files", "-z")
	if err != nil {
		return fmt.Errorf("list tracked files before full sync: %w", err)
	}
	var tracked []string
	for _, rel := range strings.Split(out, "\x00") {
		if rel != "" {
			tracked = append(tracked, rel)
		}
	}
	return deleteWorkspacePaths(root, tracked)
}

func deleteWorkspacePaths(root string, rels []string) error {
	for _, rel := range rels {
		target, err := safeDeleteTarget(root, rel)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("delete %s: %w", rel, err)
		}
		pruneEmptyParents(filepath.Dir(target), root)
	}
	return nil
}

func safeDeleteTarget(root, rel string) (string, error) {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid delete path %q", rel)
	}
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("delete path %q escapes workspace", rel)
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git/") || clean == ".bvm_base_sha" {
		return "", fmt.Errorf("refusing to delete protected path %q", rel)
	}
	return safeJoin(root, clean)
}

func pruneEmptyParents(dir, stopRoot string) {
	cleanStop := filepath.Clean(stopRoot)
	for {
		cleanDir := filepath.Clean(dir)
		if cleanDir == cleanStop || !strings.HasPrefix(cleanDir, cleanStop+string(os.PathSeparator)) {
			return
		}
		entries, err := os.ReadDir(cleanDir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(cleanDir); err != nil {
			return
		}
		dir = filepath.Dir(cleanDir)
	}
}

func (a *agent) commitBase() (string, error) {
	if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); errors.Is(err, fs.ErrNotExist) {
		if _, err := a.git("-c", "init.defaultBranch=main", "init", "-q"); err != nil {
			return "", err
		}
	}
	if err := excludeBaseSHAFile(); err != nil {
		return "", err
	}
	if _, err := a.git("add", "-A"); err != nil {
		return "", err
	}
	if _, err := a.git("commit", "-q", "--allow-empty", "--no-verify", "-m", "branchvm base"); err != nil {
		return "", err
	}
	out, err := a.git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if err := os.WriteFile(baseSHAFile, []byte(sha+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", baseSHAFile, err)
	}
	a.baseSHA = sha
	return sha, nil
}

// The base sha file lives in the workspace, so it must never show up in /diff.
func excludeBaseSHAFile() error {
	data, err := os.ReadFile(gitExcludeFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read git exclude: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == baseSHAExclude {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(gitExcludeFile), 0o755); err != nil {
		return fmt.Errorf("create git info dir: %w", err)
	}
	f, err := os.OpenFile(gitExcludeFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open git exclude: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "\n%s\n", baseSHAExclude); err != nil {
		return fmt.Errorf("append git exclude: %w", err)
	}
	return nil
}

func (a *agent) handleDiff(w http.ResponseWriter, _ *http.Request) {
	a.wsMu.Lock()
	defer a.wsMu.Unlock()
	base := a.baseSHA
	if base == "" {
		if data, err := os.ReadFile(baseSHAFile); err == nil {
			base = strings.TrimSpace(string(data))
		}
	}
	if base == "" {
		httpError(w, http.StatusConflict, errors.New("no base commit: POST /sync first"))
		return
	}
	if _, err := a.git("add", "-N", "."); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	patch, err := a.git("diff", "--binary", base)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
	if _, err := io.WriteString(w, patch); err != nil {
		log.Printf("execd: write diff: %v", err)
	}
}

func (a *agent) git(args ...string) (string, error) {
	return gitInDir(a.procs, a.mergeEnv(gitEnv), workspaceDir, args...)
}

func gitInDir(procs *procTable, env []string, dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := procs.run(cmd); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (a *agent) handleExec(w http.ResponseWriter, r *http.Request) {
	var req types.ExecRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBytes)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("decode exec request: %w", err))
		return
	}
	if len(req.Cmd) == 0 || req.Cmd[0] == "" {
		httpError(w, http.StatusBadRequest, errors.New("cmd must not be empty"))
		return
	}
	writeJSON(w, http.StatusOK, a.runExec(r.Context(), req))
}

func (a *agent) runExec(parent context.Context, req types.ExecRequest) types.ExecResponse {
	timeout := defaultExecTimeout
	if req.TimeoutSec > 0 {
		timeout = time.Duration(req.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	dir := req.Dir
	if dir == "" {
		dir = workspaceDir
	}
	cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = dir
	cmd.Env = a.mergeEnv(req.Env)
	stdout, stderr := &capWriter{limit: maxOutputBytes}, &capWriter{limit: maxOutputBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = execWaitDelay

	began := time.Now()
	err := a.procs.run(cmd)
	resp := types.ExecResponse{DurationMs: time.Since(began).Milliseconds()}
	var exitErr *exec.ExitError
	if cmd.ProcessState != nil {
		resp.ExitCode = cmd.ProcessState.ExitCode()
	} else {
		resp.ExitCode = startFailExitCode
	}
	if err != nil && !errors.As(err, &exitErr) {
		stderr.WriteString("execd: " + err.Error() + "\n")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		resp.ExitCode = timeoutExitCode
		stderr.WriteString(fmt.Sprintf("execd: timed out after %s\n", timeout))
	}
	resp.Stdout, resp.Stderr = stdout.String(), stderr.String()
	return resp
}

// capWriter keeps the first limit bytes and drops the rest so a chatty
// command cannot exhaust guest memory.
type capWriter struct {
	buf   bytes.Buffer
	limit int
}

func (c *capWriter) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capWriter) WriteString(s string) { c.buf.WriteString(s) }

func (c *capWriter) String() string { return c.buf.String() }

func extractTarGz(r io.Reader, root string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}
		if err := extractEntry(tr, hdr, root, target); err != nil {
			return fmt.Errorf("extract %s: %w", hdr.Name, err)
		}
	}
}

func safeJoin(root, name string) (string, error) {
	target := filepath.Join(root, name)
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("tar entry %q escapes %s", name, root)
	}
	return target, nil
}

func ensureParentDir(root, parent string) error {
	cleanRoot := filepath.Clean(root)
	cleanParent := filepath.Clean(parent)
	if cleanParent == cleanRoot {
		return nil
	}
	rel, err := filepath.Rel(cleanRoot, cleanParent)
	if err != nil {
		return err
	}
	cur := cleanRoot
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		if fi, err := os.Lstat(cur); err == nil {
			if fi.IsDir() {
				continue
			}
			if err := os.Remove(cur); err != nil {
				return fmt.Errorf("remove blocking file %s: %w", cur, err)
			}
		}
		if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("mkdir %s: %w", cur, err)
		}
	}
	return nil
}

func extractEntry(r io.Reader, hdr *tar.Header, root, target string) error {
	mode := hdr.FileInfo().Mode().Perm()
	if hdr.Typeflag == tar.TypeDir {
		if fi, err := os.Lstat(target); err == nil && !fi.IsDir() {
			if err := os.Remove(target); err != nil {
				return fmt.Errorf("replace file with dir: %w", err)
			}
		}
		return os.MkdirAll(target, mode|0o700)
	}
	if err := ensureParentDir(root, filepath.Dir(target)); err != nil {
		return fmt.Errorf("create parent: %w", err)
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
		if fi, err := os.Lstat(target); err == nil {
			if fi.IsDir() {
				if err := os.RemoveAll(target); err != nil {
					return fmt.Errorf("replace existing dir: %w", err)
				}
			} else if err := os.Remove(target); err != nil {
				return fmt.Errorf("replace existing: %w", err)
			}
		}
	default:
		return nil
	}
	switch hdr.Typeflag {
	case tar.TypeSymlink:
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeLink:
		src, err := safeJoin(root, hdr.Linkname)
		if err != nil {
			return err
		}
		return os.Link(src, target)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("write file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}
	return os.Chmod(target, mode)
}
