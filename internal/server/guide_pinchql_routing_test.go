package server

import (
	"strings"
	"testing"
)

// #616: bare "pinchql" in domainConcepts was matching tasks like "use
// pinchQL to find X" — the user wanted a query template, not a pointer
// at the engine's source. Fix: tightened pattern + added a dedicated
// "use pinchql" concept that recommends the `query` tool.

func TestDomainConceptHint_UsePinchQLRoutesToQueryTool(t *testing.T) {
	cases := []string{
		"use pinchQL to find functions sharing names across packages",
		"use pinchql to count edges per file",
		"via pinchQL: find dead code",
		"with pinchQL find every undocumented method",
		"write a pinchQL query for callers of Open",
		"use pinchQL to list every exported function",
	}
	for _, task := range cases {
		t.Run(task, func(t *testing.T) {
			hint := domainConceptHint(task)
			if hint == nil {
				t.Fatalf("expected a domain-concept hint for %q; got nil", task)
			}
			if (*hint)["tool"] != "query" {
				t.Errorf("hint.tool = %q, want \"query\" (user wants to USE pinchQL, not inspect its source)",
					(*hint)["tool"])
			}
			if !strings.Contains((*hint)["args"], "pinchql") {
				t.Errorf("hint.args should contain a pinchql template; got %q", (*hint)["args"])
			}
		})
	}
}

func TestDomainConceptHint_PinchQLInternalsStillRoutesToSource(t *testing.T) {
	// The original concept (pinchQL implementation investigation) must
	// still fire for tasks that genuinely want to read the engine source.
	cases := []string{
		"how does the cypher engine dispatch queries",
		"explore pinchQL implementation",
		"where is the pinchQL parser",
		"refactor where pushdown",
	}
	for _, task := range cases {
		t.Run(task, func(t *testing.T) {
			hint := domainConceptHint(task)
			if hint == nil {
				t.Fatalf("expected a domain-concept hint for %q; got nil", task)
			}
			if (*hint)["tool"] != "search" {
				t.Errorf("hint.tool = %q, want \"search\" (engine-internals investigation)", (*hint)["tool"])
			}
			if !strings.Contains((*hint)["args"], "runJoinQuery") {
				t.Errorf("hint.args should point at the dispatcher; got %q", (*hint)["args"])
			}
		})
	}
}

func TestDomainConceptHint_BarePinchQLNoLongerOverMatches(t *testing.T) {
	// Pre-#616 regression check: tasks that mention pinchql conceptually
	// without "use" / "engine" / "implementation" should NOT pick up the
	// engine-source hint by accident. They should fall through to the
	// shape-default recommendations instead. Either nil or the
	// "use pinchql" concept (query tool) is acceptable — what's NOT
	// acceptable is the runJoinQuery search hint.
	task := "this is a pinchql question"
	hint := domainConceptHint(task)
	if hint != nil && (*hint)["tool"] == "search" && strings.Contains((*hint)["args"], "runJoinQuery") {
		t.Errorf("bare pinchql mention should not route to engine source; got %v", *hint)
	}
}
