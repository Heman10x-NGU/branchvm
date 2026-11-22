//go:build linux

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
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

	payload, _ := json.Marshal(types.PTYRequest{
		Cmd: []string{"sh", "-c", "sleep 30 & echo bg-ok"},
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
			return
		}
	}
}
