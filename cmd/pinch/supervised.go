package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kwad77/pincher/internal/supervisor"
)

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
	sup.Env = os.Environ()
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
