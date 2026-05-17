package ast

import (
	"testing"
)

// #1341 v0.71: the Bash AST extractor was parser-backed at confidence
// 1.0 but emitted zero edges of any kind. Two cheap additions from
// the same AST walk:
//
//   - CALLS — function-to-function calls within a script
//   - IMPORTS — `source other.sh` / `. other.sh` includes
//
// Tests cover positive, negative control (built-in command not in
// the function table doesn't emit CALLS), cross-check (cross-function
// call resolves the FromQN correctly), and IMPORTS / external-target.

func TestExtractBash_FunctionToFunctionCall_EmitsCALLS_1341(t *testing.T) {
	src := []byte(`#!/usr/bin/env bash
foo() {
  echo "hi"
}
bar() {
  foo
  foo arg
}
`)
	result := Extract(src, "Bash", "scripts/helpers.sh")
	if result == nil {
		t.Fatal("nil result")
	}

	// Function symbols present.
	var fooFound, barFound bool
	for _, s := range result.Symbols {
		if s.Name == "foo" {
			fooFound = true
		}
		if s.Name == "bar" {
			barFound = true
		}
	}
	if !fooFound || !barFound {
		t.Fatalf("expected both foo and bar Function symbols; got %d symbols", len(result.Symbols))
	}

	// CALLS edges: bar → foo, ideally twice (two call sites in bar's
	// body), but at minimum once. FromQN = "helpers.bar", ToName =
	// "helpers.foo".
	var callsCount int
	for _, e := range result.Edges {
		if e.Kind == "CALLS" && e.FromQN == "helpers.bar" && e.ToName == "helpers.foo" {
			callsCount++
		}
	}
	if callsCount == 0 {
		t.Errorf("expected at least one CALLS edge from helpers.bar to helpers.foo; got edges: %+v", result.Edges)
	}
}

// Negative control: a CallExpr whose first word is a built-in (echo,
// printf, etc.) or any name not in the file's function table MUST NOT
// emit a CALLS edge. We don't want to synthesize edges into non-
// existent symbols.
func TestExtractBash_BuiltinCall_NoCALLS_1341(t *testing.T) {
	src := []byte(`#!/usr/bin/env bash
greet() {
  echo "hello"
  printf "world\n"
  ls -la /tmp
}
`)
	result := Extract(src, "Bash", "scripts/greet.sh")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, e := range result.Edges {
		if e.Kind == "CALLS" {
			t.Errorf("unexpected CALLS edge for built-in/external command: %+v", e)
		}
	}
}

// IMPORTS: `source ./lib/common.sh` emits an IMPORTS edge with ToName
// = the literal path. The cross-file resolver picks up resolution.
func TestExtractBash_SourceEmitsIMPORTS_1341(t *testing.T) {
	src := []byte(`#!/usr/bin/env bash
source ./lib/common.sh
. ./lib/other.sh
`)
	result := Extract(src, "Bash", "scripts/main.sh")
	if result == nil {
		t.Fatal("nil result")
	}

	var sawCommon, sawOther bool
	for _, e := range result.Edges {
		if e.Kind != "IMPORTS" {
			continue
		}
		switch e.ToName {
		case "./lib/common.sh":
			sawCommon = true
		case "./lib/other.sh":
			sawOther = true
		}
	}
	if !sawCommon {
		t.Errorf("expected IMPORTS edge for `source ./lib/common.sh`; edges: %+v", result.Edges)
	}
	if !sawOther {
		t.Errorf("expected IMPORTS edge for `. ./lib/other.sh`; edges: %+v", result.Edges)
	}
}

// Cross-check: top-level (non-function-scoped) call to an in-file
// function emits a CALLS edge with FromQN="" — the indexer attaches
// it to the file scope, matching the jinja/yaml IMPORTS convention.
func TestExtractBash_TopLevelCALLS_FromQNEmpty_1341(t *testing.T) {
	src := []byte(`#!/usr/bin/env bash
worker() {
  :
}
worker  # top-level invocation
`)
	result := Extract(src, "Bash", "scripts/run.sh")
	if result == nil {
		t.Fatal("nil result")
	}
	var found bool
	for _, e := range result.Edges {
		if e.Kind == "CALLS" && e.ToName == "run.worker" && e.FromQN == "" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected top-level CALLS edge to run.worker with FromQN=\"\"; edges: %+v", result.Edges)
	}
}

// Cross-check: a `source` with no second word (malformed, but the
// parser tolerates it) does NOT crash and does NOT emit a stray
// IMPORTS edge with empty ToName.
func TestExtractBash_SourceWithoutArg_NoEdge_1341(t *testing.T) {
	src := []byte(`#!/usr/bin/env bash
source
`)
	result := Extract(src, "Bash", "scripts/bad.sh")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, e := range result.Edges {
		if e.Kind == "IMPORTS" && e.ToName == "" {
			t.Errorf("expected no IMPORTS edge for argless `source`; got %+v", e)
		}
	}
}
