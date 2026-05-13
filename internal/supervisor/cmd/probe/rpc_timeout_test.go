package main

import (
	"testing"
	"time"
)

// rpcTimeout in this package is duplicated from internal/supervisor's
// helper (cmd/probe is its own package main; can't import the parent
// without an import-cycle hazard). Test the duplicate so coverage
// reflects the production helper, not just the supervisor-package one.
//
// All four cases match the supervisor-package tests verbatim.

func TestProbeRPCTimeout_DefaultsTo15s(t *testing.T) {
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "")
	if got := rpcTimeout(); got != 15*time.Second {
		t.Errorf("default: got %v, want 15s", got)
	}
}

func TestProbeRPCTimeout_HonorsEnv(t *testing.T) {
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "30s")
	if got := rpcTimeout(); got != 30*time.Second {
		t.Errorf("env=30s: got %v", got)
	}
}

func TestProbeRPCTimeout_RejectsGarbage(t *testing.T) {
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "not-a-duration")
	if got := rpcTimeout(); got != 15*time.Second {
		t.Errorf("env=garbage should fall back; got %v", got)
	}
}

func TestProbeRPCTimeout_RejectsNonPositive(t *testing.T) {
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "0s")
	if got := rpcTimeout(); got != 15*time.Second {
		t.Errorf("env=0s should fall back; got %v", got)
	}
	t.Setenv("PINCHER_TEST_RPC_TIMEOUT", "-1s")
	if got := rpcTimeout(); got != 15*time.Second {
		t.Errorf("env=-1s should fall back; got %v", got)
	}
}
