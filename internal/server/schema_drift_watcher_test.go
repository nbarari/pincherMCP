package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// #1374: detectSchemaDrift is the testable kernel of
// StartSchemaDriftWatcher. Verifies the comparison logic without
// invoking os.Exit, which the live watcher reaches when stored >
// expected so supervised mode respawns.

// Negative — in-sync: DB schema_version == binary's CurrentSchemaVersion.
// No drift, no exit signal.
func TestDetectSchemaDrift_InSync(t *testing.T) {
	srv, store, _ := newTestServer(t)
	defer store.Close()
	var current int
	if err := store.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&current); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if srv.detectSchemaDrift(current) {
		t.Errorf("in-sync DB (schema_version=%d, expected=%d) reported as drifted", current, current)
	}
}

// Negative — binary AHEAD of DB: a freshly-upgraded binary opening
// an older DB (the canonical pre-migration state). NOT drift — the
// migrate guard handles forward migration at db.Open(); the watcher
// only catches the reverse case where DB advances under a running
// binary.
func TestDetectSchemaDrift_BinaryAheadOfDB(t *testing.T) {
	srv, store, _ := newTestServer(t)
	defer store.Close()
	var current int
	if err := store.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&current); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	// Pass an expected that's strictly greater than stored — simulates
	// a binary that knows about migrations the DB hasn't run yet.
	if srv.detectSchemaDrift(current + 5) {
		t.Errorf("binary ahead of DB (expected=%d, stored=%d) reported as drifted",
			current+5, current)
	}
}

// Positive — DB AHEAD of binary: drift detected. The reproduction is
// an out-of-process tool migrating the DB past what the running
// binary understands. Simulate by manually bumping schema_version
// then asking the helper if drift exists.
func TestDetectSchemaDrift_DBAheadOfBinary(t *testing.T) {
	srv, store, _ := newTestServer(t)
	defer store.Close()
	var current int
	if err := store.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&current); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	// Manually advance schema_version to simulate an out-of-process
	// migration. Use UPDATE not INSERT since schema_version is a
	// single-row table seeded at migrate().
	if _, err := store.DB().Exec(`UPDATE schema_version SET version = ?`, current+2); err != nil {
		t.Fatalf("advance schema_version: %v", err)
	}
	// Expected = the binary's compile-time head, which is `current`
	// (what migrate just landed at). DB is now 2 ahead.
	if !srv.detectSchemaDrift(current) {
		t.Errorf("drifted DB (expected=%d, stored=%d) NOT reported — supervised respawn would not fire",
			current, current+2)
	}
}

// Negative — read failure (corrupted DB / closed connection) is NOT
// drift. A transient failure must not trigger a respawn loop; the
// next poll retries.
func TestDetectSchemaDrift_ReadFailureNotDrift(t *testing.T) {
	srv, store, _ := newTestServer(t)
	store.Close() // close so any read errors with "database is closed"

	// detectSchemaDrift must return false on error, not panic or
	// return true.
	if srv.detectSchemaDrift(999) {
		t.Error("read failure incorrectly reported as drift — would trigger respawn loop")
	}
}

// Positive — goroutine integration: when the DB schema is bumped
// past the binary's CurrentSchemaVersion underneath a running
// watcher, the onDrift callback fires. Uses startSchemaDriftWatcher
// directly with an injectable callback so we exercise the goroutine
// loop without invoking os.Exit.
func TestStartSchemaDriftWatcher_FiresOnDrift(t *testing.T) {
	srv, store, _ := newTestServer(t)
	defer store.Close()

	// Bump the DB schema_version past what the binary understands.
	// CurrentSchemaVersion() returns len(schemaMigrations)+1; +5 is
	// safely "newer than this binary."
	var current int
	if err := store.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&current); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if _, err := store.DB().Exec(`UPDATE schema_version SET version = ?`, current+5); err != nil {
		t.Fatalf("advance schema_version: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fired atomic.Int32
	srv.startSchemaDriftWatcher(ctx, 10*time.Millisecond, func() {
		fired.Add(1)
	})

	// Watcher uses a 10ms ticker; wait up to 1s for the first tick
	// to fire the callback. Generous margin so a slow CI runner
	// doesn't spuriously fail.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if fired.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fired.Load() == 0 {
		t.Error("onDrift callback never fired despite DB schema drift")
	}
}

// Negative — ctx cancellation cleanly stops the goroutine without
// firing onDrift. Pins the lifecycle so a server shutdown doesn't
// trigger a spurious supervised respawn at the last second.
func TestStartSchemaDriftWatcher_CancelStopsGoroutine(t *testing.T) {
	srv, store, _ := newTestServer(t)
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var fired atomic.Int32
	srv.startSchemaDriftWatcher(ctx, 10*time.Millisecond, func() {
		fired.Add(1)
	})

	// No drift exists; cancel before the first tick.
	cancel()
	// Give the goroutine a tick or two to notice cancellation.
	time.Sleep(50 * time.Millisecond)

	if fired.Load() != 0 {
		t.Errorf("onDrift fired after ctx cancel: %d times", fired.Load())
	}
}
