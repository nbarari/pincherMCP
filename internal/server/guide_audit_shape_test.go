package server

import (
	"strings"
	"testing"
)

// #467: `guide task="find an undocumented exported function"` previously
// returned a BM25 search recommendation for the literal phrase, which
// matches nothing. The fix recognises audit shapes and routes to
// pinchQL `query` against the docstring property (#438).

func TestClassifyTaskShape_AuditUndocumented(t *testing.T) {
	t.Parallel()
	cases := []string{
		"find an undocumented exported function",
		"list functions with no docstring",
		"survey undocumented APIs",
		"every exported method missing docstring",
		"functions without docstring",
		"audit functions missing comment",
	}
	for _, task := range cases {
		t.Run(task, func(t *testing.T) {
			got := classifyTaskShape(task)
			if got != shapeAudit {
				t.Errorf("classifyTaskShape(%q) = %v, want shapeAudit", task, got)
			}
		})
	}
}

// #608: the original #467 trigger only fired on docstring-flavored
// phrases ("undocumented", "no docstring"). The structural-audit
// shape is broader — any "find every <thing> without <other thing>"
// phrasing should route to query, not BM25 search of the literal
// phrase. Regression test for the canonical examples that were
// silently falling through to shapeFind.
func TestClassifyTaskShape_AuditEveryWithoutPattern(t *testing.T) {
	t.Parallel()
	// #780: "find every X with no callers" / "zero callers" phrasings
	// deliberately excluded here — they match auditShapePattern but a
	// task naming callers is a dead-code survey, so shapeDeadCode (which
	// runs first) claims them. See TestClassifyTaskShape_DeadCode.
	cases := []string{
		"find every function without a test",
		"list every endpoint without auth",
		"count every method that lacks a return type",
		"find any handler that has no error return",
		"surface all migrations without a rollback",
		"find every public field that doesn't have a tag",
	}
	for _, task := range cases {
		t.Run(task, func(t *testing.T) {
			got := classifyTaskShape(task)
			if got != shapeAudit {
				t.Errorf("classifyTaskShape(%q) = %v, want shapeAudit", task, got)
			}
		})
	}
}

// #992: tasks that name an audited noun directly ("find symbols that
// have no test coverage") were dropping to shapeFind because the regex
// required the `every|all|any` quantifier. The absence phrase is the
// load-bearing audit signal — the quantifier is optional.
func TestClassifyTaskShape_AuditWithoutQuantifier(t *testing.T) {
	t.Parallel()
	cases := []string{
		"find symbols that have no test coverage",
		"list functions without docstrings",
		"find handlers missing error returns",
		"surface migrations without rollback",
		"find tests with no assertions",
		"show methods that doesn't have a receiver",
	}
	for _, task := range cases {
		t.Run(task, func(t *testing.T) {
			got := classifyTaskShape(task)
			if got != shapeAudit {
				t.Errorf("classifyTaskShape(%q) = %v, want shapeAudit", task, got)
			}
		})
	}
}

func TestClassifyTaskShape_AuditDoesNotOvercatch(t *testing.T) {
	t.Parallel()
	// Generic find / understand tasks should NOT fall into shapeAudit.
	cases := map[string]guideShape{
		"find the auth middleware":           shapeFind,
		"understand how indexing works":      shapeUnderstand,
		"fix the docstring extraction bug":   shapeFix,
		"add docstring lookup hint":          shapeAdd,
	}
	for task, want := range cases {
		t.Run(task, func(t *testing.T) {
			if got := classifyTaskShape(task); got != want {
				t.Errorf("classifyTaskShape(%q) = %v, want %v", task, got, want)
			}
		})
	}
}

func TestGuideRecommendations_AuditEmitsPinchQL(t *testing.T) {
	t.Parallel()
	recs := guideRecommendations(shapeAudit, "undocumented exported functions", "", "find undocumented exported functions")
	if len(recs) == 0 {
		t.Fatal("audit shape should produce at least one recommendation")
	}
	first := recs[0]
	if first["tool"] != "query" {
		t.Errorf("first recommendation tool = %q, want query", first["tool"])
	}
	args := first["args"]
	if !strings.Contains(args, "docstring IS NULL") {
		t.Errorf("audit query should filter on docstring IS NULL; got args=%q", args)
	}
	if !strings.Contains(args, "is_exported=true") {
		t.Errorf("audit query should filter on is_exported=true; got args=%q", args)
	}
}

// #921: pre-fix, every shapeAudit task got the docstring/is_exported
// template — including threshold audits like "find every function with
// cyclomatic complexity above 20." inferAuditPinchQL routes on the
// task's keywords so the recommended pinchql actually addresses the
// audit the user described.
func TestInferAuditPinchQL_RoutesByKeyword(t *testing.T) {
	t.Parallel()
	cases := []struct {
		task         string
		wantContains string
	}{
		{"find every function with cyclomatic complexity above 20", "n.complexity"},
		{"list functions with complexity over 15", "n.complexity"},
		// #928: pinchQL doesn't support arithmetic in WHERE/RETURN
		// yet; line-count templates project start_line + end_line for
		// client-side diff until the engine catches up.
		{"surface functions longer than 100 lines", "n.start_line, n.end_line"},
		{"find functions with more than 80 lines", "n.start_line, n.end_line"},
		{"show me untested exported functions", "is_exported=true AND n.is_test=false"},
		{"functions missing test coverage", "is_exported=true AND n.is_test=false"},
		// Default fallback — the canonical #467 docstring example.
		{"find undocumented exported functions", "docstring IS NULL"},
		{"audit exported APIs", "docstring IS NULL"},
	}
	for _, c := range cases {
		t.Run(c.task, func(t *testing.T) {
			got, _ := inferAuditPinchQL(c.task)
			if !strings.Contains(got, c.wantContains) {
				t.Errorf("inferAuditPinchQL(%q) = %q, want it to contain %q", c.task, got, c.wantContains)
			}
		})
	}
}

// End-to-end shape: a threshold audit must produce a pinchql query
// that addresses the threshold the user named, not the docstring
// template.
func TestGuideRecommendations_ThresholdAuditUsesComplexityQuery(t *testing.T) {
	t.Parallel()
	recs := guideRecommendations(shapeAudit, "complexity above 20", "",
		"find every function with cyclomatic complexity above 20")
	if len(recs) == 0 {
		t.Fatal("audit shape should produce at least one recommendation")
	}
	first := recs[0]
	if first["tool"] != "query" {
		t.Errorf("first recommendation tool = %q, want query", first["tool"])
	}
	args := first["args"]
	if !strings.Contains(args, "n.complexity") {
		t.Errorf("threshold audit query must project n.complexity; got args=%q", args)
	}
	if strings.Contains(args, "docstring IS NULL") {
		t.Errorf("threshold audit must NOT emit the docstring template; got args=%q", args)
	}
}
