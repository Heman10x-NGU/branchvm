//go:build linux

package ptyclient

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

const (
	ioctlTCGETS     = 0x5401
	ioctlTCSETS     = 0x5402
	ioctlTIOCGWINSZ = 0x5413
)

type winsize struct {
	Rows, Cols, Xpixel, Ypixel uint16
}

func termWinsize(fd uintptr) (rows, cols uint16, ok bool) {
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlTIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Rows == 0 || ws.Cols == 0 {
		return 0, 0, false
	}
	return ws.Rows, ws.Cols, true
}

func makeRaw(fd uintptr) (func(), bool) {
	var orig syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlTCGETS, uintptr(unsafe.Pointer(&orig))); errno != 0 {
		return nil, false
	}
	raw := orig
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlTCSETS, uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, false
	}
	return func() {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlTCSETS, uintptr(unsafe.Pointer(&orig)))
	}, true
}

func watchResize(ctx context.Context, fd uintptr, onChange func(rows, cols uint16)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if rows, cols, ok := termWinsize(fd); ok {
				onChange(rows, cols)
			}
		}
	}
}
