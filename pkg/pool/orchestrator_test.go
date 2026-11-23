//go:build linux

package pool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

func TestCountPassed(t *testing.T) {
	tests := []struct {
		name     string
		branches []types.BranchState
		want     int
	}{
		{name: "empty", branches: nil, want: 0},
		{
			name: "all_fail_or_error",
			branches: []types.BranchState{
				{Index: 1, Status: StatusFail},
				{Index: 2, Status: StatusError},
				{Index: 3, Status: StatusFail},
			},
			want: 0,
		},
		{
			name: "mixed",
			branches: []types.BranchState{
				{Index: 1, Status: StatusFail},
				{Index: 2, Status: StatusPass},
				{Index: 3, Status: StatusPass},
			},
			want: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countPassed(tc.branches); got != tc.want {
				t.Fatalf("countPassed() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestShellContext(t *testing.T) {
	ctx := context.Background()

	// Interactive shell stays unbounded even with positive timeout.
	interactiveCtx, cancel := shellContext(ctx, nil, 600)
	defer cancel()
	if _, ok := interactiveCtx.Deadline(); ok {
		t.Fatal("interactive shell unexpectedly set a deadline")
	}

	// Non-interactive command with timeout > 0 gets a deadline.
	cmdCtx, cancelCmd := shellContext(ctx, []string{"go", "test"}, 10)
	defer cancelCmd()
	if _, ok := cmdCtx.Deadline(); !ok {
		t.Fatal("non-interactive command with timeout=10 missing deadline")
	}

	// Non-interactive command with timeout=0 disables timeout.
	unboundedCtx, cancelUnbounded := shellContext(ctx, []string{"go", "test"}, 0)
	defer cancelUnbounded()
	if _, ok := unboundedCtx.Deadline(); ok {
		t.Fatal("non-interactive command with timeout=0 unexpectedly set a deadline")
	}
}

func TestPGReadyCmd(t *testing.T) {
	cmd := pgReadyCmd()
	foundUser := false
	for i := 0; i+1 < len(cmd); i++ {
		if cmd[i] == "-U" && cmd[i+1] == "postgres" {
			foundUser = true
			break
		}
	}
	if !foundUser {
		t.Fatalf("pgReadyCmd() = %q, expected -U postgres", cmd)
	}
}

func TestWriteDiffStat(t *testing.T) {
	patch := []byte(`diff --git a/store.go b/store.go
index 1111111..2222222 100644
--- a/store.go
+++ b/store.go
@@ -1,2 +1,3 @@
 package main
+// added
 func main() {}
`)
	var out bytes.Buffer
	if err := writeDiffStat(context.Background(), patch, &out); err != nil {
		t.Fatalf("writeDiffStat() error: %v (out=%s)", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "store.go") || !strings.Contains(got, "1 file changed") {
		t.Fatalf("writeDiffStat() output = %q, expected store.go and 1 file changed", got)
	}
}

func TestClearGoldenSnapshotFiles(t *testing.T) {
	dir := t.TempDir()
	stale := []string{"snapshot", "mem", "ws.bin", "meta.json"}
	for _, name := range stale {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := clearGoldenSnapshotFiles(dir); err != nil {
		t.Fatalf("clearGoldenSnapshotFiles() error: %v", err)
	}
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after clearGoldenSnapshotFiles: %v", name, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file removed: %v", err)
	}
}

func TestCleanOrphanRunFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string]bool{
		"r111111.json":                   true,  // live run state
		"r111111-branch-1.patch":         true,  // belongs to live run
		"r111111-branch-1.out":           true,  // belongs to live run
		"r222222-branch-1.patch":         false, // orphan
		"r222222-branch-2.out":           false, // orphan
		"r222222-branch-1.patch.applied": true,  // applied patch must stay
		"notes.txt":                      true,  // unrelated file must stay
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanOrphanRunFiles(dir); err != nil {
		t.Fatalf("cleanOrphanRunFiles() error: %v", err)
	}
	for name, wantExist := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		if wantExist && err != nil {
			t.Fatalf("%s unexpectedly removed: %v", name, err)
		}
		if !wantExist && !os.IsNotExist(err) {
			t.Fatalf("%s still exists, expected removal (err=%v)", name, err)
		}
	}
}

func TestBranchConfigFromMeta(t *testing.T) {
	t.Run("explicit_vcpus", func(t *testing.T) {
		cfg := BranchConfigFromMeta(types.GoldenMeta{
			VCPUs: 4,
			Services: []types.ServiceSpec{
				{Name: "redis", Preset: "redis"},
			},
			Env: []string{"FOO=bar"},
		})
		if cfg.VCPUs != 4 {
			t.Fatalf("VCPUs = %d, want 4", cfg.VCPUs)
		}
		if len(cfg.Services) != 1 || cfg.Services[0].Name != "redis" {
			t.Fatalf("Services = %+v, want redis", cfg.Services)
		}
		if len(cfg.Env) != 1 || cfg.Env[0] != "FOO=bar" {
			t.Fatalf("Env = %+v, want [FOO=bar]", cfg.Env)
		}
	})

	t.Run("defaults_vcpus_when_non_positive", func(t *testing.T) {
		cfg := BranchConfigFromMeta(types.GoldenMeta{VCPUs: 0})
		if cfg.VCPUs != coldVCPUs {
			t.Fatalf("VCPUs = %d, want %d", cfg.VCPUs, coldVCPUs)
		}
		if cfg.Services == nil || len(cfg.Services) != 0 {
			t.Fatalf("Services = %#v, want empty non-nil slice", cfg.Services)
		}
	})
}

func TestHasCustomServices(t *testing.T) {
	tests := []struct {
		name     string
		services []types.ServiceSpec
		want     bool
	}{
		{name: "stateless", services: nil, want: false},
		{name: "preset_only", services: []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}, {Name: "redis", Preset: "redis"}}, want: false},
		{name: "custom_only", services: []types.ServiceSpec{{Name: "api", Cmd: []string{"node", "server.js"}}}, want: true},
		{name: "mixed", services: []types.ServiceSpec{{Name: "postgres", Preset: "postgres"}, {Name: "worker", Cmd: []string{"./worker"}}}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasCustomServices(tc.services); got != tc.want {
				t.Fatalf("hasCustomServices(%+v) = %t, want %t", tc.services, got, tc.want)
			}
		})
	}
}
