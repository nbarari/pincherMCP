package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kwad77/pincher/internal/db"
	"github.com/zeebo/xxh3"
)

// runVerifyCLI implements `pincher verify [--json] [--data-dir DIR]
// [--project NAME|ID|SUBSTRING]`. #1399.
//
// Re-hashes every indexed file's on-disk content and compares against
// the stored `files.hash` column. Drift fires when:
//   - File modified out-of-band since last index (the hash-skip path
//     in the indexer prevents re-extraction on UNCHANGED files; if the
//     file actually changed and the indexer didn't re-run, the stored
//     hash is stale and pincher's symbol byte-offsets point at wrong
//     content).
//   - File deleted (on-disk read returns ENOENT but the files row + its
//     symbols persist until the next index tail-pass GC).
//   - Persistence bug (interrupted write, partial migration) left the
//     stored hash diverged from what extraction would have produced.
//
// Doesn't auto-fix anything — `verify` reports; the user re-indexes the
// drifted project to bring the symbol-store back in sync. Mirrors the
// `pincher doctor` posture: surface, don't silently rewrite.
//
// Stoa-family pattern precedent: `stoa verify` hashes manifests as the
// integrity-check leg of the verify/doctor/probe trinity. Pincher's
// `doctor` is the doctor leg already; this issue adds the verify leg.
func runVerifyCLI(args []string) {
	log.SetOutput(io.Discard)

	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	asJSON := fs.Bool("json", false, "Emit structured JSON instead of plain text")
	projectFilter := fs.String("project", "", "Restrict to a specific project (name or id substring). Empty = all projects.")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher verify [--json] [--data-dir DIR] [--project NAME|ID|SUBSTR]")
		fmt.Fprintln(os.Stderr, "  Re-hashes every indexed file's on-disk content and reports drift")
		fmt.Fprintln(os.Stderr, "  against the stored hash. Re-index a drifted project to bring the")
		fmt.Fprintln(os.Stderr, "  symbol-store back in sync.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	dir := *dataDir
	if dir == "" {
		var err error
		dir, err = db.DataDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pincher: failed to determine data directory: %v\n", err)
			os.Exit(1)
		}
	}

	store, err := db.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher: failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	report, err := buildVerifyReport(store, *projectFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher verify: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Print(formatVerifyText(report))
	}

	// Non-zero exit when drift exists so CI scripts / hooks can branch.
	if report.FilesDrifted > 0 || report.FilesMissing > 0 || report.FilesUnreadable > 0 {
		os.Exit(2)
	}
}

// VerifyReport is the structured form `pincher verify --json` emits.
// Field names are stable across releases.
type VerifyReport struct {
	GeneratedAt     string                `json:"generated_at"`
	DataDir         string                `json:"data_dir"`
	Projects        []VerifyProjectReport `json:"projects"`
	FilesChecked    int                   `json:"files_checked"`
	FilesInSync     int                   `json:"files_in_sync"`
	FilesDrifted    int                   `json:"files_drifted"`
	FilesMissing    int                   `json:"files_missing"`
	FilesUnreadable int                   `json:"files_unreadable"`
}

// VerifyProjectReport carries per-project verify results. The Drifts /
// Missing / Unreadable slices stay empty (not null) when healthy so JSON
// consumers can iterate without a null-check (the #328 invariant).
type VerifyProjectReport struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Checked    int      `json:"checked"`
	InSync     int      `json:"in_sync"`
	Drifted    []string `json:"drifted"`
	Missing    []string `json:"missing"`
	Unreadable []string `json:"unreadable"`
}

// buildVerifyReport drives the per-project / per-file hash compare.
// projectFilter "" walks every project; non-empty matches against
// substring of project name or id (case-insensitive) — same semantics
// as `pincher project rm`.
func buildVerifyReport(store *db.Store, projectFilter string) (*VerifyReport, error) {
	r := &VerifyReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Projects:    []VerifyProjectReport{},
	}
	projects, err := store.ListProjects()
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	for _, p := range projects {
		if projectFilter != "" && !projectMatches(p, projectFilter) {
			continue
		}
		pr := VerifyProjectReport{
			ID:         p.ID,
			Name:       p.Name,
			Path:       p.Path,
			Drifted:    []string{},
			Missing:    []string{},
			Unreadable: []string{},
		}
		entries, err := store.ListFilesWithHashesForProject(p.ID)
		if err != nil {
			return nil, fmt.Errorf("list files for project %s: %w", p.Name, err)
		}
		for _, e := range entries {
			pr.Checked++
			abs := filepath.Join(p.Path, filepath.FromSlash(e.Path))
			content, readErr := os.ReadFile(abs)
			if readErr != nil {
				if os.IsNotExist(readErr) {
					pr.Missing = append(pr.Missing, e.Path)
				} else {
					pr.Unreadable = append(pr.Unreadable, e.Path)
				}
				continue
			}
			got := fmt.Sprintf("%x", xxh3.Hash(content))
			if got == e.Hash {
				pr.InSync++
				continue
			}
			pr.Drifted = append(pr.Drifted, e.Path)
		}
		r.Projects = append(r.Projects, pr)
		r.FilesChecked += pr.Checked
		r.FilesInSync += pr.InSync
		r.FilesDrifted += len(pr.Drifted)
		r.FilesMissing += len(pr.Missing)
		r.FilesUnreadable += len(pr.Unreadable)
	}
	return r, nil
}

// projectMatches reproduces the substring-match shape used by
// `pincher project rm` so the --project flag accepts the same input
// values the user already knows.
func projectMatches(p db.Project, filter string) bool {
	return substringFoldContains(p.Name, filter) || substringFoldContains(p.ID, filter)
}

func substringFoldContains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	hL := toLowerASCII(haystack)
	nL := toLowerASCII(needle)
	for i := 0; i+len(nL) <= len(hL); i++ {
		if hL[i:i+len(nL)] == nL {
			return true
		}
	}
	return false
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

// formatVerifyText renders the report as a compact plain-text block.
func formatVerifyText(r *VerifyReport) string {
	var b []byte
	b = append(b, fmt.Sprintf("pincher verify\n")...)
	if len(r.Projects) == 0 {
		b = append(b, "  No projects matched.\n"...)
		return string(b)
	}
	for _, p := range r.Projects {
		b = append(b, fmt.Sprintf("\n  %s  (%s)\n", p.Name, p.Path)...)
		b = append(b, fmt.Sprintf("    %d checked · %d in sync · %d drifted · %d missing · %d unreadable\n",
			p.Checked, p.InSync, len(p.Drifted), len(p.Missing), len(p.Unreadable))...)
		for _, f := range p.Drifted {
			b = append(b, fmt.Sprintf("    drifted   %s\n", f)...)
		}
		for _, f := range p.Missing {
			b = append(b, fmt.Sprintf("    missing   %s\n", f)...)
		}
		for _, f := range p.Unreadable {
			b = append(b, fmt.Sprintf("    unreadable %s\n", f)...)
		}
	}
	b = append(b, fmt.Sprintf("\nSummary: %d checked · %d in sync · %d drifted · %d missing · %d unreadable\n",
		r.FilesChecked, r.FilesInSync, r.FilesDrifted, r.FilesMissing, r.FilesUnreadable)...)
	if r.FilesDrifted == 0 && r.FilesMissing == 0 && r.FilesUnreadable == 0 {
		b = append(b, "\nAll indexed files match their stored hashes.\n"...)
	} else {
		b = append(b, "\nRe-index any drifted project (`pincher index <path>`) to bring its symbol-store back in sync.\n"...)
	}
	return string(b)
}
