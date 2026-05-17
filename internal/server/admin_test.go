package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #558 phase 2: doctor / rebuild_fts / self_test as MCP tools, exposed
// via the dynamic /v1/<tool> HTTP dispatcher. These tests cover the
// JSON-shape contracts; HTTP wire-up is covered by the parity test.

func TestHandleDoctor_HealthyEmptyDB(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	srv.version = "0.21.0-test"

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)

	for _, k := range []string{
		"generated_at", "binary_version", "lookback_hours",
		"schema_version", "db_size_bytes", "wal_size_bytes",
		"projects", "extraction_failures", "slow_queries", "advisories",
	} {
		if _, ok := body[k]; !ok {
			t.Errorf("doctor response missing field %q", k)
		}
	}
	if got := body["binary_version"]; got != "0.21.0-test" {
		t.Errorf("binary_version: got %v want 0.21.0-test", got)
	}
	// Empty DB → empty slices, never nil. (#328 invariant). advisories
	// is #732 — a healthy (tiny) test DB is well under the 1 GiB
	// threshold, so it must come back as [] not nil and not populated.
	for _, k := range []string{"projects", "extraction_failures", "slow_queries", "advisories"} {
		if v, ok := body[k].([]any); !ok || v == nil {
			t.Errorf("%s should be [] not nil; got %T %v", k, body[k], body[k])
		}
	}
	if adv, _ := body["advisories"].([]any); len(adv) != 0 {
		t.Errorf("healthy test DB should produce no advisories; got %v", adv)
	}
}

// #732: largeDBAdvisory turns a bare db_size_bytes number into an
// actionable health warning when the store is pathologically large.
func TestLargeDBAdvisory(t *testing.T) {
	t.Parallel()

	// Under the 1 GiB threshold → no advisory.
	if got := largeDBAdvisory(500<<20, nil); got != "" {
		t.Errorf("500 MB DB should produce no advisory; got %q", got)
	}
	if got := largeDBAdvisory(1<<30, nil); got != "" {
		t.Errorf("exactly 1 GiB should not trip the advisory (threshold is strictly greater); got %q", got)
	}

	// Over the threshold → advisory that names the heaviest project and
	// gives concrete remediation.
	projects := []doctorProjectSummary{
		{Name: "warp_rc", Symbols: 1453923, Files: 4363},
		{Name: "pincher-repo", Symbols: 5169, Files: 394},
	}
	got := largeDBAdvisory(5<<30, projects)
	if got == "" {
		t.Fatal("5 GB DB should produce an advisory")
	}
	for _, want := range []string{"5.0 GB", "warp_rc", "1453923", "prune_dead", "prune-stale", "vacuum"} {
		if !strings.Contains(got, want) {
			t.Errorf("advisory missing %q\n  got: %s", want, got)
		}
	}

	// Over the threshold but no project list → still advises, just
	// without the heaviest-project detail.
	bare := largeDBAdvisory(2<<30, nil)
	if bare == "" || strings.Contains(bare, "Heaviest project") {
		t.Errorf("empty project list should still advise, without the heaviest-project clause; got %q", bare)
	}
}

// #635 v0.67 follow-up: payload-outlier advisory. Tests pin the two
// gating conditions (ratio + absolute max) so a future tweak doesn't
// silently neuter the advisory by relaxing one but not the other.
func TestPayloadOutlierAdvisory(t *testing.T) {
	t.Parallel()

	// Empty input → silent.
	if got := payloadOutlierAdvisory(nil); got != "" {
		t.Errorf("nil rows should produce no advisory; got %q", got)
	}
	// All rows below ratio threshold → silent even if some are large.
	rows := []db.ToolCallPayloadRow{
		{Tool: "search", AvgBytes: 80_000, MaxBytes: 160_000}, // ratio 2× — below 10× cutoff
	}
	if got := payloadOutlierAdvisory(rows); got != "" {
		t.Errorf("2× spread should not trip; got %q", got)
	}
	// All rows below absolute-max threshold → silent even with big ratio.
	rows = []db.ToolCallPayloadRow{
		{Tool: "search", AvgBytes: 1_000, MaxBytes: 50_000}, // ratio 50× but max < 100 KB
	}
	if got := payloadOutlierAdvisory(rows); got != "" {
		t.Errorf("max <100 KB should not trip; got %q", got)
	}
	// Both conditions crossed → advisory names the tool, prints
	// spread × ratio, and includes remediation pointer.
	rows = []db.ToolCallPayloadRow{
		{Tool: "guide", AvgBytes: 5_000, MaxBytes: 250_000}, // ratio 50×, max 250 KB
		{Tool: "search", AvgBytes: 500, MaxBytes: 25_000},   // ratio 50× but max < 100 KB
	}
	got := payloadOutlierAdvisory(rows)
	if got == "" {
		t.Fatal("guide max=250KB / avg=5KB should produce an advisory")
	}
	for _, want := range []string{"guide", "spread", "Payload outliers", "/v1/tool-payload-stats", "min_confidence"} {
		if !strings.Contains(got, want) {
			t.Errorf("advisory missing %q\n  got: %s", want, got)
		}
	}
	// search should NOT appear — it failed the max threshold.
	if strings.Contains(got, "search") {
		t.Errorf("advisory must skip rows below the max threshold; got: %s", got)
	}
}

// #575: pre-fix the handler iterated every project and pulled `top`
// failures per project, so a 125-project install ballooned the
// response past the MCP token cap. `top` now caps the projects
// list AND the global failure list; truncation surfaces in
// `projects_truncated` / `extraction_failures_truncated` so the
// caller knows.
func TestHandleDoctor_CapsProjectsAndFailuresGlobally(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	// Seed 50 projects to repro the multi-project bloat shape.
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("p%02d", i)
		store.UpsertProject(db.Project{
			ID: id, Path: "/tmp/" + id, Name: id,
			IndexedAt: time.Now(),
			SymCount:  i, // sort by symbols desc → p49 first
		})
	}

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{"top": 5}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)

	projects, _ := body["projects"].([]any)
	if len(projects) != 5 {
		t.Errorf("expected 5 projects (capped by top=5), got %d", len(projects))
	}
	if truncated, ok := body["projects_truncated"].(float64); !ok || truncated != 45 {
		t.Errorf("expected projects_truncated=45, got %v", body["projects_truncated"])
	}
	// Sorted by symbols desc → top should be p49.
	if len(projects) > 0 {
		first := projects[0].(map[string]any)
		if first["name"] != "p49" {
			t.Errorf("expected first project p49 (largest), got %v", first["name"])
		}
	}
}

func TestHandleDoctor_WithProject(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{
		ID: "p1", Path: "/tmp/p1", Name: "p1",
		IndexedAt: time.Now(), BinaryVersion: "0.21.0",
		FileCount: 3, SymCount: 42, EdgeCount: 17,
	})

	result, err := srv.handleDoctor(context.Background(), makeReq(map[string]any{"top": 5}))
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	body := decode(t, result)
	projects, ok := body["projects"].([]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("expected 1 project, got %v", body["projects"])
	}
	p := projects[0].(map[string]any)
	if p["name"] != "p1" || p["symbols"].(float64) != 42 {
		t.Errorf("project shape wrong: %v", p)
	}
}

func TestHandleRebuildFTS_DryRunByDefault(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	result, err := srv.handleRebuildFTS(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleRebuildFTS: %v", err)
	}
	body := decode(t, result)
	if body["dry_run"] != true {
		t.Errorf("default call must be dry_run=true; got %v", body["dry_run"])
	}
	if _, ok := body["would_reindex_symbols"]; !ok {
		t.Errorf("dry-run response must include would_reindex_symbols; got %v", body)
	}
}

func TestHandleRebuildFTS_ConfirmedRebuilds(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	result, err := srv.handleRebuildFTS(context.Background(), makeReq(map[string]any{"confirm": true}))
	if err != nil {
		t.Fatalf("handleRebuildFTS confirmed: %v", err)
	}
	body := decode(t, result)
	if body["dry_run"] != false {
		t.Errorf("confirmed call must report dry_run=false; got %v", body["dry_run"])
	}
	if _, ok := body["rebuilt_rows"]; !ok {
		t.Errorf("confirmed response must include rebuilt_rows; got %v", body)
	}
	if _, ok := body["duration_ms"]; !ok {
		t.Errorf("confirmed response must include duration_ms; got %v", body)
	}
}

func TestHandleSelfTest_HealthyInstall(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	result, err := srv.handleSelfTest(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("handleSelfTest: %v", err)
	}
	body := decode(t, result)
	if body["ok"] != true {
		t.Errorf("self_test on a clean install must report ok=true; got %v\nfull body: %v", body["ok"], body)
	}
	steps, ok := body["steps"].([]any)
	if !ok || len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %v", body["steps"])
	}
	for i, raw := range steps {
		step := raw.(map[string]any)
		if step["ok"] != true {
			t.Errorf("step %d (%v) failed: %v", i, step["label"], step["error"])
		}
	}
}
