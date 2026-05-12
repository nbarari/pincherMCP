package db

import (
	"context"
	"testing"
)

// #613: CurrentSchemaVersion was 0% covered. The single-line returner
// is load-bearing — `pincher list` and `pincher doctor` use it to
// detect cross-binary drift (#236). Two regressions the test catches:
//
//  1. Off-by-one: someone changes `+1` to `+0` thinking they're cleaning
//     up dead code (the slice is 0-indexed but versions start at 1, so
//     `+1` is essential). Without this test, `list` and `doctor` would
//     under-report the current version forever.
//
//  2. Drift between the inline migration list and a freshly-opened DB.
//     The schema_version row written by Open() must equal what
//     CurrentSchemaVersion() reports — they're two independent paths to
//     the same number and a divergence means migration application skipped.

func TestCurrentSchemaVersion_PinsToMigrationsLength(t *testing.T) {
	got := CurrentSchemaVersion()
	want := len(schemaMigrations) + 1
	if got != want {
		t.Errorf("CurrentSchemaVersion() = %d, want len(schemaMigrations)+1 = %d", got, want)
	}
	// Sanity: can't be < 1 (initial schema is v1 by construction).
	if got < 1 {
		t.Errorf("CurrentSchemaVersion() = %d, must be >= 1", got)
	}
}

func TestCurrentSchemaVersion_MatchesFreshDBVersionRow(t *testing.T) {
	// A freshly-opened DB has every migration applied; the schema_version
	// row should equal CurrentSchemaVersion(). If migrate() bails before
	// writing the row OR CurrentSchemaVersion is wrong, this catches it.
	store := newTestStore(t)

	var dbVersion int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT version FROM schema_version`).Scan(&dbVersion); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}

	want := CurrentSchemaVersion()
	if dbVersion != want {
		t.Errorf("schema_version row = %d, CurrentSchemaVersion() = %d — divergence indicates an unapplied migration",
			dbVersion, want)
	}
}
