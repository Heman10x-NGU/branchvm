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
	if cfg.MemMB != 1024 || cfg.VCPUs != 2 || cfg.WarmTimeoutSec != 1800 {
		t.Fatalf("defaults = (mem=%d, vcpus=%d, warm_timeout=%d), want (1024, 2, 1800)", cfg.MemMB, cfg.VCPUs, cfg.WarmTimeoutSec)
	}
	if cfg.Database != "authdemo" {
		t.Fatalf("Database = %q, want authdemo", cfg.Database)
	}
	wantEnv := []string{"DATABASE_URL=postgres://postgres@127.0.0.1:5432/authdemo?sslmode=disable"}
	if !reflect.DeepEqual(cfg.Env, wantEnv) {
		t.Fatalf("Env = %#v, want %#v", cfg.Env, wantEnv)
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
	if cfg.Database != "" || len(cfg.Env) != 0 || cfg.WarmTimeoutSec != 1800 {
		t.Fatalf("unexpected defaults for existing config: %+v", cfg)
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
warm_timeout_sec = 2400
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

[providers.together]
upstream = "https://api.together.xyz/v1"
api_key_env = "TOGETHER_API_KEY"
base_url_env = "TOGETHER_BASE_URL"
key_env = "TOGETHER_API_KEY"
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
	if cfg.Image != "myapp:dev" || cfg.RootfsSizeMB != 4096 || cfg.MemMB != 2048 || cfg.VCPUs != 4 || cfg.WarmTimeoutSec != 2400 {
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
		Name:       "together",
		Upstream:   "https://api.together.xyz/v1",
		APIKeyEnv:  "TOGETHER_API_KEY",
		AuthHeader: "Authorization",
		BaseURLEnv: "TOGETHER_BASE_URL",
		KeyEnv:     "TOGETHER_API_KEY",
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
		{"database without postgres", "database = \"appdb\"\nservices = [\"redis\"]\n", "requires \"postgres\""},
		{"negative warm_timeout_sec", "warm_timeout_sec = -1\n", "warm_timeout_sec must be >= 0"},
		{"env missing equals", "env = [\"NO_EQUALS\"]\n", "KEY=VALUE"},
		{"literal api_key rejected", "[providers.custom]\nupstream = \"https://x.test\"\napi_key = \"secret\"\n", "api_key_env"},
		{"bad auth header", "[providers.custom]\nupstream = \"https://x.test\"\napi_key_env = \"KEY\"\nauth_header = \"Cookie\"\n", "auth_header"},
		{"bad upstream url", "[providers.custom]\nupstream = \"ftp://x.test\"\napi_key_env = \"KEY\"\n", "invalid upstream URL"},
		{"http custom provider rejected", "[providers.custom]\nupstream = \"http://x.test\"\napi_key_env = \"KEY\"\n", "must use https"},
		{"builtin anthropic rejected", "[providers.anthropic]\nupstream = \"https://x.test\"\napi_key_env = \"KEY\"\n", "built-in provider"},
		{"builtin openai rejected", "[providers.openai]\nupstream = \"https://x.test\"\napi_key_env = \"KEY\"\n", "built-in provider"},
		{"builtin openrouter rejected", "[providers.openrouter]\nupstream = \"https://x.test\"\napi_key_env = \"KEY\"\n", "built-in provider"},
		{"provider link-local ip rejected", "[providers.custom]\nupstream = \"https://169.254.169.254/latest\"\napi_key_env = \"KEY\"\n", "is not allowed"},
		{"provider mapped link-local ip rejected", "[providers.custom]\nupstream = \"https://[::ffff:169.254.169.254]/latest\"\napi_key_env = \"KEY\"\n", "is not allowed"},
		{"provider private ip rejected", "[providers.custom]\nupstream = \"https://192.168.1.10/v1\"\napi_key_env = \"KEY\"\n", "is not allowed"},
		{"provider loopback ip rejected", "[providers.custom]\nupstream = \"https://127.0.0.1/v1\"\napi_key_env = \"KEY\"\n", "is not allowed"},
		{"provider branchvm net ip rejected", "[providers.custom]\nupstream = \"https://10.200.2.2/v1\"\napi_key_env = \"KEY\"\n", "is not allowed"},
		{"unknown preset", "services = [\"mysql\"]\n", "unknown service preset"},
		{"custom service missing cmd", "[[service]]\nname = \"api\"\n", "cmd must not be empty"},
		{"custom service blank cmd", "[[service]]\nname = \"api\"\ncmd = [\"  \"]\n", "cmd must not be empty"},
		{"custom service blank ready_cmd", "[[service]]\nname = \"api\"\ncmd = [\"node\"]\nready_cmd = [\"\"]\n", "ready_cmd[0] must not be empty"},
		{"custom service blank checkpoint_cmd", "[[service]]\nname = \"api\"\ncmd = [\"node\"]\ncheckpoint_cmd = [\" \"]\n", "checkpoint_cmd[0] must not be empty"},
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
