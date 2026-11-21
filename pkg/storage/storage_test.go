package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
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
