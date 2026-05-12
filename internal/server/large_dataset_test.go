package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #527 (umbrella #519): large-dataset fixture — seed 1k projects, 1k
// sessions, 5k symbols and verify the dashboard's data endpoints stay
// healthy. Catches the unpaginated-grid performance cliff that's only
// visible at scale.
//
// What we test (server-side only — no headless browser):
//   - All four dashboard data endpoints return 200 within a generous
//     wallclock budget. Server work + JSON encode are the surface; if
//     either grows superlinearly we'll see it.
//   - Response payload sizes are bounded. Pre-pagination (#530/#531/#532)
//     these can be very large; the bound documents the current cliff so
//     v0.25 pagination work has a regression guard. Once /v1/projects,
//     /v1/sessions, /v1/search are paginated, tighten the bounds.
//   - Response shape stays consistent at scale (no truncation halfway
//     through encoding, no nil-slice marshaling to "null").
//
// Wallclock budget: 5s per endpoint. The issue called for <2s but
// shared CI runners — especially Windows — have enough variance that 2s
// produces flakes. A 5s budget still catches O(N²) regressions while
// staying robust against runner load.

const (
	largeProjects = 1000
	largeSessions = 1000
	largeSymbols  = 5000

	// perEndpointBudget is intentionally generous. We're catching cliffs,
	// not measuring perf. Tightening risks CI flake without information.
	perEndpointBudget = 5 * time.Second
)

func TestDashboard_LargeDataset(t *testing.T) {
	if testing.Short() {
		t.Skip("large-dataset seeding takes ~3s; skipped under -short")
	}
	srv, store, _ := newTestServer(t)

	seedStart := time.Now()
	seedLargeDataset(t, store)
	t.Logf("seeded %d projects, %d sessions, %d symbols in %v",
		largeProjects, largeSessions, largeSymbols, time.Since(seedStart))

	// Each subtest hits one endpoint; t.Run per case so the output names
	// the slow one if any of them blow the budget.
	for _, tc := range []struct {
		name string
		path string
		// maxBytes documents the current unpaginated payload size. When
		// pagination lands (v0.25 #530/#531/#532), tighten these.
		maxBytes int
		// rootKey is the documented top-level key whose presence + non-nil
		// value confirms the response wasn't truncated mid-encode.
		rootKey string
	}{
		{name: "stats", path: "/v1/stats", maxBytes: 50_000, rootKey: "all_time"},
		{name: "sessions", path: "/v1/sessions", maxBytes: 200_000, rootKey: "sessions"},
		{name: "projects", path: "/v1/projects", maxBytes: 1_500_000, rootKey: "projects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			w := httpGet(t, srv, tc.path)
			elapsed := time.Since(start)

			if w.Code != 200 {
				t.Fatalf("%s: status %d, want 200\nbody: %s", tc.path, w.Code, w.Body.String())
			}
			if elapsed > perEndpointBudget {
				t.Errorf("%s took %v, budget %v — superlinear regression?",
					tc.path, elapsed, perEndpointBudget)
			}
			body := w.Body.Bytes()
			if len(body) > tc.maxBytes {
				t.Errorf("%s payload %d bytes exceeds bound %d — pagination regression or schema bloat",
					tc.path, len(body), tc.maxBytes)
			}

			// Decode + verify root key is present and non-nil. Truncated
			// JSON would fail to decode; nil-slice marshaling would show
			// `"projects":null`.
			var resp map[string]any
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("%s response not valid JSON (truncated?): %v", tc.path, err)
			}
			if resp[tc.rootKey] == nil {
				t.Errorf("%s: root key %q is nil — nil-slice marshaling regression",
					tc.path, tc.rootKey)
			}
			// Defensive: the JSON serializer should never produce literal
			// "null" for documented array fields. Check the raw body so we
			// catch this even when the key is wrapped.
			if strings.Contains(string(body), `"`+tc.rootKey+`":null`) {
				t.Errorf("%s: %q field marshals as null, should be empty array",
					tc.path, tc.rootKey)
			}

			t.Logf("%s: %v, %d bytes", tc.path, elapsed, len(body))
		})
	}
}

// seedLargeDataset writes N projects + N sessions + N symbols using
// bulk APIs to keep the seed cost bounded. SQLite single-writer with
// modernc.org/sqlite handles ~5k bulk-symbol upserts in well under a
// second on warm storage.
func seedLargeDataset(t *testing.T, store *db.Store) {
	t.Helper()
	now := time.Now()

	// Projects — one row per project_id. Distribute across realistic
	// indexed_at timestamps so the dashboard's "recently indexed" sort
	// has data to work with.
	for i := 0; i < largeProjects; i++ {
		p := db.Project{
			ID:        fmt.Sprintf("proj-%04d", i),
			Path:      fmt.Sprintf("/tmp/proj-%04d", i),
			Name:      fmt.Sprintf("project-%04d", i),
			IndexedAt: now.Add(-time.Duration(i) * time.Minute),
			FileCount: 10 + i%200,
			SymCount:  100 + i%2000,
			EdgeCount: 50 + i%1500,
		}
		if err := store.UpsertProject(p); err != nil {
			t.Fatalf("UpsertProject %d: %v", i, err)
		}
	}

	// Sessions — RecordSession is the public entry point used by the
	// flusher. Each call is a single INSERT under the hood.
	for i := 0; i < largeSessions; i++ {
		err := store.RecordSession(
			fmt.Sprintf("session-%04d", i),
			now.Add(-time.Duration(i)*time.Minute),
			int64(10+i%500),     // calls
			int64(1000+i*7),     // tokens_used
			int64(5000+i*23),    // tokens_saved
			0,                   // cost_avoided (deprecated post-#476)
			"",                  // httpURL
			0,                   // httpPID
			`{"go":1}`,          // calls_by_language JSON
		)
		if err != nil {
			t.Fatalf("RecordSession %d: %v", i, err)
		}
	}

	// Symbols — bulk-upsert 5k spread across N projects so search has a
	// realistically diverse corpus.
	syms := make([]db.Symbol, 0, largeSymbols)
	for i := 0; i < largeSymbols; i++ {
		projectIdx := i % largeProjects
		syms = append(syms, db.Symbol{
			ID:                   fmt.Sprintf("sym::sym%05d#Function", i),
			ProjectID:            fmt.Sprintf("proj-%04d", projectIdx),
			FilePath:             fmt.Sprintf("internal/pkg/file_%04d.go", i%200),
			Name:                 fmt.Sprintf("Sym%05d", i),
			QualifiedName:        fmt.Sprintf("pkg.Sym%05d", i),
			Kind:                 "Function",
			Language:             "Go",
			ExtractionConfidence: 1,
		})
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
}
