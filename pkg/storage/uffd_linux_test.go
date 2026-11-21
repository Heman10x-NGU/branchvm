//go:build linux

package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

const (
	uffdioAPI      = 0xc018aa3f // _IOWR(0xAA, 0x3F, struct uffdio_api)
	uffdioRegister = 0xc020aa00 // _IOWR(0xAA, 0x00, struct uffdio_register)
	uffdModeMiss   = 1
)

func sysUserfaultfd() (uintptr, bool) {
	switch runtime.GOARCH {
	case "arm64":
		return 282, true
	case "amd64":
		return 323, true
	}
	return 0, false
}

func uffdIoctl(t *testing.T, fd int, req uintptr, arg unsafe.Pointer) {
	t.Helper()
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		t.Fatalf("ioctl %#x: %v", req, e)
	}
}

// TestUffdServerServesPages plays Firecracker: it registers anonymous
// memory with a userfaultfd, sends the mapping and the fd to the server,
// then reads the memory through write(2), so the faults are taken in the
// kernel while the goroutine is in a syscall and the Go scheduler keeps
// running the pager.
func TestUffdServerServesPages(t *testing.T) {
	nr, ok := sysUserfaultfd()
	if !ok {
		t.Skip("no userfaultfd syscall number for " + runtime.GOARCH)
	}
	ufd, _, e := syscall.Syscall(nr, syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0, 0)
	if e != 0 {
		t.Skipf("userfaultfd unavailable (%v); run as root or set vm.unprivileged_userfaultfd=1", e)
	}
	defer syscall.Close(int(ufd))
	api := [3]uint64{0xAA, 0, 0}
	uffdIoctl(t, int(ufd), uffdioAPI, unsafe.Pointer(&api))

	ps := os.Getpagesize()
	const pages = 4
	dir := t.TempDir()
	mem := make([]byte, pages*ps)
	for i := range mem {
		mem[i] = byte(i/ps + 1)
	}
	memPath := filepath.Join(dir, "mem")
	if err := os.WriteFile(memPath, mem, 0o644); err != nil {
		t.Fatal(err)
	}
	region, err := syscall.Mmap(-1, 0, len(mem), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(region)
	base := uint64(uintptr(unsafe.Pointer(&region[0])))
	reg := [4]uint64{base, uint64(len(region)), uffdModeMiss, 0}
	uffdIoctl(t, int(ufd), uffdioRegister, unsafe.Pointer(&reg))

	srv, err := NewUffdServer(memPath, filepath.Join(dir, "uffd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(context.Background()) }()

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(dir, "uffd.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload, _ := json.Marshal([]UffdMapping{{BaseHostVirtAddr: base, Size: uint64(len(region)), PageSizeKiB: uint64(ps >> 10)}})
	if _, _, err := conn.WriteMsgUnix(payload, syscall.UnixRights(int(ufd)), nil); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for p := 0; p < pages; p++ {
		if _, err := syscall.Write(int(w.Fd()), region[p*ps:(p+1)*ps]); err != nil {
			t.Fatalf("page %d: write: %v", p, err)
		}
		got := make([]byte, ps)
		if _, err := syscall.Read(int(r.Fd()), got); err != nil {
			t.Fatalf("page %d: read: %v", p, err)
		}
		if !bytes.Equal(got, mem[p*ps:(p+1)*ps]) {
			t.Fatalf("page %d: contents differ from the mem file", p)
		}
	}
	faults, copied := srv.Stats()
	if faults != pages || copied != int64(pages*ps) {
		t.Fatalf("stats: %d faults, %d bytes; want %d, %d", faults, copied, pages, pages*ps)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "uffd.sock")); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestUffdServerRejectsOversizedMapping(t *testing.T) {
	dir := t.TempDir()
	memPath := filepath.Join(dir, "mem")
	if err := os.WriteFile(memPath, make([]byte, os.Getpagesize()), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := NewUffdServer(memPath, filepath.Join(dir, "uffd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ps := uint64(os.Getpagesize())
	if err := srv.validate([]UffdMapping{{BaseHostVirtAddr: ps, Size: 2 * ps, PageSize: ps}}); err == nil {
		t.Fatal("mapping larger than the mem file accepted")
	}
	if err := srv.validate([]UffdMapping{{BaseHostVirtAddr: ps, Size: ps, PageSize: ps}}); err != nil {
		t.Fatalf("valid mapping rejected: %v", err)
	}
}
