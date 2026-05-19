package ast

import (
	"strings"
	"testing"
)

// #1622 v0.87: real Go file with heavy predeclared-identifier use.
// Pre-fix, every appearance of `int` / `string` / `error` / `len`
// in a function body emitted a READS pending edge that the resolver
// would later drop (no project Variable by that name). New
// isPredeclaredOrBlank coverage drops them at extract time.
//
// These tests pin the contract that real-world Go patterns no longer
// produce READS edges for predeclared identifiers — the asserted
// invariant is "for each predeclared name N, there is NO READS edge
// from any caller to N."

func TestExtractGoReads_NoReadsForPredeclaredTypes_1622(t *testing.T) {
	t.Parallel()
	// Source exercises the major shapes where predeclared types appear:
	// - var decl with explicit type
	// - type assertion
	// - type conversion (call subject, already filtered)
	// - composite literal type
	// - function signature parameter / return
	// - error / string / int as type names in any of the above
	src := []byte(`package p

func Sum(xs []int) int {
	total := 0
	for _, v := range xs {
		total += v
	}
	return total
}

func Concat(parts []string) string {
	var out string
	for _, p := range parts {
		out = out + p
	}
	return out
}

func TryAssert(x any) error {
	if s, ok := x.(string); ok {
		_ = s
	}
	var err error
	return err
}

func MakeSlice(n int) []byte {
	return make([]byte, n)
}
`)
	edges := extractGoEdges(t, src)
	for _, predeclaredType := range []string{
		"int", "string", "byte", "error", "any",
	} {
		for _, e := range edges {
			if strings.EqualFold(e.Kind, "READS") && e.ToName == predeclaredType {
				t.Errorf("found READS edge from %s -> %s; predeclared type must not generate READS pending edges (#1622)",
					e.FromQN, e.ToName)
			}
		}
	}
}

func TestExtractGoReads_NoReadsForPredeclaredBuiltins_1622(t *testing.T) {
	t.Parallel()
	// Builtin functions normally appear as call subjects (which
	// extractGoCalls owns and the bare-ident-call-subject branch
	// skips). This test pins the SECONDARY case: builtins passed
	// or referenced by value, where they DO get walked by the
	// generic read path.
	src := []byte(`package p

import "fmt"

type LenFunc func(string) int

func Pick(use bool) LenFunc {
	if use {
		return len  // bare-ident value reference, not a call
	}
	return nil
}

func WithBuiltins() int {
	// Bare value references in expression position. The make/new
	// shape almost always appears as a call; len/cap/append can
	// genuinely be referenced as values when wrapping or passing
	// to higher-order functions.
	f := len
	g := append
	_ = f
	_ = g
	fmt.Println("noop")
	return 0
}
`)
	edges := extractGoEdges(t, src)
	for _, builtin := range []string{
		"len", "append", "make", "new", "cap", "panic", "recover",
	} {
		for _, e := range edges {
			if strings.EqualFold(e.Kind, "READS") && e.ToName == builtin {
				t.Errorf("found READS edge from %s -> %s; predeclared builtin must not generate READS pending edges (#1622)",
					e.FromQN, e.ToName)
			}
		}
	}
}

// Control: project-level identifiers MUST still emit READS edges.
// Pre-fix the extractor was over-emitting; the fix narrows the
// drop, but a regression that broadened it would silently drop
// real reads. Pin a tiny known-good case.
func TestExtractGoReads_ProjectVariablesStillEmitReads_1622(t *testing.T) {
	t.Parallel()
	src := []byte(`package p

var (
	GlobalCounter int
	GlobalName    string
)

func ReadsBoth() {
	_ = GlobalCounter
	_ = GlobalName
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.ReadsBoth", "GlobalCounter", "READS")
	requireEdge(t, edges, "p.ReadsBoth", "GlobalName", "READS")
}
