package ast

import (
	"strings"
	"testing"
)

// Python AST extractor tests. Mirror the javascript_ast_test.go style:
// direct extractor-level assertions for behaviors regex misses, plus
// dispatch + env-var checks. The whole file skips when no working
// CPython 3 is on PATH so CI without Python still passes.

func pythonASTOrSkip(t *testing.T) {
	t.Helper()
	if pythonCommand() == nil {
		t.Skip("no working CPython 3 on PATH; AST tests skipped")
	}
}

func TestPyAST_BasicSymbols(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`def foo():
    return 1

async def bar(x: int) -> str:
    return str(x)

class C:
    def m(self):
        return 2
`)
	r, ok := extractPythonAST(src, "pkg/mod.py")
	if !ok {
		t.Fatal("expected AST parse to succeed on clean Python")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range r.Symbols {
		byName[s.Name] = s
	}
	cases := []struct {
		name, wantKind string
	}{
		{"foo", "Function"},
		{"bar", "Function"},
		{"C", "Class"},
		{"m", "Method"},
	}
	for _, c := range cases {
		s, ok := byName[c.name]
		if !ok {
			t.Errorf("missing symbol %q (got: %v)", c.name, keys(byName))
			continue
		}
		if s.Kind != c.wantKind {
			t.Errorf("%q kind = %q, want %q", c.name, s.Kind, c.wantKind)
		}
	}
}

func TestPyAST_NestedClass(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`class A:
    class B:
        def m(self):
            pass
`)
	r, ok := extractPythonAST(src, "p.py")
	if !ok {
		t.Fatal("parse failed")
	}
	byQN := map[string]ExtractedSymbol{}
	for _, s := range r.Symbols {
		byQN[s.QualifiedName] = s
	}
	for _, qn := range []string{"p.A", "p.A.B", "p.A.B.m"} {
		if _, ok := byQN[qn]; !ok {
			t.Errorf("missing QN %q (got: %v)", qn, keys(byQN))
		}
	}
	// Method's Parent should be the enclosing class QN, not just the
	// class's short name — this is what regex got wrong on nested classes.
	if m := byQN["p.A.B.m"]; m.Parent != "p.A.B" {
		t.Errorf("p.A.B.m.Parent = %q, want %q", m.Parent, "p.A.B")
	}
}

func TestPyAST_AsyncSignature(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`async def fetch(url: str) -> dict:
    return {}
`)
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	var sig string
	for _, s := range r.Symbols {
		if s.Name == "fetch" {
			sig = s.Signature
		}
	}
	if sig == "" {
		t.Fatal("missing 'fetch' symbol")
	}
	if !strings.Contains(sig, "async def") {
		t.Errorf("signature missing 'async def': %q", sig)
	}
	if !strings.Contains(sig, "-> dict") {
		t.Errorf("signature missing return type: %q", sig)
	}
	if !strings.Contains(sig, "url: str") {
		t.Errorf("signature missing annotated arg: %q", sig)
	}
}

func TestPyAST_Decorators(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`class C:
    @property
    def x(self):
        return 1

    @staticmethod
    def s():
        return 2
`)
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range r.Symbols {
		byName[s.Name] = s
	}
	x, sm := byName["x"], byName["s"]
	if !strings.Contains(x.Signature, "@property") {
		t.Errorf("x.Signature missing @property: %q", x.Signature)
	}
	if !strings.Contains(sm.Signature, "@staticmethod") {
		t.Errorf("s.Signature missing @staticmethod: %q", sm.Signature)
	}
	// Decorators don't change the kind — both are still Methods.
	if x.Kind != "Method" || sm.Kind != "Method" {
		t.Errorf("decorated methods should keep Kind=Method, got %q / %q", x.Kind, sm.Kind)
	}
}

func TestPyAST_DunderAll(t *testing.T) {
	pythonASTOrSkip(t)
	// `priv` is not underscore-prefixed but excluded from __all__ — only
	// AST can honor this since regex relies purely on the leading-underscore
	// heuristic.
	src := []byte(`__all__ = ["pub"]

def pub():
    pass

def priv():
    pass
`)
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range r.Symbols {
		byName[s.Name] = s
	}
	if !byName["pub"].IsExported {
		t.Error("pub should be IsExported=true (in __all__)")
	}
	if byName["priv"].IsExported {
		t.Error("priv should be IsExported=false (not in __all__, despite no leading underscore)")
	}
}

func TestPyAST_TypeAnnotations(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`from typing import Dict, List

def f(x: int, y: List[str]) -> Dict[str, int]:
    return {}
`)
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	var sig string
	for _, s := range r.Symbols {
		if s.Name == "f" {
			sig = s.Signature
		}
	}
	for _, want := range []string{"x: int", "y: List[str]", "-> Dict[str, int]"} {
		if !strings.Contains(sig, want) {
			t.Errorf("signature missing %q: got %q", want, sig)
		}
	}
}

func TestPyAST_ImportEdges(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`import os
from typing import List
from .sib import helper
`)
	r, ok := extractPythonAST(src, "pkg/m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	got := map[string]bool{}
	for _, e := range r.Edges {
		if e.Kind != "IMPORTS" {
			t.Errorf("unexpected edge kind: %q", e.Kind)
			continue
		}
		if e.Confidence != 1.0 {
			t.Errorf("import edge confidence = %v, want 1.0", e.Confidence)
		}
		got[e.ToName] = true
	}
	for _, want := range []string{"os", "typing.List", ".sib.helper"} {
		if !got[want] {
			t.Errorf("missing IMPORTS edge to %q (got: %v)", want, keys(got))
		}
	}
}

func TestPyAST_FallbackOnSyntaxError(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`def foo(:
    pass
`)
	if _, ok := extractPythonAST(src, "bad.py"); ok {
		t.Error("expected ok=false on SyntaxError")
	}
}

func TestPyAST_DispatchHonorsDisableEnv(t *testing.T) {
	pythonASTOrSkip(t)
	// With AST disabled, dispatch goes through the regex extractor. The
	// regex path won't emit decorators in the signature — that's our
	// behavioral marker.
	t.Setenv("PINCHER_DISABLE_PY_AST", "1")
	src := []byte(`class C:
    @property
    def x(self):
        return 1
`)
	r := Extract(src, "Python", "m.py")
	for _, s := range r.Symbols {
		if s.Name == "x" && strings.Contains(s.Signature, "@property") {
			t.Errorf("regex path should not emit decorators, but got Signature=%q", s.Signature)
		}
	}
}

func TestPyAST_DispatchUsesASTByDefault(t *testing.T) {
	pythonASTOrSkip(t)
	// Default-on: end-to-end Extract should produce a Signature that
	// includes the decorator (a regex-fallback tell).
	src := []byte(`class C:
    @property
    def x(self):
        return 1
`)
	r := Extract(src, "Python", "m.py")
	var sig string
	for _, s := range r.Symbols {
		if s.Name == "x" {
			sig = s.Signature
		}
	}
	if !strings.Contains(sig, "@property") {
		t.Errorf("default Extract should use AST and surface @property; got Signature=%q", sig)
	}
}

func TestPyAST_ByteOffsetsMatchSource(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte("def foo():\n    return 1\n\ndef bar():\n    return 2\n")
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	for _, s := range r.Symbols {
		if s.StartByte < 0 || s.EndByte > len(src) || s.StartByte >= s.EndByte {
			t.Errorf("invalid byte span for %q: [%d, %d) (len=%d)", s.Name, s.StartByte, s.EndByte, len(src))
			continue
		}
		// The Module symbol spans the whole file; per-def bodies start with `def`.
		if s.Kind == "Module" {
			if s.StartByte != 0 || s.EndByte != len(src) {
				t.Errorf("Module span = [%d, %d), want [0, %d)", s.StartByte, s.EndByte, len(src))
			}
			continue
		}
		body := string(src[s.StartByte:s.EndByte])
		if !strings.HasPrefix(body, "def ") {
			t.Errorf("body for %q should start with 'def ': %q", s.Name, body)
		}
		if !strings.Contains(body, s.Name) {
			t.Errorf("body for %q should contain its name: %q", s.Name, body)
		}
	}
}

// keys returns the keys of a string-keyed map, for error messages.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPyAST_CallsEmitsEdges(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`def helper():
    pass

def caller():
    helper()
    other()
`)
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	calls := callEdgesByFrom(r.Edges, "m.caller")
	if !calls["helper"] && !calls["m.helper"] {
		t.Errorf("expected call edge from m.caller to helper (or m.helper); got %v", calls)
	}
	if !calls["other"] && !calls["m.other"] {
		t.Errorf("expected call edge from m.caller to other; got %v", calls)
	}
}

func TestPyAST_CallsRewriteImportedNames(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`from pkg.sub import widget
from foo import bar as b

def caller():
    widget()
    b()
`)
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	calls := callEdgesByFrom(r.Edges, "m.caller")
	// `widget()` → from-import resolved to its dotted source.
	if !calls["pkg.sub.widget"] {
		t.Errorf("expected call to pkg.sub.widget; got %v", calls)
	}
	// `b()` (aliased) → original dotted target.
	if !calls["foo.bar"] {
		t.Errorf("expected call to foo.bar via alias b; got %v", calls)
	}
}

func TestPyAST_CallsRewriteAttributeChain(t *testing.T) {
	pythonASTOrSkip(t)
	src := []byte(`import requests
import os.path as op

def caller():
    requests.get("https://example.com")
    op.exists("/tmp")
`)
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	calls := callEdgesByFrom(r.Edges, "m.caller")
	// Plain `import requests` keeps the local name on the chain.
	if !calls["requests.get"] {
		t.Errorf("expected call to requests.get; got %v", calls)
	}
	// `import os.path as op` rewrites op.* → os.path.*
	if !calls["os.path.exists"] {
		t.Errorf("expected call to os.path.exists via alias op; got %v", calls)
	}
}

func TestPyAST_CallsRewriteSelfToClass(t *testing.T) {
	pythonASTOrSkip(t)
	// `self.helper()` inside a method should resolve to the class's helper —
	// that's what the resolver can match against the real method QN.
	src := []byte(`class C:
    def helper(self):
        pass

    def caller(self):
        self.helper()
`)
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	calls := callEdgesByFrom(r.Edges, "m.C.caller")
	if !calls["m.C.helper"] {
		t.Errorf("expected self.helper rewritten to m.C.helper; got %v", calls)
	}
}

func TestPyAST_CallsScopedToEnclosingFunction(t *testing.T) {
	pythonASTOrSkip(t)
	// Nested functions get their own from_qn — a call in inner() must NOT
	// appear under outer()'s edges (collect_calls_in_body stops at the
	// nested def boundary).
	src := []byte(`def outer():
    def inner():
        helper()
    inner()
`)
	r, ok := extractPythonAST(src, "m.py")
	if !ok {
		t.Fatal("parse failed")
	}
	outer := callEdgesByFrom(r.Edges, "m.outer")
	inner := callEdgesByFrom(r.Edges, "m.outer.inner")
	if outer["helper"] {
		t.Errorf("outer should not contain inner's helper() call; got %v", outer)
	}
	if !inner["helper"] {
		t.Errorf("inner should contain its helper() call; got %v", inner)
	}
	if !outer["m.outer.inner"] && !outer["inner"] {
		t.Errorf("outer should record its inner() call; got %v", outer)
	}
}

// callEdgesByFrom collects CALLS edges whose FromQN matches fromQN,
// returning a set of ToNames for easy assertion.
func callEdgesByFrom(edges []ExtractedEdge, fromQN string) map[string]bool {
	out := map[string]bool{}
	for _, e := range edges {
		if e.Kind == "CALLS" && e.FromQN == fromQN {
			out[e.ToName] = true
		}
	}
	return out
}
