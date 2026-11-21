//go:build linux

package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// ws.bin layout, little endian: 8-byte magic, then page size, page count,
// and resident count as uint64, then one bit per page (bit i%8 of byte i/8).
const (
	wsMagic      = "BVMWS1\x00\x00"
	wsHeaderSize = 32
	fadvWillNeed = 3 // POSIX_FADV_WILLNEED on every Linux arch
)

// WorkingSet summarizes a recorded ws.bin.
type WorkingSet struct {
	PageSize int64
	Pages    int64
	Resident int64
}

// ResidentBytes is the resident page count in bytes.
func (w WorkingSet) ResidentBytes() int64 { return w.Resident * w.PageSize }

// extent is a byte range of the mem file.
type extent struct{ off, len int64 }

// RecordWorkingSet maps memPath read-only, asks mincore(2) which of its
// pages sit in the host page cache, and writes that bitmap to outPath.
// Page cache residency covers every reader of the file, so the result is
// the set of pages any process touched since the cache was last dropped.
func RecordWorkingSet(memPath, outPath string) (WorkingSet, error) {
	vec, err := mincoreFile(memPath)
	if err != nil {
		return WorkingSet{}, err
	}
	ps := int64(os.Getpagesize())
	ws := WorkingSet{PageSize: ps, Pages: int64(len(vec))}
	bitmap := make([]byte, (len(vec)+7)/8)
	for i, v := range vec {
		if v&1 != 0 {
			bitmap[i/8] |= 1 << (i % 8)
			ws.Resident++
		}
	}
	var buf bytes.Buffer
	buf.WriteString(wsMagic)
	for _, v := range []int64{ws.PageSize, ws.Pages, ws.Resident} {
		_ = binary.Write(&buf, binary.LittleEndian, uint64(v))
	}
	buf.Write(bitmap)
	if err := writeFileAtomic(outPath, buf.Bytes()); err != nil {
		return WorkingSet{}, err
	}
	return ws, nil
}

// mincoreFile returns one mincore(2) byte per page of path, in the
// running kernel's page size.
func mincoreFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	size := fi.Size()
	if size <= 0 {
		return nil, fmt.Errorf("mincore %s: empty file", path)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	defer syscall.Munmap(data)
	ps := int64(os.Getpagesize())
	vec := make([]byte, (size+ps-1)/ps)
	_, _, errno := syscall.Syscall(syscall.SYS_MINCORE,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&vec[0])))
	if errno != 0 {
		return nil, fmt.Errorf("mincore %s: %w", path, errno)
	}
	return vec, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// ReadWorkingSet parses a ws.bin and returns its summary and bitmap.
func ReadWorkingSet(wsPath string) (WorkingSet, []byte, error) {
	raw, err := os.ReadFile(wsPath)
	if err != nil {
		return WorkingSet{}, nil, fmt.Errorf("read working set: %w", err)
	}
	if len(raw) < wsHeaderSize || string(raw[:8]) != wsMagic {
		return WorkingSet{}, nil, fmt.Errorf("%s: not a working set file", wsPath)
	}
	le := binary.LittleEndian
	ws := WorkingSet{
		PageSize: int64(le.Uint64(raw[8:16])),
		Pages:    int64(le.Uint64(raw[16:24])),
		Resident: int64(le.Uint64(raw[24:32])),
	}
	bitmap := raw[wsHeaderSize:]
	if ws.PageSize <= 0 || ws.PageSize&(ws.PageSize-1) != 0 || ws.Pages < 0 || int64(len(bitmap)) != (ws.Pages+7)/8 {
		return WorkingSet{}, nil, fmt.Errorf("%s: corrupt header (page size %d, pages %d, bitmap %d bytes)",
			wsPath, ws.PageSize, ws.Pages, len(bitmap))
	}
	return ws, bitmap, nil
}

// residentExtents coalesces runs of set bits into byte extents, clamping
// the last page to fileSize.
func residentExtents(bitmap []byte, pages, pageSize, fileSize int64) []extent {
	var out []extent
	for p := int64(0); p < pages; {
		if bitmap[p/8]&(1<<(p%8)) == 0 {
			p++
			continue
		}
		start := p
		for p < pages && bitmap[p/8]&(1<<(p%8)) != 0 {
			p++
		}
		off := start * pageSize
		end := min(p*pageSize, fileSize)
		if end > off {
			out = append(out, extent{off: off, len: end - off})
		}
	}
	return out
}

// Prefault issues posix_fadvise(POSIX_FADV_WILLNEED) over every resident
// extent recorded in wsPath, so the kernel starts reading those pages of
// memPath into the page cache before the guest faults on them. It returns
// the number of bytes advised. The bitmap is in the recorder's page size,
// so a ws.bin stays valid on a kernel with a different page size.
func Prefault(memPath, wsPath string) (int64, error) {
	ws, bitmap, err := ReadWorkingSet(wsPath)
	if err != nil {
		return 0, err
	}
	f, err := os.Open(memPath)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", memPath, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", memPath, err)
	}
	size := fi.Size()
	if want := (size + ws.PageSize - 1) / ws.PageSize; want != ws.Pages {
		return 0, fmt.Errorf("stale working set %s: %d pages recorded, %s has %d", wsPath, ws.Pages, memPath, want)
	}
	var advised int64
	for _, e := range residentExtents(bitmap, ws.Pages, ws.PageSize, size) {
		if err := fadvise(int(f.Fd()), e.off, e.len, fadvWillNeed); err != nil {
			return advised, fmt.Errorf("fadvise %s [%d,+%d): %w", memPath, e.off, e.len, err)
		}
		advised += e.len
	}
	return advised, nil
}

// fadvise uses the 64-bit fadvise64 argument order (fd, offset, len,
// advice), which amd64 and arm64 share.
func fadvise(fd int, off, length int64, advice int) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_FADVISE64,
		uintptr(fd), uintptr(off), uintptr(length), uintptr(advice), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
