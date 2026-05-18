package db

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestReferenceMD_SchemaVersionParity (#1416) pins the
// docs/REFERENCE.md schema-version claim + migration table against
// the runtime schemaMigrations slice. Pre-fix the inline
// "Current version: vN" wording drifted 6 versions behind (v26 vs
// actual v32) and the migration table was missing 6 rows. Caught
// by walking the file, not by tooling — this test adds the tooling.
//
// Same description-vs-runtime parity shape that pinned tool-contract
// (#688) / init targets (#1414): mechanical guard so a future schema
// bump that forgets the doc update fails fast in CI.
func TestReferenceMD_SchemaVersionParity(t *testing.T) {
	t.Parallel()

	// Locate REFERENCE.md from this test file's package dir
	// (internal/db) by walking up to the repo root.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "docs", "REFERENCE.md")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("docs/REFERENCE.md not found by walking up from test cwd")
		}
		root = parent
	}
	refPath := filepath.Join(root, "docs", "REFERENCE.md")
	body, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatalf("read REFERENCE.md: %v", err)
	}
	text := string(body)
	current := CurrentSchemaVersion()

	// 1. Inline "Current version: **vN**" claim must match runtime.
	inlineRE := regexp.MustCompile(`Current version: \*\*v(\d+)\*\*`)
	m := inlineRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf(`REFERENCE.md missing "Current version: **vN**" wording — fix the regex or restore the line`)
	}
	if got := m[1]; got != fmt.Sprintf("%d", current) {
		t.Errorf(`REFERENCE.md inline "Current version" = v%s, want v%d (runtime CurrentSchemaVersion). Update the line near the Schema section.`,
			got, current)
	}

	// 2. Migration table must include a row for every vN→vN+1 hop.
	// The table header is `| Version | Summary |`; rows look like
	// `| v25→v26 | description |`. The arrow is "→" (U+2192).
	rowRE := regexp.MustCompile(`\| v(\d+)→v(\d+) \|`)
	have := map[string]bool{}
	for _, m := range rowRE.FindAllStringSubmatch(text, -1) {
		have[fmt.Sprintf("v%s→v%s", m[1], m[2])] = true
	}
	for i := 1; i < current; i++ {
		key := fmt.Sprintf("v%d→v%d", i, i+1)
		if !have[key] {
			t.Errorf(`REFERENCE.md migration table missing row for %s — add it. (Schema section, "Migration history" table.)`, key)
		}
	}
	// Don't fail when the doc has EXTRA rows (e.g. v0→v1 baseline);
	// only fail when the doc is BEHIND the runtime. Pre-existing
	// rows are intentional.

	// Sanity check: ensure the v1 baseline row exists too — its
	// shape is slightly different (`| v1 | Baseline: ... |`) and
	// would be missed by the vN→vN+1 regex.
	if !strings.Contains(text, "| v1 |") && !strings.Contains(text, "| v1 ") {
		t.Error("REFERENCE.md migration table missing v1 baseline row")
	}
}

// TestClaudeMD_SchemaVersionParity (#1418) pins the CLAUDE.md
// "Current schema: vN" claim against the runtime CurrentSchemaVersion.
// The line itself warned about prior drift ("was 7 versions stale
// before #998/#999 caught it") yet drifted AGAIN — same shape that
// motivated #1416's REFERENCE.md parity test. Adding the parallel
// guard so this can't recur silently in CLAUDE.md either.
func TestClaudeMD_SchemaVersionParity(t *testing.T) {
	t.Parallel()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("CLAUDE.md not found by walking up from test cwd")
		}
		root = parent
	}
	claudePath := filepath.Join(root, "CLAUDE.md")
	body, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	text := string(body)
	current := CurrentSchemaVersion()

	// CLAUDE.md uses "Current schema: **vN**" — distinct from
	// REFERENCE.md's "Current version: **vN**" wording.
	claudeRE := regexp.MustCompile(`Current schema: \*\*v(\d+)\*\*`)
	m := claudeRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf(`CLAUDE.md missing "Current schema: **vN**" wording — fix the regex or restore the line`)
	}
	if got := m[1]; got != fmt.Sprintf("%d", current) {
		t.Errorf(`CLAUDE.md "Current schema" = v%s, want v%d (runtime CurrentSchemaVersion). Update the line in the db.go bullet.`,
			got, current)
	}
}
