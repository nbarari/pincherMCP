package server

import "testing"

// #1012: auditShapePattern previously matched only the literal
// "doesn't have" / "does not have" for negated-verb absence. Tasks
// using any OTHER verb after "doesn't" — "doesn't run", "doesn't
// return", "doesn't compile" — fell through to shapeTest / shapeFix /
// shapeFind, recommending BM25 of the literal phrase instead of the
// pinchQL audit query path. Same family as #992 (made every|all|any
// optional) and #924 (untested/undocumented adjective).
//
// Behavior pinned: any "doesn't <verb>" / "don't <verb>" / "does not
// <verb>" / "do not <verb>" trailing-verb form must classify as
// shapeAudit when preceded by find/list/count/show/surface + a noun.

func TestClassifyTaskShape_AuditDoesntVerb(t *testing.T) {
	t.Parallel()
	for _, task := range []string{
		"find every test that doesn't run in parallel",
		"find every handler that doesn't return an error",
		"list functions that doesn't compile",
		"find all callbacks that don't return a promise",
		"surface every config that does not have a default",
		"count fields that do not validate input",
	} {
		t.Run(task, func(t *testing.T) {
			got := classifyTaskShape(task)
			if got != shapeAudit {
				t.Errorf("classifyTaskShape(%q) = %v, want shapeAudit", task, got)
			}
		})
	}
}

// Confirm the original "doesn't have" path still routes to audit —
// the regex rewrite folds "doesn't have" into the broader pattern, so
// this is a guard against accidental regression.
func TestClassifyTaskShape_AuditDoesntHaveStillWorks(t *testing.T) {
	t.Parallel()
	for _, task := range []string{
		"find every function that doesn't have a docstring",
		"list functions that does not have an error return",
	} {
		t.Run(task, func(t *testing.T) {
			if got := classifyTaskShape(task); got != shapeAudit {
				t.Errorf("classifyTaskShape(%q) = %v, want shapeAudit", task, got)
			}
		})
	}
}
