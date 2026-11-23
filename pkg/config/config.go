// Package config loads and validates per-repo branchvm.toml settings
// using only the Go standard library.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

// FileName is the repository config file read by branchvm.
const FileName = "branchvm.toml"

const (
	defaultMemMB          = 1024
	defaultVCPUs          = 2
	defaultWarmTimeoutSec = 1800
	minMemMB              = 128
	maxVCPUs              = 32
)

var (
	dbNameRe    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,62}$`)
	provNameRe  = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	envNameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	branchvmNet = netip.MustParsePrefix("10.200.0.0/16")
)

// Config holds branchvm configuration for a repository.
type Config struct {
	Image          string
	Dockerfile     string
	RootfsSizeMB   int64
	MemMB          int64
	VCPUs          int
	Warm           string
	WarmTimeoutSec int
	Database       string
	Env            []string
	Services       []types.ServiceSpec
	Providers      []types.ProviderSpec
}

// DefaultConfig returns the v0.1 defaults used when no branchvm.toml exists.
func DefaultConfig() Config {
	return Config{
		MemMB:          defaultMemMB,
		VCPUs:          defaultVCPUs,
		WarmTimeoutSec: defaultWarmTimeoutSec,
		Database:       "authdemo",
		Env:            []string{"DATABASE_URL=postgres://postgres@127.0.0.1:5432/authdemo?sslmode=disable"},
		Services:       []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}},
	}
}

// Load reads branchvm.toml from repoDir. When absent, it returns
// DefaultConfig(), false, nil.
func Load(repoDir string) (Config, bool, error) {
	path := filepath.Join(repoDir, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, true, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, true, nil
}

// Parse parses and validates a branchvm.toml document. When a config file
// exists and omits services and [[service]], Services defaults to empty
// (stateless).
func Parse(data []byte) (Config, error) {
	p := &parser{
		src: strings.ReplaceAll(string(data), "\r\n", "\n"),
		cfg: Config{
			MemMB:          defaultMemMB,
			VCPUs:          defaultVCPUs,
			WarmTimeoutSec: defaultWarmTimeoutSec,
			Services:       []types.ServiceSpec{},
		},
		topSeen: map[string]bool{},
		provIdx: map[string]int{},
	}
	if err := p.parseDoc(); err != nil {
		return Config{}, err
	}
	if err := validate(&p.cfg); err != nil {
		return Config{}, err
	}
	return p.cfg, nil
}

type sectionKind int

const (
	secTop sectionKind = iota
	secService
	secProvider
)

type parser struct {
	src  string
	pos  int
	line int

	cfg     Config
	sec     sectionKind
	secIdx  int
	topSeen map[string]bool
	secSeen map[string]bool
	presets []types.ServiceSpec
	customs []types.ServiceSpec
	provIdx map[string]int
}

func (p *parser) parseDoc() error {
	p.line = 1
	for p.pos < len(p.src) {
		p.skipSpaceAndComments()
		if p.pos >= len(p.src) {
			break
		}
		if p.src[p.pos] == '\n' {
			p.advance()
			continue
		}
		if p.src[p.pos] == '[' {
			if err := p.parseHeader(); err != nil {
				return err
			}
			continue
		}
		if err := p.parseAssign(); err != nil {
			return err
		}
	}
	p.cfg.Services = append(p.presets, p.customs...)
	if p.cfg.Services == nil {
		p.cfg.Services = []types.ServiceSpec{}
	}
	return nil
}

func (p *parser) parseHeader() error {
	line := p.line
	arrayTable := strings.HasPrefix(p.src[p.pos:], "[[")
	if arrayTable {
		p.pos += 2
	} else {
		p.pos++
	}
	start := p.pos
	for p.pos < len(p.src) {
		ch := p.src[p.pos]
		if ch == '\n' {
			return fmt.Errorf("line %d: unclosed section header", line)
		}
		if arrayTable && strings.HasPrefix(p.src[p.pos:], "]]") {
			name := strings.TrimSpace(p.src[start:p.pos])
			p.pos += 2
			if err := p.expectLineEnd(line); err != nil {
				return err
			}
			return p.enterArrayTable(line, name)
		}
		if !arrayTable && ch == ']' {
			name := strings.TrimSpace(p.src[start:p.pos])
			p.pos++
			if err := p.expectLineEnd(line); err != nil {
				return err
			}
			return p.enterTable(line, name)
		}
		p.pos++
	}
	return fmt.Errorf("line %d: unclosed section header", line)
}

func (p *parser) enterArrayTable(line int, name string) error {
	if name != "service" {
		return fmt.Errorf("line %d: unknown array table [[%s]]", line, name)
	}
	p.sec = secService
	p.customs = append(p.customs, types.ServiceSpec{})
	p.secIdx = len(p.customs) - 1
	p.secSeen = map[string]bool{}
	return nil
}

func (p *parser) enterTable(line int, name string) error {
	prov, ok := strings.CutPrefix(name, "providers.")
	if !ok || prov == "" {
		return fmt.Errorf("line %d: unknown section [%s]", line, name)
	}
	prov = unquoteHeaderPart(prov)
	if !provNameRe.MatchString(prov) {
		return fmt.Errorf("line %d: invalid provider name %q", line, prov)
	}
	if _, exists := p.provIdx[prov]; exists {
		return fmt.Errorf("line %d: duplicate section [providers.%s]", line, prov)
	}
	p.sec = secProvider
	p.cfg.Providers = append(p.cfg.Providers, types.ProviderSpec{Name: prov, AuthHeader: "Authorization"})
	p.secIdx = len(p.cfg.Providers) - 1
	p.provIdx[prov] = p.secIdx
	p.secSeen = map[string]bool{}
	return nil
}

func unquoteHeaderPart(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func (p *parser) parseAssign() error {
	line := p.line
	key := p.readBareKey()
	if key == "" {
		return fmt.Errorf("line %d: expected key", line)
	}
	p.skipHorizontalSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '=' {
		return fmt.Errorf("line %d: expected '=' after %q", line, key)
	}
	p.pos++
	p.skipHorizontalSpace()
	val, err := p.parseValue(line)
	if err != nil {
		return err
	}
	if err := p.expectLineEnd(line); err != nil {
		return err
	}
	return p.assign(line, key, val)
}

type parsedVal struct {
	kind string // "string", "int", "array"
	str  string
	num  int64
	arr  []string
}

func (p *parser) parseValue(line int) (parsedVal, error) {
	if p.pos >= len(p.src) || p.src[p.pos] == '\n' || p.src[p.pos] == '#' {
		return parsedVal{}, fmt.Errorf("line %d: missing value", line)
	}
	switch p.src[p.pos] {
	case '"':
		s, err := p.readBasicString(line)
		return parsedVal{kind: "string", str: s}, err
	case '\'':
		s, err := p.readLiteralString(line)
		return parsedVal{kind: "string", str: s}, err
	case '[':
		arr, err := p.readStringArray(line)
		return parsedVal{kind: "array", arr: arr}, err
	default:
		n, err := p.readInt(line)
		return parsedVal{kind: "int", num: n}, err
	}
}

func (p *parser) readBareKey() string {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			p.pos++
			continue
		}
		break
	}
	return p.src[start:p.pos]
}

func (p *parser) readBasicString(line int) (string, error) {
	p.pos++ // skip opening '"'
	var b strings.Builder
	for p.pos < len(p.src) {
		ch := p.src[p.pos]
		if ch == '"' {
			p.pos++
			return b.String(), nil
		}
		if ch == '\n' {
			return "", fmt.Errorf("line %d: unclosed string literal", line)
		}
		if ch == '\\' {
			p.pos++
			if p.pos >= len(p.src) {
				return "", fmt.Errorf("line %d: incomplete escape sequence", line)
			}
			esc := p.src[p.pos]
			p.pos++
			switch esc {
			case '"', '\\':
				b.WriteByte(esc)
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			default:
				return "", fmt.Errorf("line %d: unsupported escape \\%c", line, esc)
			}
			continue
		}
		b.WriteByte(ch)
		p.pos++
	}
	return "", fmt.Errorf("line %d: unclosed string literal", line)
}

func (p *parser) readLiteralString(line int) (string, error) {
	p.pos++ // skip opening '\''
	start := p.pos
	for p.pos < len(p.src) {
		ch := p.src[p.pos]
		if ch == '\'' {
			s := p.src[start:p.pos]
			p.pos++
			return s, nil
		}
		if ch == '\n' {
			return "", fmt.Errorf("line %d: unclosed literal string", line)
		}
		p.pos++
	}
	return "", fmt.Errorf("line %d: unclosed literal string", line)
}

func (p *parser) readStringArray(startLine int) ([]string, error) {
	p.pos++ // skip '['
	out := []string{}
	for {
		p.skipArrayWhitespace()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("line %d: unclosed array", startLine)
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return out, nil
		}
		itemLine := p.line
		var s string
		var err error
		switch p.src[p.pos] {
		case '"':
			s, err = p.readBasicString(itemLine)
		case '\'':
			s, err = p.readLiteralString(itemLine)
		default:
			return nil, fmt.Errorf("line %d: expected string element in array", itemLine)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		p.skipArrayWhitespace()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("line %d: unclosed array", startLine)
		}
		if p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return out, nil
		}
		return nil, fmt.Errorf("line %d: expected ',' or ']' in array", p.line)
	}
}

func (p *parser) readInt(line int) (int64, error) {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' {
			p.pos++
			continue
		}
		break
	}
	raw := p.src[start:p.pos]
	if raw == "" {
		return 0, fmt.Errorf("line %d: unexpected token %q", line, p.peekChar())
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("line %d: invalid integer %q: %w", line, raw, err)
	}
	return n, nil
}

func (p *parser) assign(line int, key string, v parsedVal) error {
	switch p.sec {
	case secTop:
		return p.assignTop(line, key, v)
	case secService:
		return p.assignService(line, key, v)
	case secProvider:
		return p.assignProvider(line, key, v)
	default:
		return fmt.Errorf("line %d: internal section error", line)
	}
}

func (p *parser) assignTop(line int, key string, v parsedVal) error {
	if p.topSeen[key] {
		return fmt.Errorf("line %d: duplicate key %q", line, key)
	}
	p.topSeen[key] = true
	switch key {
	case "image":
		return needString(line, key, v, &p.cfg.Image)
	case "dockerfile":
		return needString(line, key, v, &p.cfg.Dockerfile)
	case "rootfs_size_mb":
		return needInt64(line, key, v, &p.cfg.RootfsSizeMB)
	case "mem":
		return needInt64(line, key, v, &p.cfg.MemMB)
	case "cpus":
		var n int64
		if err := needInt64(line, key, v, &n); err != nil {
			return err
		}
		p.cfg.VCPUs = int(n)
		return nil
	case "warm":
		return needString(line, key, v, &p.cfg.Warm)
	case "warm_timeout_sec":
		var n int64
		if err := needInt64(line, key, v, &n); err != nil {
			return err
		}
		p.cfg.WarmTimeoutSec = int(n)
		return nil
	case "database":
		return needString(line, key, v, &p.cfg.Database)
	case "env":
		return needStringArray(line, key, v, &p.cfg.Env)
	case "services":
		var names []string
		if err := needStringArray(line, key, v, &names); err != nil {
			return err
		}
		for _, name := range names {
			if name != "postgres" && name != "redis" {
				return fmt.Errorf("line %d: unknown service preset %q (want \"postgres\" or \"redis\", or use [[service]])", line, name)
			}
			p.presets = append(p.presets, types.ServiceSpec{Name: name, Preset: name})
		}
		return nil
	default:
		return fmt.Errorf("line %d: unknown key %q", line, key)
	}
}

func (p *parser) assignService(line int, key string, v parsedVal) error {
	if p.secSeen[key] {
		return fmt.Errorf("line %d: duplicate key %q in [[service]]", line, key)
	}
	p.secSeen[key] = true
	svc := &p.customs[p.secIdx]
	switch key {
	case "name":
		return needString(line, key, v, &svc.Name)
	case "cmd":
		return needStringArray(line, key, v, &svc.Cmd)
	case "user":
		return needString(line, key, v, &svc.User)
	case "env":
		return needStringArray(line, key, v, &svc.Env)
	case "ready_tcp":
		return needString(line, key, v, &svc.ReadyTCP)
	case "ready_cmd":
		return needStringArray(line, key, v, &svc.ReadyCmd)
	case "checkpoint_cmd":
		return needStringArray(line, key, v, &svc.CheckpointCmd)
	case "ready_timeout_sec":
		var n int64
		if err := needInt64(line, key, v, &n); err != nil {
			return err
		}
		svc.ReadyTimeoutSec = int(n)
		return nil
	default:
		return fmt.Errorf("line %d: unknown key %q in [[service]]", line, key)
	}
}

func (p *parser) assignProvider(line int, key string, v parsedVal) error {
	if key == "api_key" {
		return fmt.Errorf("line %d: do not hardcode \"api_key\" in %s; set \"api_key_env\" to a host env var name instead", line, FileName)
	}
	if p.secSeen[key] {
		return fmt.Errorf("line %d: duplicate key %q in [providers.%s]", line, key, p.cfg.Providers[p.secIdx].Name)
	}
	p.secSeen[key] = true
	pr := &p.cfg.Providers[p.secIdx]
	switch key {
	case "upstream":
		return needString(line, key, v, &pr.Upstream)
	case "api_key_env":
		return needString(line, key, v, &pr.APIKeyEnv)
	case "auth_header":
		return needString(line, key, v, &pr.AuthHeader)
	case "base_url_env":
		return needString(line, key, v, &pr.BaseURLEnv)
	case "key_env":
		return needString(line, key, v, &pr.KeyEnv)
	default:
		return fmt.Errorf("line %d: unknown key %q in [providers.%s]", line, key, pr.Name)
	}
}

func needString(line int, key string, v parsedVal, dst *string) error {
	if v.kind != "string" {
		return fmt.Errorf("line %d: %q must be a string", line, key)
	}
	*dst = v.str
	return nil
}

func needInt64(line int, key string, v parsedVal, dst *int64) error {
	if v.kind != "int" {
		return fmt.Errorf("line %d: %q must be an integer", line, key)
	}
	*dst = v.num
	return nil
}

func needStringArray(line int, key string, v parsedVal, dst *[]string) error {
	if v.kind != "array" {
		return fmt.Errorf("line %d: %q must be a string array", line, key)
	}
	*dst = v.arr
	return nil
}

func (p *parser) skipHorizontalSpace() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' {
			p.pos++
			continue
		}
		break
	}
}

func (p *parser) skipSpaceAndComments() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' {
			p.pos++
			continue
		}
		if c == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
}

func (p *parser) skipArrayWhitespace() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' {
			p.advance()
			continue
		}
		if c == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
}

func (p *parser) expectLineEnd(line int) error {
	p.skipSpaceAndComments()
	if p.pos >= len(p.src) {
		return nil
	}
	if p.src[p.pos] == '\n' {
		p.advance()
		return nil
	}
	return fmt.Errorf("line %d: unexpected trailing token %q", line, p.peekChar())
}

func (p *parser) advance() {
	if p.pos < len(p.src) && p.src[p.pos] == '\n' {
		p.line++
	}
	p.pos++
}

func (p *parser) peekChar() string {
	if p.pos >= len(p.src) {
		return "EOF"
	}
	r := rune(p.src[p.pos])
	if !unicode.IsPrint(r) {
		return fmt.Sprintf("0x%x", p.src[p.pos])
	}
	return string(r)
}

func validate(cfg *Config) error {
	if cfg.Image != "" && cfg.Dockerfile != "" {
		return errors.New(`cannot set both "image" and "dockerfile"`)
	}
	if cfg.RootfsSizeMB < 0 {
		return fmt.Errorf("rootfs_size_mb must be >= 0, got %d", cfg.RootfsSizeMB)
	}
	if cfg.MemMB < minMemMB {
		return fmt.Errorf("mem must be >= %d MiB, got %d", minMemMB, cfg.MemMB)
	}
	if cfg.VCPUs < 1 || cfg.VCPUs > maxVCPUs {
		return fmt.Errorf("cpus must be between 1 and %d, got %d", maxVCPUs, cfg.VCPUs)
	}
	if cfg.WarmTimeoutSec < 0 {
		return fmt.Errorf("warm_timeout_sec must be >= 0, got %d", cfg.WarmTimeoutSec)
	}
	if cfg.Database != "" && !dbNameRe.MatchString(cfg.Database) {
		return fmt.Errorf("invalid database name %q", cfg.Database)
	}
	if err := validateEnvList("env", cfg.Env); err != nil {
		return err
	}
	seenSvc := map[string]bool{}
	hasPostgres := false
	for _, s := range cfg.Services {
		if s.Name == "" {
			return errors.New("service name must not be empty")
		}
		if seenSvc[s.Name] {
			return fmt.Errorf("duplicate service %q", s.Name)
		}
		seenSvc[s.Name] = true
		if s.Preset == "postgres" {
			hasPostgres = true
		}
		if s.Preset == "" {
			if len(s.Cmd) == 0 || strings.TrimSpace(s.Cmd[0]) == "" {
				return fmt.Errorf("service %q: cmd must not be empty", s.Name)
			}
			if len(s.ReadyCmd) > 0 && strings.TrimSpace(s.ReadyCmd[0]) == "" {
				return fmt.Errorf("service %q: ready_cmd[0] must not be empty", s.Name)
			}
			if len(s.CheckpointCmd) > 0 && strings.TrimSpace(s.CheckpointCmd[0]) == "" {
				return fmt.Errorf("service %q: checkpoint_cmd[0] must not be empty", s.Name)
			}
		}
		if s.ReadyTimeoutSec < 0 {
			return fmt.Errorf("service %q: ready_timeout_sec must be >= 0", s.Name)
		}
		if s.ReadyTCP != "" {
			if err := validateHostPort(s.ReadyTCP); err != nil {
				return fmt.Errorf("service %q ready_tcp: %w", s.Name, err)
			}
		}
		if err := validateEnvList("service "+s.Name+" env", s.Env); err != nil {
			return err
		}
	}
	if cfg.Database != "" && !hasPostgres {
		return fmt.Errorf("database %q requires \"postgres\" in services", cfg.Database)
	}
	for _, pr := range cfg.Providers {
		switch pr.Name {
		case "anthropic", "openai", "openrouter":
			return fmt.Errorf("provider %q: built-in provider cannot be overridden in %s", pr.Name, FileName)
		}
		if pr.Upstream == "" {
			return fmt.Errorf("provider %q: upstream is required", pr.Name)
		}
		u, err := url.Parse(pr.Upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("provider %q: invalid upstream URL %q", pr.Name, pr.Upstream)
		}
		if u.Scheme != "https" {
			return fmt.Errorf("provider %q: upstream %q must use https", pr.Name, pr.Upstream)
		}
		if addr, err := netip.ParseAddr(u.Hostname()); err == nil {
			addr = addr.Unmap()
			if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
				addr.IsLoopback() || addr.IsPrivate() ||
				addr.IsUnspecified() || addr.IsMulticast() ||
				branchvmNet.Contains(addr) {
				return fmt.Errorf("provider %q: upstream IP %s is not allowed", pr.Name, addr)
			}
		}
		if !envNameRe.MatchString(pr.APIKeyEnv) {
			return fmt.Errorf("provider %q: invalid api_key_env %q", pr.Name, pr.APIKeyEnv)
		}
		if pr.AuthHeader != "Authorization" && pr.AuthHeader != "X-Api-Key" {
			return fmt.Errorf("provider %q: auth_header must be \"Authorization\" or \"X-Api-Key\", got %q", pr.Name, pr.AuthHeader)
		}
		if pr.BaseURLEnv != "" && !envNameRe.MatchString(pr.BaseURLEnv) {
			return fmt.Errorf("provider %q: invalid base_url_env %q", pr.Name, pr.BaseURLEnv)
		}
		if pr.KeyEnv != "" && !envNameRe.MatchString(pr.KeyEnv) {
			return fmt.Errorf("provider %q: invalid key_env %q", pr.Name, pr.KeyEnv)
		}
	}
	return nil
}

func validateEnvList(label string, items []string) error {
	for _, item := range items {
		k, _, ok := strings.Cut(item, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return fmt.Errorf("%s entry %q must be KEY=VALUE", label, item)
		}
	}
	return nil
}

func validateHostPort(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("invalid host:port %q", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port in %q", addr)
	}
	return nil
}
