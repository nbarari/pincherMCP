package server

import (
	"log/slog"
	"os"
)

// autoRestartEnvVar is the opt-in flag for #352 self-restart-on-drift.
// When set to "1", a fresh binary on disk + drift detection together
// cause the MCP server to exit cleanly; Claude Code's MCP client then
// respawns the configured command, which loads the new binary. Default
// off — opt-in only because the respawn behaviour depends on the
// caller's MCP client implementation.
const autoRestartEnvVar = "PINCHER_AUTO_RESTART_ON_DRIFT"

// maybeAutoRestart checks whether this server should exit so that a
// fresh binary on disk takes over on the next request. Three conditions
// gate the exit:
//  1. Env var PINCHER_AUTO_RESTART_ON_DRIFT must be "1".
//  2. binaryReplaced must be true (the on-disk binary's mtime advanced
//     past s.binaryStartMTime — i.e. a `go build` shipped a new file
//     while this process was running).
//  3. s.autoRestartOnce guards the actual exit so concurrent tool
//     calls in flight don't all race to call os.Exit.
//
// When all three hold, log a single line and call s.exitFn(0). Tests
// substitute s.exitFn with a recording stub to assert the path fired
// without actually killing the test process.
//
// driftDetected is informational — included in the log line so the
// reason for the restart is searchable. Not part of the gate (binary
// replacement alone is sufficient signal).
func (s *Server) maybeAutoRestart(binaryReplaced, driftDetected bool) {
	if os.Getenv(autoRestartEnvVar) != "1" {
		return
	}
	if !binaryReplaced {
		// Drift but no new binary on disk → nothing to restart into.
		// Just warn-only via the existing binary_stale signal.
		return
	}
	s.autoRestartOnce.Do(func() {
		slog.Info("pincher.auto_restart",
			"reason", "fresh binary on disk + drift detected",
			"version", s.version,
			"binary_path", s.binaryPath,
			"drift_detected", driftDetected,
			"env_var", autoRestartEnvVar+"=1")
		// Exit 0 — clean shutdown. Claude Code's MCP transport sees
		// the stdio EOF and respawns the configured command. The new
		// process loads the rebuilt binary and serves the next call.
		s.exitFn(0)
	})
}

// checkAutoRestart is the per-tool-call entry point for the #352
// self-restart-on-drift path. Called from jsonResultWithMeta /
// textResultWithMeta so every tool response — not just `health` — is
// a restart trigger. When PINCHER_AUTO_RESTART_ON_DRIFT is unset,
// maybeAutoRestart returns at the first os.Getenv check; that's the
// only steady-state cost (one syscall, sub-µs). When the env var is
// set, this also stat's the binary path. driftDetected is reported
// false here because we don't have the index_drift signal outside of
// health; binary replacement alone is the load-bearing condition.
func (s *Server) checkAutoRestart() {
	s.maybeAutoRestart(s.binaryReplacedSinceStart(), false)
}

// binaryReplacedSinceStart reports whether the on-disk binary's mtime
// has advanced past s.binaryStartMTime. Returns false when the binary
// path or start mtime weren't captured (e.g. embedded test runs), or
// when stat fails.
func (s *Server) binaryReplacedSinceStart() bool {
	if s.binaryPath == "" || s.binaryStartMTime.IsZero() {
		return false
	}
	info, err := os.Stat(s.binaryPath)
	if err != nil {
		return false
	}
	return info.ModTime().After(s.binaryStartMTime)
}
