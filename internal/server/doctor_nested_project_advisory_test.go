package server

import (
	"strings"
	"testing"
)

// #1209: when two indexed projects are nested (one project's path is a
// strict subdirectory of another's), doctor pre-fix said nothing — the
// user saw both projects in the list but had no signal the same source
// files were getting indexed twice. The per-project collision detector
// (#1209 part 1) was already correct; this is the missing UX signal
// that surfaces the structural duplication.

// Positive: parent + nested child are detected; inner project is named.
func TestNestedProjectAdvisory_FlagsNestedPair(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "warp_rc", Path: `D:\ClaudeCode\warp_rc`, Symbols: 1340000},
		{Name: "warp-fork", Path: `D:\ClaudeCode\warp_rc\warp-fork`, Symbols: 1070000},
	}
	got := nestedProjectAdvisory(projects)
	if got == "" {
		t.Fatal("expected advisory for nested pair; got empty")
	}
	if !strings.Contains(got, "warp-fork") {
		t.Errorf("advisory must name the inner project; got %q", got)
	}
	if !strings.Contains(got, "warp_rc") {
		t.Errorf("advisory must name the outer project; got %q", got)
	}
	if !strings.Contains(got, "nested") {
		t.Errorf("advisory should describe the relationship as nested; got %q", got)
	}
	if !strings.Contains(got, "#1209") {
		t.Errorf("advisory must reference issue #1209 for remediation context; got %q", got)
	}
}

// Negative (siblings): two projects sharing a parent dir but not nested
// in each other must NOT trip the advisory.
func TestNestedProjectAdvisory_SiblingsIgnored(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "warp_rc", Path: `D:\ClaudeCode\warp_rc`, Symbols: 1340000},
		{Name: "Codex", Path: `D:\ClaudeCode\Codex`, Symbols: 248000},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf("expected empty advisory for sibling projects; got %q", got)
	}
}

// Control (prefix-substring false-positive guard): `warp_rc` is NOT
// nested inside `warp_rc_fork`. The trailing-separator check in the
// algorithm is what prevents this false positive — verify it.
func TestNestedProjectAdvisory_PrefixSubstring_NotNested(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "warp_rc", Path: `D:\ClaudeCode\warp_rc`, Symbols: 1340000},
		{Name: "warp_rc_fork", Path: `D:\ClaudeCode\warp_rc_fork`, Symbols: 50000},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf("expected empty advisory: paths share a string prefix but are not nested; got %q", got)
	}
}

// Cross-check (case + separator insensitivity): the same nesting must
// be detected when paths differ in case or separator style (Windows
// `D:\` vs forward-slash variants come back from different code paths).
func TestNestedProjectAdvisory_CaseAndSeparatorInsensitive(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "outer", Path: `D:\ClaudeCode\warp_rc`, Symbols: 1000000},
		// Mixed case + forward slashes: still nested under outer.
		{Name: "inner", Path: `d:/claudecode/WARP_RC/warp-fork`, Symbols: 500000},
	}
	got := nestedProjectAdvisory(projects)
	if got == "" {
		t.Fatal("expected advisory to detect nesting across case/separator differences; got empty")
	}
	if !strings.Contains(got, "inner") {
		t.Errorf("advisory must name the inner project; got %q", got)
	}
}

// Cap: with >3 nested pairs, advisory limits to the worst 3 by inner
// symbol count to stay scannable.
func TestNestedProjectAdvisory_CapsAtThreePairs(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "root", Path: `/repo`, Symbols: 5000000},
		{Name: "nest-a", Path: `/repo/a`, Symbols: 400000},
		{Name: "nest-b", Path: `/repo/b`, Symbols: 300000},
		{Name: "nest-c", Path: `/repo/c`, Symbols: 200000},
		{Name: "nest-d", Path: `/repo/d`, Symbols: 100000},
	}
	got := nestedProjectAdvisory(projects)
	if got == "" {
		t.Fatal("expected advisory; got empty")
	}
	// Top 3 by symbol count are nest-a/b/c; nest-d must be dropped.
	for _, want := range []string{"nest-a", "nest-b", "nest-c"} {
		if !strings.Contains(got, want) {
			t.Errorf("advisory should include top-3 inner project %q; got %q", want, got)
		}
	}
	if strings.Contains(got, "nest-d") {
		t.Errorf("advisory should drop nest-d (smallest); got %q", got)
	}
}

// All-flat: no nesting → empty advisory.
func TestNestedProjectAdvisory_AllFlat_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "a", Path: `/home/u/a`, Symbols: 1000},
		{Name: "b", Path: `/home/u/b`, Symbols: 2000},
		{Name: "c", Path: `/home/u/c`, Symbols: 3000},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf("expected empty advisory for non-nested projects; got %q", got)
	}
}

// #1644: pincher's pinned corpus fixtures (testdata/corpus/*) are
// indexed as standalone projects on purpose — the snapshot-test gates
// (make corpus-test, TestCorpusSnapshot_*) require it. The advisory
// must NOT flag them as nested; recommending `pincher project rm` on
// these would break the corpus snapshot test suite.
func TestNestedProjectAdvisory_TestdataCorpus_Suppressed(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "pincher-repo", Path: `D:\ClaudeCode\pincher-repo`, Symbols: 8000},
		{Name: "k8s-ops", Path: `D:\ClaudeCode\pincher-repo\testdata\corpus\k8s-ops`, Symbols: 42},
		{Name: "terraform-stack", Path: `D:\ClaudeCode\pincher-repo\testdata\corpus\terraform-stack`, Symbols: 40},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf("testdata/corpus/* fixtures must be suppressed; got advisory %q", got)
	}
}

// #1644 cross-check: the suppression must be specific. A genuine
// nested-project mistake (warp-fork inside warp_rc) still fires, even
// when a sibling corpus fixture is also present.
func TestNestedProjectAdvisory_MixedRealAndCorpus_FiresOnReal(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "pincher-repo", Path: `D:\ClaudeCode\pincher-repo`, Symbols: 8000},
		{Name: "k8s-ops", Path: `D:\ClaudeCode\pincher-repo\testdata\corpus\k8s-ops`, Symbols: 42},
		{Name: "warp_rc", Path: `D:\ClaudeCode\warp_rc`, Symbols: 1500000},
		{Name: "warp-fork", Path: `D:\ClaudeCode\warp_rc\warp-fork`, Symbols: 1500000},
	}
	got := nestedProjectAdvisory(projects)
	if got == "" {
		t.Fatal("expected advisory for real warp-fork nesting; got empty")
	}
	if !strings.Contains(got, "warp-fork") {
		t.Errorf("advisory must still flag warp-fork; got %q", got)
	}
	if strings.Contains(got, "k8s-ops") {
		t.Errorf("advisory must NOT flag testdata corpus k8s-ops; got %q", got)
	}
}

// #1644 cross-check: .atrium/work/ worktree paths are also suppressed.
func TestNestedProjectAdvisory_AtriumWorktree_Suppressed(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "slopbuster", Path: `D:\ClaudeCode\slopbuster`, Symbols: 4500},
		{Name: "i-7", Path: `D:\ClaudeCode\slopbuster\.atrium\work\r-2026-04-14-002\i-7`, Symbols: 874},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf(".atrium/work/* worktree nesting must be suppressed; got %q", got)
	}
}

// #1644 direct unit: isIntentionallyNested matrix.
func TestIsIntentionallyNested(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		inner, outer     string
		wantIntentional  bool
	}{
		{"testdata/corpus suppressed", "d:/repo/testdata/corpus/k8s-ops", "d:/repo", true},
		{"testdata/__fixtures__ suppressed", "/home/u/repo/testdata/__fixtures__/probe", "/home/u/repo", true},
		{".atrium/work suppressed", "d:/repo/.atrium/work/r-1/i-7", "d:/repo", true},
		{"genuine warp-fork case fires", "d:/code/warp_rc/warp-fork", "d:/code/warp_rc", false},
		{"non-nested returns false", "/home/u/a", "/home/u/b", false},
		{"identical paths return false (no rel)", "/home/u/x", "/home/u/x", false},
	}
	for _, c := range cases {
		if got := isIntentionallyNested(c.inner, c.outer); got != c.wantIntentional {
			t.Errorf("%s: isIntentionallyNested(%q, %q) = %v, want %v",
				c.name, c.inner, c.outer, got, c.wantIntentional)
		}
	}
}

// Direct unit: normalizePathForNesting behavior.
func TestNormalizePathForNesting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{`D:\ClaudeCode\warp_rc`, "d:/claudecode/warp_rc"},
		{`/home/u/project/`, "/home/u/project"},
		{`d:/claudecode/warp_rc`, "d:/claudecode/warp_rc"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizePathForNesting(c.in); got != c.want {
			t.Errorf("normalizePathForNesting(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
