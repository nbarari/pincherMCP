package server

import (
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// Tests for the #240 per-language call attribution surface:
//   - snapshotCallsByLanguage (in-memory → JSON serialization)
//   - recordCallLanguage (atomic increment under concurrent calls)
//   - languageRE (regex matching against marshalled tool payloads)
//   - end-to-end: record → flushSession → DB read aggregates correctly

// snapshotCallsByLanguage with no recorded calls must return "" so the
// sessions row stores SQL NULL — pre-v15 rows then render distinct from
// "v15 row, no language data yet". An empty `{}` would lose that signal.
func TestSnapshotCallsByLanguage_EmptyReturnsEmptyString(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if got := srv.snapshotCallsByLanguage(); got != "" {
		t.Errorf("snapshot on empty server = %q, want \"\"", got)
	}
}

// Recorded languages serialize to a stable-sorted JSON object — sort
// order matters for snapshot test reproducibility (#33).
func TestSnapshotCallsByLanguage_StableSortedJSON(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// Insert in non-alphabetical order; expect sorted output.
	srv.recordCallLanguage("Python")
	srv.recordCallLanguage("Go")
	srv.recordCallLanguage("Go")
	srv.recordCallLanguage("Markdown")

	got := srv.snapshotCallsByLanguage()
	want := `{"Go":2,"Markdown":1,"Python":1}`
	if got != want {
		t.Errorf("snapshot = %q, want %q", got, want)
	}
}

// recordCallLanguage("") must be a no-op so callers don't have to gate
// the language extraction themselves. A nil-or-empty regex match would
// otherwise pollute the map with an empty-string key.
func TestRecordCallLanguage_EmptyIsNoOp(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.recordCallLanguage("")
	srv.recordCallLanguage("")

	if got := srv.snapshotCallsByLanguage(); got != "" {
		t.Errorf("after no-op records, snapshot = %q, want \"\"", got)
	}
}

// recordCallLanguage must be safe under concurrent goroutines — every
// tool call increments via this path with no caller-side locking. Pin
// the increment correctness so a future refactor that drops sync.Map
// or atomic surfaces the regression as a flaky count.
func TestRecordCallLanguage_AtomicUnderConcurrentCalls(t *testing.T) {
	srv, _, _ := newTestServer(t)

	const goroutines = 50
	const incrementsPerGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < incrementsPerGoroutine; j++ {
				srv.recordCallLanguage("Go")
			}
		}()
	}
	wg.Wait()

	var got map[string]int64
	if err := json.Unmarshal([]byte(srv.snapshotCallsByLanguage()), &got); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v", err)
	}
	want := int64(goroutines * incrementsPerGoroutine)
	if got["Go"] != want {
		t.Errorf("Go count = %d, want %d (lost increments under concurrency)", got["Go"], want)
	}
}

// languageRE pulls the FIRST `"language":"X"` occurrence from a
// marshalled response payload. Verify against representative shapes
// the real tools emit so a future regex tweak that loses one of these
// cases surfaces in CI.
func TestLanguageRE_MatchShapes(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string // empty = no match expected
	}{
		{"flat-top-level", `{"language":"Go","kind":"Function"}`, "Go"},
		{"with-spaces", `{"language" : "Markdown"}`, "Markdown"},
		{"nested-in-result-array", `{"results":[{"language":"Python","name":"foo"}]}`, "Python"},
		{"first-of-many-wins", `{"results":[{"language":"Go"},{"language":"YAML"}]}`, "Go"},
		{"multi-line-pretty-printed", "{\n  \"language\": \"Rust\"\n}", "Rust"},
		{"absent", `{"name":"foo","kind":"Function"}`, ""},
		{"present-as-key-only-no-value", `{"language":""}`, ""}, // empty value is "no signal"; recordCallLanguage gates it
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := languageRE.FindStringSubmatch(tc.input)
			var got string
			if len(m) > 1 {
				got = m[1]
			}
			if got != tc.want {
				t.Errorf("input=%q got=%q want=%q", tc.input, got, tc.want)
			}
		})
	}
}

// End-to-end: incrementing the in-memory counter, flipping mcpConnected
// so flushSession persists, then reading via the new db aggregator must
// yield the recorded counts. This is the load-bearing path for the
// #240 diagnostic — bypass-detection downstream depends on it surviving
// the round-trip.
func TestRecordAndFlush_AggregatesViaDB(t *testing.T) {
	srv, store, _ := newTestServer(t)

	// Simulate "an MCP client connected" so flushSession actually writes.
	atomic.StoreInt32(&srv.mcpConnected, 1)

	// Three Go calls, two Markdown calls.
	for i := 0; i < 3; i++ {
		srv.recordCallLanguage("Go")
	}
	srv.recordCallLanguage("Markdown")
	srv.recordCallLanguage("Markdown")

	// flushSession short-circuits when statsCalls == 0; bump it so the
	// row gets written. This mirrors what jsonResultWithMeta does on a
	// real tool call.
	atomic.AddInt64(&srv.statsCalls, 5)

	srv.flushSession()

	got, err := store.GetAllTimeCallsByLanguage()
	if err != nil {
		t.Fatalf("GetAllTimeCallsByLanguage: %v", err)
	}
	want := map[string]int64{"Go": 3, "Markdown": 2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("post-flush aggregate:\n  got  %v\n  want %v", got, want)
	}
}
