package main

import (
	"strings"
	"testing"
)

// #1507 v0.83: CLI mirrors of three doctor advisories that previously
// shipped only on the MCP side (#815/#1009 ghost-project, #1206
// wal-bloat, #1209 nested-project). Each test pins the same condition
// the MCP test pins, so the bounded-duplication convention can be
// audited at PR review time without diffing line-by-line.

// ─────────────────────────────────────────────────────────────
// ghostProjectAdvisory
// ─────────────────────────────────────────────────────────────

func TestGhostProjectAdvisory_CLI_FlagsZeroEdges(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "ghost", Symbols: 5000, Files: 200, Edges: 0},
	}
	got := ghostProjectAdvisory(projects)
	if got == "" {
		t.Fatal("ghost project with 5000 symbols / 0 edges should produce an advisory")
	}
	for _, want := range []string{"ghost", "5000 symbols", "0 edges", "ghost-extraction signature", "re-index"} {
		if !strings.Contains(got, want) {
			t.Errorf("advisory missing %q\n  got: %s", want, got)
		}
	}
}

func TestGhostProjectAdvisory_CLI_FlagsLowRatio(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "barely_edged", Symbols: 100_000, Files: 500, Edges: 50},
	}
	got := ghostProjectAdvisory(projects)
	if got == "" {
		t.Fatal("ratio 0.0005 (well below 0.001 floor) should fire")
	}
	if !strings.Contains(got, "barely_edged") {
		t.Errorf("advisory should name the project; got %q", got)
	}
}

func TestGhostProjectAdvisory_CLI_SmallProjectIgnored(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "tiny", Symbols: 50, Files: 3, Edges: 0},
	}
	if got := ghostProjectAdvisory(projects); got != "" {
		t.Errorf("project below 1000-symbol floor should NOT fire; got %q", got)
	}
}

func TestGhostProjectAdvisory_CLI_HealthyReturnsEmpty(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "healthy", Symbols: 5000, Files: 200, Edges: 1500},
	}
	if got := ghostProjectAdvisory(projects); got != "" {
		t.Errorf("healthy ratio 0.3 should produce no advisory; got %q", got)
	}
}

func TestGhostProjectAdvisory_CLI_CapsAtThree(t *testing.T) {
	t.Parallel()
	var projects []DoctorProjectSummary
	for i := 0; i < 5; i++ {
		projects = append(projects, DoctorProjectSummary{
			Name:    "ghost" + string(rune('A'+i)),
			Symbols: 5000 - i*100, // pre-sorted descending
			Files:   200,
			Edges:   0,
		})
	}
	got := ghostProjectAdvisory(projects)
	if got == "" {
		t.Fatal("expected an advisory")
	}
	// Must mention the top 3.
	for _, name := range []string{"ghostA", "ghostB", "ghostC"} {
		if !strings.Contains(got, name) {
			t.Errorf("advisory should mention %q; got %s", name, got)
		}
	}
	// Must NOT mention the 4th + 5th.
	for _, name := range []string{"ghostD", "ghostE"} {
		if strings.Contains(got, name) {
			t.Errorf("advisory should not exceed cap of 3 (got %q in message)", name)
		}
	}
}

// ─────────────────────────────────────────────────────────────
// walBloatAdvisory
// ─────────────────────────────────────────────────────────────

func TestWalBloatAdvisory_CLI_AbsoluteThreshold(t *testing.T) {
	t.Parallel()
	// 600 MiB WAL exceeds the 512 MiB absolute floor.
	got := walBloatAdvisory(2<<30, 600<<20)
	if got == "" {
		t.Fatal("600 MiB WAL should produce an advisory")
	}
	for _, want := range []string{"WAL file is 600 MB", "checkpoint", "pincher vacuum", "#1206"} {
		if !strings.Contains(got, want) {
			t.Errorf("advisory missing %q\n  got: %s", want, got)
		}
	}
}

func TestWalBloatAdvisory_CLI_PercentBloat(t *testing.T) {
	t.Parallel()
	// 2 GiB DB, 250 MiB WAL — under absolute floor but >10% of DB.
	got := walBloatAdvisory(2<<30, 250<<20)
	if got == "" {
		t.Fatal("WAL > 10% of >100 MiB DB should fire even under absolute floor")
	}
	if !strings.Contains(got, "12%") && !strings.Contains(got, "13%") {
		t.Errorf("advisory should mention the ratio; got %s", got)
	}
}

func TestWalBloatAdvisory_CLI_SmallDBSilent(t *testing.T) {
	t.Parallel()
	// 50 MiB DB, 10 MiB WAL — 20% but DB too small for ratio rule.
	if got := walBloatAdvisory(50<<20, 10<<20); got != "" {
		t.Errorf("DB under 100 MiB shouldn't trip the ratio rule; got %q", got)
	}
}

func TestWalBloatAdvisory_CLI_Healthy(t *testing.T) {
	t.Parallel()
	if got := walBloatAdvisory(2<<30, 50<<20); got != "" {
		t.Errorf("50 MiB WAL / 2 GiB DB is healthy; got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────
// nestedProjectAdvisory
// ─────────────────────────────────────────────────────────────

func TestNestedProjectAdvisory_CLI_FlagsNestedPair(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "outer", Path: "/repos/outer", Symbols: 50_000},
		{Name: "inner", Path: "/repos/outer/internal", Symbols: 8000},
	}
	got := nestedProjectAdvisory(projects)
	if got == "" {
		t.Fatal("nested pair should produce an advisory")
	}
	for _, want := range []string{"inner", "outer", "indexed inside", "#1209"} {
		if !strings.Contains(got, want) {
			t.Errorf("advisory missing %q\n  got: %s", want, got)
		}
	}
}

func TestNestedProjectAdvisory_CLI_SiblingsIgnored(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "alpha", Path: "/repos/alpha", Symbols: 5000},
		{Name: "beta", Path: "/repos/beta", Symbols: 5000},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf("siblings shouldn't fire; got %q", got)
	}
}

func TestNestedProjectAdvisory_CLI_PrefixSubstringNotNested(t *testing.T) {
	t.Parallel()
	// `warp_rc` is a prefix of `warp_rc_fork` but not a directory parent.
	projects := []DoctorProjectSummary{
		{Name: "warp_rc", Path: "/repos/warp_rc", Symbols: 5000},
		{Name: "warp_rc_fork", Path: "/repos/warp_rc_fork", Symbols: 5000},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf("substring prefix should not be treated as nested; got %q", got)
	}
}

func TestNestedProjectAdvisory_CLI_CaseAndSeparatorInsensitive(t *testing.T) {
	t.Parallel()
	// Windows-style paths with mixed case — the helper normalises.
	projects := []DoctorProjectSummary{
		{Name: "Outer", Path: `C:\Repos\Outer`, Symbols: 50_000},
		{Name: "Inner", Path: `c:\repos\outer\sub`, Symbols: 8000},
	}
	got := nestedProjectAdvisory(projects)
	if got == "" {
		t.Fatal("case- and separator-mismatched paths should still be detected as nested")
	}
}

func TestNestedProjectAdvisory_CLI_AllFlatReturnsEmpty(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "a", Path: "/a", Symbols: 1000},
		{Name: "b", Path: "/b", Symbols: 1000},
		{Name: "c", Path: "/c", Symbols: 1000},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf("flat layout should produce no advisory; got %q", got)
	}
}

// #1644: pincher-repo's pinned corpus fixtures (testdata/corpus/*) are
// indexed as standalone projects on purpose. The advisory's remediation
// — `pincher project rm <inner>` — would break `make corpus-test`.
func TestNestedProjectAdvisory_CLI_TestdataCorpus_Suppressed(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "pincher-repo", Path: `D:\ClaudeCode\pincher-repo`, Symbols: 8000},
		{Name: "k8s-ops", Path: `D:\ClaudeCode\pincher-repo\testdata\corpus\k8s-ops`, Symbols: 42},
		{Name: "terraform-stack", Path: `D:\ClaudeCode\pincher-repo\testdata\corpus\terraform-stack`, Symbols: 40},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf("testdata/corpus/* fixtures must be suppressed; got advisory %q", got)
	}
}

// #1644 cross-check: suppression is specific. A real nested-project
// mistake (warp-fork inside warp_rc) still fires even when a sibling
// corpus fixture is present.
func TestNestedProjectAdvisory_CLI_MixedRealAndCorpus_FiresOnReal(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "pincher-repo", Path: `D:\ClaudeCode\pincher-repo`, Symbols: 8000},
		{Name: "k8s-ops", Path: `D:\ClaudeCode\pincher-repo\testdata\corpus\k8s-ops`, Symbols: 42},
		{Name: "warp_rc", Path: `D:\ClaudeCode\warp_rc`, Symbols: 1_500_000},
		{Name: "warp-fork", Path: `D:\ClaudeCode\warp_rc\warp-fork`, Symbols: 1_500_000},
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

// #1644: .atrium/work/ worktree paths are also suppressed.
func TestNestedProjectAdvisory_CLI_AtriumWorktree_Suppressed(t *testing.T) {
	t.Parallel()
	projects := []DoctorProjectSummary{
		{Name: "slopbuster", Path: `D:\ClaudeCode\slopbuster`, Symbols: 4500},
		{Name: "i-7", Path: `D:\ClaudeCode\slopbuster\.atrium\work\r-2026-04-14-002\i-7`, Symbols: 874},
	}
	if got := nestedProjectAdvisory(projects); got != "" {
		t.Errorf(".atrium/work/* worktree nesting must be suppressed; got %q", got)
	}
}

// ─────────────────────────────────────────────────────────────
// Parity gate (forward direction): every advisory function the CLI
// declares matches a sibling in the MCP doctor. The compile-time
// match is structural — the runtime parity (identical thresholds +
// message shape) is what these per-advisory tests pin together with
// the matching tests in internal/server/doctor_*_advisory_test.go.
// ─────────────────────────────────────────────────────────────

func TestDoctorAdvisories_CLI_SymbolsExist(t *testing.T) {
	t.Parallel()
	// Compile-time check by referencing the symbols. If any is renamed
	// or removed without a corresponding update, the build breaks here
	// before the runtime tests ever run.
	_ = ghostProjectAdvisory
	_ = walBloatAdvisory
	_ = nestedProjectAdvisory
	_ = normalizePathForNesting
	_ = pluralS
}
