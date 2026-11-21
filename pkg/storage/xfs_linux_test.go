//go:build linux

package storage

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func hashFile(t *testing.T, p string) [32]byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func TestCloneRootfsXFS(t *testing.T) {
	switch {
	case os.Getenv("BRANCHVM_XFS_TEST") != "1":
		t.Skip("set BRANCHVM_XFS_TEST=1 to run the XFS reflink test")
	case os.Geteuid() != 0:
		t.Skip("XFS reflink test requires root")
	case !IsXFSMount(DefaultPoolMount):
		t.Skip(DefaultPoolMount + " is not an XFS mount")
	}
	work, err := os.MkdirTemp(DefaultPoolMount, "t2test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)

	base := filepath.Join(work, "base", "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 64<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, data, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := hashFile(t, base)

	const n = 5
	clones := make([]string, n)
	for i := range clones {
		clones[i] = filepath.Join(work, "clones", string(rune('a'+i)), "rootfs.ext4")
		if err := CloneRootfs(base, clones[i]); err != nil {
			t.Fatalf("clone %d: %v", i, err)
		}
		f, err := os.OpenFile(clones[i], os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		patch := bytes.Repeat([]byte{byte(0xA0 + i)}, 4096)
		if _, err := f.WriteAt(patch, int64(i+1)<<20); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if hashFile(t, base) != orig {
		t.Fatal("base rootfs was mutated by clone writes")
	}
	seen := map[[32]byte]int{orig: -1}
	for i, c := range clones {
		h := hashFile(t, c)
		if j, dup := seen[h]; dup {
			t.Fatalf("clone %d hash collides with %d", i, j)
		}
		seen[h] = i
	}
	for _, c := range clones {
		if err := RemoveClone(c); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Dir(c)); !os.IsNotExist(err) {
			t.Fatalf("clone dir %s left behind", filepath.Dir(c))
		}
	}
}
