// Package gateway is the host-side LLM API gateway for branch VMs.
//
// The guest never holds real API keys. It sends plain HTTP to the gateway
// with a per-branch dummy key; the gateway checks that key, swaps in the real
// provider key, and forwards the request over normal Go TLS. No fake root CA
// is ever installed in the guest.
package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// MaxBodyBytes caps the request body forwarded upstream.
const MaxBodyBytes = 25 << 20

// Provider route names. They are also the keys of Config.Upstreams.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

var defaultUpstreams = map[string]string{
	ProviderAnthropic: "https://api.anthropic.com",
	ProviderOpenAI:    "https://api.openai.com",
}

// Config configures a Gateway.
type Config struct {
	// ListenAddr is a specific host:port such as 10.200.3.1:18080.
	ListenAddr string
	// AllowLoopbackForTest permits a loopback ListenAddr. Tests only.
	AllowLoopbackForTest bool
	// BranchKey is the dummy key the guest must present, e.g. bvm_dummy_3.
	BranchKey string
	// AnthropicKey and OpenAIKey are the real keys, read from the host env.
	AnthropicKey string
	OpenAIKey    string
	// Upstreams overrides provider base URLs, keyed by provider name. Tests only.
	Upstreams map[string]string
}

// Gateway is a key-injecting reverse proxy for a single branch.
type Gateway struct {
	cfg       Config
	upstreams map[string]*url.URL
	proxy     *httputil.ReverseProxy
}

type ctxKey struct{}

type route struct {
	provider string
	rest     string
}

// New validates cfg and builds a Gateway.
func New(cfg Config) (*Gateway, error) {
	if err := validateListenAddr(cfg.ListenAddr, cfg.AllowLoopbackForTest); err != nil {
		return nil, err
	}
	if cfg.BranchKey == "" {
		return nil, errors.New("gateway: BranchKey is required")
	}
	ups := make(map[string]*url.URL, len(defaultUpstreams))
	for name, raw := range defaultUpstreams {
		if o, ok := cfg.Upstreams[name]; ok {
			raw = o
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("gateway: upstream %s: %w", name, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("gateway: upstream %s: missing scheme or host", name)
		}
		ups[name] = u
	}
	g := &Gateway{cfg: cfg, upstreams: ups}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:      g.rewrite,
		ErrorHandler: proxyError,
		ErrorLog:     log.New(io.Discard, "", 0),
	}
	return g, nil
}

func validateListenAddr(addr string, allowLoopback bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("gateway: listen addr %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("gateway: listen addr %q: host must be a specific IP", addr)
	}
	if port == "" || port == "0" {
		return fmt.Errorf("gateway: listen addr %q: port must be set", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("gateway: listen addr %q: host must be an IP literal", addr)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("gateway: listen addr %q: unspecified address is not allowed", addr)
	}
	if ip.IsLoopback() && !allowLoopback {
		return fmt.Errorf("gateway: listen addr %q: loopback is only allowed in tests", addr)
	}
	return nil
}

// Serve listens on cfg.ListenAddr until ctx is done.
func (g *Gateway) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", g.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("gateway: listen: %w", err)
	}
	srv := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return fmt.Errorf("gateway: serve: %w", err)
}

// ServeHTTP authenticates, routes, and proxies one request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rt, ok := parseRoute(r.URL)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.ContentLength > MaxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, rt))
	g.proxy.ServeHTTP(w, r)
}

func (g *Gateway) authorized(r *http.Request) bool {
	want := []byte(g.cfg.BranchKey)
	if k := r.Header.Get("x-api-key"); k != "" {
		return subtle.ConstantTimeCompare([]byte(k), want) == 1
	}
	auth := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), want) == 1
}

// parseRoute accepts only /anthropic/<rest> and /openai/<rest> with no dot
// segments, checked on both the decoded and the raw path.
func parseRoute(u *url.URL) (route, bool) {
	for _, p := range []string{u.Path, u.EscapedPath()} {
		if hasDotSegment(p) {
			return route{}, false
		}
	}
	for _, name := range []string{ProviderAnthropic, ProviderOpenAI} {
		rest, ok := strings.CutPrefix(u.Path, "/"+name+"/")
		if ok && rest != "" {
			return route{provider: name, rest: rest}, true
		}
	}
	return route{}, false
}

func hasDotSegment(p string) bool {
	lp := strings.ToLower(p)
	if strings.Contains(lp, "%2e") || strings.Contains(lp, "%2f") || strings.Contains(p, "\\") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	rt, _ := pr.In.Context().Value(ctxKey{}).(route)
	target := g.upstreams[rt.provider]
	out := pr.Out
	out.URL.Scheme = target.Scheme
	out.URL.Host = target.Host
	out.URL.Path = strings.TrimSuffix(target.Path, "/") + "/" + rt.rest
	out.URL.RawPath = ""
	out.URL.RawQuery = pr.In.URL.RawQuery
	out.Host = target.Host

	out.Header.Del("Authorization")
	out.Header.Del("X-Api-Key")
	out.Header.Del("Cookie")
	out.Header.Del("Proxy-Authorization")
	switch rt.provider {
	case ProviderAnthropic:
		out.Header.Set("X-Api-Key", g.cfg.AnthropicKey)
	case ProviderOpenAI:
		out.Header.Set("Authorization", "Bearer "+g.cfg.OpenAIKey)
	}
}

func proxyError(w http.ResponseWriter, _ *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "upstream unavailable", http.StatusBadGateway)
}
