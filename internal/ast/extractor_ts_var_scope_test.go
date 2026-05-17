package ast

import "testing"

// #1375: TSX/TS regex extractor's varRE matches `const X = ...`
// anywhere — including inside function bodies. Pre-fix, every Variable
// got QN = `<module>.<name>` regardless of enclosing scope, so sibling
// functions each declaring `const res = await fetch(...)` (the
// canonical Next.js App Router page.tsx shape) all collided on
// `<module>.res` → all but one silently dropped by the
// qualified_name_collision guard.
//
// Fix: track the enclosing function's QN + end-line and scope the
// Variable to it (`<module>.<funcName>.<name>`).

// Positive — App Router page.tsx with two async handlers each
// declaring local `const res` and `const json`. All four locals
// must survive with distinct QNs.
func TestExtractTypeScript_VarScope_NextJSAppRouter(t *testing.T) {
	src := `
export async function GET() {
  const res = await fetch("/api/a");
  const json = await res.json();
  return Response.json(json);
}

export async function POST() {
  const res = await fetch("/api/b");
  const json = await res.json();
  return Response.json(json);
}
`
	r := Extract([]byte(src), "TSX", "app/admin/page.tsx")
	if r == nil {
		t.Fatal("nil result")
	}

	gotQNs := make(map[string]int)
	for _, s := range r.Symbols {
		gotQNs[s.QualifiedName]++
	}

	// The pre-fix bug: `app.admin.page.res` would appear ≥2 (with
	// the qualified_name_collision diagnostic firing and only one
	// row surviving the dedup). Post-fix: distinct QNs per function.
	for _, want := range []string{
		"app.admin.page.GET",
		"app.admin.page.POST",
		"app.admin.page.GET.res",
		"app.admin.page.GET.json",
		"app.admin.page.POST.res",
		"app.admin.page.POST.json",
	} {
		if gotQNs[want] == 0 {
			t.Errorf("missing QN %q; got: %v", want, gotQNs)
		}
		if gotQNs[want] > 1 {
			t.Errorf("QN %q appears %d times (would trigger qualified_name_collision)", want, gotQNs[want])
		}
	}

	// And the canonical bug shape must NOT appear: the bare
	// `app.admin.page.res` (parent=module) shouldn't exist.
	if gotQNs["app.admin.page.res"] > 0 {
		t.Errorf("module-scoped `app.admin.page.res` exists (= the pre-fix bug); got: %v", gotQNs)
	}
}

// Positive — Parent stamping mirrors the Method case: the containing
// function's QN is set as Parent so consumers can drill.
func TestExtractTypeScript_VarScope_ParentStamped(t *testing.T) {
	src := `
export function handler() {
  const cache = new Map();
}
`
	r := Extract([]byte(src), "TSX", "app/api/page.tsx")
	var cacheVar *ExtractedSymbol
	for i, s := range r.Symbols {
		if s.QualifiedName == "app.api.page.handler.cache" {
			cacheVar = &r.Symbols[i]
		}
	}
	if cacheVar == nil {
		t.Fatalf("missing scoped variable; got QNs: %v", qnsOf(r.Symbols))
	}
	if cacheVar.Parent != "app.api.page.handler" {
		t.Errorf("Parent = %q, want %q", cacheVar.Parent, "app.api.page.handler")
	}
}

// Negative — true top-level Variable (outside any function body) keeps
// its module-scoped QN. Module-level constants are the common
// `export const config = {...}` shape; scoping them under a function
// would break callers searching for them.
func TestExtractTypeScript_VarScope_ModuleLevelUnchanged(t *testing.T) {
	src := `
export const config = { matcher: "/api/:path*" };

export function handler() {
  const local = 1;
}
`
	r := Extract([]byte(src), "TSX", "app/api/page.tsx")
	var configVar *ExtractedSymbol
	for i, s := range r.Symbols {
		if s.Name == "config" {
			configVar = &r.Symbols[i]
		}
	}
	if configVar == nil {
		t.Fatalf("missing config Variable; got QNs: %v", qnsOf(r.Symbols))
	}
	if configVar.QualifiedName != "app.api.page.config" {
		t.Errorf("module-level config QN = %q, want %q",
			configVar.QualifiedName, "app.api.page.config")
	}
	if configVar.Parent != "" {
		t.Errorf("module-level config Parent = %q, want empty", configVar.Parent)
	}
}

// Negative — Variable AFTER its enclosing function's end-line must
// revert to module scope. Pre-implementation, a naive most-recent-
// function tracker would scope every subsequent Variable under the
// previous function forever.
func TestExtractTypeScript_VarScope_AfterFunctionEnd(t *testing.T) {
	src := `
export function inner() {
  const a = 1;
}

export const outer = 2;
`
	r := Extract([]byte(src), "TSX", "app/x/page.tsx")
	gotQNs := make(map[string]bool)
	for _, s := range r.Symbols {
		gotQNs[s.QualifiedName] = true
	}
	if !gotQNs["app.x.page.inner.a"] {
		t.Errorf("inner-scoped a missing; got: %v", gotQNs)
	}
	// `outer` is declared on a line after inner's end — must NOT
	// inherit inner's scope.
	if gotQNs["app.x.page.inner.outer"] {
		t.Errorf("outer was scoped to inner (=bug: tracker didn't reset past end-line)")
	}
	if !gotQNs["app.x.page.outer"] {
		t.Errorf("module-scoped outer missing; got: %v", gotQNs)
	}
}

func qnsOf(syms []ExtractedSymbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.QualifiedName)
	}
	return out
}
