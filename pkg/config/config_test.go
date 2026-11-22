package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

func TestLoadMissingReturnsDefaultWithPostgres(t *testing.T) {
	dir := t.TempDir()
	cfg, found, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Fatal("found = true on empty dir, want false")
	}
	if cfg.MemMB != 1024 || cfg.VCPUs != 2 {
		t.Fatalf("defaults = (%d, %d), want (1024, 2)", cfg.MemMB, cfg.VCPUs)
	}
	wantSvc := []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}}
	if !reflect.DeepEqual(cfg.Services, wantSvc) {
		t.Fatalf("Services = %+v, want %+v", cfg.Services, wantSvc)
	}
}

func TestMinimalConfigDefaultsStateless(t *testing.T) {
	cfg, err := Parse([]byte("dockerfile = \"Dockerfile\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Dockerfile != "Dockerfile" {
		t.Fatalf("Dockerfile = %q, want Dockerfile", cfg.Dockerfile)
	}
	if cfg.Services == nil || len(cfg.Services) != 0 {
		t.Fatalf("Services = %#v, want empty non-nil slice", cfg.Services)
	}
}

func TestAuthDemoConfigLoads(t *testing.T) {
	cfg, found, err := Load(filepath.Join("..", "..", "examples", "auth-demo"))
	if err != nil {
		t.Fatalf("Load auth-demo: %v", err)
	}
	if !found {
		t.Fatal("examples/auth-demo/branchvm.toml not found")
	}
	if cfg.Database != "authdemo" || cfg.Warm != "go test -run ^$ ./..." {
		t.Fatalf("unexpected auth-demo config: %+v", cfg)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].Preset != "postgres" {
		t.Fatalf("auth-demo services = %+v, want postgres", cfg.Services)
	}
}

func TestParseFullConfigWithMultilineAndQuotedHashes(t *testing.T) {
	raw := `# comment line
image = "myapp:dev"
rootfs_size_mb = 4096
mem = 2048
cpus = 4
warm = "npm run build"
database = "app_db-1"

services = [
  "postgres", # inline comment
  "redis",
]

env = [
  "DATABASE_URL=postgres://user:p#ss@127.0.0.1:5432/app_db-1?sslmode=disable",
  'LITERAL_PATH=C:\raw\#value',
]

[[service]]
name = "worker"
cmd = ["node", "worker.js"]
user = "node"
env = ["WORKER_MODE=1"]
ready_tcp = "127.0.0.1:4000"
ready_cmd = ["curl", "-sf", "http://127.0.0.1:4000/health"]
checkpoint_cmd = ["sync"]
ready_timeout_sec = 25

[providers.openrouter]
upstream = "https://openrouter.ai/api"
api_key_env = "OPENROUTER_API_KEY"
base_url_env = "OPENROUTER_BASE_URL"
key_env = "OPENROUTER_API_KEY"
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if cfg.Image != "myapp:dev" || cfg.RootfsSizeMB != 4096 || cfg.MemMB != 2048 || cfg.VCPUs != 4 {
		t.Fatalf("scalar mismatch: %+v", cfg)
	}
	wantEnv := []string{
		"DATABASE_URL=postgres://user:p#ss@127.0.0.1:5432/app_db-1?sslmode=disable",
		`LITERAL_PATH=C:\raw\#value`,
	}
	if !reflect.DeepEqual(cfg.Env, wantEnv) {
		t.Fatalf("Env = %#v, want %#v", cfg.Env, wantEnv)
	}
	if len(cfg.Services) != 3 || cfg.Services[0].Preset != "postgres" || cfg.Services[1].Preset != "redis" {
		t.Fatalf("Services = %+v", cfg.Services)
	}
	worker := cfg.Services[2]
	if worker.Name != "worker" || worker.User != "node" || worker.ReadyTCP != "127.0.0.1:4000" || worker.ReadyTimeoutSec != 25 {
		t.Fatalf("worker service = %+v", worker)
	}
	wantProv := []types.ProviderSpec{{
		Name:       "openrouter",
		Upstream:   "https://openrouter.ai/api",
		APIKeyEnv:  "OPENROUTER_API_KEY",
		AuthHeader: "Authorization",
		BaseURLEnv: "OPENROUTER_BASE_URL",
		KeyEnv:     "OPENROUTER_API_KEY",
	}}
	if !reflect.DeepEqual(cfg.Providers, wantProv) {
		t.Fatalf("Providers = %+v, want %+v", cfg.Providers, wantProv)
	}
}

func TestParseValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"unknown key", "mem = 1024\nbogus = \"x\"\n", "line 2: unknown key"},
		{"both image and dockerfile", "image = \"a\"\ndockerfile = \"Dockerfile\"\n", "cannot set both"},
		{"bad database", "database = \"bad;drop\"\n", "invalid database name"},
		{"env missing equals", "env = [\"NO_EQUALS\"]\n", "KEY=VALUE"},
		{"literal api_key rejected", "[providers.custom]\nupstream = \"https://x.test\"\napi_key = \"secret\"\n", "api_key_env"},
		{"bad auth header", "[providers.custom]\nupstream = \"https://x.test\"\napi_key_env = \"KEY\"\nauth_header = \"Cookie\"\n", "auth_header"},
		{"bad upstream url", "[providers.custom]\nupstream = \"ftp://x.test\"\napi_key_env = \"KEY\"\n", "invalid upstream URL"},
		{"unknown preset", "services = [\"mysql\"]\n", "unknown service preset"},
		{"custom service missing cmd", "[[service]]\nname = \"api\"\n", "cmd must not be empty"},
		{"duplicate service", "services = [\"redis\"]\n[[service]]\nname = \"redis\"\ncmd = [\"redis-server\"]\n", "duplicate service"},
		{"unclosed quote", "image = \"nope\n", "unclosed string literal"},
		{"unclosed array", "env = [\n\"A=1\",\n", "unclosed array"},
		{"low mem", "mem = 64\n", "mem must be >= 128"},
		{"bad ready_tcp", "[[service]]\nname = \"web\"\ncmd = [\"node\"]\nready_tcp = \"badport\"\n", "ready_tcp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
