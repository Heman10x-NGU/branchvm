package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	dummyKey     = "bvm_dummy_1"
	realAnthKey  = "sk-ant-test-real"
	realOpenAIKy = "sk-openai-test-real"
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
		Upstreams:            map[string]string{ProviderAnthropic: anth, ProviderOpenAI: oai},
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(g, http.MethodPost, tc.path, tc.hdr, "{}")
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if strings.Contains(rec.Body.String(), realAnthKey) || strings.Contains(rec.Body.String(), realOpenAIKy) {
				t.Fatal("real key echoed in response")
			}
		})
	}
}

func TestKeyInjectionAndPassthrough(t *testing.T) {
	anthGot := make(chan seen, 1)
	oaiGot := make(chan seen, 1)
	anth := fakeUpstream(t, anthGot, 201, `{"id":"msg_1"}`)
	oai := fakeUpstream(t, oaiGot, 429, `{"error":"rate"}`)
	g := newTestGateway(t, anth.URL, oai.URL)

	rec := do(g, http.MethodPost, "/anthropic/v1/messages?beta=1",
		map[string]string{"x-api-key": dummyKey}, `{"model":"m"}`)
	// do() sets Path only; query is not carried, so check it separately below.
	if rec.Code != 201 || rec.Body.String() != `{"id":"msg_1"}` || rec.Header().Get("X-Upstream") != "yes" {
		t.Fatalf("anthropic passthrough: %d %q", rec.Code, rec.Body.String())
	}
	s := <-anthGot
	if s.apiKey != realAnthKey || s.auth != "" {
		t.Fatalf("anthropic upstream headers: apiKey=%q auth=%q", s.apiKey, s.auth)
	}
	if s.method != http.MethodPost || s.body != `{"model":"m"}` {
		t.Fatalf("anthropic upstream request: %+v", s)
	}

	rec = do(g, http.MethodGet, "/openai/v1/models",
		map[string]string{"Authorization": "Bearer " + dummyKey}, "")
	if rec.Code != 429 || rec.Body.String() != `{"error":"rate"}` {
		t.Fatalf("openai passthrough: %d %q", rec.Code, rec.Body.String())
	}
	s = <-oaiGot
	if s.auth != "Bearer "+realOpenAIKy || s.apiKey != "" || s.path != "/v1/models" || s.method != http.MethodGet {
		t.Fatalf("openai upstream: %+v", s)
	}
	if strings.Contains(s.auth+s.apiKey, dummyKey) {
		t.Fatal("upstream saw dummy key")
	}
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
