package ast

import (
	"strings"
	"testing"
)

// #1422 v0.72: pre-#1422, TS/JS regex extractor's single-slot
// most-recent-function tracker (`currentFuncQN`) was overwritten
// when a nested `async function` was detected. After the inner
// function ended, the slot still pointed at the (now-ended)
// inner. Sibling inner functions each declaring `const res =
// fetch(...)` ALL fell back to module scope because their
// `lineNum > currentFuncEnd` (their parent had been overwritten,
// then expired). The qualified_name_collision guard then
// dropped all but one — silent symbol loss on every Next.js App
// Router page.tsx file with two-or-more fetch helpers.
//
// Doctor confirmed the collision pattern on real dogfood
// projects: `app.admin.page.res` repeated 6×, `app.builder.page.
// onChange` repeated 2×, etc.
//
// Fix: stack-based function tracker. Push on funcRE match, pop
// at top of each loop iteration when current line moves past
// stack.top.endLine, Variable scopes to stack.top.qn (innermost
// active function).

// Positive — two sibling inner functions each declare `const
// res`. Pre-fix both module-scoped to `pkg.page.res` and
// collided. Post-fix each scopes to its inner function.
func TestExtractTS_NestedFunctions_SiblingResVariablesDistinctScopes(t *testing.T) {
	src := `export default function AdminPage() {
  async function fetchRuns() {
    const res = await fetch("/api/runs");
    return res;
  }
  async function fetchWorkflows() {
    const res = await fetch("/api/wf");
    return res;
  }
}
`
	r := Extract([]byte(src), "TypeScript", "pkg/page.tsx")
	if r == nil {
		t.Fatal("nil result")
	}
	// Walk every Variable; map QN→count.
	qnCounts := map[string]int{}
	for _, s := range r.Symbols {
		if s.Kind == "Variable" && s.Name == "res" {
			qnCounts[s.QualifiedName]++
		}
	}
	// Pre-fix: both `res` collide at `pkg.page.res` → count=2 in
	// the same QN, dedup keeps one, both functions lose their
	// `res` Variable.
	// Post-fix: distinct QNs, count=1 each.
	wantQNs := []string{
		"pkg.page.AdminPage.fetchRuns.res",
		"pkg.page.AdminPage.fetchWorkflows.res",
	}
	for _, want := range wantQNs {
		if qnCounts[want] != 1 {
			t.Errorf("expected exactly one Variable with QN %q; got count=%d (full map: %v)",
				want, qnCounts[want], qnCounts)
		}
	}
	// Negative — no Variable should collapse to the module-level
	// `pkg.page.res` (the pre-fix bug shape).
	if qnCounts["pkg.page.res"] > 0 {
		t.Errorf("Variable collapsed to module-level QN pkg.page.res (count=%d) — #1422 bug shape", qnCounts["pkg.page.res"])
	}
}

// Cross-check — outer-then-inner-then-outer-var. After the
// inner function ends, the outer's stack frame must be restored
// so subsequent outer Variables scope to the outer function.
// Pre-fix the outer's frame was overwritten and never popped
// back, so the second outer Variable fell to module scope.
func TestExtractTS_NestedFunctions_OuterVarAfterInnerEnds_StaysScopedToOuter(t *testing.T) {
	src := `export default function Page() {
  const x = 1;
  async function fetchRuns() {
    const inner = 2;
  }
  const y = 3;
}
`
	r := Extract([]byte(src), "TypeScript", "pkg/page.tsx")
	if r == nil {
		t.Fatal("nil result")
	}
	got := map[string]string{} // name → qn
	for _, s := range r.Symbols {
		if s.Kind == "Variable" {
			got[s.Name] = s.QualifiedName
		}
	}
	wantOuter := "pkg.page.Page.x"
	wantInner := "pkg.page.Page.fetchRuns.inner"
	wantOuterAfter := "pkg.page.Page.y"
	if got["x"] != wantOuter {
		t.Errorf("x (declared before inner fn): got QN %q; want %q", got["x"], wantOuter)
	}
	if got["inner"] != wantInner {
		t.Errorf("inner (declared inside fetchRuns): got QN %q; want %q", got["inner"], wantInner)
	}
	// This is the crux: pre-fix y would scope to "pkg.page.y"
	// because the single slot still pointed at fetchRuns (then
	// fell to module when lineNum > fetchRuns.end).
	if got["y"] != wantOuterAfter {
		t.Errorf("y (declared in outer AFTER inner exits): got QN %q; want %q (post-#1422 must pop back to outer scope)", got["y"], wantOuterAfter)
	}
}

// Control — top-level module Variables get bare-module scope
// (no enclosing function). Existing behavior must not regress.
func TestExtractTS_TopLevelModuleVariable_ScopedToModuleOnly(t *testing.T) {
	src := `export const TIMEOUT_MS = 5000;
export const RETRIES = 3;
`
	r := Extract([]byte(src), "TypeScript", "pkg/constants.ts")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, s := range r.Symbols {
		if s.Kind == "Variable" {
			if !strings.HasPrefix(s.QualifiedName, "pkg.constants.") {
				t.Errorf("top-level module Variable %q got non-module QN %q",
					s.Name, s.QualifiedName)
			}
			// Negative — no enclosing-function segment.
			if strings.Count(s.QualifiedName, ".") > 2 {
				t.Errorf("top-level Variable %q has unexpected nesting in QN %q", s.Name, s.QualifiedName)
			}
		}
	}
}

// Cross-check — deep nesting (3 levels). Stack must hold all
// three; innermost wins for Variable scoping.
func TestExtractTS_DeeplyNestedFunctions_InnermostScopeWins(t *testing.T) {
	src := `function outer() {
  function middle() {
    function inner() {
      const data = "deep";
    }
  }
}
`
	r := Extract([]byte(src), "TypeScript", "p/f.ts")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, s := range r.Symbols {
		if s.Kind == "Variable" && s.Name == "data" {
			want := "p.f.outer.middle.inner.data"
			if s.QualifiedName != want {
				t.Errorf("deeply-nested Variable: got QN %q; want %q", s.QualifiedName, want)
			}
			return
		}
	}
	t.Error("no `data` Variable extracted at all")
}

// Cross-check — same shape against the JavaScript regex
// extractor (shares the regexExtractor framework with TypeScript).
// The bug isn't language-specific; the fix must benefit JS too.
func TestExtractJS_NestedFunctions_SiblingResVariablesDistinctScopes(t *testing.T) {
	src := `function Page() {
  function fetchA() {
    const res = await fetch("/a");
  }
  function fetchB() {
    const res = await fetch("/b");
  }
}
`
	r := Extract([]byte(src), "JavaScript", "pkg/page.js")
	if r == nil {
		t.Fatal("nil result")
	}
	// Note: JS uses the AST extractor (#266), so this serves as
	// a guard that the AST path handles nesting via its own
	// mechanism. The regex tracker change is in the regex path;
	// confirm the JS AST path also keeps inner-fn vars scoped.
	qnCounts := map[string]int{}
	for _, s := range r.Symbols {
		if s.Kind == "Variable" && s.Name == "res" {
			qnCounts[s.QualifiedName]++
		}
	}
	if qnCounts["pkg.page.res"] > 0 {
		t.Errorf("JS sibling-fn vars collided at module level (count=%d) — JS extractor regressed", qnCounts["pkg.page.res"])
	}
}
