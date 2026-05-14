package server

import (
	"strings"
	"testing"
)

// guide had no awareness of the dead_code tool: a task like "find
// functions with zero callers" routed to shapeTest (matched "coverage")
// and recommended search+context — never dead_code, the purpose-built
// tool. And the domainConcepts "callers" pattern wrongly prepended a
// trace-internals source pointer to any task mentioning callers.

func TestClassifyTaskShape_DeadCode(t *testing.T) {
	t.Parallel()
	cases := []string{
		"find functions that have zero test coverage and zero callers",
		"find dead code in the server package",
		"list unused functions",
		"which methods are never called",
		"surface unreachable code",
		"find uncalled helpers",
		// #768: "no callers" missed these because a word sits between
		// "no" and "callers" / the phrasing is a synonym.
		"which methods have no inbound callers",
		"functions with no inbound edges",
		"find helpers that nothing calls",
		"list types that are never used",
		// #780: these match auditShapePattern ("find/show/list every|all|
		// any X with no Y") but name callers — pre-fix the audit regex
		// ran first and routed them to shapeAudit, which recommends the
		// hardcoded `docstring IS NULL` query, ignoring the actual task.
		// shapeDeadCode now runs before auditShapePattern.
		"show every exported symbol with no callers",
		"list any function with zero callers",
		"find all exported functions that have no callers",
	}
	for _, task := range cases {
		t.Run(task, func(t *testing.T) {
			if got := classifyTaskShape(task); got != shapeDeadCode {
				t.Errorf("classifyTaskShape(%q) = %v, want shapeDeadCode", task, got)
			}
		})
	}
}

// "find FPs in dead_code" carries the tool name and must still route to
// shapeToolAudit (empirical output audit), not shapeDeadCode.
func TestClassifyTaskShape_DeadCodeToolAuditStillWins(t *testing.T) {
	t.Parallel()
	if got := classifyTaskShape("find false positives in dead_code"); got != shapeToolAudit {
		t.Errorf("got %v, want shapeToolAudit", got)
	}
}

func TestGuideRecommendations_DeadCodeEmitsDeadCodeTool(t *testing.T) {
	t.Parallel()
	recs := guideRecommendations(shapeDeadCode, "zero callers", "")
	if len(recs) == 0 {
		t.Fatal("dead_code shape should produce recommendations")
	}
	if recs[0]["tool"] != "dead_code" {
		t.Errorf("first recommendation tool = %q, want dead_code", recs[0]["tool"])
	}
}

// domainConceptHint must no longer fire on a bare "callers" mention —
// "find functions with zero callers" is a dead-code survey, not a
// request for trace's internal implementation.
func TestDomainConceptHint_NoTraceInternalsOnBareCallers(t *testing.T) {
	t.Parallel()
	if hint := domainConceptHint("find functions that have zero callers"); hint != nil {
		t.Errorf("bare 'callers' should not match a domain concept; got %v", *hint)
	}
	// But an explicit trace-internals task still matches.
	hint := domainConceptHint("how does trace work internally")
	if hint == nil || (*hint)["tool"] != "search" {
		t.Errorf("'how does trace' should still point at trace internals; got %v", hint)
	}
	if hint != nil && !strings.Contains((*hint)["args"], "traceViaCTE") {
		t.Errorf("trace-internals hint should reference traceViaCTE; got %v", *hint)
	}
}
