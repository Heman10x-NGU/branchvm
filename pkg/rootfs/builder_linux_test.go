//go:build linux

package rootfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBuildExt4Integration(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 unavailable")
	}
	if _, err := exec.LookPath("e2fsck"); err != nil {
		t.Skip("e2fsck unavailable")
	}
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "test.ext4")
	if err := buildExt4FromDir(context.Background(), staging, dst, 64, 4096, os.Stdout); err != nil {
		t.Fatalf("buildExt4FromDir: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Size() != 64<<20 {
		t.Fatalf("ext4 stat = (%v, %v), want 64 MiB", fi, err)
	}
}
