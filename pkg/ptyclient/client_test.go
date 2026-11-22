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
	"testing"
	"time"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

func startMockGuestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestAttachRoundtripAndExitCode(t *testing.T) {
	var seenReq types.PTYRequest
	addr := startMockGuestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pty" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&seenReq); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: bvm-pty\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()

		_ = types.WritePTYFrame(conn, types.PTYFrameData, []byte("hello "))
		kind, payload, err := types.ReadPTYFrame(rw.Reader)
		if err == nil && kind == types.PTYFrameData {
			_ = types.WritePTYFrame(conn, types.PTYFrameData, payload)
		}
		var code [4]byte
		binary.BigEndian.PutUint32(code[:], uint32(int32(7)))
		_ = types.WritePTYFrame(conn, types.PTYFrameExit, code[:])
	}))

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	go func() {
		_, _ = pw.WriteString("world")
		_ = pw.Close()
	}()

	var out bytes.Buffer
	code, err := Attach(context.Background(), addr, types.PTYRequest{Cmd: []string{"echo", "ok"}}, pr, &out)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if out.String() != "hello world" {
		t.Fatalf("stdout = %q, want %q", out.String(), "hello world")
	}
	if len(seenReq.Cmd) != 2 || seenReq.Cmd[0] != "echo" {
		t.Fatalf("seenReq = %+v", seenReq)
	}
}

func TestAttachUpgradeError(t *testing.T) {
	addr := startMockGuestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	_, err := Attach(context.Background(), addr, types.PTYRequest{}, nil, io.Discard)
	if err == nil {
		t.Fatal("expected upgrade error")
	}
}

func TestForwardEchoRoundtrip(t *testing.T) {
	addr := startMockGuestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tcp" || r.URL.Query().Get("port") != "3000" {
			http.Error(w, "bad route", 400)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: bvm-tcp\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		br := bufio.NewReader(rw.Reader)
		line, err := br.ReadString('\n')
		if err == nil {
			_, _ = conn.Write([]byte("echo:" + line))
		}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	portCh := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- Forward(ctx, addr, 3000, 0, func(p int) { portCh <- p })
	}()

	var hostPort int
	select {
	case hostPort = <-portCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for forward listener")
	}

	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), 2*time.Second)
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	_, _ = c.Write([]byte("ping\n"))
	got, err := bufio.NewReader(c).ReadString('\n')
	_ = c.Close()
	if err != nil || got != "echo:ping\n" {
		t.Fatalf("forward read = (%q, %v), want echo:ping\\n", got, err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Forward returned err on cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Forward did not exit on cancel")
	}

	_ = sync.Mutex{} // ensure sync import clean if needed
}

func TestAttachUnexpectedEOF(t *testing.T) {
	addr := startMockGuestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: bvm-pty\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		_ = types.WritePTYFrame(conn, types.PTYFrameData, []byte("partial"))
		_ = conn.Close()
	}))

	var out bytes.Buffer
	code, err := Attach(context.Background(), addr, types.PTYRequest{Cmd: []string{"true"}}, nil, &out)
	if code == 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got (%d, %v), want non-zero exit with ErrUnexpectedEOF", code, err)
	}
	if out.String() != "partial" {
		t.Fatalf("stdout = %q, want partial", out.String())
	}
}

func TestAttachCancelDuringHandshake(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	addr := startMockGuestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	code, err := Attach(ctx, addr, types.PTYRequest{Cmd: []string{"true"}}, nil, io.Discard)
	if code == 0 || err == nil {
		t.Fatalf("got (%d, %v), want error on canceled handshake", code, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Attach took too long on canceled handshake: %s", time.Since(start))
	}
}
