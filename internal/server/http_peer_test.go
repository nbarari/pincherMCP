package server

import (
	"os"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// TestHasHTTPPeer_NoOtherProcess pins #204: an empty sessions table
// (or one with only my own PID's rows) reports no peer. Without this
// guard the flusher would oscillate to fast cadence on the first
// flush of a single-process run.
func TestHasHTTPPeer_NoOtherProcess(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if srv.hasHTTPPeer() {
		t.Error("hasHTTPPeer should return false on an empty sessions table")
	}
}

// TestHasHTTPPeer_FreshPeerDetected pins the positive case: another
// PID has flushed an http_url row within the staleness window, so we
// detect the peer and the flusher should accelerate.
func TestHasHTTPPeer_FreshPeerDetected(t *testing.T) {
	srv, store, _ := newTestServer(t)

	// Simulate another pincher process: write a session row with a
	// PID that isn't ours and a recent last_seen.
	otherPID := os.Getpid() + 1
	if err := store.RecordSession("peer-session", time.Now().Add(-2*time.Second), 1, 100, 200, 0.05,
		"http://localhost:7777", otherPID, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	if !srv.hasHTTPPeer() {
		t.Error("hasHTTPPeer should detect a fresh row from another PID")
	}
}

// TestHasHTTPPeer_StalePeerIgnored pins the staleness gate: a row
// from another PID older than httpPeerStaleAfter is ignored. Without
// this, a long-dead HTTP process would strand the flusher at fast
// cadence.
func TestHasHTTPPeer_StalePeerIgnored(t *testing.T) {
	srv, store, _ := newTestServer(t)

	// last_seen is set to time.Now() at write time inside RecordSession,
	// so we can't simulate a stale row through the public API. Drop
	// directly into the DB.
	otherPID := os.Getpid() + 1
	staleSecs := time.Now().Add(-(httpPeerStaleAfter + 5*time.Second)).Unix()
	if _, err := store.DB().Exec(
		`INSERT INTO sessions (session_id, started_at, last_seen, calls, tokens_used, tokens_saved, cost_avoided, http_url, http_pid)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"stale-peer", staleSecs, staleSecs, 1, 100, 200, 0.05, "http://localhost:7777", otherPID,
	); err != nil {
		t.Fatalf("insert stale row: %v", err)
	}

	if srv.hasHTTPPeer() {
		t.Error("hasHTTPPeer should ignore a row whose last_seen is older than httpPeerStaleAfter")
	}
}

// TestHasHTTPPeer_MyOwnPIDIgnored pins the self-skip: we don't want
// to fast-track the flusher because of our own row. (The MCP-stdio +
// HTTP same-process case writes its own http_url; flusher should
// stay slow because there's no peer to accelerate for.)
func TestHasHTTPPeer_MyOwnPIDIgnored(t *testing.T) {
	srv, store, _ := newTestServer(t)

	// Write a row with our own PID.
	if err := store.RecordSession("self-session", time.Now(), 1, 100, 200, 0.05,
		"http://localhost:7777", os.Getpid(), ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	if srv.hasHTTPPeer() {
		t.Error("hasHTTPPeer should skip rows with our own PID — they're not a peer")
	}
}

// TestHasHTTPPeer_NoHTTPURLIgnored pins a corner case: a stdio-only
// process flushes session rows with empty http_url. Those are not a
// dashboard signal, so they shouldn't trigger acceleration.
func TestHasHTTPPeer_NoHTTPURLIgnored(t *testing.T) {
	srv, store, _ := newTestServer(t)

	otherPID := os.Getpid() + 1
	if err := store.RecordSession("stdio-only", time.Now(), 1, 100, 200, 0.05,
		"", otherPID, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	if srv.hasHTTPPeer() {
		t.Error("hasHTTPPeer should ignore rows with empty http_url — they're not a dashboard")
	}
}

// TestHasHTTPPeer_NotificationCheck — a stub regression test that
// hits the query path against a real schema. Confirms the SQL parses
// and no-rows path is wired correctly.
func TestHasHTTPPeer_NotificationCheck(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	srv := New(store, nil, "test")

	// Empty DB: should not crash, should return false.
	if srv.hasHTTPPeer() {
		t.Error("hasHTTPPeer on empty DB should return false")
	}
}
