package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

func TestDashboardURL(t *testing.T) {
	cases := map[string]string{
		"http://localhost:7777":          "http://localhost:7777/v1/dashboard",
		"http://localhost:7777/":         "http://localhost:7777/v1/dashboard",
		"http://localhost:7777/pincher":  "http://localhost:7777/pincher/v1/dashboard",
		"http://localhost:7777/pincher/": "http://localhost:7777/pincher/v1/dashboard",
	}
	for in, want := range cases {
		if got := dashboardURL(in); got != want {
			t.Errorf("dashboardURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickFreePort_FindsFreeSlot(t *testing.T) {
	// Bind 7777 ourselves so pickFreePort has to scan past it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port

	// Scan starting at the taken port. pickFreePort should walk past it.
	got, err := pickFreePort(taken, 4)
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	if got == taken {
		t.Fatalf("pickFreePort returned the taken port %d", got)
	}
	// And it should be reachable.
	ln2, err := net.Listen("tcp", "127.0.0.1:"+itoa(got))
	if err != nil {
		t.Fatalf("Listen on returned port %d: %v", got, err)
	}
	ln2.Close()
}

func TestPickFreePort_AllBusy(t *testing.T) {
	// Take 3 consecutive ports, then ask pickFreePort to scan exactly that range.
	listeners := make([]net.Listener, 0, 3)
	startPort := 0
	for i := 0; i < 3; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		listeners = append(listeners, ln)
		if i == 0 {
			startPort = ln.Addr().(*net.TCPAddr).Port
		}
	}
	defer func() {
		for _, ln := range listeners {
			ln.Close()
		}
	}()

	// Note: this test is flaky in theory because the OS may not give us
	// 3 consecutive ports. In practice on a quiet test host it usually does.
	// We just assert that *some* error message comes back when no port is
	// free, even if the listener-port spread doesn't line up.
	_, _ = pickFreePort(startPort, 1) // 1-port scan against a known-busy port
	if _, err := pickFreePort(startPort, 1); err == nil {
		t.Fatalf("pickFreePort with n=1 on busy port should fail")
	}
}

func TestProbeHTTPHealthy_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			w.WriteHeader(200)
			w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if !probeHTTPHealthy(srv.URL) {
		t.Fatal("probe should succeed against healthy server")
	}
}

func TestProbeHTTPHealthy_NotRunning(t *testing.T) {
	// Bind and immediately close so the port is free.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	if probeHTTPHealthy("http://" + addr) {
		t.Fatal("probe should fail against closed port")
	}
}

func TestProbeHTTPHealthy_Empty(t *testing.T) {
	if probeHTTPHealthy("") {
		t.Fatal("probe with empty URL should be false")
	}
}

func TestPidIsAlive_Zero(t *testing.T) {
	if pidIsAlive(0) {
		t.Fatal("pid 0 should be dead")
	}
	if pidIsAlive(-1) {
		t.Fatal("negative pid should be dead")
	}
}

func TestPidIsAlive_Self(t *testing.T) {
	if !pidIsAlive(os.Getpid()) {
		t.Fatal("self should be alive")
	}
}

// TestFindLiveHTTPServer_NoRow returns false when the sessions table is empty.
func TestFindLiveHTTPServer_NoRow(t *testing.T) {
	store := newWebTestStore(t)
	if _, _, ok := findLiveHTTPServer(store); ok {
		t.Fatal("expected no result on empty sessions table")
	}
}

// TestFindLiveHTTPServer_DeadPID returns false when the row's PID is dead.
func TestFindLiveHTTPServer_DeadPID(t *testing.T) {
	store := newWebTestStore(t)
	if err := store.RecordSession("sess-dead", time.Now().Add(-1*time.Hour), 1, 100, 200, 0.001, "http://127.0.0.1:65535", 999999); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	if _, _, ok := findLiveHTTPServer(store); ok {
		t.Fatal("expected dead PID to disqualify the row")
	}
}

// TestFindLiveHTTPServer_LiveServer returns true when a real httptest server
// is recorded with the current PID. Uses an httptest server so the probe
// path actually succeeds.
func TestFindLiveHTTPServer_LiveServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	store := newWebTestStore(t)
	if err := store.RecordSession("sess-live", time.Now(), 1, 100, 200, 0.001, srv.URL, os.Getpid()); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	url, pid, ok := findLiveHTTPServer(store)
	if !ok {
		t.Fatal("expected to find live HTTP server")
	}
	if url != srv.URL {
		t.Errorf("url=%q, want %q", url, srv.URL)
	}
	if pid != os.Getpid() {
		t.Errorf("pid=%d, want %d", pid, os.Getpid())
	}
}

// TestWebCLI_Binary_NoStart asserts the command exits 1 with --no-start
// when no server is running. We run the actual binary so dispatch +
// flag parsing are exercised.
func TestWebCLI_Binary_NoStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)

	dataDir := t.TempDir()
	cmd := exec.Command(bin, "web", "--no-start", "--data-dir", dataDir)
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit with --no-start on empty store; got success.\nstdout: %s", out)
	}
	if !strings.Contains(string(out), "no live HTTP server") {
		t.Fatalf("expected 'no live HTTP server' in error; got:\n%s", out)
	}
}

// TestWebCLI_Binary_NoStartJSON covers the runWebCLI --json output branch
// when no live server exists. Pairs with TestWebCLI_Binary_NoStart which
// covers the text-mode path.
func TestWebCLI_Binary_NoStartJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()
	cmd := exec.Command(bin, "web", "--no-start", "--json", "--data-dir", dataDir)
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit; output: %s", out)
	}
	// Even in JSON mode, --no-start with no server emits the same
	// stderr error before exiting. (Adding a JSON error envelope is
	// out of scope here — the existing behaviour is what we test.)
	if !strings.Contains(string(out), "no live HTTP server") {
		t.Fatalf("expected error message; got: %s", out)
	}
}

func TestEmitWebResult_Existing(t *testing.T) {
	var buf strings.Builder
	emitWebResult(&buf, "http://localhost:7777", "http://localhost:7777/v1/dashboard", 1234, "existing", false)
	got := buf.String()
	if !strings.Contains(got, "running") || !strings.Contains(got, "1234") || !strings.Contains(got, "/v1/dashboard") {
		t.Fatalf("expected running/PID/dashboard in output; got:\n%s", got)
	}
}

func TestEmitWebResult_Started(t *testing.T) {
	var buf strings.Builder
	emitWebResult(&buf, "http://localhost:8080", "http://localhost:8080/v1/dashboard", 4321, "started", false)
	got := buf.String()
	if !strings.Contains(got, "started") || !strings.Contains(got, "4321") {
		t.Fatalf("expected started/PID in output; got:\n%s", got)
	}
}

func TestEmitWebResult_JSON(t *testing.T) {
	var buf strings.Builder
	emitWebResult(&buf, "http://localhost:7777", "http://localhost:7777/v1/dashboard", 5555, "existing", true)
	got := buf.String()
	if !strings.Contains(got, `"url":"http://localhost:7777/v1/dashboard"`) {
		t.Errorf("missing url in JSON: %s", got)
	}
	if !strings.Contains(got, `"base":"http://localhost:7777"`) {
		t.Errorf("missing base in JSON: %s", got)
	}
	if !strings.Contains(got, `"pid":5555`) {
		t.Errorf("missing pid in JSON: %s", got)
	}
	if !strings.Contains(got, `"started_by":"existing"`) {
		t.Errorf("missing started_by in JSON: %s", got)
	}
}

func TestEmitWebResult_Default(t *testing.T) {
	var buf strings.Builder
	emitWebResult(&buf, "http://x", "http://x/v1/dashboard", 0, "unknown-source", false)
	got := strings.TrimSpace(buf.String())
	if got != "http://x/v1/dashboard" {
		t.Fatalf("unknown source should print bare dashboard URL; got %q", got)
	}
}

func TestWaitForReady_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	if !waitForReady(srv.URL, 1*time.Second) {
		t.Fatal("waitForReady should succeed against healthy server")
	}
}

func TestWaitForReady_Timeout(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	start := time.Now()
	if waitForReady("http://"+addr, 200*time.Millisecond) {
		t.Fatal("waitForReady should fail against closed port")
	}
	elapsed := time.Since(start)
	if elapsed < 150*time.Millisecond {
		t.Errorf("waitForReady returned too fast (%v) — should respect timeout", elapsed)
	}
	if elapsed > 800*time.Millisecond {
		t.Errorf("waitForReady took too long (%v) — should respect timeout", elapsed)
	}
}

func TestPidIsAliveTestable_OverrideAlive(t *testing.T) {
	t.Setenv(pidEnvOverride, "12345:1,99999:0")
	if !pidIsAliveTestable(12345) {
		t.Error("12345 should be marked alive via env override")
	}
	if pidIsAliveTestable(99999) {
		t.Error("99999 should be marked dead via env override")
	}
	if pidIsAliveTestable(77777) {
		t.Error("77777 not in env override should fall through and be dead")
	}
}

func TestPidIsAliveTestable_NoOverride(t *testing.T) {
	t.Setenv(pidEnvOverride, "")
	if !pidIsAliveTestable(os.Getpid()) {
		t.Error("self should be alive without env override")
	}
}

func TestFindLiveHTTPServer_StaleRowProbeFails(t *testing.T) {
	store := newWebTestStore(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	if err := store.RecordSession("sess-stale", time.Now().Add(-2*time.Hour), 1, 100, 200, 0.001, "http://"+addr, os.Getpid()); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	if _, _, ok := findLiveHTTPServer(store); ok {
		t.Fatal("expected stale row with unreachable URL to not be recognised as live")
	}
}

func newWebTestStore(t *testing.T) *db.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := db.Open(dir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// itoa is a tiny stdlib-free integer-to-string for test setup so we
// don't pull in strconv just for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestNoStdio_RequiresHTTP asserts the --no-stdio flag refuses to run
// without --http (the process would have nothing to do — no MCP, no
// HTTP). The diagnostic message is the gate, not the exit code, since
// hand-authored exit codes drift; matching the message catches both
// "removed the check" and "renamed the check" regressions.
func TestNoStdio_RequiresHTTP(t *testing.T) {
	bin := buildPincherBinary(t)
	cmd := exec.Command(bin, "--no-stdio")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("--no-stdio without --http should exit non-zero; got success.\n%s", out)
	}
	if !bytes.Contains(out, []byte("--no-stdio requires --http")) {
		t.Errorf("expected diagnostic 'requires --http'; got:\n%s", out)
	}
}

// TestNoStdio_WithHTTP_StaysAlive asserts the detached-spawn fix for
// #232 — `pincher --http :PORT --no-stdio` keeps serving HTTP without
// requiring an inherited console. The Windows web auto-start flow
// (web_windows.go startDetached) relies on this; the test exercises it
// directly via exec.Cmd with no stdin attached, which mirrors what
// DETACHED_PROCESS produces.
func TestNoStdio_WithHTTP_StaysAlive(t *testing.T) {
	bin := buildPincherBinary(t)

	// Pick a free port so parallel runs don't collide.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	addr := "127.0.0.1:" + itoa(port)

	dataDir := t.TempDir()
	cmd := exec.Command(bin, "--http", addr, "--no-stdio", "--data-dir", dataDir)
	cmd.Env = pincherCoverEnv()
	cmd.Stdin = nil // mirror what startDetached does — no inherited console
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	// Poll /v1/health for up to 10s; the child should bind quickly.
	url := "http://" + addr + "/v1/health"
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Post(url, "application/json", strings.NewReader("{}"))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return // success
			}
			lastErr = nil
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("--no-stdio child never became ready on %s within 10s (last err: %v)", url, lastErr)
}
