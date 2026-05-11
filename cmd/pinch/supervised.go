package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kwad77/pincher/internal/supervisor"
)

// ensureSessionIDEnv returns env with a PINCHER_SESSION_ID set if the
// caller hadn't provided one. The supervisor stamps this once per
// supervisor lifetime so all inner respawns share one sessions row
// (#420). Pure function; the helper exists so the env-building logic
// is unit-testable without spinning up the full supervisor.
func ensureSessionIDEnv(env []string) []string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "PINCHER_SESSION_ID=") {
			return env
		}
	}
	return append(env, fmt.Sprintf("PINCHER_SESSION_ID=sup-%d", time.Now().UnixNano()))
}

// runSupervisedCLI is the `pincher supervised` entry point. It runs a
// long-lived process that wraps an inner pincher MCP server with
// auto-respawn + initialize-replay, so the MCP client (Claude Code,
// Codex, etc.) sees an unbroken stdio session even when the inner
// exits — whether from PINCHER_AUTO_RESTART_ON_DRIFT firing on a binary
// upgrade, an unrecoverable panic, or an OS-level kill.
//
// Configure your MCP client to invoke `pincher supervised` instead of
// `pincher`, and the manual `/mcp` reconnect dance disappears.
//
// Note: passes through any args after `supervised` to the inner pincher
// (`pincher supervised --slow-query-ms 100` runs the inner with that
// flag). This lets users keep using their existing pincher flags
// transparently.
func runSupervisedCLI(args []string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher supervised: cannot resolve own binary: %v\n", err)
		os.Exit(1)
	}

	sup := supervisor.New(exe)
	sup.InnerArgs = args
	// #420: stamp a stable PINCHER_SESSION_ID so successive inner
	// processes share one sessions-table row. The inner reads this on
	// startup and seeds atomic counters from the prior flush, so the
	// SESSION stats survive supervised respawn instead of resetting
	// to zero on every binary swap. Inherits an existing value when
	// the user already set one (test harnesses, deliberate
	// multi-supervisor sharing).
	sup.Env = ensureSessionIDEnv(os.Environ())
	sup.Stdin = os.Stdin
	sup.Stdout = os.Stdout
	sup.Stderr = os.Stderr

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := sup.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "pincher supervised: %v\n", err)
		os.Exit(1)
	}
}
