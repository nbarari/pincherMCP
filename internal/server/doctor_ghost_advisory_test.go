package server

import (
	"strings"
	"testing"
)

// #1009: when a project has substantial symbols but ZERO edges, the
// resolver phase silently failed — symbols got persisted but the call
// graph never built. This is the zelosMCP ghost-extraction pattern
// (#815). Pre-fix, doctor reported the totals but didn't flag the
// shape. handleDoctor reads doctor advisories; this test pins the
// pure helper's shape without the DB-backed handler dance.

func TestGhostProjectAdvisory_FlagsSubstantialSymbolsZeroEdges(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "zelosMCP", Symbols: 5000, Files: 100, Edges: 0},
		{Name: "healthy-repo", Symbols: 2000, Files: 80, Edges: 1500},
	}
	got := ghostProjectAdvisory(projects)
	if got == "" {
		t.Fatal("expected advisory for project with 5000 syms / 0 edges; got empty")
	}
	if !strings.Contains(got, "zelosMCP") {
		t.Errorf("advisory must name the ghost project; got %q", got)
	}
	if strings.Contains(got, "healthy-repo") {
		t.Errorf("advisory must NOT name a healthy project; got %q", got)
	}
	if !strings.Contains(got, "ZERO edges") {
		t.Errorf("advisory must call out the zero-edges shape; got %q", got)
	}
	if !strings.Contains(got, "re-index") && !strings.Contains(got, "pincher index") {
		t.Errorf("advisory must suggest remediation; got %q", got)
	}
}

func TestGhostProjectAdvisory_SmallProjectStillHealthy(t *testing.T) {
	t.Parallel()
	// 800 symbols < 1000 threshold; pure-config / pure-docs repos can
	// legitimately land at 0 edges below this scale.
	projects := []doctorProjectSummary{
		{Name: "tiny-docs-repo", Symbols: 800, Files: 40, Edges: 0},
	}
	if got := ghostProjectAdvisory(projects); got != "" {
		t.Errorf("expected NO advisory below 1000-symbol threshold; got %q", got)
	}
}

func TestGhostProjectAdvisory_AllHealthy_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	projects := []doctorProjectSummary{
		{Name: "a", Symbols: 5000, Files: 100, Edges: 1500},
		{Name: "b", Symbols: 2000, Files: 80, Edges: 800},
	}
	if got := ghostProjectAdvisory(projects); got != "" {
		t.Errorf("expected empty advisory for all-healthy projects; got %q", got)
	}
}

func TestGhostProjectAdvisory_CapsAtThreeGhosts(t *testing.T) {
	t.Parallel()
	// Five ghosts of decreasing size. Advisory must mention top 3,
	// not balloon into a wall of text.
	// Names chosen so substring checks don't false-positive against the
	// advisory's own boilerplate (e.g. "ghost-extraction signature").
	// Quote-suffix is the disambiguator.
	projects := []doctorProjectSummary{
		{Name: "zombie-alpha", Symbols: 50000, Edges: 0},
		{Name: "zombie-bravo", Symbols: 40000, Edges: 0},
		{Name: "zombie-charlie", Symbols: 30000, Edges: 0},
		{Name: "zombie-delta", Symbols: 20000, Edges: 0},
		{Name: "zombie-echo", Symbols: 10000, Edges: 0},
	}
	got := ghostProjectAdvisory(projects)
	if !strings.Contains(got, "zombie-alpha") || !strings.Contains(got, "zombie-bravo") || !strings.Contains(got, "zombie-charlie") {
		t.Errorf("expected top 3 ghosts named; got %q", got)
	}
	if strings.Contains(got, "zombie-delta") || strings.Contains(got, "zombie-echo") {
		t.Errorf("advisory must cap at 3 ghosts to stay scannable; got %q", got)
	}
}

func TestGhostProjectAdvisory_PluralizesCorrectly(t *testing.T) {
	t.Parallel()
	one := []doctorProjectSummary{{Name: "lonely", Symbols: 5000, Edges: 0}}
	if got := ghostProjectAdvisory(one); !strings.Contains(got, "project with") {
		t.Errorf("singular phrasing for 1 ghost; got %q", got)
	}
	two := []doctorProjectSummary{
		{Name: "a", Symbols: 5000, Edges: 0},
		{Name: "b", Symbols: 4000, Edges: 0},
	}
	if got := ghostProjectAdvisory(two); !strings.Contains(got, "projects with") {
		t.Errorf("plural phrasing for 2+ ghosts; got %q", got)
	}
}
