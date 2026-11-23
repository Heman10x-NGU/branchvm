package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, rel, data string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// newRepo builds a repo with committed base files, then dirties it.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write(t, dir, "a.txt", "a\n", 0o644)
	write(t, dir, "b.txt", "b\n", 0o644)
	write(t, dir, "del.txt", "gone\n", 0o644)
	write(t, dir, "old.txt", "rename me\n", 0o644)
	write(t, dir, "bin/run.sh", "#!/bin/sh\n", 0o755)
	write(t, dir, ".gitignore", "*.log\n", 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")

	write(t, dir, "a.txt", "a modified\n", 0o644)
	write(t, dir, "staged.txt", "staged\n", 0o644)
	git(t, dir, "add", "staged.txt")
	write(t, dir, "untracked/new.txt", "new\n", 0o600)
	write(t, dir, "debug.log", "ignored\n", 0o644)
	if err := os.Remove(filepath.Join(dir, "del.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "mv", "old.txt", "renamed.txt")
	return dir
}

func TestDirtyFiles(t *testing.T) {
	dir := newRepo(t)
	got, err := DirtyFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt", "renamed.txt", "staged.txt", "untracked/new.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DirtyFiles = %v, want %v", got, want)
	}
}

type entry struct {
	data string
	mode int64
}

func untar(t *testing.T, blob []byte) ([]string, map[string]entry) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var order []string
	files := map[string]entry{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, h.Name)
		files[h.Name] = entry{string(b), h.Mode}
	}
	return order, files
}

func TestPackWorkspace(t *testing.T) {
	dir := newRepo(t)
	blob, err := PackWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	order, files := untar(t, blob)
	wantOrder := []string{".gitignore", "a.txt", "b.txt", "bin/run.sh", "renamed.txt", "staged.txt", "untracked/new.txt"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}
	tests := []struct {
		name, data string
		mode       int64
	}{
		{"a.txt", "a modified\n", 0o644},
		{"bin/run.sh", "#!/bin/sh\n", 0o755},
		{"renamed.txt", "rename me\n", 0o644},
		{"untracked/new.txt", "new\n", 0o600},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := files[tc.name]
			if got.data != tc.data || got.mode != tc.mode {
				t.Fatalf("%s = %q %o, want %q %o", tc.name, got.data, got.mode, tc.data, tc.mode)
			}
		})
	}
	again, err := PackWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob, again) {
		t.Fatal("PackWorkspace is not deterministic")
	}
}

func TestApplyPatch(t *testing.T) {
	src := t.TempDir()
	git(t, src, "init", "-q")
	write(t, src, "f.txt", "one\ntwo\n", 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "base")
	write(t, src, "f.txt", "one\nTWO\n", 0o644)
	write(t, src, "added.txt", "hi\n", 0o644)
	git(t, src, "add", "-A")
	good := []byte(git(t, src, "diff", "--cached", "--binary", "HEAD"))
	corrupt := bytes.Replace(good, []byte("-two"), []byte("-xyz"), 1)

	tests := []struct {
		name    string
		patch   []byte
		wantErr bool
		wantF   string
	}{
		{"happy", good, false, "one\nTWO\n"},
		{"corrupt", corrupt, true, "one\ntwo\n"},
		{"empty", nil, false, "one\ntwo\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			git(t, dst, "init", "-q")
			write(t, dst, "f.txt", "one\ntwo\n", 0o644)
			git(t, dst, "add", "-A")
			git(t, dst, "commit", "-q", "-m", "base")
			err := ApplyPatch(dst, tc.patch)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			b, rerr := os.ReadFile(filepath.Join(dst, "f.txt"))
			if rerr != nil || string(b) != tc.wantF {
				t.Fatalf("f.txt = %q (%v), want %q", b, rerr, tc.wantF)
			}
			_, statErr := os.Stat(filepath.Join(dst, "added.txt"))
			if tc.name == "happy" && statErr != nil {
				t.Fatal("added.txt missing after apply")
			}
			if tc.name != "happy" && statErr == nil {
				t.Fatal("added.txt present though patch not applied")
			}
		})
	}
}

func TestManifestDeltaSync(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write(t, dir, "keep.txt", "stay\n", 0o644)
	write(t, dir, "edit.txt", "v1\n", 0o644)
	write(t, dir, "del.txt", "bye\n", 0o644)
	write(t, dir, "rename_src.txt", "moveme\n", 0o644)
	write(t, dir, "mode.sh", "echo hi\n", 0o644)
	if err := os.Symlink("keep.txt", filepath.Join(dir, "sym.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")

	sha0, golden, err := WorkspaceManifest(dir)
	if err != nil || sha0 == "" {
		t.Fatalf("WorkspaceManifest init = (%q, %v)", sha0, err)
	}

	write(t, dir, "edit.txt", "v2\n", 0o644)
	write(t, dir, "added.txt", "new\n", 0o644)
	if err := os.Remove(filepath.Join(dir, "del.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "mv", "rename_src.txt", "rename_dst.txt")
	if err := os.Chmod(filepath.Join(dir, "mode.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "sym.txt"))
	if err := os.Symlink("edit.txt", filepath.Join(dir, "sym.txt")); err != nil {
		t.Fatal(err)
	}

	_, cur, err := WorkspaceManifest(dir)
	if err != nil {
		t.Fatalf("WorkspaceManifest cur: %v", err)
	}
	changed, deleted := DiffManifests(golden, cur)
	wantChanged := []string{"added.txt", "edit.txt", "mode.sh", "rename_dst.txt", "sym.txt"}
	wantDeleted := []string{"del.txt", "rename_src.txt"}
	if !reflect.DeepEqual(changed, wantChanged) {
		t.Fatalf("changed = %v, want %v", changed, wantChanged)
	}
	if !reflect.DeepEqual(deleted, wantDeleted) {
		t.Fatalf("deleted = %v, want %v", deleted, wantDeleted)
	}

	// Part 2: dirty file at init reverted later + untracked file deleted + commit amend.
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "clean base for part2")

	write(t, dir, "keep.txt", "dirty at init\n", 0o644)
	write(t, dir, "untracked_init.txt", "temp\n", 0o644)
	_, dirtyGolden, err := WorkspaceManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "checkout", "--", "keep.txt")
	if err := os.Remove(filepath.Join(dir, "untracked_init.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "--allow-empty", "--amend", "-q", "-m", "amended without touching files")

	_, afterRevert, err := WorkspaceManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	revChanged, revDeleted := DiffManifests(dirtyGolden, afterRevert)
	if !reflect.DeepEqual(revChanged, []string{"keep.txt"}) {
		t.Fatalf("revert changed = %v, want [keep.txt]", revChanged)
	}
	if !reflect.DeepEqual(revDeleted, []string{"untracked_init.txt"}) {
		t.Fatalf("untracked deleted = %v, want [untracked_init.txt]", revDeleted)
	}
}

func TestEncodeSyncMultipart(t *testing.T) {
	meta := types.SyncMeta{Mode: "delta", Delete: []string{"old.txt"}}
	archive := []byte("fake-tgz")
	body, ctype, err := EncodeSyncMultipart(meta, archive)
	if err != nil {
		t.Fatal(err)
	}
	media, params, err := mime.ParseMediaType(ctype)
	if err != nil || media != "multipart/form-data" {
		t.Fatalf("ctype = %q (%v)", ctype, err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var gotMeta types.SyncMeta
	var gotArchive []byte
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		switch p.FormName() {
		case "meta":
			if err := json.Unmarshal(b, &gotMeta); err != nil {
				t.Fatal(err)
			}
		case "archive":
			gotArchive = b
		}
		_ = p.Close()
	}
	if !reflect.DeepEqual(gotMeta, meta) || !bytes.Equal(gotArchive, archive) {
		t.Fatalf("roundtrip mismatch: meta=%+v archive=%q", gotMeta, gotArchive)
	}
}

func TestSubdirectoryRepoSyncAndApply(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	write(t, root, "sub/app.txt", "v1\n", 0o644)
	write(t, root, "other/sibling.txt", "keep\n", 0o644)
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "base")

	subDir := filepath.Join(root, "sub")
	_, golden, err := WorkspaceManifest(subDir)
	if err != nil {
		t.Fatalf("WorkspaceManifest init: %v", err)
	}

	// Dirty sibling outside subDir and add a new untracked file inside subDir.
	write(t, root, "other/sibling.txt", "dirty outside sub\n", 0o644)
	write(t, root, "sub/new.txt", "hello\n", 0o644)

	dirty, err := DirtyFiles(subDir)
	if err != nil {
		t.Fatalf("DirtyFiles: %v", err)
	}
	if !reflect.DeepEqual(dirty, []string{"new.txt"}) {
		t.Fatalf("DirtyFiles(subDir) = %v, want [new.txt]", dirty)
	}

	blob, err := PackWorkspace(subDir)
	if err != nil {
		t.Fatalf("PackWorkspace(subDir): %v", err)
	}
	order, _ := untar(t, blob)
	if !reflect.DeepEqual(order, []string{"app.txt", "new.txt"}) {
		t.Fatalf("PackWorkspace(subDir) order = %v, want [app.txt new.txt]", order)
	}

	if err := os.Remove(filepath.Join(subDir, "app.txt")); err != nil {
		t.Fatal(err)
	}
	_, cur, err := WorkspaceManifest(subDir)
	if err != nil {
		t.Fatalf("WorkspaceManifest cur: %v", err)
	}
	changed, deleted := DiffManifests(golden, cur)
	if !reflect.DeepEqual(changed, []string{"new.txt"}) || !reflect.DeepEqual(deleted, []string{"app.txt"}) {
		t.Fatalf("DiffManifests = (changed=%v, deleted=%v), want ([new.txt], [app.txt])", changed, deleted)
	}

	// Restore app.txt cleanly, then apply a patch targeting a/app.txt from subDir.
	git(t, root, "checkout", "--", "sub/app.txt")
	patch := []byte("diff --git a/app.txt b/app.txt\n--- a/app.txt\n+++ b/app.txt\n@@ -1 +1 @@\n-v1\n+v2\n")
	if err := ApplyPatch(subDir, patch); err != nil {
		t.Fatalf("ApplyPatch(subDir): %v", err)
	}
	gotApp, err := os.ReadFile(filepath.Join(subDir, "app.txt"))
	if err != nil || string(gotApp) != "v2\n" {
		t.Fatalf("sub/app.txt = %q (%v), want %q", gotApp, err, "v2\\n")
	}
	gotSibling, err := os.ReadFile(filepath.Join(root, "other/sibling.txt"))
	if err != nil || string(gotSibling) != "dirty outside sub\n" {
		t.Fatalf("sibling altered unexpectedly: %q (%v)", gotSibling, err)
	}
}

func TestApplyPatchBinaryAndNewFileViaStdin(t *testing.T) {
	src := t.TempDir()
	git(t, src, "init", "-q")
	write(t, src, "base.txt", "base\n", 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "base")

	binPayload := []byte{0x00, 0x01, 0x02, 0x7f, 'E', 'L', 'F', 0xff, 0xfe, '\n', 0x00}
	if err := os.MkdirAll(filepath.Join(src, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "assets", "blob.bin"), binPayload, 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, src, "scripts/run.sh", "#!/bin/sh\necho ok\n", 0o755)
	git(t, src, "add", "-A")
	patch := []byte(git(t, src, "diff", "--cached", "--binary", "HEAD"))

	dst := t.TempDir()
	git(t, dst, "init", "-q")
	write(t, dst, "base.txt", "base\n", 0o644)
	git(t, dst, "add", "-A")
	git(t, dst, "commit", "-q", "-m", "base")

	if err := ApplyPatch(dst, patch); err != nil {
		t.Fatalf("ApplyPatch binary/new-file: %v", err)
	}
	gotBin, err := os.ReadFile(filepath.Join(dst, "assets", "blob.bin"))
	if err != nil {
		t.Fatalf("read blob.bin: %v", err)
	}
	if !bytes.Equal(gotBin, binPayload) {
		t.Fatalf("blob.bin = %x, want %x", gotBin, binPayload)
	}
	fi, err := os.Stat(filepath.Join(dst, "scripts", "run.sh"))
	if err != nil {
		t.Fatalf("stat scripts/run.sh: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("scripts/run.sh mode = %o, expected executable", fi.Mode().Perm())
	}
}
