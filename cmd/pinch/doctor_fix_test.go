package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1260 §3: tests for `pincher doctor --fix`.
//
// Covers the safe-action allowlist (currently just VACUUM-when-
// bloated), the noop/error/skipped/applied status taxonomy, and
// the text + JSON output formats.

// TestFixVacuumIfBloated_FreshDBNoop pins the threshold gate: a
// fresh DB with no bloat must report noop, not "applied". The
// threshold (50 MB reclaimable) prevents `doctor --fix` from
// paying the VACUUM cost on a clean install.
func TestFixVacuumIfBloated_FreshDBNoop(t *testing.T) {
	store, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	action := fixVacuumIfBloated(store)
	if action.Name != "vacuum-db" {
		t.Errorf("action.Name = %q, want vacuum-db", action.Name)
	}
	if action.Status != "noop" {
		t.Errorf("fresh DB should report noop; got status=%q details=%q", action.Status, action.Details)
	}
	if !strings.Contains(action.Details, "threshold") {
		t.Errorf("noop details should mention the threshold; got: %s", action.Details)
	}
}

// TestRunDoctorFix_FullReport pins the orchestration: runDoctorFix
// builds a FixReport with at least one action (vacuum-db) and emits
// it through the writer. The text format must include all the
// status counts in the trailing summary.
func TestRunDoctorFix_FullReport(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	var buf bytes.Buffer
	runDoctorFix(store, dir, false, &buf)
	out := buf.String()
	if !strings.Contains(out, "vacuum-db") {
		t.Errorf("output missing vacuum-db action; got:\n%s", out)
	}
	if !strings.Contains(out, "Summary:") {
		t.Errorf("output missing Summary line; got:\n%s", out)
	}
	if !strings.Contains(out, "noop") {
		t.Errorf("fresh DB run should report noop in summary; got:\n%s", out)
	}
}

// TestRunDoctorFix_JSONShape pins the --json output: parses cleanly
// into FixReport, carries data_dir + actions array, action statuses
// are from the documented taxonomy.
func TestRunDoctorFix_JSONShape(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	var buf bytes.Buffer
	runDoctorFix(store, dir, true, &buf)
	var report FixReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("JSON parse failed: %v\nbody:\n%s", err, buf.String())
	}
	if report.DataDir != dir {
		t.Errorf("data_dir = %q, want %q", report.DataDir, dir)
	}
	if len(report.Actions) == 0 {
		t.Fatal("actions array is empty; expected at least vacuum-db")
	}
	validStatuses := map[string]bool{
		"applied": true, "skipped": true, "noop": true, "error": true,
	}
	for _, a := range report.Actions {
		if !validStatuses[a.Status] {
			t.Errorf("action %q has invalid status %q; want one of applied/skipped/noop/error", a.Name, a.Status)
		}
	}
}

// TestFormatFixText_EmptyActionsAdvisesNothingNeeded pins the helpful-
// trailing-message branch: when no fixes ran and no errors fired,
// the formatter should add the "no safe fixes were needed" advisory
// pointing the user at explicit-action subcommands for the rest.
func TestFormatFixText_EmptyActionsAdvisesNothingNeeded(t *testing.T) {
	r := &FixReport{
		DataDir: "/tmp/test",
		Actions: []FixAction{
			{Name: "vacuum-db", Status: "noop", Details: "DB is healthy"},
		},
	}
	out := formatFixText(r)
	if !strings.Contains(out, "No safe fixes were needed") {
		t.Errorf("expected the no-fixes-needed advisory; got:\n%s", out)
	}
	if !strings.Contains(out, "destructive remediations") {
		t.Errorf("advisory should mention destructive remediations stay explicit; got:\n%s", out)
	}
}

// TestVacuumThresholdConstant pins the 50 MB threshold value. A
// future tightening to e.g. 10 MB would silently make `doctor --fix`
// more aggressive — pin the value so the change is deliberate.
func TestVacuumThresholdConstant(t *testing.T) {
	const expected = int64(50 * 1024 * 1024)
	if vacuumThresholdBytes != expected {
		t.Errorf("vacuumThresholdBytes = %d, want %d (50 MB) — a deliberate change requires updating this test", vacuumThresholdBytes, expected)
	}
}
