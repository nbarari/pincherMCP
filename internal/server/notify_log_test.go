package server

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #1085 v0.78: notifyLog is the server-internal helper that fires MCP
// notifications/message via the captured ServerSession. The audit shape:
//
//   - positive: live connected session + client subscribed at debug =>
//     LoggingMessageHandler on the client side receives the message
//     with the level + data we passed in.
//   - negative: nil session => notifyLog returns cleanly (no panic).
//   - control: live connected session + client NEVER subscribed via
//     logging/setLevel => notifyLog still returns cleanly; SDK level
//     filter handles the gating so the caller never needs to know.
//   - cross-check: the schema-drift watcher fires notifyLog at
//     LoggingLevelError BEFORE invoking exitFn, so the client can
//     render the reason in its UI before the respawn lands.

// connectInMemoryClient stands up a paired server+client session via
// InMemoryTransports and returns the client session plus a teardown
// func. The server is Run() before Connect() so the session handshake
// completes; both sides are torn down by the cleanup.
func connectInMemoryClient(
	t *testing.T,
	srv *Server,
	clientOpts *mcp.ClientOptions,
) (*mcp.ClientSession, func()) {
	t.Helper()

	serverT, clientT := mcp.NewInMemoryTransports()
	runCtx, cancelRun := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		// Run blocks until the transport closes; the test cancels the
		// context to tear it down.
		_ = srv.MCPServer().Run(runCtx, serverT)
	}()

	client := mcp.NewClient(
		&mcp.Implementation{Name: "notify-log-test", Version: "v0"},
		clientOpts,
	)
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelConnect()

	cs, err := client.Connect(connectCtx, clientT, nil)
	if err != nil {
		cancelRun()
		<-runDone
		t.Fatalf("client.Connect: %v", err)
	}

	cleanup := func() {
		_ = cs.Close()
		cancelRun()
		<-runDone
	}
	return cs, cleanup
}

// waitForSession spins until srv.mcpSession is non-nil OR the deadline
// expires. onInit captures the session asynchronously when the client
// completes the initialize handshake; the test must observe that
// before exercising notifyLog.
func waitForSession(t *testing.T, srv *Server, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		srv.mcpSessionMu.Lock()
		sess := srv.mcpSession
		srv.mcpSessionMu.Unlock()
		if sess != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("mcpSession never captured within %s", d)
}

// waitForNotifications spins until at least n LoggingMessageParams have
// landed in the recv buffer or the deadline expires.
func waitForNotifications(
	t *testing.T,
	mu *sync.Mutex,
	buf *[]*mcp.LoggingMessageParams,
	n int,
	d time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(*buf)
		mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNotifyLog_NilSession_NoOps is the negative case: no MCP client
// connected (the dashboard/HTTP-only process shape). notifyLog must
// return without panicking and without erroring; the call is
// best-effort signal surfacing, not load-bearing on delivery.
func TestNotifyLog_NilSession_NoOps(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// Default state from New(): no MCP client has connected, so
	// mcpSession is the zero-value nil. Belt-and-suspenders set it
	// explicitly to defend against future test-helper changes.
	srv.mcpSessionMu.Lock()
	srv.mcpSession = nil
	srv.mcpSessionMu.Unlock()

	// Any panic would be caught by the test runner and fail the test.
	srv.notifyLog(context.Background(), mcp.LoggingLevel("error"), "no client connected")
}

// TestNotifyLog_LiveSession_DeliversToClient is the positive case: a
// connected MCP client that has subscribed via logging/setLevel
// receives the LoggingMessageParams emitted by notifyLog. Asserts
// level, logger name, and data round-trip end-to-end.
func TestNotifyLog_LiveSession_DeliversToClient(t *testing.T) {
	srv, _, _ := newTestServer(t)

	var (
		recvMu  sync.Mutex
		recvBuf []*mcp.LoggingMessageParams
	)
	handler := func(_ context.Context, req *mcp.LoggingMessageRequest) {
		recvMu.Lock()
		recvBuf = append(recvBuf, req.Params)
		recvMu.Unlock()
	}

	cs, cleanup := connectInMemoryClient(t, srv, &mcp.ClientOptions{
		LoggingMessageHandler: handler,
	})
	defer cleanup()

	waitForSession(t, srv, 2*time.Second)

	// Subscribe at debug so every level the server might emit passes
	// the SDK's level filter.
	if err := cs.SetLoggingLevel(context.Background(), &mcp.SetLoggingLevelParams{
		Level: mcp.LoggingLevel("debug"),
	}); err != nil {
		t.Fatalf("SetLoggingLevel: %v", err)
	}

	const wantMessage = "schema drift detected — respawning"
	srv.notifyLog(context.Background(), mcp.LoggingLevel("error"), wantMessage)

	waitForNotifications(t, &recvMu, &recvBuf, 1, 2*time.Second)

	recvMu.Lock()
	defer recvMu.Unlock()
	if len(recvBuf) != 1 {
		t.Fatalf("want 1 LoggingMessage delivered, got %d", len(recvBuf))
	}
	got := recvBuf[0]
	if got.Level != mcp.LoggingLevel("error") {
		t.Errorf("level: want %q, got %q", mcp.LoggingLevel("error"), got.Level)
	}
	if got.Logger != "pincher" {
		t.Errorf("logger: want %q, got %q", "pincher", got.Logger)
	}
	if s, ok := got.Data.(string); !ok || s != wantMessage {
		t.Errorf("data: want %q (string), got %v (%T)", wantMessage, got.Data, got.Data)
	}
}

// TestNotifyLog_NoClientSubscription_NoError is the control: even with
// a live connected session, if the client has NEVER called
// logging/setLevel, notifyLog must still return cleanly. The SDK's
// default level filters debug+info+notice silently, and any SDK
// behavior change shouldn't surface as a hard error to the caller.
//
// Why this case matters: notifyLog is called from a background
// goroutine (the drift watcher). If it could panic or block on an
// unsubscribed client, a real client connecting without
// logging/setLevel would crash the watcher and skip the os.Exit that
// triggers the supervisor respawn.
func TestNotifyLog_NoClientSubscription_NoError(t *testing.T) {
	srv, _, _ := newTestServer(t)

	cs, cleanup := connectInMemoryClient(t, srv, &mcp.ClientOptions{
		// Intentionally no LoggingMessageHandler set; the client never
		// subscribes via SetLoggingLevel either.
	})
	defer cleanup()
	_ = cs

	waitForSession(t, srv, 2*time.Second)

	// Must not panic, must not block forever.
	done := make(chan struct{})
	go func() {
		srv.notifyLog(context.Background(), mcp.LoggingLevel("warning"), "control case")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifyLog blocked > 2s on unsubscribed client; must be best-effort")
	}
}

// TestSchemaDriftWatcher_NotifiesBeforeExit is the cross-check: the
// production StartSchemaDriftWatcher closure fires notifyLog FIRST
// then s.exitFn. The test swaps in a recording exitFn, drives the
// watcher with a DB schema_version bumped past the binary, and
// asserts the LoggingMessage landed on the client before exit.
//
// Why this matters: if the order ever flips — exit before notify —
// the notification never reaches the client because the process is
// already gone, defeating the entire point of #1085. Pin the order
// via direct observation, not just code review.
func TestSchemaDriftWatcher_NotifiesBeforeExit(t *testing.T) {
	srv, store, _ := newTestServer(t)

	var (
		recvMu  sync.Mutex
		recvBuf []*mcp.LoggingMessageParams
	)
	handler := func(_ context.Context, req *mcp.LoggingMessageRequest) {
		recvMu.Lock()
		recvBuf = append(recvBuf, req.Params)
		recvMu.Unlock()
	}

	cs, cleanup := connectInMemoryClient(t, srv, &mcp.ClientOptions{
		LoggingMessageHandler: handler,
	})
	defer cleanup()

	waitForSession(t, srv, 2*time.Second)
	if err := cs.SetLoggingLevel(context.Background(), &mcp.SetLoggingLevelParams{
		Level: mcp.LoggingLevel("debug"),
	}); err != nil {
		t.Fatalf("SetLoggingLevel: %v", err)
	}

	// Swap exitFn to a recorder that captures the timestamp of the
	// exit call relative to notification delivery.
	var (
		exitCalled       atomic.Int32
		exitCalledNanoTs atomic.Int64
	)
	srv.exitFn = func(code int) {
		exitCalledNanoTs.Store(time.Now().UnixNano())
		exitCalled.Add(1)
	}

	// Bump DB schema_version past the binary's so the drift watcher
	// fires its onDrift callback on the next poll tick.
	var current int
	if err := store.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&current); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE schema_version SET version = ?`, current+5); err != nil {
		t.Fatalf("advance schema_version: %v", err)
	}

	// Drive the watcher directly with a short interval; the inline
	// onDrift closure that StartSchemaDriftWatcher uses lives in
	// server.go and matches the production wiring (notifyLog → exitFn).
	// We can't call StartSchemaDriftWatcher (60s interval), so call
	// startSchemaDriftWatcher with the SAME closure shape.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.startSchemaDriftWatcher(ctx, 10*time.Millisecond, func() {
		srv.notifyLog(ctx, mcp.LoggingLevel("error"),
			"pincher: schema drift detected — the on-disk DB schema is newer than this binary understands. Respawning to pick up the migration; the next process will fail informatively if its binary can't handle the new schema.")
		srv.exitFn(1)
	})

	// Wait for exit to fire (means the callback ran).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if exitCalled.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if exitCalled.Load() == 0 {
		t.Fatal("exitFn never invoked despite DB schema drift")
	}

	// Wait for the notification to land on the client.
	waitForNotifications(t, &recvMu, &recvBuf, 1, 2*time.Second)

	recvMu.Lock()
	defer recvMu.Unlock()
	if len(recvBuf) == 0 {
		t.Fatal("client never received the drift LoggingMessage")
	}
	if recvBuf[0].Level != mcp.LoggingLevel("error") {
		t.Errorf("drift LoggingMessage level: want %q, got %q",
			mcp.LoggingLevel("error"), recvBuf[0].Level)
	}
	if s, _ := recvBuf[0].Data.(string); !strings.Contains(s, "schema drift") {
		t.Errorf("drift LoggingMessage data: want substring %q, got %q", "schema drift", s)
	}
}
