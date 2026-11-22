package ptyclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
)

// Forward listens on host 127.0.0.1:hostPort and proxies each connection to
// guest 127.0.0.1:guestPort via execd's POST /tcp upgrade endpoint.
func Forward(ctx context.Context, guestAPIAddr string, guestPort, hostPort int, ready func(actualHostPort int)) error {
	if guestPort < 1 || guestPort > 65535 {
		return fmt.Errorf("invalid guest port %d", guestPort)
	}
	if hostPort < 0 || hostPort > 65535 {
		return fmt.Errorf("invalid host port %d", hostPort)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return fmt.Errorf("listen 127.0.0.1:%d: %w", hostPort, err)
	}
	defer ln.Close()
	if ready != nil {
		ready(ln.Addr().(*net.TCPAddr).Port)
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		client, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept forward conn: %w", err)
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			proxyOneConn(ctx, guestAPIAddr, guestPort, c)
		}(client)
	}
}

func proxyOneConn(ctx context.Context, guestAPIAddr string, guestPort int, client net.Conn) {
	defer client.Close()

	remote, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", guestAPIAddr)
	if err != nil {
		return
	}
	defer remote.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
			_ = remote.Close()
		case <-done:
		}
	}()

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/tcp?port=%d", guestAPIAddr, guestPort), nil)
	if err != nil {
		return
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "bvm-tcp")
	if err := req.Write(remote); err != nil {
		return
	}

	br := bufio.NewReader(remote)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(remote, client)
		if cw, ok := remote.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, br)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
}
