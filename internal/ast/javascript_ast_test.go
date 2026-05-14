package ast

import (
	"strings"
	"testing"
)

// JS AST extractor tests (#266). Direct extractor-level assertions —
// pinpoint the three failure modes from the dogfooding bugs (#259,
// #260, #261) plus sanity checks for the env-flag dispatch and the
// IIFE-recovery path.

// #259: `const NAME = (expr).method(…)` was emitted as Function by the
// regex extractor (the parens after NAME tripped the function pattern).
// AST extracts it as a VarDecl regardless of RHS shape — Variable is
// the right kind by construction.
func TestJSAST_ConstAssignmentIsVariableNotFunction(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`const iface = (document.getElementById('iface').value || 'trm_wwan').toLowerCase();
const zone  = (document.getElementById('zone').value  || 'wan').toLowerCase();
const vpnMatch = (info.data.ext_hooks || '').match(/vpn:\s*(.)/);
`)
	r, ok := extractJavaScriptAST(src, "overview.js")
	if !ok {
		t.Fatal("expected AST parse to succeed on clean ES")
	}
	if len(r.Symbols) != 3 {
		t.Fatalf("expected 3 Variable symbols, got %d: %+v", len(r.Symbols), r.Symbols)
	}
	for _, s := range r.Symbols {
		if s.Kind != "Variable" {
			t.Errorf("symbol %q kind = %q, want Variable (was the #259 false positive)", s.Name, s.Kind)
		}
	}
}

// #260: object-literal methods (LuCI's `view.extend({load: function () {}})`)
// were invisible to the regex extractor. AST descent into ObjectExpr
// properties now surfaces them as Method, scoped to the synthetic
// module parent so qualified names don't collide.
func TestJSAST_ObjectLiteralMethodsExtracted_LuCIPattern(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`'use strict';
return view.extend({
    load: function () {
        return Promise.all([1, 2, 3]);
    },
    render: function (result) {
        var x = 1;
        return x;
    }
});
`)
	r, ok := extractJavaScriptAST(src, "overview.js")
	if !ok {
		t.Fatal("expected AST parse to succeed via IIFE recovery")
	}
	got := map[string]string{} // name → kind
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
	}
	for _, want := range []string{"load", "render"} {
		if got[want] != "Method" {
			t.Errorf("expected Method %q, got kind=%q (was the #260 invisible-method bug)", want, got[want])
		}
	}
}

// #261: `export const NAME = {...}` modern-ESM was emitted as zero
// symbols by the regex extractor (eslint.config.mjs returned 0 from
// 189 lines). AST emits each ExportStmt's inner VarDecl as Variable,
// so the file becomes searchable.
func TestJSAST_TopLevelExportConstIsVariable(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`export const jsdoc_less_relaxed_rules = {
    'jsdoc/check-alignment': 'warn',
};

export const jsdoc_relaxed_rules = {
    'jsdoc/check-tag-names': 'off',
};

export default defineConfig([]);
`)
	r, ok := extractJavaScriptAST(src, "eslint.config.mjs")
	if !ok {
		t.Fatal("expected AST parse to succeed")
	}
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
	}
	for _, want := range []string{"jsdoc_less_relaxed_rules", "jsdoc_relaxed_rules"} {
		if got[want] != "Variable" {
			t.Errorf("expected Variable %q, got kind=%q (was the #261 invisible-config bug)", want, got[want])
		}
	}
}

// IMPORTS edges — sanity that the AST path produces them correctly.
// The regex extractor often missed multi-line imports; AST is
// trivially correct here.
func TestJSAST_ImportStatementsEmitEdges(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`import { something } from './foo';
import * as bar from "./bar.js";
import baz from 'baz-pkg';
`)
	r, ok := extractJavaScriptAST(src, "index.js")
	if !ok {
		t.Fatal("expected AST parse to succeed")
	}
	want := map[string]bool{"./foo": false, "./bar.js": false, "baz-pkg": false}
	for _, e := range r.Edges {
		if e.Kind != "IMPORTS" {
			t.Errorf("expected IMPORTS edge kind, got %q", e.Kind)
		}
		if _, ok := want[e.ToName]; ok {
			want[e.ToName] = true
		}
	}
	for path, seen := range want {
		if !seen {
			t.Errorf("missing IMPORTS edge to %q; got edges: %+v", path, r.Edges)
		}
	}
}

// Top-level FuncDecl + ClassDecl + class methods — the bread-and-butter
// case. Cursor advances per emit so source order is preserved and same-
// named methods on different classes don't collide.
func TestJSAST_FunctionsClassesAndMethods(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`function topLevel() {
    return 1;
}

class Greeter {
    constructor(name) { this.name = name; }
    greet() { return 'hi ' + this.name; }
}

class Other {
    greet() { return 'bye'; }
}
`)
	r, ok := extractJavaScriptAST(src, "src/svc.js")
	if !ok {
		t.Fatal("expected AST parse to succeed")
	}
	got := map[string]string{}
	parents := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name+"@"+s.QualifiedName] = s.Kind
		parents[s.Name+"@"+s.QualifiedName] = s.Parent
	}
	if got["topLevel@src.svc.topLevel"] != "Function" {
		t.Errorf("topLevel: kind=%q, want Function; got=%v", got["topLevel@src.svc.topLevel"], got)
	}
	if got["Greeter@src.svc.Greeter"] != "Class" {
		t.Errorf("Greeter: want Class; got=%v", got)
	}
	if got["Other@src.svc.Other"] != "Class" {
		t.Errorf("Other: want Class; got=%v", got)
	}
	// Each class contributes its own greet() method, scoped to its parent.
	greetGreeter := got["greet@src.svc.Greeter.greet"]
	greetOther := got["greet@src.svc.Other.greet"]
	if greetGreeter != "Method" || greetOther != "Method" {
		t.Errorf("expected two Method greets (one per class); got Greeter=%q Other=%q full=%v",
			greetGreeter, greetOther, got)
	}
	if parents["greet@src.svc.Greeter.greet"] != "src.svc.Greeter" {
		t.Errorf("Greeter.greet parent=%q, want src.svc.Greeter", parents["greet@src.svc.Greeter.greet"])
	}
}

// #809: a blank line before a class method must not let
// locateMethodInRange's leading-whitespace match span newlines —
// pre-fix the `[\s]*` (and the `\s*` before the name) swallowed the
// blank line, landing startByte on a stray newline so findBlockEnd
// returned a zero-width span.
func TestJSAST_BlankLineBeforeMethodNoZeroSpan(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`class Service {
    constructor(name) {
        this.name = name;
    }

    say() {
        return this.name;
    }

    async loadAndSay(path) {
        return this.say() + path;
    }
}
`)
	r, ok := extractJavaScriptAST(src, "src/svc.js")
	if !ok {
		t.Fatal("expected AST parse to succeed")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range r.Symbols {
		byName[s.Name] = s
		if s.StartByte >= s.EndByte {
			t.Errorf("%s has zero/negative byte range: start=%d end=%d", s.Name, s.StartByte, s.EndByte)
		}
	}
	// say() starts on line 6, after a blank line — must point at the
	// method, not the preceding newline.
	if got := byName["say"]; got.StartLine != 6 {
		t.Errorf("say.StartLine = %d, want 6 (the method line, not a stray blank line)", got.StartLine)
	}
	if got := byName["loadAndSay"]; got.StartLine != 10 {
		t.Errorf("loadAndSay.StartLine = %d, want 10", got.StartLine)
	}
}

// Flag off → AST extractor not invoked; falls through to regex via the
// registry's adapter. Verifies the dispatch in extractor.go's init().
func TestJSAST_FlagOff_FallsThroughToRegex(t *testing.T) {
	// Don't set the env var. Direct call to extractJavaScript (regex
	// path) and confirm symbols come back at the regex-confidence
	// shape — proves the AST extractor isn't accidentally always-on.
	src := []byte(`function regexExtractsThis() {}`)
	r := extractJavaScript(src, "x.js")
	if r == nil || len(r.Symbols) == 0 {
		t.Fatal("regex extractor should emit a symbol for a plain function")
	}
	if r.Symbols[0].Name != "regexExtractsThis" {
		t.Errorf("regex extractor returned unexpected symbols: %+v", r.Symbols)
	}
}

// jsASTEnabled reads the env vars on every call so test set/unset
// cycles don't require re-registering the extractor. Pre-flip this
// pinned the opt-IN semantics; post-#562 (v0.20.0 default-on) it pins
// both opt-out channels and the legacy-opt-in becoming a no-op.
func TestJSASTEnabled_ReadsEnvOnEachCall(t *testing.T) {
	// Default-on: no env vars set → AST enabled.
	t.Setenv("PINCHER_DISABLE_JS_AST", "")
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "")
	if !jsASTEnabled() {
		t.Error("expected enabled by default (post-#562 flip)")
	}
	// Canonical opt-out.
	t.Setenv("PINCHER_DISABLE_JS_AST", "1")
	if jsASTEnabled() {
		t.Error("expected disabled when PINCHER_DISABLE_JS_AST=1")
	}
	t.Setenv("PINCHER_DISABLE_JS_AST", "")
	// Legacy opt-in env var is a no-op when set to 1 — already on.
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	if !jsASTEnabled() {
		t.Error("expected still enabled when legacy var=1")
	}
	// Legacy var doubles as opt-OUT when set to 0 — kept for one
	// release in case users baked it into their config.
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "0")
	if jsASTEnabled() {
		t.Error("expected disabled when legacy var=0 (opt-out compat)")
	}
}

// Top-level `return` recovery: parseJSWithRecovery wraps in IIFE on the
// LuCI-style error so symbols inside still extract. Pin the recovery
// path independently of the object-literal walker.
func TestJSAST_TopLevelReturnRecoveryParses(t *testing.T) {
	src := []byte(`return { a: 1 };`)
	parsed, ok := parseJSWithRecovery(src)
	if !ok {
		t.Fatal("expected IIFE recovery to allow top-level return to parse")
	}
	if parsed == nil {
		t.Fatal("parsed is nil despite ok=true")
	}
}

// Garbage source falls back to regex. Verifies the ok=false path of
// extractJavaScriptAST so the registry adapter knows to use regex.
func TestJSAST_GarbageReturnsOkFalse(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`<<<this is not JavaScript at all<<<`)
	_, ok := extractJavaScriptAST(src, "junk.js")
	if ok {
		t.Error("expected ok=false on garbage input")
	}
}

// Signature is a single line, capped at 200 chars — mirrors the regex
// extractor's signature shape so search snippets don't change format.
func TestJSAST_SignatureIsSingleLineBounded(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`function fn() {
    return 1;
}
`)
	r, ok := extractJavaScriptAST(src, "x.js")
	if !ok || len(r.Symbols) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(r.Symbols))
	}
	sig := r.Symbols[0].Signature
	if strings.Contains(sig, "\n") {
		t.Errorf("signature should be single-line; got %q", sig)
	}
	if !strings.Contains(sig, "function fn") {
		t.Errorf("signature should contain the declaration; got %q", sig)
	}
}

// `export function`, `export class`, `export const` should each emit
// the underlying decl (covers all three branches of emitExport).
func TestJSAST_ExportStatementsEmitInnerDecl(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`export function namedFn() {}
export class NamedClass {
    method() {}
}
export const namedConst = 42;
`)
	r, ok := extractJavaScriptAST(src, "exports.js")
	if !ok {
		t.Fatal("expected AST parse to succeed")
	}
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
	}
	if got["namedFn"] != "Function" {
		t.Errorf("export function: got kind=%q, want Function; full=%v", got["namedFn"], got)
	}
	if got["NamedClass"] != "Class" {
		t.Errorf("export class: got kind=%q, want Class; full=%v", got["NamedClass"], got)
	}
	if got["method"] != "Method" {
		t.Errorf("class method via export: got kind=%q, want Method; full=%v", got["method"], got)
	}
	if got["namedConst"] != "Variable" {
		t.Errorf("export const: got kind=%q, want Variable; full=%v", got["namedConst"], got)
	}
}

// findStatementEnd must honour string and template-literal boundaries —
// `;` inside a quoted/backticked string must not terminate the statement.
// Pin behaviour by extracting Variables whose initialisers embed `;`.
func TestJSAST_VarInitWithEmbeddedSemicolonInString(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`const greeting = "hello; world";
const template = ` + "`" + `tpl ${1 + 2}; tail` + "`" + `;
const escaped = 'has \';\' inside';
`)
	r, ok := extractJavaScriptAST(src, "vars.js")
	if !ok {
		t.Fatalf("expected AST parse to succeed; src=%s", src)
	}
	got := map[string]bool{}
	for _, s := range r.Symbols {
		if s.Kind == "Variable" {
			got[s.Name] = true
		}
	}
	for _, name := range []string{"greeting", "template", "escaped"} {
		if !got[name] {
			t.Errorf("Variable %q not emitted (string/template/escape boundary skipped?); got: %+v", name, r.Symbols)
		}
	}
}

// findStatementEnd handles line and block comments without losing track
// of statement boundaries.
func TestJSAST_VarInitWithEmbeddedComments(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`const x = 1; // line comment with ; inside
const y = 2 /* block ; comment */ + 3;
`)
	r, ok := extractJavaScriptAST(src, "vars.js")
	if !ok {
		t.Fatal("expected parse")
	}
	got := map[string]bool{}
	for _, s := range r.Symbols {
		got[s.Name] = true
	}
	for _, name := range []string{"x", "y"} {
		if !got[name] {
			t.Errorf("Variable %q missing; got %+v", name, r.Symbols)
		}
	}
}

// #825: a var initializer that continues onto the next line via a
// method chain must span the whole statement — findStatementEnd used
// to stop at the first newline at depth 0, truncating the span.
func TestJSAST_MethodChainSpansAllLines(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`const chain = items
    .filter(x => x > 0)
    .map(x => x * 2);

const plain = 5;
`)
	r, ok := extractJavaScriptAST(src, "vars.js")
	if !ok {
		t.Fatal("expected parse")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range r.Symbols {
		byName[s.Name] = s
	}
	if got := byName["chain"]; got.StartLine != 1 || got.EndLine != 3 {
		t.Errorf("chain span = %d-%d, want 1-3 (the full method chain)", got.StartLine, got.EndLine)
	}
	// A plain single-line statement must still end at its own line.
	if got := byName["plain"]; got.StartLine != 5 || got.EndLine != 5 {
		t.Errorf("plain span = %d-%d, want 5-5", got.StartLine, got.EndLine)
	}
}

// propertyNameToString filters out string-literal and computed property
// names so we don't emit `Method "'string-name'"` or `Method "[expr]"`.
func TestJSAST_StringLiteralPropertyNamesSkipped(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`class C {
    'string-name'() {}
    realMethod() {}
}
`)
	r, ok := extractJavaScriptAST(src, "x.js")
	if !ok {
		t.Fatal("expected parse")
	}
	for _, s := range r.Symbols {
		if s.Kind != "Method" {
			continue
		}
		if strings.HasPrefix(s.Name, "'") || strings.HasPrefix(s.Name, `"`) ||
			strings.HasPrefix(s.Name, "`") || strings.HasPrefix(s.Name, "[") {
			t.Errorf("string/computed property name should be skipped; got %q", s.Name)
		}
	}
}

// unquoteJSString handles single, double, and backtick quotes; returns
// "" when input isn't quoted (defensive against caller misuse).
func TestUnquoteJSString_AllQuoteStyles(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`"double"`, "double"},
		{`'single'`, "single"},
		{"`backtick`", "backtick"},
		{`unquoted`, ""},
		{`""`, ""},
		{`"`, ""},
		{``, ""},
		{`"mismatched'`, ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := unquoteJSString([]byte(c.in)); got != c.want {
				t.Errorf("unquoteJSString(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Vue-style nested object methods: { methods: { foo() {}, bar() {} } }
// — exercises the recursive descent in walkExprForObjectMethods +
// emitObjectProperty's recursion into non-function property values.
func TestJSAST_NestedObjectMethodsExtracted(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`Vue.component('x', {
    methods: {
        clickHandler() { return 1; },
        submitHandler: function () { return 2; }
    }
});
`)
	r, ok := extractJavaScriptAST(src, "vue.js")
	if !ok {
		t.Fatal("expected parse")
	}
	got := map[string]bool{}
	for _, s := range r.Symbols {
		if s.Kind == "Method" {
			got[s.Name] = true
		}
	}
	for _, name := range []string{"clickHandler", "submitHandler"} {
		if !got[name] {
			t.Errorf("expected nested Method %q; got symbols=%+v", name, r.Symbols)
		}
	}
}

// Destructuring binders (`const { a, b } = obj`) must produce no
// symbols — bindingName returns "" for non-Var bindings. Pin the
// silent skip so a future refactor doesn't accidentally start emitting
// empty-name symbols.
func TestJSAST_DestructuringBindingProducesNoSymbol(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`const { a, b } = obj;
const realName = 5;
`)
	r, ok := extractJavaScriptAST(src, "x.js")
	if !ok {
		t.Fatal("expected parse")
	}
	for _, s := range r.Symbols {
		if s.Name == "" || s.Name == "a" || s.Name == "b" {
			t.Errorf("destructuring binders should be skipped; got name=%q", s.Name)
		}
	}
	gotReal := false
	for _, s := range r.Symbols {
		if s.Name == "realName" {
			gotReal = true
		}
	}
	if !gotReal {
		t.Error("realName should still extract alongside the skipped destructuring")
	}
}

// Anonymous function/class declarations (no Name) hit the early-return
// branches of emitFunc and emitClass. Pin that no symbol leaks.
func TestJSAST_AnonymousDefaultExportsAreSkipped(t *testing.T) {
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "1")
	src := []byte(`export default function () { return 1; }
`)
	r, ok := extractJavaScriptAST(src, "x.js")
	if !ok {
		t.Fatal("expected parse")
	}
	for _, s := range r.Symbols {
		if s.Kind == "Function" {
			t.Errorf("anonymous default-export function leaked through guard; got %+v", s)
		}
	}
}
