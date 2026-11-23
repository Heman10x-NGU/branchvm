package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	dummyKey          = "bvm_dummy_1"
	realAnthKey       = "sk-ant-test-real"
	realOpenAIKy      = "sk-openai-test-real"
	realOpenRouterKey = "sk-or-test-real"
)

type seen struct {
	method, path, query, apiKey, auth, body string
}

func fakeUpstream(t *testing.T, got chan<- seen, status int, respBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), string(b)}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestGateway(t *testing.T, anth, oai string) *Gateway {
	t.Helper()
	g, err := New(Config{
		ListenAddr:           "127.0.0.1:18080",
		AllowLoopbackForTest: true,
		BranchKey:            dummyKey,
		AnthropicKey:         realAnthKey,
		OpenAIKey:            realOpenAIKy,
		OpenRouterKey:        realOpenRouterKey,
		Upstreams: map[string]string{
			ProviderAnthropic:  anth,
			ProviderOpenAI:     oai,
			ProviderOpenRouter: oai,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func do(g *Gateway, method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://gw.local/", strings.NewReader(body))
	req.URL.Path = path
	req.URL.RawPath = ""
	req.RequestURI = path
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

func TestListenAddrValidation(t *testing.T) {
	cases := []struct {
		name, addr string
		loopback   bool
		ok         bool
	}{
		{"specific ip", "10.200.3.1:18080", false, true},
		{"empty host", ":18080", false, false},
		{"unspecified v4", "0.0.0.0:18080", false, false},
		{"unspecified v6", "[::]:18080", false, false},
		{"loopback without flag", "127.0.0.1:18080", false, false},
		{"loopback with flag", "127.0.0.1:18080", true, true},
		{"hostname", "localhost:18080", true, false},
		{"no port", "10.200.3.1", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{ListenAddr: tc.addr, AllowLoopbackForTest: tc.loopback, BranchKey: dummyKey})
			if (err == nil) != tc.ok {
				t.Fatalf("addr %q: err=%v, want ok=%v", tc.addr, err, tc.ok)
			}
		})
	}
}

func TestAuthAndAllowlist(t *testing.T) {
	got := make(chan seen, 10)
	up := fakeUpstream(t, got, 200, "ok")
	g := newTestGateway(t, up.URL, up.URL)
	good := map[string]string{"x-api-key": dummyKey}
	cases := []struct {
		name string
		path string
		hdr  map[string]string
		want int
	}{
		{"missing key", "/anthropic/v1/messages", nil, 401},
		{"wrong x-api-key", "/anthropic/v1/messages", map[string]string{"x-api-key": "bvm_dummy_2"}, 401},
		{"wrong bearer", "/openai/v1/chat", map[string]string{"Authorization": "Bearer nope"}, 401},
		{"real key rejected", "/openai/v1/chat", map[string]string{"Authorization": "Bearer " + realOpenAIKy}, 401},
		{"other path", "/other", good, 403},
		{"bare provider", "/anthropic/", good, 403},
		{"traversal", "/anthropic/../x", good, 403},
		{"inner traversal", "/openai/v1/../../x", good, 403},
		{"encoded traversal", "/anthropic/%2e%2e/x", good, 403},
		{"allowed", "/anthropic/v1/messages", good, 200},
		{"openrouter allowed", "/openrouter/v1/chat/completions", good, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(g, http.MethodPost, tc.path, tc.hdr, "{}")
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if tc.want == 403 && !strings.Contains(rec.Body.String(), "branchvm.toml") {
				t.Fatalf("expected actionable 403 message mentioning branchvm.toml, got %q", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), realAnthKey) || strings.Contains(rec.Body.String(), realOpenAIKy) || strings.Contains(rec.Body.String(), realOpenRouterKey) {
				t.Fatal("real key echoed in response")
			}
		})
	}
}

func TestKeyInjectionAndPassthrough(t *testing.T) {
	anthGot := make(chan seen, 1)
	oaiGot := make(chan seen, 2)
	anth := fakeUpstream(t, anthGot, 201, `{"id":"msg_1"}`)
	oai := fakeUpstream(t, oaiGot, 429, `{"error":"rate"}`)
	g := newTestGateway(t, anth.URL, oai.URL)

	rec := do(g, http.MethodPost, "/anthropic/v1/messages?beta=1",
		map[string]string{"x-api-key": dummyKey}, `{"model":"m"}`)
	if rec.Code != 201 || rec.Body.String() != `{"id":"msg_1"}` || rec.Header().Get("X-Upstream") != "yes" {
		t.Fatalf("anthropic passthrough: %d %q", rec.Code, rec.Body.String())
	}
	s := <-anthGot
	if s.apiKey != realAnthKey || s.auth != "" {
		t.Fatalf("anthropic upstream headers: apiKey=%q auth=%q", s.apiKey, s.auth)
	}

	rec = do(g, http.MethodGet, "/openai/v1/models",
		map[string]string{"Authorization": "Bearer " + dummyKey}, "")
	if rec.Code != 429 || rec.Body.String() != `{"error":"rate"}` {
		t.Fatalf("openai passthrough: %d %q", rec.Code, rec.Body.String())
	}
	s = <-oaiGot
	if s.auth != "Bearer "+realOpenAIKy || s.apiKey != "" || s.path != "/v1/models" {
		t.Fatalf("openai upstream: %+v", s)
	}

	rec = do(g, http.MethodPost, "/openrouter/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + dummyKey}, "{}")
	if rec.Code != 429 {
		t.Fatalf("openrouter status = %d", rec.Code)
	}
	s = <-oaiGot
	if s.auth != "Bearer "+realOpenRouterKey || s.apiKey != "" || s.path != "/v1/chat/completions" {
		t.Fatalf("openrouter upstream: %+v", s)
	}
}

func TestExtraProvidersAndValidation(t *testing.T) {
	got := make(chan seen, 2)
	up := fakeUpstream(t, got, 200, "ok")

	if _, err := New(Config{
		ListenAddr: "127.0.0.1:18080", AllowLoopbackForTest: true, BranchKey: dummyKey,
		ExtraProviders: []ProviderRoute{{Name: "anthropic", Upstream: up.URL}},
	}); err == nil {
		t.Fatal("expected duplicate builtin provider rejection")
	}
	if _, err := New(Config{
		ListenAddr: "127.0.0.1:18080", AllowLoopbackForTest: true, BranchKey: dummyKey,
		ExtraProviders: []ProviderRoute{{Name: "Bad Name", Upstream: up.URL}},
	}); err == nil {
		t.Fatal("expected invalid provider name rejection")
	}
	if _, err := New(Config{
		ListenAddr: "127.0.0.1:18080", AllowLoopbackForTest: true, BranchKey: dummyKey,
		ExtraProviders: []ProviderRoute{{Name: "imds", Upstream: "http://169.254.169.254/latest"}},
	}); err == nil {
		t.Fatal("expected link-local IMDS upstream rejection")
	}

	g, err := New(Config{
		ListenAddr:           "127.0.0.1:18080",
		AllowLoopbackForTest: true,
		BranchKey:            dummyKey,
		ExtraProviders: []ProviderRoute{
			{Name: "custom-x", Upstream: up.URL + "/prefix", Key: "real-x", AuthHeader: "X-Api-Key"},
			{Name: "custom-b", Upstream: up.URL, Key: "real-b", AuthHeader: "Authorization"},
		},
	})
	if err != nil {
		t.Fatalf("New extra providers: %v", err)
	}
	if rec := do(g, http.MethodPost, "/custom-x/v1/call", map[string]string{"x-api-key": dummyKey}, "{}"); rec.Code != 200 {
		t.Fatalf("custom-x code = %d", rec.Code)
	}
	sx := <-got
	if sx.path != "/prefix/v1/call" || sx.apiKey != "real-x" || sx.auth != "" {
		t.Fatalf("custom-x got %+v", sx)
	}
	if rec := do(g, http.MethodPost, "/custom-b/v1/call", map[string]string{"Authorization": "Bearer " + dummyKey}, "{}"); rec.Code != 200 {
		t.Fatalf("custom-b code = %d", rec.Code)
	}
	sb := <-got
	if sb.path != "/v1/call" || sb.auth != "Bearer real-b" || sb.apiKey != "" {
		t.Fatalf("custom-b got %+v", sb)
	}
}

func TestSSEStreamingFlushesImmediately(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: chunk1\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: chunk2\n\n")
	}))
	defer up.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	g, err := New(Config{
		ListenAddr: addr, AllowLoopbackForTest: true, BranchKey: dummyKey,
		AnthropicKey: realAnthKey, Upstreams: map[string]string{ProviderAnthropic: up.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = g.Serve(ctx) }()

	var resp *http.Response
	for i := 0; i < 50; i++ {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/anthropic/v1/messages", strings.NewReader("{}"))
		req.Header.Set("x-api-key", dummyKey)
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if err != nil {
		close(release)
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()

	lineCh := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(resp.Body).ReadString('\n')
		lineCh <- s
	}()
	select {
	case line := <-lineCh:
		if line != "data: chunk1\n" {
			close(release)
			t.Fatalf("first chunk = %q, want data: chunk1\\n", line)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("first SSE chunk stalled behind reverse proxy buffer")
	}
	close(release)
}

func TestServeBindsAndForwardsQuery(t *testing.T) {
	got := make(chan seen, 1)
	up := fakeUpstream(t, got, 200, "pong")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	g, err := New(Config{ListenAddr: addr, AllowLoopbackForTest: true, BranchKey: dummyKey,
		AnthropicKey: realAnthKey, Upstreams: map[string]string{ProviderAnthropic: up.URL}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- g.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	var resp *http.Response
	for i := 0; i < 50; i++ {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/anthropic/v1/messages?beta=1", strings.NewReader("hi"))
		req.Header.Set("x-api-key", dummyKey)
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "pong" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
	s := <-got
	if s.query != "beta=1" || s.body != "hi" || s.apiKey != realAnthKey {
		t.Fatalf("upstream: %+v", s)
	}
}

func TestBodyCap(t *testing.T) {
	got := make(chan seen, 1)
	up := fakeUpstream(t, got, 200, "ok")
	g := newTestGateway(t, up.URL, up.URL)
	big := strings.Repeat("a", MaxBodyBytes+1)
	rec := do(g, http.MethodPost, "/anthropic/v1/messages", map[string]string{"x-api-key": dummyKey}, big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rec.Code)
	}
}

func TestValidateUpstreamIP(t *testing.T) {
	cases := []struct {
		name          string
		addr          netip.Addr
		allowLoopback bool
		wantOK        bool
	}{
		{"invalid", netip.Addr{}, true, false},
		{"imds v4 no loopback", netip.MustParseAddr("169.254.169.254"), false, false},
		{"imds v4 allow loopback", netip.MustParseAddr("169.254.169.254"), true, false},
		{"imds v4-mapped v6 no loopback", netip.MustParseAddr("::ffff:169.254.169.254"), false, false},
		{"imds v4-mapped v6 allow loopback", netip.MustParseAddr("::ffff:169.254.169.254"), true, false},
		{"branchvm net no loopback", netip.MustParseAddr("10.200.2.2"), false, false},
		{"branchvm net allow loopback", netip.MustParseAddr("10.200.2.2"), true, false},
		{"unspecified", netip.MustParseAddr("0.0.0.0"), true, false},
		{"multicast", netip.MustParseAddr("224.0.0.1"), true, false},
		{"loopback rejected", netip.MustParseAddr("127.0.0.1"), false, false},
		{"loopback allowed", netip.MustParseAddr("127.0.0.1"), true, true},
		{"private rejected", netip.MustParseAddr("192.168.1.1"), false, false},
		{"private allowed", netip.MustParseAddr("192.168.1.1"), true, true},
		{"public v4 no loopback", netip.MustParseAddr("8.8.8.8"), false, true},
		{"public v4 allow loopback", netip.MustParseAddr("8.8.8.8"), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUpstreamIP(tc.addr, tc.allowLoopback)
			if (err == nil) != tc.wantOK {
				t.Fatalf("validateUpstreamIP(%v, %v) = %v, wantOK=%v", tc.addr, tc.allowLoopback, err, tc.wantOK)
			}
		})
	}
}

func TestNonHTTPSCustomProviderRejectedWithoutTestFlag(t *testing.T) {
	if _, err := New(Config{
		ListenAddr: "10.200.3.1:18080",
		BranchKey:  dummyKey,
		ExtraProviders: []ProviderRoute{
			{Name: "together", Upstream: "http://api.together.xyz/v1", Key: "k"},
		},
	}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected https error, got %v", err)
	}

	if _, err := New(Config{
		ListenAddr: "10.200.3.1:18080",
		BranchKey:  dummyKey,
		ExtraProviders: []ProviderRoute{
			{Name: "priv", Upstream: "https://192.168.1.1/v1", Key: "k"},
		},
	}); err == nil {
		t.Fatal("expected private IP upstream rejection when !AllowLoopbackForTest")
	}
}

func TestDialTimeBlocksDisallowedIPsEvenWithAllowLoopback(t *testing.T) {
	g := newTestGateway(t, "http://127.0.0.1:19999", "http://127.0.0.1:19999")
	tr, ok := g.proxy.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatal("expected custom *http.Transport with DialContext on proxy")
	}
	for _, target := range []string{
		"10.200.2.2:443",
		"169.254.169.254:80",
		"[::ffff:169.254.169.254]:80",
	} {
		t.Run(target, func(t *testing.T) {
			conn, err := tr.DialContext(context.Background(), "tcp", target)
			if conn != nil {
				_ = conn.Close()
			}
			if err == nil {
				t.Fatalf("expected dial to %s to be blocked", target)
			}
		})
	}
}

func TestListenReturnsImmediateEADDRINUSE(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	g, err := New(Config{
		ListenAddr:           busy.Addr().String(),
		AllowLoopbackForTest: true,
		BranchKey:            dummyKey,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := g.Listen()
	if ln != nil {
		_ = ln.Close()
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("Listen on busy port err = %v, want EADDRINUSE", err)
	}
}
