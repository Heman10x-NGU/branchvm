// Package ptyclient implements pure-Go HTTP-upgraded interactive PTY and TCP
// port forwarding clients against execd inside a branch microVM.
package ptyclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

const dialTimeout = 5 * time.Second

// Attach connects to guestAPIAddr's POST /pty endpoint, switches stdin to raw
// mode when it is a TTY, forwards resize events, and returns the remote exit code.
func Attach(ctx context.Context, guestAPIAddr string, req types.PTYRequest, stdin *os.File, stdout io.Writer) (int, error) {
	var restore func()
	if stdin != nil {
		if rows, cols, ok := termWinsize(stdin.Fd()); ok {
			if req.Rows == 0 {
				req.Rows = rows
			}
			if req.Cols == 0 {
				req.Cols = cols
			}
		}
		if rfn, ok := makeRaw(stdin.Fd()); ok {
			restore = rfn
			defer restore()
		}
	}
	if req.Rows == 0 {
		req.Rows = 24
	}
	if req.Cols == 0 {
		req.Cols = 80
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return 1, fmt.Errorf("encode pty request: %w", err)
	}

	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", guestAPIAddr)
	if err != nil {
		return 1, fmt.Errorf("dial guest pty %s: %w", guestAPIAddr, err)
	}
	defer conn.Close()

	httpReq, err := http.NewRequest(http.MethodPost, "http://"+guestAPIAddr+"/pty", bytes.NewReader(payload))
	if err != nil {
		return 1, fmt.Errorf("build pty request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Connection", "Upgrade")
	httpReq.Header.Set("Upgrade", "bvm-pty")
	httpReq.ContentLength = int64(len(payload))
	if err := httpReq.Write(conn); err != nil {
		return 1, fmt.Errorf("write pty upgrade: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, httpReq)
	if err != nil {
		return 1, fmt.Errorf("read pty handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return 1, fmt.Errorf("pty upgrade failed (%d): %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-sessionCtx.Done()
		_ = conn.Close()
	}()

	var writeMu sync.Mutex
	sendFrame := func(kind byte, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return types.WritePTYFrame(conn, kind, data)
	}

	if restore != nil && stdin != nil {
		go watchResize(sessionCtx, stdin.Fd(), func(rows, cols uint16) {
			var b [4]byte
			binary.BigEndian.PutUint16(b[0:2], rows)
			binary.BigEndian.PutUint16(b[2:4], cols)
			_ = sendFrame(types.PTYFrameResize, b[:])
		})
	}

	if stdin != nil {
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, rerr := stdin.Read(buf)
				if n > 0 {
					if werr := sendFrame(types.PTYFrameData, buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					if errors.Is(rerr, io.EOF) && len(req.Cmd) == 0 {
						// Non-TTY stdin pipe closed; send EOT to interactive shell.
						_ = sendFrame(types.PTYFrameData, []byte{4})
					}
					return
				}
			}
		}()
	}

	exitCode := 0
	for {
		kind, data, err := types.ReadPTYFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) || sessionCtx.Err() != nil {
				return exitCode, nil
			}
			return 1, fmt.Errorf("read pty frame: %w", err)
		}
		switch kind {
		case types.PTYFrameData:
			if len(data) > 0 && stdout != nil {
				if _, werr := stdout.Write(data); werr != nil {
					return 1, werr
				}
			}
		case types.PTYFrameExit:
			if len(data) == 4 {
				exitCode = int(int32(binary.BigEndian.Uint32(data)))
			}
			return exitCode, nil
		}
	}
}
