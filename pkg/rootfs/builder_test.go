package rootfs

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

// minimalELF constructs a tiny synthetic 64-bit ELF binary header for testing
// ValidateExecdELF without invoking a cross-compiler.
func minimalELF(machine elf.Machine, withInterp bool) []byte {
	var buf bytes.Buffer
	ehdr := elf.Header64{
		Ident:     [elf.EI_NIDENT]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)},
		Type:      uint16(elf.ET_EXEC),
		Machine:   uint16(machine),
		Version:   uint32(elf.EV_CURRENT),
		Phoff:     64,
		Ehsize:    64,
		Phentsize: 56,
		Phnum:     1,
	}
	_ = binary.Write(&buf, binary.LittleEndian, &ehdr)
	pType := uint32(elf.PT_LOAD)
	if withInterp {
		pType = uint32(elf.PT_INTERP)
	}
	phdr := elf.Prog64{Type: pType}
	_ = binary.Write(&buf, binary.LittleEndian, &phdr)
	return buf.Bytes()
}

func TestValidateExecdELF(t *testing.T) {
	dir := t.TempDir()
	armStatic := filepath.Join(dir, "execd-arm64")
	amdStatic := filepath.Join(dir, "execd-amd64")
	amdDyn := filepath.Join(dir, "execd-dyn")
	nonELF := filepath.Join(dir, "execd-txt")

	for _, f := range []struct {
		path string
		data []byte
	}{
		{armStatic, minimalELF(elf.EM_AARCH64, false)},
		{amdStatic, minimalELF(elf.EM_X86_64, false)},
		{amdDyn, minimalELF(elf.EM_X86_64, true)},
		{nonELF, []byte("not an elf")},
	} {
		if err := os.WriteFile(f.path, f.data, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := ValidateExecdELF(armStatic, "arm64"); err != nil {
		t.Fatalf("arm64 static ELF failed: %v", err)
	}
	if err := ValidateExecdELF(amdStatic, "amd64"); err != nil {
		t.Fatalf("amd64 static ELF failed: %v", err)
	}
	if err := ValidateExecdELF(armStatic, "amd64"); err == nil {
		t.Fatal("expected arch mismatch error")
	}
	if err := ValidateExecdELF(amdDyn, "amd64"); err == nil || !strings.Contains(err.Error(), "dynamically linked") {
		t.Fatalf("expected dynamically linked error, got %v", err)
	}
	if err := ValidateExecdELF(nonELF, "amd64"); err == nil {
		t.Fatal("expected non-ELF error")
	}
}

func TestSafeStagePathSymlinkContainment(t *testing.T) {
	staging := t.TempDir()

	// Simulate Debian/Ubuntu merged-/usr: /sbin -> /usr/sbin, and
	// /etc/resolv.conf -> /run/systemd/resolve/stub-resolv.conf (or absolute /etc/host-escape).
	if err := os.MkdirAll(filepath.Join(staging, "usr", "sbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/sbin", filepath.Join(staging, "sbin")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staging, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// If followed naively, writing /etc/resolv.conf would touch /tmp/should-never-exist.
	outsideTarget := filepath.Join(t.TempDir(), "escaped")
	if err := os.Symlink(outsideTarget, filepath.Join(staging, "etc", "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	// Also test a relative escaping symlink inside staging: /escape -> ../../../etc/passwd
	if err := os.Symlink("../../../../etc/passwd", filepath.Join(staging, "escape")); err != nil {
		t.Fatal(err)
	}

	if err := safeWriteFile(staging, "/sbin/init", []byte("execd-bytes"), 0o755); err != nil {
		t.Fatalf("safeWriteFile /sbin/init: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(staging, "usr", "sbin", "init"))
	if err != nil || string(got) != "execd-bytes" {
		t.Fatalf("merged-/usr /usr/sbin/init = %q (%v), want execd-bytes", got, err)
	}

	if err := safeWriteFile(staging, "/etc/resolv.conf", []byte("nameserver 127.0.0.1\n"), 0o644); err != nil {
		t.Fatalf("safeWriteFile /etc/resolv.conf: %v", err)
	}
	if _, err := os.Stat(outsideTarget); !os.IsNotExist(err) {
		t.Fatalf("outside symlink target was modified: %v", err)
	}
	gotResolv, err := os.ReadFile(filepath.Join(staging, "etc", "resolv.conf"))
	if err != nil || string(gotResolv) != "nameserver 127.0.0.1\n" {
		t.Fatalf("staging/etc/resolv.conf = %q (%v)", gotResolv, err)
	}

	resolvedEscape, err := resolveInsideStaging(staging, "/escape/child", false)
	if err != nil {
		t.Fatalf("resolveInsideStaging escape: %v", err)
	}
	if !strings.HasPrefix(resolvedEscape, staging+string(os.PathSeparator)) {
		t.Fatalf("resolved path %q escaped staging %q", resolvedEscape, staging)
	}

	if err := os.Symlink("../../../../", filepath.Join(staging, "toRoot")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInsideStaging(staging, "/toRoot", true); err == nil {
		t.Fatal("expected error for symlink resolving to staging root")
	}
}

func TestStageGuestOverlay(t *testing.T) {
	staging := t.TempDir()
	binDir := filepath.Join(staging, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateGuestBins(staging, nil); err != nil {
		t.Fatalf("validateGuestBins: %v", err)
	}

	fakeExecd := filepath.Join(t.TempDir(), "execd")
	if err := os.WriteFile(fakeExecd, []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stageGuestOverlay(staging, fakeExecd, nil, []string{"PATH=/usr/local/bin:/bin"}); err != nil {
		t.Fatalf("stageGuestOverlay: %v", err)
	}
	svcData, err := os.ReadFile(filepath.Join(staging, "etc", "branchvm", "services.json"))
	if err != nil || strings.TrimSpace(string(svcData)) != "[]" {
		t.Fatalf("services.json = %q (%v), want []", svcData, err)
	}
	claudeData, err := os.ReadFile(filepath.Join(staging, "root", ".claude.json"))
	if err != nil || !strings.Contains(string(claudeData), "hasCompletedOnboarding") {
		t.Fatalf(".claude.json = %q (%v)", claudeData, err)
	}
}

func TestComputeExt4SizingAndCacheKey(t *testing.T) {
	size, inodes := ComputeExt4Sizing(200<<20, 1000, 0)
	if size != 1536 || inodes != minInodes {
		t.Fatalf("small staging got (%d MiB, %d inodes), want (1536, %d)", size, inodes, minInodes)
	}
	bigSize, bigInodes := ComputeExt4Sizing(2000<<20, 80000, 0)
	if bigSize < 3200 || bigSize%64 != 0 {
		t.Fatalf("bigSize = %d MiB, expected >= 3200 aligned to 64", bigSize)
	}
	if bigInodes != 80000*2+inodeMargin {
		t.Fatalf("bigInodes = %d, want %d", bigInodes, 80000*2+inodeMargin)
	}
	overrideSize, _ := ComputeExt4Sizing(200<<20, 1000, 4096)
	if overrideSize != 4096 {
		t.Fatalf("overrideSize = %d, want 4096", overrideSize)
	}

	k1, err := CacheKey("sha256:abc", "deadbeef", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := CacheKey("sha256:abc", "deadbeef", []types.ServiceSpec{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	k3, err := CacheKey("sha256:abc", "deadbeef", []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 || k1 == k3 {
		t.Fatalf("cache keys unexpected: k1=%s k2=%s k3=%s", k1, k2, k3)
	}
}

func TestResolveDockerfileContainment(t *testing.T) {
	repo := t.TempDir()
	df := filepath.Join(repo, "Dockerfile")
	if err := os.WriteFile(df, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, got, err := ResolveDockerfile(repo, "Dockerfile")
	if err != nil || got != df {
		t.Fatalf("ResolveDockerfile valid = (%q, %v), want %q", got, err, df)
	}
	if _, _, err := ResolveDockerfile(repo, "../outside/Dockerfile"); err == nil {
		t.Fatal("expected containment error for ../outside/Dockerfile")
	}

	outside := filepath.Join(t.TempDir(), "OutsideDockerfile")
	if err := os.WriteFile(outside, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "EscapeDockerfile")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveDockerfile(repo, "EscapeDockerfile"); err == nil {
		t.Fatal("expected containment error for symlinked EscapeDockerfile")
	}
}

func TestComputeExt4SizingHighInodeAndBlockRounding(t *testing.T) {
	// 500,000 small files occupying one 4 KiB block each.
	sizeMB, inodes := ComputeExt4Sizing(500000*4096, 500000, 0)
	if sizeMB < 2300 || sizeMB%roundStepMB != 0 {
		t.Fatalf("500k small files sizeMB = %d, want >= 2300 aligned to %d", sizeMB, roundStepMB)
	}
	wantInodes := int64(500000*2 + inodeMargin)
	if inodes != wantInodes {
		t.Fatalf("500k small files inodes = %d, want %d", inodes, wantInodes)
	}

	// Increasing entry count with fixed byte total must grow image size for inode table overhead.
	lowEntriesMB, _ := ComputeExt4Sizing(1000<<20, 1000, 0)
	highEntriesMB, _ := ComputeExt4Sizing(1000<<20, 2000000, 0)
	if highEntriesMB <= lowEntriesMB {
		t.Fatalf("expected high entry count size (%d MiB) > low entry count size (%d MiB)", highEntriesMB, lowEntriesMB)
	}
}

func TestMeasureDirExt4BlocksAndHardlinks(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	tiny := filepath.Join(sub, "tiny.txt")
	if err := os.WriteFile(tiny, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	medium := filepath.Join(sub, "medium.bin")
	if err := os.WriteFile(medium, bytes.Repeat([]byte("a"), 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(tiny, filepath.Join(sub, "tiny-hardlink.txt")); err != nil {
		t.Fatal(err)
	}

	gotBytes, gotEntries, err := measureDir(root)
	if err != nil {
		t.Fatalf("measureDir: %v", err)
	}
	// Entries: root dir, sub dir, tiny.txt, medium.bin, tiny-hardlink.txt = 5.
	if gotEntries != 5 {
		t.Fatalf("measureDir entries = %d, want 5", gotEntries)
	}
	// Allocated ext4 4 KiB blocks: 2 dirs (8192) + 1-byte file (4096) + 5000-byte file (8192) = 20480.
	// Hardlink shares tiny.txt's inode and adds 0 bytes.
	const wantBytes = int64(4096 + 4096 + 4096 + 8192)
	if gotBytes != wantBytes {
		t.Fatalf("measureDir bytesTotal = %d, want %d", gotBytes, wantBytes)
	}
}

func TestUntarPreservingRootRequiresTarBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := untarPreservingRoot(context.Background(), strings.NewReader(""), t.TempDir())
	if err == nil {
		t.Fatal("expected error when tar binary is missing from PATH")
	}
	if !strings.Contains(err.Error(), "tar") {
		t.Fatalf("expected clear tar missing error, got %v", err)
	}
}

func TestInjectBaseOverlayValidatesExecd(t *testing.T) {
	badExecd := filepath.Join(t.TempDir(), "execd-bad")
	if err := os.WriteFile(badExecd, []byte("not-an-elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := InjectBaseOverlay(context.Background(), filepath.Join(t.TempDir(), "rootfs.ext4"), badExecd, nil)
	if err == nil {
		t.Fatal("expected error for non-ELF execd")
	}
}
