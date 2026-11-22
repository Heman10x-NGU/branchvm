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
	"regexp"
	"sort"
	"strings"
	"time"
)

// MaxBodyBytes caps the request body forwarded upstream.
const MaxBodyBytes = 25 << 20

// Built-in provider route names. They are also the keys of Config.Upstreams.
const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenAI     = "openai"
	ProviderOpenRouter = "openrouter"
)

var providerNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

type builtinSpec struct {
	upstream   string
	authHeader string
}

var defaultUpstreams = map[string]builtinSpec{
	ProviderAnthropic:  {upstream: "https://api.anthropic.com", authHeader: "X-Api-Key"},
	ProviderOpenAI:     {upstream: "https://api.openai.com", authHeader: "Authorization"},
	ProviderOpenRouter: {upstream: "https://openrouter.ai/api", authHeader: "Authorization"},
}

// ProviderRoute configures an extra upstream API route on the gateway.
type ProviderRoute struct {
	Name       string
	Upstream   string
	Key        string
	AuthHeader string // "Authorization" (default) or "X-Api-Key"
}

// Config configures a Gateway.
type Config struct {
	// ListenAddr is a specific host:port such as 10.200.3.1:18080.
	ListenAddr string
	// AllowLoopbackForTest permits a loopback ListenAddr. Tests only.
	AllowLoopbackForTest bool
	// BranchKey is the dummy key the guest must present, e.g. bvm_dummy_3.
	BranchKey string
	// AnthropicKey, OpenAIKey, and OpenRouterKey are the real keys read from host env.
	AnthropicKey  string
	OpenAIKey     string
	OpenRouterKey string
	// ExtraProviders registers custom routes defined in branchvm.toml.
	ExtraProviders []ProviderRoute
	// Upstreams overrides built-in provider base URLs, keyed by provider name. Tests only.
	Upstreams map[string]string
}

type activeRoute struct {
	upstream   *url.URL
	key        string
	authHeader string
}

// Gateway is a key-injecting reverse proxy for a single branch.
type Gateway struct {
	cfg        Config
	routes     map[string]activeRoute
	routeNames []string
	proxy      *httputil.ReverseProxy
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

	routes := make(map[string]activeRoute, len(defaultUpstreams)+len(cfg.ExtraProviders))
	builtinKeys := map[string]string{
		ProviderAnthropic:  cfg.AnthropicKey,
		ProviderOpenAI:     cfg.OpenAIKey,
		ProviderOpenRouter: cfg.OpenRouterKey,
	}
	for name, spec := range defaultUpstreams {
		raw := spec.upstream
		if o, ok := cfg.Upstreams[name]; ok {
			raw = o
		}
		u, err := parseUpstreamURL(name, raw)
		if err != nil {
			return nil, err
		}
		routes[name] = activeRoute{upstream: u, key: builtinKeys[name], authHeader: spec.authHeader}
	}

	for _, ep := range cfg.ExtraProviders {
		if !providerNameRe.MatchString(ep.Name) {
			return nil, fmt.Errorf("gateway: invalid provider name %q", ep.Name)
		}
		if _, dup := routes[ep.Name]; dup {
			return nil, fmt.Errorf("gateway: duplicate provider route %q", ep.Name)
		}
		u, err := parseUpstreamURL(ep.Name, ep.Upstream)
		if err != nil {
			return nil, err
		}
		hdr := ep.AuthHeader
		if hdr == "" {
			hdr = "Authorization"
		}
		if hdr != "Authorization" && hdr != "X-Api-Key" {
			return nil, fmt.Errorf("gateway: provider %s auth_header %q must be Authorization or X-Api-Key", ep.Name, hdr)
		}
		routes[ep.Name] = activeRoute{upstream: u, key: ep.Key, authHeader: hdr}
	}

	names := make([]string, 0, len(routes))
	for name := range routes {
		names = append(names, name)
	}
	sort.Strings(names)

	g := &Gateway{cfg: cfg, routes: routes, routeNames: names}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:       g.rewrite,
		FlushInterval: -1,
		ErrorHandler:  proxyError,
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	return g, nil
}

func parseUpstreamURL(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("gateway: upstream %s: %w", name, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("gateway: upstream %s: must be http(s) URL with host", name)
	}
	return u, nil
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
		http.Error(w, "unauthorized: present branch dummy token in x-api-key or Authorization Bearer", http.StatusUnauthorized)
		return
	}
	rt, ok := g.parseRoute(r.URL)
	if !ok {
		msg := fmt.Sprintf("forbidden: path %q is not an allowlisted provider route (%s); configure [providers.<name>] in branchvm.toml for custom endpoints",
			r.URL.Path, strings.Join(g.routeNames, ", "))
		http.Error(w, msg, http.StatusForbidden)
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

// parseRoute accepts only registered /<provider>/<rest> paths with no dot
// segments, checked on both the decoded and the raw path.
func (g *Gateway) parseRoute(u *url.URL) (route, bool) {
	for _, p := range []string{u.Path, u.EscapedPath()} {
		if hasDotSegment(p) {
			return route{}, false
		}
	}
	for _, name := range g.routeNames {
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
	ar := g.routes[rt.provider]
	target := ar.upstream
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
	switch ar.authHeader {
	case "X-Api-Key":
		out.Header.Set("X-Api-Key", ar.key)
	default:
		out.Header.Set("Authorization", "Bearer "+ar.key)
	}
}

func proxyError(w http.ResponseWriter, _ *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "upstream unavailable: gateway could not reach configured provider endpoint", http.StatusBadGateway)
}
