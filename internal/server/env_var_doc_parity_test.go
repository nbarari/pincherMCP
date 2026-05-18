package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// #672 workstream 4 (v0.79 capability-advertisement audit, env-var
// surface half). Audit gap: `PINCHER_DATA_DIR` was recommended in
// docs/tutorials/vscode-copilot.md to keep per-editor session
// counters isolated, but the variable was never added to
// docs/REFERENCE.md's data-directory section. A user reading the
// canonical reference doc found `--data-dir` but no env-var form,
// even though our own tutorial relies on it.
//
// This test pins the inverse drift: if the docs/tutorials/* or
// docs/deployment/* surfaces tell users to set a PINCHER_* env var,
// REFERENCE.md MUST document it. The reverse direction (REFERENCE.md
// mentions an env var no doc uses) is fine — many env vars are
// internal escape hatches we intentionally don't promote.

func TestEnvVars_TutorialMentionsDocumentedInReference(t *testing.T) {
	t.Parallel()

	refBytes, err := os.ReadFile("../../docs/REFERENCE.md")
	if err != nil {
		t.Fatalf("read REFERENCE.md: %v", err)
	}
	ref := string(refBytes)

	// Walk docs/tutorials/* and docs/deployment/* for any PINCHER_*
	// env-var mention. We treat the user-facing docs surface as the
	// "promised" set; anything we promise must be documented.
	envRE := regexp.MustCompile(`PINCHER_[A-Z][A-Z0-9_]*`)
	promised := make(map[string]map[string]bool) // var -> { file: true }

	for _, dir := range []string{"../../docs/tutorials", "../../docs/deployment"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// deployment/ may not exist on all branches; tutorials/ is required.
			if dir == "../../docs/tutorials" {
				t.Fatalf("read %s: %v", dir, err)
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			b, err := os.ReadFile(full)
			if err != nil {
				t.Errorf("read %s: %v", full, err)
				continue
			}
			text := string(b)
			for _, name := range envRE.FindAllString(text, -1) {
				// Skip test-only vars; they're internal.
				if strings.HasPrefix(name, "PINCHER_TEST_") {
					continue
				}
				if promised[name] == nil {
					promised[name] = map[string]bool{}
				}
				promised[name][full] = true
			}
		}
	}

	if len(promised) == 0 {
		t.Fatal("scanned docs/tutorials and docs/deployment but found zero PINCHER_* mentions — regex / paths drifted")
	}

	for envVar, files := range promised {
		if !strings.Contains(ref, envVar) {
			var where []string
			for f := range files {
				where = append(where, f)
			}
			sort.Strings(where)
			t.Errorf("env var %q is referenced in user-facing docs (%s) but not documented in docs/REFERENCE.md — add a row in the relevant env-var table or Data directory section, or drop the mention from the tutorial",
				envVar, strings.Join(where, ", "))
		}
	}
}
