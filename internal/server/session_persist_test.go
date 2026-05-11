package server

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #420: with PINCHER_SESSION_ID set, the server reads prior session
// totals from the sessions table on startup and seeds in-memory
// atomics. Counter values survive a supervised respawn instead of
// resetting to zero on every binary swap.

func TestPickPersistentSessionID_RespectsEnv(t *testing.T) {
	t.Setenv("PINCHER_SESSION_ID", "sup-fixed-123")
	got := pickPersistentSessionID(time.Now())
	if got != "sup-fixed-123" {
		t.Errorf("pickPersistentSessionID with env = %q, want sup-fixed-123", got)
	}
}

func TestPickPersistentSessionID_FallsBackToTimestamp(t *testing.T) {
	t.Setenv("PINCHER_SESSION_ID", "")
	got := pickPersistentSessionID(time.Unix(1700000000, 0))
	want := "sess-1700000000000000000"
	if got != want {
		t.Errorf("pickPersistentSessionID fallback = %q, want %q", got, want)
	}
}

func TestServerStartup_SeedsCountersFromPriorSession(t *testing.T) {
	// Build a store + pre-populate a sessions row for the fixed session ID.
	tmpDir := t.TempDir()
	store, err := db.Open(tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	const sessionID = "sup-respawn-test"
	started := time.Unix(1700000000, 0)
	qm := db.QueryMetrics{
		QueriesTotal:            7,
		QueriesZeroResult:       2,
		QueriesRetriedSucceeded: 1,
		TokensBurnedOnFailures:  450,
	}
	if err := store.RecordSessionWithMetrics(
		sessionID, started, 42, 12345, 67890, 0.06, "", 0, `{"Go":40,"Markdown":2}`, qm,
	); err != nil {
		t.Fatalf("RecordSessionWithMetrics: %v", err)
	}

	// Spin up the server with PINCHER_SESSION_ID pointing at that row.
	t.Setenv("PINCHER_SESSION_ID", sessionID)
	srv := New(store, nil, "v0.16.0-test")

	if got := atomic.LoadInt64(&srv.statsCalls); got != 42 {
		t.Errorf("statsCalls = %d, want 42 (seeded from sessions row)", got)
	}
	if got := atomic.LoadInt64(&srv.statsTokensUsed); got != 12345 {
		t.Errorf("statsTokensUsed = %d, want 12345", got)
	}
	if got := atomic.LoadInt64(&srv.statsTokensSaved); got != 67890 {
		t.Errorf("statsTokensSaved = %d, want 67890", got)
	}
	if got := atomic.LoadInt64(&srv.statsQueriesTotal); got != 7 {
		t.Errorf("statsQueriesTotal = %d, want 7", got)
	}
	if got := atomic.LoadInt64(&srv.statsTokensBurned); got != 450 {
		t.Errorf("statsTokensBurned = %d, want 450", got)
	}
	if !srv.sessionStartedAt.Equal(started) {
		t.Errorf("sessionStartedAt = %v, want %v (preserved across respawn)", srv.sessionStartedAt, started)
	}
	v, _ := srv.statsCallsByLanguage.Load("Go")
	if ptr, ok := v.(*int64); !ok || ptr == nil || atomic.LoadInt64(ptr) != 40 {
		t.Errorf("statsCallsByLanguage[Go] not seeded; got %v", v)
	}
}

func TestServerStartup_NoSessionEnvDoesNotSeed(t *testing.T) {
	// Without the env var, the server picks a fresh per-process ID and
	// must NOT inherit some random other session's counters.
	tmpDir := t.TempDir()
	store, err := db.Open(tmpDir)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer store.Close()

	// Pre-existing row under a different ID — should be ignored.
	_ = store.RecordSessionWithMetrics(
		"sup-other", time.Now(), 99, 99999, 99999, 0.99, "", 0, "", db.QueryMetrics{},
	)

	os.Unsetenv("PINCHER_SESSION_ID")
	srv := New(store, nil, "v0.16.0-test")

	if got := atomic.LoadInt64(&srv.statsCalls); got != 0 {
		t.Errorf("statsCalls = %d, want 0 (no env, no seed)", got)
	}
}
