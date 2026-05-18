package ast

import (
	"testing"
)

// #1423 v0.72: Go READS-pass binding-pass falsely emits a CALLS edge
// when a parameter (or other local) name shadows a project Function
// with the same name. Pre-fix repro: `func finishIndexSpan(...,
// totalSymbols int, ...)` referencing `totalSymbols` in the body
// produced a READS edge with ToName="totalSymbols"; the #565 binding-
// pass then converted that to a phantom CALLS edge to the test
// helper `func totalSymbols(...)`. Real impact: polluted `trace`
// (test helper appears called from production), polluted `dead_code`
// (test helper looks "live" via the spurious caller), and any param
// name that matches a project Function (`count`, `value`, `data`,
// `result`, `total*`, `err`, `index`) cross-binds the same way.
//
// Table shape (#1152): positive (shadowed param doesn't emit READS),
// negative (true call to same-named function still emits CALLS),
// control (unrelated reads still flow through correctly), cross-check
// (selector reads `local.Field` still emit so #760's field-type path
// keeps working).

// Positive — parameter `totalSymbols` shadows a hypothetical
// project Function named totalSymbols. The body reads the parameter;
// no READS edge to it should leave the extractor (and so no spurious
// CALLS edge from the #565 binding-pass can manufacture).
func TestExtractGoReads_ParamShadow_NoFalseRead(t *testing.T) {
	src := `package svc
func finishIndexSpan(totalFiles, totalSymbols int) {
	_ = totalFiles
	_ = totalSymbols
}
`
	r := Extract([]byte(src), "Go", "svc/svc.go")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, e := range r.Edges {
		if e.Kind != "READS" {
			continue
		}
		if e.ToName == "totalSymbols" || e.ToName == "totalFiles" {
			t.Errorf("extractor emitted READS to %q (parameter, should be skipped — #1423 bug)", e.ToName)
		}
	}
}

// Positive — receiver shadow: receiver name `s` reads with no
// selector must NOT emit READS.
func TestExtractGoReads_ReceiverShadow_NoFalseRead(t *testing.T) {
	src := `package svc
type S struct{}
func (s *S) M() {
	_ = s
}
`
	r := Extract([]byte(src), "Go", "svc/svc.go")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, e := range r.Edges {
		if e.Kind == "READS" && e.ToName == "s" {
			t.Errorf("extractor emitted READS to receiver %q (should be skipped — #1423)", e.ToName)
		}
	}
}

// Positive — in-body local: `var local int` references must NOT
// emit READS for `local`.
func TestExtractGoReads_InBodyLocal_NoFalseRead(t *testing.T) {
	src := `package svc
func f() {
	var local int
	_ = local
}
`
	r := Extract([]byte(src), "Go", "svc/svc.go")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, e := range r.Edges {
		if e.Kind == "READS" && e.ToName == "local" {
			t.Errorf("extractor emitted READS to in-body local %q (should be skipped — #1423)", e.ToName)
		}
	}
}

// Negative — a REAL call to a function with the same name as a
// parameter still emits a CALLS edge. The call subject is owned by
// extractGoCalls (not the READS pass), so the param-shadow filter on
// READS doesn't suppress it.
func TestExtractGoReads_ParamShadow_RealCallStillEmitsCALLS(t *testing.T) {
	src := `package svc
func helper() {}
func f(helper int) {
	helper()
	_ = helper
}
`
	r := Extract([]byte(src), "Go", "svc/svc.go")
	if r == nil {
		t.Fatal("nil result")
	}
	// Note: `helper()` here calls a different scope than the local
	// `helper int`; in Go this would actually be a compile error
	// (can't call an int). For the extractor's purposes the body's
	// call-site emits a CALLS edge regardless — the test asserts
	// the CALLS edge survives even when the READS for the param is
	// suppressed by #1423.
	var sawCall, sawRead bool
	for _, e := range r.Edges {
		if e.FromQN != "svc.f" {
			continue
		}
		if e.Kind == "CALLS" && e.ToName == "helper" {
			sawCall = true
		}
		if e.Kind == "READS" && e.ToName == "helper" {
			sawRead = true
		}
	}
	if !sawCall {
		t.Error("CALLS edge to helper missing — real call must still emit even when param shadows the name")
	}
	if sawRead {
		t.Error("READS edge to helper present — shadowed param should be skipped (#1423)")
	}
}

// Control — package-level Variable reads still emit READS. The
// shadowing filter only fires when the name is a known local; a
// genuine cross-function package-var read must not be suppressed.
func TestExtractGoReads_PackageVarRead_StillEmits(t *testing.T) {
	src := `package svc
var Cache int
func f() {
	_ = Cache
}
`
	r := Extract([]byte(src), "Go", "svc/svc.go")
	if r == nil {
		t.Fatal("nil result")
	}
	var found bool
	for _, e := range r.Edges {
		if e.Kind == "READS" && e.ToName == "Cache" && e.FromQN == "svc.f" {
			found = true
			break
		}
	}
	if !found {
		t.Error("package-var Cache READS missing — non-local names must still emit (regression guard)")
	}
}

// Cross-check — selector read `local.Field` still emits the
// `.Field` READS even though `local` is a parameter. The selector
// path stamps BaseType so the #760 / binding-pass logic can do
// type-aware filtering further downstream.
func TestExtractGoReads_SelectorOnLocal_StillEmitsField(t *testing.T) {
	src := `package svc
type T struct{ Field int }
func f(local *T) {
	_ = local.Field
}
`
	r := Extract([]byte(src), "Go", "svc/svc.go")
	if r == nil {
		t.Fatal("nil result")
	}
	var fieldRead bool
	for _, e := range r.Edges {
		if e.Kind == "READS" && e.ToName == "Field" && e.FromQN == "svc.f" {
			fieldRead = true
			if e.BaseType == "" {
				t.Errorf("selector READS on Field missing BaseType — #760 path broken")
			}
		}
		if e.Kind == "READS" && e.ToName == "local" {
			t.Errorf("bare-name READS to local parameter %q surfaced — #1423 filter regressed", e.ToName)
		}
	}
	if !fieldRead {
		t.Error("selector READS on Field missing — #760 path broken")
	}
}
