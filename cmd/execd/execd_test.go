//go:build linux

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

func TestMergeEnvPrecedenceAndPATH(t *testing.T) {
	got := mergeEnv(
		baseEnv(),
		[]string{"PATH=/usr/local/go/bin:/usr/bin", "NODE_ENV=prod", "FOO=1"},
		[]string{"NODE_ENV=test", "PATH=/root/.local/bin"},
	)
	m := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	if m["NODE_ENV"] != "test" || m["FOO"] != "1" || m["HOME"] != "/root" {
		t.Fatalf("unexpected merged env: %+v", m)
	}
	for _, need := range []string{"/root/.local/bin", "/usr/local/go/bin", "/usr/bin", "/bin"} {
		if !strings.Contains(m["PATH"], need) {
			t.Fatalf("PATH %q missing %q", m["PATH"], need)
		}
	}
}

func TestSafeDeleteTargetRejectsProtectedAndEscapes(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"", ".", "..", ".git", ".git/config", ".bvm_base_sha", "../outside", "/etc/passwd"} {
		if _, err := safeDeleteTarget(root, bad); err == nil {
			t.Fatalf("expected error for delete path %q", bad)
		}
	}
	got, err := safeDeleteTarget(root, "src/old.go")
	if err != nil {
		t.Fatalf("valid delete path failed: %v", err)
	}
	if got != filepath.Join(root, "src", "old.go") {
		t.Fatalf("got = %q, want %q", got, filepath.Join(root, "src", "old.go"))
	}
}

func TestLoadServiceSpecsFallbackAndStateless(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "services.json")
	specs := loadServiceSpecs(missing, nil)
	want := []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}}
	if !reflect.DeepEqual(specs, want) {
		t.Fatalf("missing fallback = %+v, want %+v", specs, want)
	}

	emptyFile := filepath.Join(t.TempDir(), "services.json")
	if err := os.WriteFile(emptyFile, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateless := loadServiceSpecs(emptyFile, nil)
	if stateless == nil || len(stateless) != 0 {
		t.Fatalf("empty file = %#v, want empty non-nil slice", stateless)
	}
}

func makeTestTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func makeMultipartSync(t *testing.T, meta types.SyncMeta, archive []byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mp, err := mw.CreateFormField("meta")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(mp).Encode(meta); err != nil {
		t.Fatal(err)
	}
	ap, err := mw.CreateFormFile("archive", "ws.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if len(archive) > 0 {
		if _, err := ap.Write(archive); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), buf.Bytes()
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestMultipartSyncDeltaDeleteAndFullPreservesIgnored(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	_ = os.MkdirAll(filepath.Join(root, "node_modules", "dep"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "dep", "index.js"), []byte("cached"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "sub", "nested"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "sub", "nested", "old.txt"), []byte("old"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0o644)
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-q", "-m", "base")

	a := &agent{procs: newProcTable()}
	// Delta sync: delete sub/nested/old.txt (and prune sub/nested + sub), update keep.txt.
	ctype, body := makeMultipartSync(t, types.SyncMeta{Mode: "delta", Delete: []string{"sub/nested/old.txt"}}, makeTestTarGz(t, map[string]string{
		"keep.txt": "updated",
	}))
	if err := a.applySyncRequest(ctype, bytes.NewReader(body), root); err != nil {
		t.Fatalf("delta applySyncRequest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub")); !os.IsNotExist(err) {
		t.Fatalf("empty parent dir sub/ was not pruned: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "keep.txt")); err != nil || string(b) != "updated" {
		t.Fatalf("keep.txt = %q (%v)", b, err)
	}

	// Full sync: removes tracked stale.txt? Let's test keep.txt disappears when omitted from full sync,
	// while git-ignored node_modules/dep/index.js remains untouched!
	ctypeFull, bodyFull := makeMultipartSync(t, types.SyncMeta{Mode: "full"}, makeTestTarGz(t, map[string]string{
		"fresh.txt": "new",
	}))
	if err := a.applySyncRequest(ctypeFull, bytes.NewReader(bodyFull), root); err != nil {
		t.Fatalf("full applySyncRequest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale tracked keep.txt survived full sync: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "node_modules", "dep", "index.js")); err != nil || string(b) != "cached" {
		t.Fatalf("git-ignored cache was wiped on full sync: %q (%v)", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "fresh.txt")); err != nil || string(b) != "new" {
		t.Fatalf("fresh.txt = %q (%v)", b, err)
	}
}

func TestExtractTarDirDoesNotFollowParentSymlink(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()

	var tarBuf bytes.Buffer
	gz := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "esc", Linkname: outside, Mode: 0o777})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "esc/pwned", Mode: 0o755})
	_ = tw.Close()
	_ = gz.Close()

	if err := extractTarGz(bytes.NewReader(tarBuf.Bytes()), root); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("tar TypeDir escaped through parent symlink into %s/pwned", outside)
	}
	if fi, err := os.Stat(filepath.Join(root, "esc", "pwned")); err != nil || !fi.IsDir() {
		t.Fatalf("expected dir inside root/esc/pwned, got err=%v", err)
	}
}

func TestServiceDeadBlocksLateReady(t *testing.T) {
	a := &agent{
		svcState: map[string]bool{"worker": false},
		svcDead:  map[string]bool{},
	}
	a.markServiceDead("worker")
	a.setServiceReady("worker", true)
	if a.servicesReady.Load() || a.svcState["worker"] {
		t.Fatalf("dead service transitioned back to ready: ready=%t state=%+v", a.servicesReady.Load(), a.svcState)
	}
}

func TestHandlePTYExitsWithBackgroundChild(t *testing.T) {
	a := &agent{procs: newProcTable()}
	srv := httptest.NewServer(http.HandlerFunc(a.handlePTY))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	started := time.Now()
	payload, _ := json.Marshal(types.PTYRequest{
		Cmd: []string{"sh", "-c", "trap '' HUP; sleep 5 & echo bg-ok"},
		Dir: t.TempDir(),
	})
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/pty", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "bvm-pty")
	req.ContentLength = int64(len(payload))
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake failed: resp=%+v err=%v", resp, err)
	}

	var out bytes.Buffer
	for {
		kind, data, err := types.ReadPTYFrame(br)
		if err != nil {
			t.Fatalf("ReadPTYFrame: %v (output so far=%q)", err, out.String())
		}
		if kind == types.PTYFrameData {
			out.Write(data)
			continue
		}
		if kind == types.PTYFrameExit {
			code := int(int32(binary.BigEndian.Uint32(data)))
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (out=%q)", code, out.String())
			}
			if !strings.Contains(out.String(), "bg-ok") {
				t.Fatalf("output %q missing bg-ok", out.String())
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("handlePTY waited for background child: %s", elapsed)
			}
			return
		}
	}
}

func TestTwoPhaseCustomServiceBoot(t *testing.T) {
	ws := t.TempDir()
	marker := filepath.Join(ws, "custom.started")
	a := &agent{
		procs: newProcTable(),
		services: []types.ServiceSpec{
			{Name: "web", Cmd: []string{"sh", "-c", fmt.Sprintf("touch %q && sleep 5", marker)}},
		},
		svcState: map[string]bool{},
		svcDead:  map[string]bool{},
		wsDir:    ws,
	}
	a.runServices()

	if !a.presetsReady.Load() || !a.pgReady.Load() {
		t.Fatalf("expected presetsReady and pgReady true before custom start, got presets=%t pg=%t", a.presetsReady.Load(), a.pgReady.Load())
	}
	if a.servicesReady.Load() {
		t.Fatalf("expected servicesReady false before /services/start")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("custom service ran before /services/start: err=%v", err)
	}

	h := a.routes()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var hr types.HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&hr); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if !hr.PresetsReady || hr.ServicesReady || hr.Version != execdVersion {
		t.Fatalf("health = %+v, want presets_ready=true services_ready=false version=%q", hr, execdVersion)
	}

	startReq := httptest.NewRequest(http.MethodPost, "/services/start", nil)
	startRec := httptest.NewRecorder()
	h.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("POST /services/start status = %d, want 200", startRec.Code)
	}

	// Second call must be idempotent.
	startRec2 := httptest.NewRecorder()
	h.ServeHTTP(startRec2, httptest.NewRequest(http.MethodPost, "/services/start", nil))
	if startRec2.Code != http.StatusOK {
		t.Fatalf("second POST /services/start status = %d, want 200", startRec2.Code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a.servicesReady.Load() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !a.servicesReady.Load() {
		t.Fatalf("custom service did not become ready after /services/start (state=%+v)", a.serviceSnapshot())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file %s missing: %v", marker, err)
	}
}

func TestWaitServiceReadyRequiresBothTCPAndCmd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ws := t.TempDir()
	flagFile := filepath.Join(ws, "ready.flag")
	a := &agent{procs: newProcTable(), wsDir: ws}
	exited := make(chan struct{})

	// TCP is listening, but ReadyCmd fails until flagFile exists.
	svcFail := types.ServiceSpec{
		Name:     "both-fail",
		ReadyTCP: ln.Addr().String(),
		ReadyCmd: []string{"test", "-f", flagFile},
	}
	if err := a.waitServiceReady(svcFail, exited, 400*time.Millisecond); err == nil {
		t.Fatal("waitServiceReady succeeded even though ReadyCmd failed")
	}

	if err := os.WriteFile(flagFile, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.waitServiceReady(svcFail, exited, 2*time.Second); err != nil {
		t.Fatalf("waitServiceReady failed when both TCP and ReadyCmd pass: %v", err)
	}
}

func TestHandleRebaseAndDiff(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "app.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &agent{procs: newProcTable(), wsDir: ws}
	h := a.routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rebase", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /rebase status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var sr syncResponse
	if err := json.NewDecoder(rec.Body).Decode(&sr); err != nil {
		t.Fatalf("decode rebase response: %v", err)
	}
	if !sr.OK || sr.BaseSHA == "" {
		t.Fatalf("unexpected rebase response: %+v", sr)
	}

	if err := os.WriteFile(filepath.Join(ws, "app.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diffRec := httptest.NewRecorder()
	h.ServeHTTP(diffRec, httptest.NewRequest(http.MethodGet, "/diff", nil))
	if diffRec.Code != http.StatusOK {
		t.Fatalf("GET /diff status = %d, body=%s", diffRec.Code, diffRec.Body.String())
	}
	if !strings.Contains(diffRec.Body.String(), "+v2") {
		t.Fatalf("diff missing +v2: %s", diffRec.Body.String())
	}
}

func TestHandleProbe(t *testing.T) {
	a := &agent{procs: newProcTable()}
	h := a.routes()

	// Local HTTP server checking custom header.
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Probe-Token") != "secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer targetSrv.Close()

	doProbe := func(req types.ProbeRequest) types.ProbeResponse {
		body, _ := json.Marshal(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/probe", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /probe status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var resp types.ProbeResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode probe response: %v", err)
		}
		return resp
	}

	httpResp := doProbe(types.ProbeRequest{
		Kind:      "http",
		URL:       targetSrv.URL,
		HeaderKey: "X-Probe-Token",
		HeaderVal: "secret",
	})
	if !httpResp.OK || httpResp.StatusCode != http.StatusNoContent {
		t.Fatalf("http probe = %+v, want OK with 204", httpResp)
	}

	// Closed TCP port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedTCPAddr := ln.Addr().String()
	_ = ln.Close()
	tcpResp := doProbe(types.ProbeRequest{Kind: "tcp", Addr: closedTCPAddr, TimeoutMs: 200})
	if tcpResp.OK || tcpResp.Error == "" {
		t.Fatalf("closed tcp probe = %+v, want OK=false with error", tcpResp)
	}

	// Unreachable UDP port.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedUDPAddr := pc.LocalAddr().String()
	_ = pc.Close()
	udpResp := doProbe(types.ProbeRequest{Kind: "udp-dns", Addr: closedUDPAddr, TimeoutMs: 200})
	if udpResp.OK || udpResp.Error == "" {
		t.Fatalf("unreachable udp-dns probe = %+v, want OK=false with error", udpResp)
	}
}

func TestHandlePTYEOFPipedCat(t *testing.T) {
	a := &agent{procs: newProcTable()}
	srv := httptest.NewServer(http.HandlerFunc(a.handlePTY))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	runCat := func(input, wantSubstr string) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		payload, _ := json.Marshal(types.PTYRequest{Cmd: []string{"cat"}, Dir: t.TempDir()})
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/pty", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "bvm-pty")
		req.ContentLength = int64(len(payload))
		if err := req.Write(conn); err != nil {
			t.Fatal(err)
		}

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, req)
		if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("handshake failed: resp=%+v err=%v", resp, err)
		}

		if err := types.WritePTYFrame(conn, types.PTYFrameData, []byte(input)); err != nil {
			t.Fatal(err)
		}
		if err := types.WritePTYFrame(conn, types.PTYFrameEOF, nil); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer
		for {
			kind, data, err := types.ReadPTYFrame(br)
			if err != nil {
				t.Fatalf("ReadPTYFrame: %v (out=%q)", err, out.String())
			}
			if kind == types.PTYFrameData {
				out.Write(data)
				continue
			}
			if kind == types.PTYFrameExit {
				code := int(int32(binary.BigEndian.Uint32(data)))
				if code != 0 {
					t.Fatalf("exit code = %d, want 0 (out=%q)", code, out.String())
				}
				if !strings.Contains(out.String(), wantSubstr) {
					t.Fatalf("output %q missing %q", out.String(), wantSubstr)
				}
				return
			}
		}
	}

	runCat("hello\n", "hello")
	runCat("no-newline", "no-newline")
}

func TestFindPgDataAndConfFromImageEnv(t *testing.T) {
	t.Setenv("PGDATA", "/from/process-env")
	dd, cf := findPgDataAndConf([]string{"FOO=bar", "PGDATA=/from/image-env"})
	if dd != "/from/image-env" || cf != "" {
		t.Fatalf("findPgDataAndConf = (%q, %q), want (%q, %q)", dd, cf, "/from/image-env", "")
	}

	t.Setenv("PGDATA", "")
	dd, cf = findPgDataAndConf([]string{"FOO=bar"})
	if dd != "/var/lib/postgresql/data" || cf != "" {
		t.Fatalf("fallback findPgDataAndConf = (%q, %q), want (%q, %q)", dd, cf, "/var/lib/postgresql/data", "")
	}
}

func TestSafeDeleteTargetRejectsIntermediateSymlink(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeDeleteTarget(root, "link/secret.txt"); err == nil {
		t.Fatal("expected safeDeleteTarget to reject intermediate symlink")
	}
	if _, err := safeDeleteTarget(root, "link"); err != nil {
		t.Fatalf("unexpected error deleting leaf symlink: %v", err)
	}
}
