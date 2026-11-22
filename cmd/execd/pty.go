//go:build linux

package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

const (
	ioctlTIOCSCTTY = 0x540E
	ioctlTIOCSWINSZ = 0x5414
	ioctlTIOCGPTN  = 0x80045430
	ioctlTIOCSPTLCK = 0x40045431
	maxPTYReqBytes = 64 << 10
)

type ptyWinsize struct {
	Rows, Cols, Xpixel, Ypixel uint16
}

func (a *agent) handlePTY(w http.ResponseWriter, r *http.Request) {
	var req types.PTYRequest
	if r.Body != nil {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPTYReqBytes))
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			httpError(w, http.StatusBadRequest, fmt.Errorf("decode pty request: %w", err))
			return
		}
	}
	if len(req.Cmd) == 0 || req.Cmd[0] == "" {
		if _, err := os.Stat("/bin/bash"); err == nil {
			req.Cmd = []string{"/bin/bash"}
		} else {
			req.Cmd = []string{"/bin/sh"}
		}
	}
	if req.Dir == "" {
		req.Dir = workspaceDir
	}
	if req.Rows == 0 {
		req.Rows = 24
	}
	if req.Cols == 0 {
		req.Cols = 80
	}

	master, slave, err := openPTY()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	defer master.Close()
	_ = setWinsize(master.Fd(), req.Rows, req.Cols)

	cmd := exec.Command(req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = a.mergeEnv([]string{"TERM=xterm-256color"}, req.Env)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	if err := a.procs.start(cmd); err != nil {
		_ = slave.Close()
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	_ = slave.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = a.procs.wait(cmd)
		httpError(w, http.StatusInternalServerError, errors.New("http hijacking unsupported"))
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = a.procs.wait(cmd)
		return
	}
	defer conn.Close()

	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: bvm-pty\r\nConnection: Upgrade\r\n\r\n")
	if err := rw.Flush(); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = a.procs.wait(cmd)
		return
	}

	var (
		writeMu  sync.Mutex
		exitSent bool
	)
	sendFrame := func(kind byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if exitSent {
			return io.ErrClosedPipe
		}
		err := types.WritePTYFrame(conn, kind, payload)
		if kind == types.PTYFrameExit {
			exitSent = true
		}
		return err
	}

	masterDone := make(chan struct{})
	go func() {
		defer close(masterDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				if werr := sendFrame(types.PTYFrameData, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EIO) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, os.ErrDeadlineExceeded) {
					_ = sendFrame(types.PTYFrameData, []byte(fmt.Sprintf("\r\n[pty read error: %v]\r\n", err)))
				}
				return
			}
		}
	}()

	var (
		exited    atomic.Bool
		killMu    sync.Mutex
		killTimer *time.Timer
	)

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		for {
			kind, payload, err := types.ReadPTYFrame(rw.Reader)
			if err != nil {
				if !exited.Load() {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
					killMu.Lock()
					if !exited.Load() {
						killTimer = time.AfterFunc(250*time.Millisecond, func() {
							if !exited.Load() {
								_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
							}
						})
					}
					killMu.Unlock()
				}
				return
			}
			switch kind {
			case types.PTYFrameData:
				if len(payload) > 0 {
					if _, err := master.Write(payload); err != nil {
						return
					}
				}
			case types.PTYFrameResize:
				if len(payload) == 4 {
					rows := binary.BigEndian.Uint16(payload[0:2])
					cols := binary.BigEndian.Uint16(payload[2:4])
					_ = setWinsize(master.Fd(), rows, cols)
				}
			}
		}
	}()

	waitErr := a.procs.wait(cmd)
	exited.Store(true)
	killMu.Lock()
	if killTimer != nil {
		killTimer.Stop()
	}
	killMu.Unlock()

	select {
	case <-masterDone:
	case <-time.After(200 * time.Millisecond):
		_ = master.SetReadDeadline(time.Now())
		select {
		case <-masterDone:
		case <-time.After(100 * time.Millisecond):
		}
	}

	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if waitErr != nil {
		exitCode = 1
	}
	var codeBuf [4]byte
	binary.BigEndian.PutUint32(codeBuf[:], uint32(int32(exitCode)))
	_ = sendFrame(types.PTYFrameExit, codeBuf[:])
	_ = master.Close()
	select {
	case <-clientDone:
	case <-time.After(100 * time.Millisecond):
	}
}

func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	cleanup := func(e error) (*os.File, *os.File, error) {
		_ = master.Close()
		return nil, nil, e
	}

	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), ioctlTIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		return cleanup(fmt.Errorf("TIOCSPTLCK: %w", errno))
	}
	var ptn uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), ioctlTIOCGPTN, uintptr(unsafe.Pointer(&ptn))); errno != 0 {
		return cleanup(fmt.Errorf("TIOCGPTN: %w", errno))
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", ptn)
	slave, err = os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return cleanup(fmt.Errorf("open %s: %w", slavePath, err))
	}
	return master, slave, nil
}

func setWinsize(fd uintptr, rows, cols uint16) error {
	ws := ptyWinsize{Rows: rows, Cols: cols}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlTIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return errno
	}
	return nil
}

func (a *agent) handleTCP(w http.ResponseWriter, r *http.Request) {
	portStr := r.URL.Query().Get("port")
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		httpError(w, http.StatusBadRequest, fmt.Errorf("invalid port %q", portStr))
		return
	}
	target, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		httpError(w, http.StatusBadGateway, fmt.Errorf("dial guest 127.0.0.1:%d: %w", port, err))
		return
	}
	defer target.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		httpError(w, http.StatusInternalServerError, errors.New("http hijacking unsupported"))
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: bvm-tcp\r\nConnection: Upgrade\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(target, rw.Reader)
		if cw, ok := target.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, target)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
}
