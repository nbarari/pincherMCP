// workflow-lint catches a recurring class of GitHub Actions workflow bugs
// where a job's `run:` block references a repo-relative script (bash
// scripts/foo.sh, ./scripts/bar.sh, make corpus-test, go run ./cmd/baz)
// without an earlier `actions/checkout@v4` step in the same job.
//
// Symptom in the wild (#690): v0.54.0-beta.1 release run failed at the
// "Determine release channel" step with `bash: scripts/release-channel.sh:
// No such file or directory` because the checksums job ran on a fresh
// runner that downloaded artifacts but never checked out the repo. That
// failure mode is invisible to YAML linting, invisible to local test
// suites, and only surfaces at tag time — slow, public, and the failed
// tag artifacts linger.
//
// What this tool catches:
//
//   - jobs with at least one `run:` step that calls into the repo
//     (bash scripts/, ./scripts/, ./.bin/, make <target>, go run ./...)
//     but no `uses: actions/checkout@v4` step earlier in the job's
//     step list
//
// What this tool catches (Bucket 2 — inline divergence):
//
//   - jobs with a `run:` step that reimplements logic that already lives
//     in a canonical script under scripts/ — detected by fingerprint
//     regex match across the run-block text WITHOUT a reference to the
//     canonical script. Symptom: v0.54.0-beta.1 release-channel logic
//     was duplicated inline in release.yml AND in scripts/release-channel.sh,
//     and they had drifted — workflow log mis-labelled the beta tag as
//     "dev" until #689 made release.yml shell out to the canonical script.
//
// What this tool does NOT catch (file as follow-ups if needed):
//
//   - missing tag-shaped probe before merge (bucket 3 in #690)
//
// Run via: `go run ./cmd/workflow-lint`. Exit 0 = clean; exit 1 =
// at least one job has a missing-checkout violation. CI invokes this
// from the new `workflow-lint` job in .github/workflows/ci.yml.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// scriptRefPattern matches the script-reference shapes we want to gate
// against. Substring match — we don't constrain the preceding character
// because real-world scripts get wrapped in $(...), assigned to vars,
// chained after &&, etc. False positives from script-ref strings that
// only appear inside comments are vanishingly rare and not worth the
// regex complexity to suppress.
var scriptRefPattern = regexp.MustCompile(`bash\s+(?:\./)?scripts/|(?:\./)?scripts/[^\s]+\.sh|(?:\./)?\.bin/|\bmake\s+\w|\bgo\s+run\s+\./`)

// checkoutPattern matches any actions/checkout invocation. Version-agnostic
// since lint should pass for v3, v4, v5 — the bug is missing-checkout, not
// wrong-version.
var checkoutPattern = regexp.MustCompile(`actions/checkout@v\d+`)

type step struct {
	Uses string `yaml:"uses"`
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

type job struct {
	Name  string `yaml:"name"`
	Steps []step `yaml:"steps"`
}

type workflow struct {
	Name string         `yaml:"name"`
	Jobs map[string]job `yaml:"jobs"`
}

type violation struct {
	File    string
	JobID   string
	JobName string
	StepIdx int
	Snippet string
	Kind    string // "missing-checkout" or "inline-divergence"
	Hint    string // remediation hint shown to operator
}

// canonicalScript names a repo-local script that has a stable identifying
// fingerprint. When a workflow `run:` block matches the fingerprint but
// does not reference the script path, we suspect inline divergence from
// the canonical implementation (#690 Bucket 2 — exactly the v0.54.0-beta.1
// failure shape).
type canonicalScript struct {
	Path        string           // repo-relative script path (e.g., "scripts/release-channel.sh")
	Fingerprint []*regexp.Regexp // ALL must match the run-block text
	Hint        string           // what to do instead
}

// canonicalScripts hardcodes the divergence-prone scripts we know about.
// Adding entries here is cheaper than building a generalized AST-equivalence
// engine; the lint catches exactly the bug shapes we've actually hit.
var canonicalScripts = []canonicalScript{
	{
		Path: "scripts/release-channel.sh",
		Fingerprint: []*regexp.Regexp{
			// Modulo-10 stable-channel rule (e.g., `MINOR % 10 == 0`).
			regexp.MustCompile(`%\s*10|\bmod\s+10`),
			// Pre-release suffix routing — beta/alpha/rc string match.
			regexp.MustCompile(`-(beta|alpha|rc)\.|"(beta|alpha|rc)"`),
		},
		Hint: "shell out to scripts/release-channel.sh instead — keep one canonical channel-detection implementation",
	},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable body. Returns the process exit code; never
// calls os.Exit so tests can call run() repeatedly. stdout receives
// the clean-state message; stderr receives violation reports + the
// walk-error message.
func run(args []string, stdout, stderr io.Writer) int {
	root := ".github/workflows"
	if len(args) > 0 {
		root = args[0]
	}
	violations, err := lintTree(root)
	if err != nil {
		fmt.Fprintf(stderr, "workflow-lint: %v\n", err)
		return 2
	}
	if len(violations) == 0 {
		fmt.Fprintf(stdout, "workflow-lint: clean (scanned %s)\n", root)
		return 0
	}
	fmt.Fprintf(stderr, "workflow-lint: %d violation(s):\n\n", len(violations))
	for _, v := range violations {
		fmt.Fprintf(stderr, "  [%s] %s :: job=%s (%s) :: step %d\n    %s:\n      %s\n",
			v.Kind, v.File, v.JobID, v.JobName, v.StepIdx, v.Hint, v.Snippet)
		fmt.Fprintln(stderr)
	}
	return 1
}

// lintTree walks `root` looking for *.yml/*.yaml files and lints each.
func lintTree(root string) ([]violation, error) {
	var out []violation
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(p); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		vs, err := lintFile(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, vs...)
		return nil
	})
	return out, err
}

// lintFile parses one workflow YAML and returns any missing-checkout
// violations across its jobs.
func lintFile(p string) ([]violation, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("yaml parse: %w", err)
	}
	var out []violation
	for jobID, j := range wf.Jobs {
		out = append(out, lintJob(p, jobID, j)...)
	}
	return out, nil
}

// lintJob walks a job's step list, applying the missing-checkout AND
// inline-divergence rules to each `run:` step.
func lintJob(file, jobID string, j job) []violation {
	var out []violation
	checkedOut := false
	for i, s := range j.Steps {
		if checkoutPattern.MatchString(s.Uses) {
			checkedOut = true
			continue
		}
		if s.Run == "" {
			continue
		}
		// Rule 1 — missing-checkout. Only fires when no checkout has run yet.
		if !checkedOut {
			if loc := scriptRefPattern.FindString(s.Run); loc != "" {
				out = append(out, violation{
					File:    file,
					JobID:   jobID,
					JobName: j.Name,
					StepIdx: i,
					Snippet: firstNonEmptyLine(s.Run),
					Kind:    "missing-checkout",
					Hint:    "references repo without prior actions/checkout@vN; add `- uses: actions/checkout@v4` earlier in this job",
				})
			}
		}
		// Rule 2 — inline-divergence from canonical scripts. Fires regardless
		// of checkout state — divergence is always a bug.
		for _, cs := range canonicalScripts {
			if matchesAllFingerprints(s.Run, cs.Fingerprint) && !strings.Contains(s.Run, cs.Path) {
				out = append(out, violation{
					File:    file,
					JobID:   jobID,
					JobName: j.Name,
					StepIdx: i,
					Snippet: firstNonEmptyLine(s.Run),
					Kind:    "inline-divergence",
					Hint:    cs.Hint,
				})
			}
		}
	}
	return out
}

// matchesAllFingerprints returns true when every regex matches somewhere
// in text. Used by the inline-divergence check to require multiple
// signal regexes (single-pattern matches fire too eagerly).
func matchesAllFingerprints(text string, patterns []*regexp.Regexp) bool {
	for _, p := range patterns {
		if !p.MatchString(text) {
			return false
		}
	}
	return len(patterns) > 0
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if len(t) > 120 {
			return t[:117] + "..."
		}
		return t
	}
	return "<empty>"
}
