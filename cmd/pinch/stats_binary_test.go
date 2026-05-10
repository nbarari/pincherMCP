package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// TestStatsCLI_Binary_TextEmpty exercises runStatsCLI's text-mode path
// end-to-end via the cover-instrumented binary so the dispatch wrapper
// picks up coverage credit (#185 plumbing). Pairs with the existing
// in-process tests for the report builder.
func TestStatsCLI_Binary_TextEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()

	cmd := exec.Command(bin, "stats", "--data-dir", dataDir)
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher stats: %v\n%s", err, out)
	}
	got := string(out)
	// Empty DB still renders the all-time / projects sections.
	for _, want := range []string{"ALL-TIME", "Tool calls"} {
		if !strings.Contains(got, want) {
			t.Errorf("text output missing %q in:\n%s", want, got)
		}
	}
}

func TestStatsCLI_Binary_JSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()

	cmd := exec.Command(bin, "stats", "--data-dir", dataDir, "--json")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher stats --json: %v\n%s", err, out)
	}

	var report StatsReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if report.DataDir == "" {
		t.Error("expected data_dir to be populated")
	}
}

func TestStatsCLI_Binary_Reset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()

	// Seed one session row so --reset has something to delete.
	store, err := db.Open(dataDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.RecordSession("session-binary", time.Now(), 1, 100, 200, 0.05, "", 0, ""); err != nil {
		t.Fatalf("record session: %v", err)
	}
	store.Close()

	cmd := exec.Command(bin, "stats", "--data-dir", dataDir, "--reset")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher stats --reset: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Wiped") {
		t.Errorf("expected 'Wiped' in reset output, got: %s", out)
	}
}

// TestStatsCLI_Binary_BadDataDir covers the dispatch wrapper's
// db.Open error branch (data-dir points at something that can't host
// pincher.db).
func TestStatsCLI_Binary_BadDataDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	// Use a path that's a regular file — db.Open will try to open
	// <file>/pincher.db which fails identically on every platform.
	notADir := t.TempDir() + "/not_a_dir"
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write notADir: %v", err)
	}
	cmd := exec.Command(bin, "stats", "--data-dir", notADir)
	cmd.Env = pincherCoverEnv()
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "open database") && !strings.Contains(string(out), "stats failed") {
		t.Errorf("expected db error; got: %s", out)
	}
}

func TestStatsCLI_Binary_ResetJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI binary build in -short mode")
	}

	bin := buildPincherBinary(t)
	dataDir := t.TempDir()

	cmd := exec.Command(bin, "stats", "--data-dir", dataDir, "--reset", "--json")
	cmd.Env = pincherCoverEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pincher stats --reset --json: %v\n%s", err, out)
	}
	var receipt map[string]any
	if err := json.Unmarshal(out, &receipt); err != nil {
		t.Fatalf("reset --json output not valid JSON: %v\n%s", err, out)
	}
	if reset, _ := receipt["reset"].(bool); !reset {
		t.Errorf("expected reset=true in receipt, got %v", receipt["reset"])
	}
}
