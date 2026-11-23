//go:build linux

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Heman10x-NGU/branchvm/pkg/pool"
	"github.com/Heman10x-NGU/branchvm/pkg/types"
)

func TestParseShellArgs(t *testing.T) {
	t.Run("interactive_default_timeout", func(t *testing.T) {
		idx, runID, timeout, cmd, err := parseShellArgs([]string{"2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if idx != 2 || runID != "" || timeout != defaultShellTimeoutSec || len(cmd) != 0 {
			t.Fatalf("got (%d, %q, %d, %q), want (2, \"\", %d, nil)", idx, runID, timeout, cmd, defaultShellTimeoutSec)
		}
	})

	t.Run("non_interactive_flags_after_index", func(t *testing.T) {
		idx, runID, timeout, cmd, err := parseShellArgs([]string{"3", "--run", "r123", "--timeout", "15", "--", "go", "test", "./..."})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantCmd := []string{"go", "test", "./..."}
		if idx != 3 || runID != "r123" || timeout != 15 || !reflect.DeepEqual(cmd, wantCmd) {
			t.Fatalf("got (%d, %q, %d, %q), want (3, \"r123\", 15, %q)", idx, runID, timeout, cmd, wantCmd)
		}
	})

	t.Run("flags_before_index_disable_timeout", func(t *testing.T) {
		idx, _, timeout, cmd, err := parseShellArgs([]string{"--timeout", "0", "1", "--", "sh"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if idx != 1 || timeout != 0 || !reflect.DeepEqual(cmd, []string{"sh"}) {
			t.Fatalf("got (%d, %d, %q), want (1, 0, [sh])", idx, timeout, cmd)
		}
	})

	t.Run("negative_timeout_rejected", func(t *testing.T) {
		if _, _, _, _, err := parseShellArgs([]string{"1", "--timeout", "-5"}); err == nil {
			t.Fatal("expected error for negative --timeout")
		}
	})
}

func TestParsePager(t *testing.T) {
	if got, err := parsePager("mmap"); err != nil || got != types.FileRestore {
		t.Fatalf("parsePager(mmap) = (%q, %v), want (%q, nil)", got, err, types.FileRestore)
	}
	if got, err := parsePager("fadvise"); err != nil || got != pool.PrefaultRestore {
		t.Fatalf("parsePager(fadvise) = (%q, %v), want (%q, nil)", got, err, pool.PrefaultRestore)
	}
	if _, err := parsePager("uffd"); err == nil || !strings.Contains(err.Error(), "bench") {
		t.Fatalf("parsePager(uffd) err = %v, want error mentioning bench", err)
	}
}
