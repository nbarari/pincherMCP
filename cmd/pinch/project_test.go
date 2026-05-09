package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// ── matchProject ─────────────────────────────────────────────────────────────

func projectsFixture() []db.Project {
	return []db.Project{
		{ID: "abc123", Name: "pincher", Path: "/home/dev/pincher"},
		{ID: "def456", Name: "pincher-fork", Path: "/home/dev/pincher-fork"},
		{ID: "ghi789", Name: "other", Path: "/home/dev/other-project"},
	}
}

func TestMatchProject_ExactID(t *testing.T) {
	hits, status := matchProject(projectsFixture(), "abc123")
	if status != matchExact {
		t.Fatalf("status=%v, want matchExact", status)
	}
	if len(hits) != 1 || hits[0].Name != "pincher" {
		t.Errorf("hits=%v, want exactly [pincher]", hits)
	}
}

func TestMatchProject_ExactNameCaseInsensitive(t *testing.T) {
	hits, status := matchProject(projectsFixture(), "OTHER")
	if status != matchExact {
		t.Fatalf("status=%v, want matchExact", status)
	}
	if len(hits) != 1 || hits[0].ID != "ghi789" {
		t.Errorf("hits=%v, want [other]", hits)
	}
}

func TestMatchProject_AmbiguousSubstring(t *testing.T) {
	// "pinc" is a substring of both "pincher" and "pincher-fork" but
	// matches neither name exactly, so matching falls through to
	// substring-on-name and produces an ambiguity. (When the input IS
	// an exact name like "pincher", exact-match wins over substring —
	// that's covered by TestMatchProject_ExactNameCaseInsensitive's
	// implicit invariant.)
	hits, status := matchProject(projectsFixture(), "pinc")
	if status != matchAmbiguous {
		t.Fatalf("status=%v, want matchAmbiguous (matches both pincher* projects)", status)
	}
	if len(hits) != 2 {
		t.Errorf("hits=%v, want 2 matches", hits)
	}
}

func TestMatchProject_ExactNameWinsOverSubstring(t *testing.T) {
	// "pincher" is the exact name of one project but also a prefix
	// of "pincher-fork". Exact name match should resolve to the one
	// project, not bail with ambiguity.
	hits, status := matchProject(projectsFixture(), "pincher")
	if status != matchExact {
		t.Fatalf("status=%v, want matchExact (exact name beats substring)", status)
	}
	if len(hits) != 1 || hits[0].Name != "pincher" {
		t.Errorf("hits=%v, want exactly [pincher]", hits)
	}
}

func TestMatchProject_UniqueSubstringOnPath(t *testing.T) {
	hits, status := matchProject(projectsFixture(), "other-project")
	if status != matchExact {
		t.Fatalf("status=%v, want matchExact (path substring)", status)
	}
	if len(hits) != 1 || hits[0].Name != "other" {
		t.Errorf("hits=%v, want [other]", hits)
	}
}

func TestMatchProject_NoMatch(t *testing.T) {
	hits, status := matchProject(projectsFixture(), "nonexistent")
	if status != matchNone {
		t.Errorf("status=%v, want matchNone", status)
	}
	if len(hits) != 0 {
		t.Errorf("expected no hits, got %v", hits)
	}
}

func TestMatchProject_EmptyTarget(t *testing.T) {
	_, status := matchProject(projectsFixture(), "")
	if status != matchNone {
		t.Errorf("empty target: status=%v, want matchNone", status)
	}
}

// ── confirmYesFrom ───────────────────────────────────────────────────────────

func TestConfirmYesFrom(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"n\n", false},
		{"N\n", false},
		{"\n", false},
		{"", false},
		{"maybe\n", false},
	}
	for _, c := range cases {
		got := confirmYesFrom(strings.NewReader(c.input))
		if got != c.want {
			t.Errorf("confirmYesFrom(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

// ── formatProjectList ────────────────────────────────────────────────────────

func TestFormatProjectList_Empty(t *testing.T) {
	got := formatProjectList(nil)
	if !strings.Contains(got, "No projects indexed.") {
		t.Errorf("empty list output should mention 'No projects indexed.', got: %s", got)
	}
}

func TestFormatProjectList_RendersTable(t *testing.T) {
	got := formatProjectList(projectsFixture())
	for _, want := range []string{"PROJECT", "FILES", "SYMBOLS", "EDGES", "PATH", "pincher", "other", "3 project(s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("table output missing %q in:\n%s", want, got)
		}
	}
}

// ── end-to-end via test binary ───────────────────────────────────────────────

// (project tests use buildPincherBinary from coverbuild_test.go so
// integration-style runs contribute to the merged coverage profile —
// see comment there for the full #185 rationale.)

func TestProjectCLI_NoArgsShowsUsage(t *testing.T) {
	exe := buildPincherBinary(t)
	cmd := exec.Command(exe, "project")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit when no verb given")
	}
	if !strings.Contains(string(out), "usage: pincher project") {
		t.Errorf("expected usage banner, got: %s", out)
	}
}

func TestProjectCLI_UnknownVerbExits2(t *testing.T) {
	exe := buildPincherBinary(t)
	cmd := exec.Command(exe, "project", "frobnicate")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for unknown verb")
	}
	if !strings.Contains(string(out), "unknown verb") {
		t.Errorf("expected 'unknown verb' message, got: %s", out)
	}
}

func TestProjectCLI_ListEmptyJSON(t *testing.T) {
	exe := buildPincherBinary(t)
	dataDir := t.TempDir()
	cmd := exec.Command(exe, "project", "list", "--json", "--data-dir", dataDir)
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list --json on empty DB failed: %v\n%s", err, out)
	}
	var report projectListReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if report.Count != 0 {
		t.Errorf("count=%d, want 0", report.Count)
	}
}

func TestProjectCLI_ListEmptyText(t *testing.T) {
	exe := buildPincherBinary(t)
	dataDir := t.TempDir()
	cmd := exec.Command(exe, "project", "list", "--data-dir", dataDir)
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list on empty DB failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "No projects indexed") {
		t.Errorf("expected 'No projects indexed' in empty list, got: %s", out)
	}
}

// ── rm via real DB ───────────────────────────────────────────────────────────

// seedProject opens a real DB at dataDir and writes one project row.
// Returns the project ID for later assertions.
func seedProject(t *testing.T, dataDir, name, path string) string {
	t.Helper()
	store, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	id := db.ProjectIDFromPath(path)
	p := db.Project{ID: id, Path: path, Name: name, FileCount: 1, SymCount: 5, EdgeCount: 3}
	if err := store.UpsertProject(p); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return id
}

func TestProjectCLI_RmForceJSON(t *testing.T) {
	exe := buildPincherBinary(t)
	dataDir := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	id := seedProject(t, dataDir, "myproj", projectPath)

	cmd := exec.Command(exe, "project", "rm", "--force", "--json", "--data-dir", dataDir, "myproj")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rm failed: %v\n%s", err, out)
	}
	var receipt map[string]any
	if err := json.Unmarshal(out, &receipt); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if removed, _ := receipt["removed"].(bool); !removed {
		t.Errorf("removed=%v, want true", receipt["removed"])
	}
	if got, _ := receipt["id"].(string); got != id {
		t.Errorf("receipt id=%q, want %q", got, id)
	}

	// Verify the project is gone from the DB.
	store, err := db.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, _ := store.GetProject(id)
	if got != nil {
		t.Errorf("project %q still exists after rm", id)
	}
}

func TestProjectCLI_RmNoMatchExitsNonZero(t *testing.T) {
	exe := buildPincherBinary(t)
	dataDir := t.TempDir()
	cmd := exec.Command(exe, "project", "rm", "--force", "--data-dir", dataDir, "nope")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("rm on empty DB should error; got output: %s", out)
	}
	if !strings.Contains(string(out), "no project matches") {
		t.Errorf("expected 'no project matches' in stderr, got: %s", out)
	}
}

func TestProjectCLI_RmAmbiguousErrorsWithList(t *testing.T) {
	exe := buildPincherBinary(t)
	dataDir := t.TempDir()
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	seedProject(t, dataDir, "myproj-a", a)
	seedProject(t, dataDir, "myproj-b", b)

	cmd := exec.Command(exe, "project", "rm", "--force", "--data-dir", dataDir, "myproj")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("rm with ambiguous match should error; output: %s", out)
	}
	s := string(out)
	if !strings.Contains(s, "matches multiple") {
		t.Errorf("expected ambiguity message, got: %s", s)
	}
	if !strings.Contains(s, "myproj-a") || !strings.Contains(s, "myproj-b") {
		t.Errorf("expected both candidates listed, got: %s", s)
	}
}

func TestProjectCLI_RmJSONRequiresForce(t *testing.T) {
	exe := buildPincherBinary(t)
	dataDir := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	id := seedProject(t, dataDir, "myproj", projectPath)

	// --json without --force should refuse rather than hang on a y/N
	// prompt nobody can answer in a JSON workflow.
	cmd := exec.Command(exe, "project", "rm", "--json", "--data-dir", dataDir, "myproj")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit; output: %s", out)
	}
	if !strings.Contains(string(out), "--json requires --force") {
		t.Errorf("expected '--json requires --force' message, got: %s", out)
	}

	// And the project should still exist (no delete happened).
	store, _ := db.Open(dataDir)
	defer store.Close()
	got, _ := store.GetProject(id)
	if got == nil {
		t.Error("project was deleted despite the refusal — should still exist")
	}
}

func TestProjectCLI_RmConfirmAccepts(t *testing.T) {
	exe := buildPincherBinary(t)
	dataDir := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	seedProject(t, dataDir, "myproj", projectPath)

	cmd := exec.Command(exe, "project", "rm", "--data-dir", dataDir, "myproj")
	cmd.Stdin = strings.NewReader("y\n")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rm with y confirmation failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Removed project") {
		t.Errorf("expected 'Removed project' confirmation, got: %s", out)
	}
}

func TestProjectCLI_RmConfirmRejects(t *testing.T) {
	exe := buildPincherBinary(t)
	dataDir := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	id := seedProject(t, dataDir, "myproj", projectPath)

	cmd := exec.Command(exe, "project", "rm", "--data-dir", dataDir, "myproj")
	cmd.Stdin = strings.NewReader("n\n")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rm with n confirmation should NOT error: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Aborted") {
		t.Errorf("expected 'Aborted' message, got: %s", out)
	}
	// Project should still exist.
	store, _ := db.Open(dataDir)
	defer store.Close()
	got, _ := store.GetProject(id)
	if got == nil {
		t.Error("project should still exist after declined confirmation")
	}
}

func TestProjectCLI_ListPopulated(t *testing.T) {
	exe := buildPincherBinary(t)
	dataDir := t.TempDir()
	projectPath := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	seedProject(t, dataDir, "alpha", projectPath)

	cmd := exec.Command(exe, "project", "list", "--json", "--data-dir", dataDir)
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	var report projectListReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if report.Count != 1 {
		t.Errorf("count=%d, want 1", report.Count)
	}
	if len(report.Projects) != 1 || report.Projects[0].Name != "alpha" {
		t.Errorf("expected one project named 'alpha', got: %v", report.Projects)
	}
	if report.Projects[0].Symbols != 5 {
		t.Errorf("expected symbols=5, got %d", report.Projects[0].Symbols)
	}
}
