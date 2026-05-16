package server

import "testing"

// #1114: auditShapePattern's lazy `\w+( \w+){0,3}?` quantifier bottomed
// out at 4 trailing words between the noun and the absence verb, too
// tight for natural-language phrasings like "find handlers in this
// codebase that don't have a test" (5 intervening words). Pre-fix
// these fell through to shapeUnknown / shapeFind and the agent got
// search+context recommendations instead of pinchQL audit-query
// recommendations. Bumped to {0,6}.

func TestClassifyTaskShape_Audit_NaturalLanguageWordCount(t *testing.T) {
	t.Parallel()
	cases := []string{
		// 5+ words between noun and absence verb
		"find handlers in this codebase that don't have a test",
		"list every function in the internal package that doesn't return an error",
		"find symbols anywhere in the repo that have no docstring",
		"surface methods declared in the auth module that are missing a test",
		"find every function in the cmd directory that doesn't have a doc comment",
	}
	for _, task := range cases {
		t.Run(task, func(t *testing.T) {
			if got := classifyTaskShape(task); got != shapeAudit {
				t.Errorf("classifyTaskShape(%q) = %v, want shapeAudit", task, got)
			}
		})
	}
}

// Control: the existing tighter phrasings still classify correctly.
// Regression guard against accidental over-broadening.
func TestClassifyTaskShape_Audit_TightPhrasingsStillWork(t *testing.T) {
	t.Parallel()
	cases := []string{
		"find functions with no callers",
		"find every handler that doesn't return an error",
		"list all functions with no docstring",
	}
	for _, task := range cases {
		t.Run(task, func(t *testing.T) {
			// Note: "find functions with no callers" routes to
			// shapeDeadCode (more specific than shapeAudit). The
			// other two route to shapeAudit. Verify either way that
			// they don't fall to shapeUnknown.
			got := classifyTaskShape(task)
			if got != shapeAudit && got != shapeDeadCode {
				t.Errorf("classifyTaskShape(%q) = %v, want shapeAudit or shapeDeadCode", task, got)
			}
		})
	}
}
