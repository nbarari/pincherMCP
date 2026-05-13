package supervisor

import (
	"os"
	"testing"
	"time"
)

// rpcTimeout returns the timeout used by supervisor stdio integration
// tests. Defaults to 15s; tunable via PINCHER_TEST_RPC_TIMEOUT
// (Go duration syntax, e.g. "30s"). Default bumped from 5s → 15s
// in v0.55 (#681 Bucket A) — the original budget was borderline on
// the windows-latest CI runner under -p 2 parallelism, surfacing as
// `TestSupervisor_CapturesAndReplaysInit` and similar flakes.
//
// The same helper is duplicated in cmd/probe/main.go because that
// command is its own `package main` and can't import this package
// without an import-cycle hazard. Two-line duplication < shared
// package overhead.
func rpcTimeout() time.Duration {
	if v := os.Getenv("PINCHER_TEST_RPC_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 15 * time.Second
}

func TestRPCTimeout_DefaultsTo15s(t *testing.T) {
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "")
	if got := rpcTimeout(); got != 15*time.Second {
		t.Errorf("default: got %v, want 15s", got)
	}
}

func TestRPCTimeout_HonorsEnv(t *testing.T) {
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "30s")
	if got := rpcTimeout(); got != 30*time.Second {
		t.Errorf("env=30s: got %v", got)
	}
}

func TestRPCTimeout_RejectsGarbageEnv(t *testing.T) {
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "not-a-duration")
	if got := rpcTimeout(); got != 15*time.Second {
		t.Errorf("env=garbage should fall back to 15s default; got %v", got)
	}
}

func TestRPCTimeout_RejectsZeroOrNegative(t *testing.T) {
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "0s")
	if got := rpcTimeout(); got != 15*time.Second {
		t.Errorf("env=0s should fall back to default; got %v", got)
	}
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "-1s")
	if got := rpcTimeout(); got != 15*time.Second {
		t.Errorf("env=-1s should fall back to default; got %v", got)
	}
}
