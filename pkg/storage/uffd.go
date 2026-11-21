//go:build linux

package storage

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// userfaultfd ABI shared by amd64 and arm64.
const (
	uffdMsgSize        = 32
	uffdEventPagefault = 0x12
	uffdEventRemove    = 0x15
	// _IOWR(0xAA, 0x03, struct uffdio_copy), 40-byte struct.
	uffdioCopy = 0xc028aa03
	// _IOWR(0xAA, 0x04, struct uffdio_zeropage), 32-byte struct.
	uffdioZeropage = 0xc020aa04
	copyRetries    = 64
)

// UffdMapping is one guest memory region as Firecracker describes it in
// the Uffd handshake. Newer releases send page_size in bytes alongside
// the older page_size_kib.
type UffdMapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSizeKiB      uint64 `json:"page_size_kib"`
	PageSize         uint64 `json:"page_size"`
}

func (m UffdMapping) pageSize() uint64 {
	ps := m.PageSize
	if ps == 0 {
		ps = m.PageSizeKiB << 10
	}
	return max(ps, uint64(os.Getpagesize()))
}

type uffdioCopyArg struct {
	dst, src, len, mode uint64
	copy                int64
}

type uffdioZeropageArg struct {
	start, len, mode uint64
	zeropage         int64
}

// UffdServer serves the guest memory of one restored VM from the golden
// mem file through userfaultfd. Firecracker connects to its socket during
// LoadSnapshot, hands over the userfaultfd, and every first touch of a
// guest page becomes one UFFDIO_COPY from this process.
type UffdServer struct {
	sockPath string
	mem      []byte
	ln       *net.UnixListener

	mu       sync.Mutex
	conn     *net.UnixConn
	uffd     *os.File
	loopDone chan struct{}
	loopErr  error
	closed   bool

	maps    []UffdMapping
	removed [][2]uint64 // touched only by the fault loop

	faults atomic.Int64
	bytes  atomic.Int64
}

// NewUffdServer maps memFilePath read-only and listens on socketPath.
func NewUffdServer(memFilePath, socketPath string) (*UffdServer, error) {
	f, err := os.Open(memFilePath)
	if err != nil {
		return nil, fmt.Errorf("uffd: open %s: %w", memFilePath, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("uffd: stat %s: %w", memFilePath, err)
	}
	if fi.Size() <= 0 {
		return nil, fmt.Errorf("uffd: %s is empty", memFilePath)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("uffd: mmap %s: %w", memFilePath, err)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		syscall.Munmap(mem)
		return nil, fmt.Errorf("uffd: remove stale socket %s: %w", socketPath, err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		syscall.Munmap(mem)
		return nil, fmt.Errorf("uffd: listen %s: %w", socketPath, err)
	}
	return &UffdServer{sockPath: socketPath, mem: mem, ln: ln}, nil
}

// Serve accepts Firecracker's single connection, receives the mappings
// and the userfaultfd, starts the fault loop, and returns. The loop runs
// until Close or until Firecracker closes the connection.
func (s *UffdServer) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { s.ln.Close() })
	conn, err := s.ln.AcceptUnix()
	stop()
	if err != nil {
		return fmt.Errorf("uffd: accept on %s: %w", s.sockPath, err)
	}
	maps, fd, err := recvHandshake(conn)
	if err != nil {
		conn.Close()
		return err
	}
	if err := s.validate(maps); err != nil {
		conn.Close()
		syscall.Close(fd)
		return err
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		conn.Close()
		syscall.Close(fd)
		return fmt.Errorf("uffd: set nonblock: %w", err)
	}
	uffd := os.NewFile(uintptr(fd), "userfaultfd")
	rc, err := uffd.SyscallConn()
	if err != nil {
		conn.Close()
		uffd.Close()
		return fmt.Errorf("uffd: raw conn: %w", err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.Close()
		uffd.Close()
		return errors.New("uffd: server closed during handshake")
	}
	s.conn, s.uffd, s.maps, s.loopDone = conn, uffd, maps, make(chan struct{})
	s.mu.Unlock()
	go s.watchConn(conn, uffd)
	go s.faultLoop(uffd, rc)
	return nil
}

func recvHandshake(conn *net.UnixConn) ([]UffdMapping, int, error) {
	buf, oob := make([]byte, 64<<10), make([]byte, 4096)
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, -1, fmt.Errorf("uffd: recvmsg: %w", err)
	}
	cmsgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, -1, fmt.Errorf("uffd: parse control message: %w", err)
	}
	var fds []int
	for i := range cmsgs {
		got, err := syscall.ParseUnixRights(&cmsgs[i])
		if err == nil {
			fds = append(fds, got...)
		}
	}
	if len(fds) == 0 {
		return nil, -1, errors.New("uffd: handshake carried no file descriptor")
	}
	for _, extra := range fds[1:] {
		syscall.Close(extra)
	}
	var maps []UffdMapping
	if err := json.Unmarshal(buf[:n], &maps); err != nil {
		syscall.Close(fds[0])
		return nil, -1, fmt.Errorf("uffd: decode mappings (%d bytes): %w", n, err)
	}
	return maps, fds[0], nil
}

func (s *UffdServer) validate(maps []UffdMapping) error {
	if len(maps) == 0 {
		return errors.New("uffd: handshake has no mappings")
	}
	for i, m := range maps {
		ps := m.pageSize()
		switch {
		case ps&(ps-1) != 0:
			return fmt.Errorf("uffd: mapping %d: page size %d is not a power of two", i, ps)
		case m.Size == 0 || m.BaseHostVirtAddr%ps != 0:
			return fmt.Errorf("uffd: mapping %d: size %d, base %#x not aligned to %d", i, m.Size, m.BaseHostVirtAddr, ps)
		case m.Offset+m.Size > uint64(len(s.mem)) || m.Offset+m.Size < m.Offset:
			return fmt.Errorf("uffd: mapping %d: [%d,+%d) exceeds mem file (%d bytes)", i, m.Offset, m.Size, len(s.mem))
		}
	}
	return nil
}

// watchConn closes the userfaultfd when Firecracker hangs up, which ends
// the fault loop: a userfaultfd whose VM exited never reports EOF itself.
func (s *UffdServer) watchConn(conn *net.UnixConn, uffd *os.File) {
	buf := make([]byte, 64)
	for {
		if _, err := conn.Read(buf); err != nil {
			uffd.Close()
			return
		}
	}
}

func (s *UffdServer) faultLoop(uffd *os.File, rc syscall.RawConn) {
	defer close(s.loopDone)
	buf := make([]byte, uffdMsgSize*64)
	for {
		n, err := uffd.Read(buf)
		if err != nil {
			if !errors.Is(err, os.ErrClosed) {
				s.setErr(fmt.Errorf("uffd: read events: %w", err))
			}
			return
		}
		for off := 0; off+uffdMsgSize <= n; off += uffdMsgSize {
			if err := s.handle(rc, buf[off:off+uffdMsgSize]); err != nil {
				s.setErr(err)
				return
			}
		}
	}
}

func (s *UffdServer) handle(rc syscall.RawConn, msg []byte) error {
	le := binary.LittleEndian
	switch msg[0] {
	case uffdEventPagefault:
		return s.serveFault(rc, le.Uint64(msg[16:24]))
	case uffdEventRemove:
		// Balloon is never attached, so this is rare; pages in a removed
		// range must read back as zeros, not golden contents.
		s.removed = append(s.removed, [2]uint64{le.Uint64(msg[8:16]), le.Uint64(msg[16:24])})
	}
	return nil
}

func (s *UffdServer) serveFault(rc syscall.RawConn, addr uint64) error {
	var m *UffdMapping
	for i := range s.maps {
		if addr >= s.maps[i].BaseHostVirtAddr && addr < s.maps[i].BaseHostVirtAddr+s.maps[i].Size {
			m = &s.maps[i]
			break
		}
	}
	if m == nil {
		return fmt.Errorf("uffd: fault at %#x outside every mapping", addr)
	}
	ps := m.pageSize()
	page := addr &^ (ps - 1)
	if s.isRemoved(page) {
		arg := uffdioZeropageArg{start: page, len: ps}
		return s.ioctl(rc, uffdioZeropage, unsafe.Pointer(&arg), "UFFDIO_ZEROPAGE", page)
	}
	off := m.Offset + (page - m.BaseHostVirtAddr)
	arg := uffdioCopyArg{dst: page, src: uint64(uintptr(unsafe.Pointer(&s.mem[off]))), len: ps}
	if err := s.ioctl(rc, uffdioCopy, unsafe.Pointer(&arg), "UFFDIO_COPY", page); err != nil {
		return err
	}
	if arg.copy > 0 {
		s.faults.Add(1)
		s.bytes.Add(arg.copy)
	}
	return nil
}

// ioctl retries EAGAIN (the mm is changing under us) and treats EEXIST as
// success: another vCPU already populated the page.
func (s *UffdServer) ioctl(rc syscall.RawConn, req uintptr, arg unsafe.Pointer, name string, page uint64) error {
	for try := 0; try < copyRetries; try++ {
		var errno syscall.Errno
		if err := rc.Control(func(fd uintptr) {
			_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
		}); err != nil {
			return fmt.Errorf("uffd: %s at %#x: %w", name, page, err)
		}
		switch errno {
		case 0, syscall.EEXIST:
			return nil
		case syscall.EAGAIN:
			continue
		default:
			return fmt.Errorf("uffd: %s at %#x: %w", name, page, errno)
		}
	}
	return fmt.Errorf("uffd: %s at %#x: still EAGAIN after %d tries", name, page, copyRetries)
}

func (s *UffdServer) isRemoved(page uint64) bool {
	for _, r := range s.removed {
		if page >= r[0] && page < r[1] {
			return true
		}
	}
	return false
}

func (s *UffdServer) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loopErr == nil && !s.closed {
		s.loopErr = err
	}
}

// Stats reports page faults served and bytes copied into the guest.
func (s *UffdServer) Stats() (faults, bytesCopied int64) {
	return s.faults.Load(), s.bytes.Load()
}

// Close stops the listener and fault loop, unmaps the mem file, and
// removes the socket. It returns the first fault loop error, if any.
func (s *UffdServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.ln.Close()
	if s.conn != nil {
		s.conn.Close()
		s.uffd.Close()
	}
	done := s.loopDone
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	var errs []error
	if err := syscall.Munmap(s.mem); err != nil {
		errs = append(errs, fmt.Errorf("uffd: munmap: %w", err))
	}
	if err := os.Remove(s.sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("uffd: remove socket: %w", err))
	}
	s.mu.Lock()
	errs = append(errs, s.loopErr)
	s.mu.Unlock()
	return errors.Join(errs...)
}
