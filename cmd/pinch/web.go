package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

const (
	// webDefaultStartPort is the first port web tries when auto-starting an
	// HTTP server. Scans upward by webPortScanRange ports to find a free one.
	webDefaultStartPort = 7777
	webPortScanRange    = 16

	// webBackgroundReadyTimeout caps how long `web` waits for an auto-spawned
	// pincher process to start serving its /v1/health endpoint. Above this we
	// give up and report failure so the user isn't left staring at a hang.
	webBackgroundReadyTimeout = 8 * time.Second

	// webMaxStaleAge is the cutoff for "freshly-flushed". Rows older than
	// this are still considered if PID is alive (a long-running pincher
	// process whose flusher last fired hours ago is still valid), but a
	// row newer than this skips the PID liveness probe entirely as a
	// fast-path.
	webMaxStaleAge = 30 * time.Second
)

// runWebCLI implements `pincher web [--data-dir DIR] [--no-start] [--port N]`.
//
// Looks up the most recently flushed session in the sessions table that
// advertised an HTTP listener; if PID liveness checks pass, prints the URL
// and exits 0. If no live HTTP server is found and --no-start is unset,
// auto-spawns `pincher --http 127.0.0.1:N` in the background, starting at
// --port (default 7777) and scanning upward until a free port binds, then
// prints the URL of the new server.
func runWebCLI(args []string) {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	noStart := fs.Bool("no-start", false, "Do not auto-start an HTTP server when none is running; exit 1 instead")
	port := fs.Int("port", webDefaultStartPort, "Starting port for auto-start scan")
	jsonOut := fs.Bool("json", false, "Emit a single JSON line {url, pid, started_by} instead of a human banner")
	timeoutSec := fs.Int("timeout", int(webBackgroundReadyTimeout/time.Second), "Seconds to wait for an auto-started server to become ready")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher web [--data-dir DIR] [--no-start] [--port N] [--json] [--timeout SEC]")
		fmt.Fprintln(os.Stderr, "  Resolves the active pincher HTTP URL. Auto-starts a server on demand.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	dir := *dataDir
	if dir == "" {
		var err error
		dir, err = db.DataDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pincher web: data dir: %v\n", err)
			os.Exit(1)
		}
	}

	store, err := db.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher web: open db: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	if base, pid, ok := findLiveHTTPServer(store); ok {
		// #706: warn loudly when the running HTTP server's version
		// doesn't match the on-disk binary. Common dev-loop trap: dev
		// rebuilds, runs `pincher web`, gets the URL of the previous
		// dev session's stale server, and dogfoods against old code
		// without realizing. Best-effort — a probe failure or empty
		// version field leaves the banner suppressed (don't make `web`
		// fragile on flaky networks).
		if runningVer := fetchRunningServerVersion(base); runningVer != "" && runningVer != version {
			fmt.Fprintf(os.Stderr,
				"pincher web: WARNING — running HTTP server is %q but the on-disk binary is %q.\n"+
					"  The dashboard will reflect the running (older) code, not the binary you just built.\n"+
					"  To use the fresh binary: kill PID %d, then re-run `pincher web` to auto-start a new server.\n",
				runningVer, version, pid)
		}
		emitWebResult(os.Stdout, base, dashboardURL(base), pid, "existing", *jsonOut)
		return
	}

	if *noStart {
		fmt.Fprintln(os.Stderr, "pincher web: no live HTTP server and --no-start set")
		os.Exit(1)
	}

	base, pid, err := startBackgroundHTTPServer(*port, time.Duration(*timeoutSec)*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher web: auto-start failed: %v\n", err)
		os.Exit(1)
	}
	emitWebResult(os.Stdout, base, dashboardURL(base), pid, "started", *jsonOut)
}

// dashboardURL turns a base URL (no path) into the full dashboard URL.
// The base may include a reverse-proxy prefix (e.g. http://host/pincher);
// the dashboard always lives under /v1/dashboard relative to that prefix.
func dashboardURL(base string) string {
	return strings.TrimRight(base, "/") + "/v1/dashboard"
}

// findLiveHTTPServer queries the sessions table for the most recent row
// with an HTTP URL, then checks the PID is still alive. Returns
// (url, pid, true) when a live server is found, (zero, zero, false) otherwise.
func findLiveHTTPServer(store *db.Store) (string, int, bool) {
	row, err := store.GetLatestHTTPSession()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, false
		}
		// Non-fatal: log via stderr but don't bail — caller can still auto-start.
		fmt.Fprintf(os.Stderr, "pincher web: query latest http session: %v\n", err)
		return "", 0, false
	}

	// Fast path: row was flushed within the recent window AND a probe
	// confirms the URL responds. Skip PID liveness — a probe is more
	// authoritative than PID existence (PID could be reused).
	if time.Since(row.LastSeen) < webMaxStaleAge && probeHTTPHealthy(row.HTTPURL) {
		return row.HTTPURL, row.HTTPPID, true
	}

	// Slow path: PID liveness check, then probe.
	if !pidIsAlive(row.HTTPPID) {
		return "", 0, false
	}
	if !probeHTTPHealthy(row.HTTPURL) {
		return "", 0, false
	}
	return row.HTTPURL, row.HTTPPID, true
}

// fetchRunningServerVersion (#706) GETs <url>/v1/health and parses the
// `version` field from the JSON body. Returns the version string, or ""
// when the probe fails, the response isn't JSON, or `version` is empty.
// Best-effort — used only to surface a soft warning, never to fail the
// `web` flow.
func fetchRunningServerVersion(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	url := strings.TrimRight(rawURL, "/") + "/v1/health"
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return ""
	}
	return body.Version
}

// probeHTTPHealthy issues a 1-second GET to <url>/v1/health and returns
// true on a 2xx response. Uses a short timeout so a hung server doesn't
// cause `web` itself to hang.
func probeHTTPHealthy(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	url := strings.TrimRight(rawURL, "/") + "/v1/health"
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// startBackgroundHTTPServer spawns `pincher --http 127.0.0.1:N` detached
// from the current TTY, scans upward from startPort until net.Listen
// confirms a free port (probe-then-claim is racy, but the spawned process
// also retries internally; collisions are surfaced as errors here only
// when every probed port is busy). Polls until the new server's
// /v1/health responds, then returns its URL+PID.
func startBackgroundHTTPServer(startPort int, readyTimeout time.Duration) (string, int, error) {
	port, err := pickFreePort(startPort, webPortScanRange)
	if err != nil {
		return "", 0, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	exe, err := os.Executable()
	if err != nil {
		return "", 0, fmt.Errorf("locate self: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// --no-stdio prevents the detached child from running the MCP stdio
	// loop. On Windows, DETACHED_PROCESS gives the child no inherited
	// console; the stdio reader would error immediately and tear down
	// the HTTP server before our readiness probe fires (#232). On Unix
	// the child inherits no stdin (we redirect to /dev/null effectively
	// via cmd.Stdin = nil below), so the same flag matters there too.
	cmd := exec.Command(exe, "--http", addr, "--no-stdio")
	cmd.Env = os.Environ()
	// Detach from parent's stdin/stdout — the child should outlive this `web`
	// invocation. Stderr is silenced; fatal startup errors surface via the
	// readiness probe failing.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := startDetached(cmd); err != nil {
		return "", 0, fmt.Errorf("spawn pincher --http: %w", err)
	}

	url := "http://" + addr
	if waitForReady(url, readyTimeout) {
		return url, cmd.Process.Pid, nil
	}

	// Best-effort cleanup if the child failed to come up in time.
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return "", 0, fmt.Errorf("auto-started pincher did not become ready within %s on %s", readyTimeout, addr)
}

// pickFreePort scans n consecutive ports starting at start, returning the
// first one that net.Listen accepts. The listener is closed before returning
// so the caller can re-bind it.
func pickFreePort(start, n int) (int, error) {
	for i := 0; i < n; i++ {
		port := start + i
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free port in [%d, %d)", start, start+n)
}

// waitForReady polls url+/v1/health until 2xx or timeout. Returns true on success.
func waitForReady(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for {
		if probeHTTPHealthy(url) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// emitWebResult prints the dashboard URL on stdout, in either human or
// JSON form. The base (root of the API) and dashboard URL are both
// returned in JSON mode so scripts can build their own paths if needed.
func emitWebResult(out io.Writer, base, dashboard string, pid int, source string, asJSON bool) {
	if asJSON {
		fmt.Fprintf(out, `{"url":%q,"base":%q,"pid":%d,"started_by":%q}`+"\n", dashboard, base, pid, source)
		return
	}
	switch source {
	case "existing":
		fmt.Fprintf(out, "pincherMCP HTTP server is running:\n  Dashboard: %s\n  API base:  %s\n  PID:       %d\n", dashboard, base, pid)
	case "started":
		fmt.Fprintf(out, "started pincherMCP HTTP server:\n  Dashboard: %s\n  API base:  %s\n  PID:       %d\n", dashboard, base, pid)
	default:
		fmt.Fprintln(out, dashboard)
	}
}

// pidIsAlive returns true when a process with the given PID exists.
//
// On Unix this means os.FindProcess + Signal(0) (no-op signal that surfaces
// "process does not exist" without disturbing the target). On Windows
// os.FindProcess returns success even for dead PIDs, so we Stat() the
// `pseudo-file` /proc-style — but Windows doesn't expose that. Instead
// the Windows path uses syscall.OpenProcess via os.FindProcess + a 0
// access right, which returns ERROR_INVALID_PARAMETER for dead PIDs.
//
// PID 0 means "no PID recorded" → treat as not alive.
func pidIsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Fall through to the platform-specific helper.
	return platformPIDAlive(pid)
}

// pidEnvOverride is honored by tests so they can short-circuit the
// platform helper without spinning up a real subprocess. Format:
// "pid1:1,pid2:0,..." where 1=alive, 0=dead. Empty = no override.
const pidEnvOverride = "PINCHER_TEST_PID_ALIVE"

func pidIsAliveTestable(pid int) bool {
	if v := os.Getenv(pidEnvOverride); v != "" {
		for _, kv := range strings.Split(v, ",") {
			parts := strings.SplitN(kv, ":", 2)
			if len(parts) != 2 {
				continue
			}
			if p, err := strconv.Atoi(parts[0]); err == nil && p == pid {
				return parts[1] == "1"
			}
		}
		return false
	}
	return pidIsAlive(pid)
}

// runtimeOSCheck is a placeholder hook so future test code can mock GOOS.
var _ = runtime.GOOS
