//go:build linux

package pool

import (
	"bytes"
	"context"
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
