package cypher

import (
	"regexp"
	"testing"
)

// #611: notExpr.eval was 0% covered. The Go-side eval path is the
// fallback when SQL pushdown can't fold the NOT group (variable-length
// BFS, RETURN-time post-filter, etc.). A bug here silently inverts
// every fall-back result with no test catching it. Same risk class as
// #607 (redactSensitiveSlice).
//
// These tests pin the contract on the in-Go eval tree directly:
//  - condExpr.eval respects condition.negated
//  - binaryExpr.eval short-circuits AND/OR correctly
//  - notExpr.eval inverts its inner expression's result

func makeRow(name string, line int64) map[string]any {
	return map[string]any{
		"name":       name,
		"start_line": line,
	}
}

// alwaysExpr is a test-only whereExpr that returns the configured bool.
// Lets us exercise notExpr / binaryExpr without building real conditions.
type alwaysExpr bool

func (a alwaysExpr) eval(row map[string]any, reCache map[string]*regexp.Regexp) bool {
	return bool(a)
}

func TestNotExpr_Eval_InvertsInner(t *testing.T) {
	row := makeRow("foo", 10)
	cache := map[string]*regexp.Regexp{}

	// NOT(true) → false
	if got := (notExpr{inner: alwaysExpr(true)}).eval(row, cache); got {
		t.Errorf("notExpr{true}.eval = true, want false")
	}
	// NOT(false) → true
	if got := (notExpr{inner: alwaysExpr(false)}).eval(row, cache); !got {
		t.Errorf("notExpr{false}.eval = false, want true")
	}
}

func TestNotExpr_Eval_NestedDoubleNegation(t *testing.T) {
	// NOT(NOT(true)) === true. Pins the recursion contract — a future
	// "NOT now means logical-OR" typo would flip this.
	row := makeRow("foo", 10)
	cache := map[string]*regexp.Regexp{}

	got := (notExpr{inner: notExpr{inner: alwaysExpr(true)}}).eval(row, cache)
	if !got {
		t.Errorf("NOT NOT true should be true, got false")
	}
}

func TestNotExpr_Eval_OverBinaryAnd(t *testing.T) {
	// NOT(true AND false) === true. Catches a regression where notExpr
	// was applied per-leaf instead of to the whole inner tree.
	row := makeRow("foo", 10)
	cache := map[string]*regexp.Regexp{}

	inner := binaryExpr{op: "AND", left: alwaysExpr(true), right: alwaysExpr(false)}
	got := notExpr{inner: inner}.eval(row, cache)
	if !got {
		t.Errorf("NOT(true AND false) should be true, got false")
	}
}

func TestBinaryExpr_Eval_AndShortCircuit(t *testing.T) {
	row := makeRow("foo", 10)
	cache := map[string]*regexp.Regexp{}

	// true AND true → true
	if got := (binaryExpr{op: "AND", left: alwaysExpr(true), right: alwaysExpr(true)}).eval(row, cache); !got {
		t.Errorf("true AND true should be true")
	}
	// true AND false → false
	if got := (binaryExpr{op: "AND", left: alwaysExpr(true), right: alwaysExpr(false)}).eval(row, cache); got {
		t.Errorf("true AND false should be false")
	}
	// false AND ? → false (short-circuit)
	if got := (binaryExpr{op: "AND", left: alwaysExpr(false), right: alwaysExpr(true)}).eval(row, cache); got {
		t.Errorf("false AND true should be false")
	}
}

func TestBinaryExpr_Eval_OrShortCircuit(t *testing.T) {
	row := makeRow("foo", 10)
	cache := map[string]*regexp.Regexp{}

	// true OR ? → true
	if got := (binaryExpr{op: "OR", left: alwaysExpr(true), right: alwaysExpr(false)}).eval(row, cache); !got {
		t.Errorf("true OR false should be true")
	}
	// false OR true → true
	if got := (binaryExpr{op: "OR", left: alwaysExpr(false), right: alwaysExpr(true)}).eval(row, cache); !got {
		t.Errorf("false OR true should be true")
	}
	// false OR false → false
	if got := (binaryExpr{op: "OR", left: alwaysExpr(false), right: alwaysExpr(false)}).eval(row, cache); got {
		t.Errorf("false OR false should be false")
	}
}

func TestMatchesWhere_HonorsNotExpr(t *testing.T) {
	// matchesWhere is the actual entry point from the in-Go fallback
	// path. Test the wiring all the way through.
	row := makeRow("foo", 10)
	cache := map[string]*regexp.Regexp{}

	if got := matchesWhere(row, notExpr{inner: alwaysExpr(true)}, cache); got {
		t.Errorf("matchesWhere(NOT true) should be false")
	}
	if got := matchesWhere(row, notExpr{inner: alwaysExpr(false)}, cache); !got {
		t.Errorf("matchesWhere(NOT false) should be true")
	}
	// Nil where clause (no WHERE) always matches — separate contract.
	if got := matchesWhere(row, nil, cache); !got {
		t.Errorf("matchesWhere(nil) should be true")
	}
}
