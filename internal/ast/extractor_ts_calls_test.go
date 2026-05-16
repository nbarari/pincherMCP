package ast

import (
	"strings"
	"testing"
)

// #1158 v0.61: per-file CALLS pass for TS, parallel to C's #858.
// Pre-fix, the TS extractor emitted no CALLS edges at all — every TS
// project's trace/dead_code/neighborhood graph was empty (caught by
// the #858 edge-graph-empty warning). With extractCalls=true on the
// TS opts, every function body's `name(` call site emits a CALLS
// edge. Cross-file resolution still drops until the v0.61 resolver
// piece lands; same-file calls resolve immediately.
//
// Tests follow the table-from-the-start shape (#1152): positive
// (CALLS emitted), negative (control-flow keywords filtered),
// control (real function-without-calls emits zero edges), and
// cross-check (each emitted edge has the right FromQN/ToName shape).

const tsWithCallsSrc = `export function bootstrap(): void {
	loadConfig();
	const c = parseConfig();
	render(c);
}

export function loadConfig(): Config {
	return readFile();
}

export function parseConfig(): Config {
	return JSON.parse(readFile());
}
`

// Positive: bootstrap's body has three call sites — loadConfig,
// parseConfig, render. Three CALLS edges from bootstrap must emit.
func TestExtractTypeScript_PerFileCalls_EmitsEdgesFromBody(t *testing.T) {
	r := Extract([]byte(tsWithCallsSrc), "TypeScript", "src/boot.ts")
	if r == nil {
		t.Fatal("nil result")
	}
	wantTargets := map[string]bool{
		"loadConfig":  false,
		"parseConfig": false,
		"render":      false,
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" {
			continue
		}
		if !strings.HasSuffix(e.FromQN, ".bootstrap") {
			continue
		}
		if _, expected := wantTargets[e.ToName]; expected {
			wantTargets[e.ToName] = true
		}
	}
	for target, found := range wantTargets {
		if !found {
			t.Errorf("expected CALLS edge bootstrap → %q; missing", target)
		}
	}
}

// Negative: control-flow keywords that look like calls (`if(...)`,
// `return(...)`, `throw(...)`, `typeof(...)`) must NOT emit CALLS
// edges. The shared regexCallKeywords blocklist covers these.
const tsControlFlowCallsSrc = `export function guard(x: number): boolean {
	if (x === 0) {
		return true;
	}
	for (let i = 0; i < x; i++) {
		while (i > 100) { break; }
	}
	return false;
}
`

func TestExtractTypeScript_PerFileCalls_FiltersControlFlowFromCalls(t *testing.T) {
	r := Extract([]byte(tsControlFlowCallsSrc), "TypeScript", "src/guard.ts")
	if r == nil {
		t.Fatal("nil result")
	}
	banned := map[string]struct{}{
		"if": {}, "for": {}, "while": {}, "switch": {},
		"return": {}, "throw": {}, "do": {}, "else": {},
		"case": {}, "typeof": {}, "new": {}, "delete": {},
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" {
			continue
		}
		if _, b := banned[e.ToName]; b {
			t.Errorf("control-flow keyword %q emitted as CALLS target — should be filtered (from %s)", e.ToName, e.FromQN)
		}
	}
}

// Control: a function with no calls in its body emits zero CALLS
// edges. Pre-fix this was the behaviour for ALL TS functions; post-
// fix it's the behaviour ONLY for functions whose bodies are
// genuinely call-free.
const tsNoCallsSrc = `export function constant(): number {
	return 42;
}
`

func TestExtractTypeScript_PerFileCalls_EmptyBodyEmitsNoCalls(t *testing.T) {
	r := Extract([]byte(tsNoCallsSrc), "TypeScript", "src/const.ts")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, e := range r.Edges {
		if e.Kind == "CALLS" && strings.HasSuffix(e.FromQN, ".constant") {
			t.Errorf("call-free function emitted CALLS edge to %q", e.ToName)
		}
	}
}

// Cross-check: every CALLS edge carries Confidence 0.6 (the
// regex-tier signal documented in regexCallScan). Pinning the
// confidence prevents an accidental promotion to AST-tier
// confidence that would confuse downstream tools.
func TestExtractTypeScript_PerFileCalls_ConfidenceIsRegexTier(t *testing.T) {
	r := Extract([]byte(tsWithCallsSrc), "TypeScript", "src/boot.ts")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" {
			continue
		}
		if e.Confidence != 0.6 {
			t.Errorf("CALLS edge from %s confidence = %v; want 0.6 (regex-tier)", e.FromQN, e.Confidence)
		}
	}
}

// Cross-check: class method bodies also emit CALLS. This is the
// piece that makes the v0.61 receiver-type resolver work in later
// PRs — the resolver needs CALLS edges from inside methods to bind
// against. Without method-body CALLS extraction, the whole stack is
// foundation-less.
const tsMethodCallsSrc = `export class Service {
	process(data: string): void {
		this.validate(data);
		this.persist(data);
	}

	validate(data: string): void {
		check(data);
	}

	persist(data: string): void {
		write(data);
	}
}
`

func TestExtractTypeScript_PerFileCalls_MethodBodiesEmitCalls(t *testing.T) {
	r := Extract([]byte(tsMethodCallsSrc), "TypeScript", "src/svc.ts")
	if r == nil {
		t.Fatal("nil result")
	}
	// Validate at least one CALLS edge from `process` to its
	// in-method calls. The `this.validate(...)` shape captures
	// `validate` as the call target via regexCallRE; same for
	// `persist`. We don't bind through `this` here — that's the
	// receiver-type resolver's job in a later PR.
	processCalls := map[string]bool{
		"validate": false,
		"persist":  false,
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" || !strings.HasSuffix(e.FromQN, ".process") {
			continue
		}
		if _, expected := processCalls[e.ToName]; expected {
			processCalls[e.ToName] = true
		}
	}
	for target, found := range processCalls {
		if !found {
			t.Errorf("Method process should emit CALLS → %q; missing", target)
		}
	}
}
