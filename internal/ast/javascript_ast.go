package ast

import (
	"bytes"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

// JS AST extractor (#266). Hybrid design — tdewolff/parse/v2/js gives
// us authoritative kind + name (so `const X = (expr).method()` lands
// as Variable not Function, and modern ESM `export const NAME = ...`
// emits as Variable instead of vanishing into the regex extractor's
// blind spots), but its AST nodes carry no byte positions. We re-locate
// each named declaration in source via regex past a forward-only cursor
// to recover StartByte/EndByte for byte-offset retrieval.
//
// Source order in the AST.BlockStmt.List matches source order in bytes
// for top-level decls, so the cursor advance is monotonic and the first
// regex match past it is the right declaration. Nested class methods
// search inside the class's byte range only.
//
// Default-on as of v0.20.0 (#562 closed the four #557 polish bugs +
// the const-object-literal descent gap). Falls back to the existing
// regex extractor on parse failure (parseJSWithRecovery returns ok=false).
//
// Opt-out via `PINCHER_DISABLE_JS_AST=1` for one release in case a
// user hits an AST-mode regression we missed; planned removal in v0.21.
// The legacy `PINCHER_EXPERIMENTAL_JS_AST` env var is still honored
// (no-op when default is on, opt-out when set to "0") so anyone with
// it baked into their config doesn't see surprise behavior.

// jsASTEnabled reads the env vars on every call so tests can flip the
// flag with t.Setenv without re-registering the extractor.
//
// Resolution order:
//  1. `PINCHER_DISABLE_JS_AST=1` → false (explicit opt-out wins)
//  2. `PINCHER_EXPERIMENTAL_JS_AST=0` → false (legacy explicit-off)
//  3. otherwise → true (default-on)
func jsASTEnabled() bool {
	if os.Getenv("PINCHER_DISABLE_JS_AST") == "1" {
		return false
	}
	if os.Getenv("PINCHER_EXPERIMENTAL_JS_AST") == "0" {
		return false
	}
	return true
}

// extractJavaScriptAST parses source with tdewolff and emits symbols +
// IMPORTS edges. Returns ok=false when parsing fails irrecoverably; the
// caller (the registered extractor) falls back to the regex path.
func extractJavaScriptAST(source []byte, relPath string) (*FileResult, bool) {
	parsed, parseOk := parseJSWithRecovery(source)
	if !parseOk {
		return nil, false
	}
	w := &jsASTWalker{
		source:      source,
		relPath:     relPath,
		lineOffsets: buildLineOffsets(source),
		regexCache:  map[string]*regexp.Regexp{},
	}
	w.walkBlock(parsed.BlockStmt, false /*insideFunc*/)
	// #1328 v0.71: signal to ExtractWithModule that this FileResult is
	// AST-tier. The JavaScript langAdapter registers confidence=0.85
	// (the regex fallback's honest floor — kept for the post-default-on
	// failure-path); without ConfidenceOverride, AST-extracted symbols
	// stamp the regex value and there's no way to distinguish AST from
	// regex output in min_confidence filters, dashboards, or
	// extraction_failures triage. Mirrors Python's pattern at #944.
	return &FileResult{
		Symbols:            w.symbols,
		Edges:              w.edges,
		ConfidenceOverride: 1.0,
	}, true
}

// JavaScriptASTEnabled reports whether the JS AST extractor would run
// for the next JS file. Exported so callers in /internal/server can
// upgrade the "parser" label from "Regex" to "AST" at runtime, the same
// way ast.PythonAvailable supports Python's auto-upgrade carve-out.
// Pre-fix `pincher health` always reported `parser: "Regex"` for JS,
// even though the AST path has been default-on since v0.20.0 (#266 /
// #1328 drift).
func JavaScriptASTEnabled() bool {
	return jsASTEnabled()
}

// parseJSWithRecovery parses source. If the parser rejects a top-level
// `return` statement (LuCI views, older module loaders, non-spec but
// real-world common), wrap source in an IIFE and retry once.
//
// #1477 v0.84: log the parse failure path at slog.Debug so users
// debugging "why are my JS symbols at regex confidence" can correlate
// their files with parser errors. Default-off (Debug level); enable
// via PINCHER_LOG_LEVEL=debug.
func parseJSWithRecovery(source []byte) (*js.AST, bool) {
	parsed, err := js.Parse(parse.NewInputBytes(source), js.Options{})
	if err == nil {
		return parsed, true
	}
	firstErr := err.Error()
	if strings.Contains(firstErr, "unexpected return") ||
		strings.Contains(firstErr, "return outside") {
		var buf bytes.Buffer
		buf.WriteString("(function(){\n")
		buf.Write(source)
		buf.WriteString("\n})()")
		if r, e := js.Parse(parse.NewInputBytes(buf.Bytes()), js.Options{}); e == nil {
			return r, true
		} else {
			slog.Debug("pincher.ast.js.parse_failed_after_iife_recovery",
				"first_err", firstErr,
				"iife_err", e.Error())
		}
	} else {
		slog.Debug("pincher.ast.js.parse_failed",
			"err", firstErr)
	}
	return nil, false
}

type jsASTWalker struct {
	source      []byte
	relPath     string
	lineOffsets []int
	symbols     []ExtractedSymbol
	edges       []ExtractedEdge
	classStack  []string // qualified-name stack for nested-class scope
	cursor      int      // byte offset; locator searches forward of this
	regexCache  map[string]*regexp.Regexp
}

// walkBlock walks a block's top-level statements. Inside function bodies
// we still emit IMPORTS edges (for dynamic-style requires inside an IIFE
// — common in CommonJS), but skip nested function/class/var declarations
// per the direction in #266 ("emit only top-level + class/object methods
// + IMPORTS edges").
//
// IIFE recovery (parseJSWithRecovery wrapping for top-level `return`)
// lands the user's source inside an anonymous function expression. To
// keep the visible-symbol shape consistent with non-recovered files,
// we transparently descend into a sole IIFE-shaped top-level statement.
func (w *jsASTWalker) walkBlock(b js.BlockStmt, insideFunc bool) {
	if !insideFunc {
		if inner, ok := unwrapIIFE(b); ok {
			w.walkBlock(inner, false)
			return
		}
	}
	parent := moduleQN(w.relPath, ".") + ".__module"
	for _, stmt := range b.List {
		switch s := stmt.(type) {
		case *js.FuncDecl:
			if !insideFunc {
				w.emitFunc(s, "Function", "", false)
			}
		case *js.ClassDecl:
			if !insideFunc {
				w.emitClass(s, false)
			}
		case *js.VarDecl:
			if !insideFunc {
				w.emitVarDecl(s, false)
			}
		case *js.ImportStmt:
			w.emitImport(s)
		case *js.ExportStmt:
			w.emitExport(s, insideFunc)
		case *js.ReturnStmt:
			// Top-level return (after IIFE recovery): LuCI views land
			// here as `return view.extend({load: …, render: …})`.
			// Descend the expression looking for object-literal methods.
			if !insideFunc {
				w.walkExprForObjectMethods(s.Value, parent, 0)
			}
		case *js.ExprStmt:
			// `module.exports = { foo() {…} }` — descend the rhs.
			if !insideFunc {
				w.walkExprForObjectMethods(s.Value, parent, 0)
			}
		}
	}
}

// unwrapIIFE returns the body of `(function () { … })()` when the input
// block is exactly that shape — the IIFE-recovery wrapper added by
// parseJSWithRecovery. Returns ok=false otherwise so the normal walker
// keeps running.
func unwrapIIFE(b js.BlockStmt) (js.BlockStmt, bool) {
	if len(b.List) != 1 {
		return js.BlockStmt{}, false
	}
	expr, ok := b.List[0].(*js.ExprStmt)
	if !ok || expr == nil {
		return js.BlockStmt{}, false
	}
	call, ok := expr.Value.(*js.CallExpr)
	if !ok || call == nil {
		return js.BlockStmt{}, false
	}
	// `(fn)()` — tdewolff wraps the parens as GroupExpr around the
	// anonymous FuncDecl. Look through the group to find the FuncDecl.
	x := call.X
	if g, ok := x.(*js.GroupExpr); ok && g != nil {
		x = g.X
	}
	if fd, ok := x.(*js.FuncDecl); ok && fd != nil && fd.Name == nil {
		return fd.Body, true
	}
	return js.BlockStmt{}, false
}

// walkExprForObjectMethods recurses through an expression looking for
// ObjectExpr nodes whose properties are function-like — emits them as
// Methods scoped to `parent`. Depth-limited so pathological deeply-
// nested expressions can't blow the stack.
//
// Common patterns this catches:
//   - `return view.extend({ load: function () {…}, render: function (r) {…} })` (LuCI)
//   - `module.exports = { foo() {…} }` (CommonJS shorthand)
//   - `Vue.component('x', { methods: { foo() {…} } })` (nested via recursion)
func (w *jsASTWalker) walkExprForObjectMethods(expr js.IExpr, parent string, depth int) {
	if expr == nil || depth > 8 {
		return
	}
	switch e := expr.(type) {
	case *js.ObjectExpr:
		for _, p := range e.List {
			w.emitObjectProperty(p, parent, depth+1)
		}
	case *js.CallExpr:
		// e.g. `view.extend({...})` — descend into call arguments.
		for _, a := range e.Args.List {
			w.walkExprForObjectMethods(a.Value, parent, depth+1)
		}
	}
}

// emitObjectProperty emits a Method symbol for a Property whose value
// is a function-like declaration. Two name sources, depending on which
// JS object-method form was used:
//
//   - **shorthand** `{ name() {…} }` — Property.Name is nil; the name
//     lives on Property.Value (a *MethodDecl whose Name field carries
//     the PropertyName). This is the modern ES6+ form.
//   - **explicit** `{ name: function () {…} }` — Property.Name carries
//     the PropertyName; Value is a *FuncDecl. Older but still common
//     in framework code (LuCI's view.extend, jQuery plugins, AMD).
//
// For non-function Value types (nested objects, arrays, primitives),
// recurse to keep walking — Vue's `methods: { … }` and similar nested
// patterns surface their methods this way.
func (w *jsASTWalker) emitObjectProperty(p js.Property, parent string, depth int) {
	var name string
	switch v := p.Value.(type) {
	case *js.MethodDecl:
		// Shorthand: name is on the method, Property.Name is nil.
		if v != nil {
			name = propertyNameToString(v.Name.PropertyName)
		}
	case *js.FuncDecl:
		// Explicit `name: function () {…}`: name is on Property.Name.
		if p.Name != nil {
			name = propertyNameToString(*p.Name)
		}
	case *js.ArrowFunc:
		// Arrow-function value: `name: () => {…}` or `name: async () => …`.
		// React event handlers, Vue computed props, and modern Node
		// frameworks lean on this form heavily — pre-fix they were
		// silently dropped (#266 follow-up).
		if p.Name != nil {
			name = propertyNameToString(*p.Name)
		}
	default:
		// Not a function-shaped value. Always recurse so nested objects
		// like Vue's `methods: { … }` keep getting walked.
		w.walkExprForObjectMethods(p.Value, parent, depth)
		return
	}
	if name == "" {
		// Function-shaped value but couldn't resolve a name (computed
		// or string-literal property name) — skip silently.
		return
	}
	sb, eb, ok := w.locateObjectMember(name)
	if !ok {
		return
	}
	w.appendSymbol(ExtractedSymbol{
		Name:          name,
		QualifiedName: parent + "." + name,
		Kind:          "Method",
		Parent:        parent,
		StartByte:     sb, EndByte: eb,
		StartLine: offsetToLine(w.lineOffsets, sb),
		EndLine:   offsetToLine(w.lineOffsets, eb),
		Signature: w.signatureFromSource(sb),
	})
}

// locateObjectMember finds `NAME(` or `NAME: function` past the cursor.
// Used by emitObjectProperty for both shorthand and explicit-function
// object-literal members.
func (w *jsASTWalker) locateObjectMember(name string) (int, int, bool) {
	// Three function-shaped forms: shorthand `name(...)`, explicit
	// `name: function (...)`, and arrow `name: (params) => ...` (or
	// `name: async (params) => ...`). Pre-fix the third form was
	// silently dropped because the regex only knew about the first
	// two — onChange-style React callbacks vanished from the index.
	pattern := `(?m)(?:^|[\s,{])(?:async\s+)?\*?\s*` + regexp.QuoteMeta(name) +
		`\s*(?:\(|:\s*(?:async\s+)?(?:function\b|\(|[A-Za-z_$][\w$]*\s*=>))`
	loc := w.regexFor(pattern).FindIndex(w.source[w.cursor:])
	if loc == nil {
		return 0, 0, false
	}
	startByte := w.cursor + loc[0]
	// Skip the leading whitespace / comma / brace match.
	for startByte < len(w.source) && (w.source[startByte] == ' ' || w.source[startByte] == '\t' ||
		w.source[startByte] == '\n' || w.source[startByte] == ',' || w.source[startByte] == '{') {
		startByte++
	}
	endByte := findBlockEnd(w.source, startByte, '{')
	w.cursor = endByte
	return startByte, endByte, true
}

func (w *jsASTWalker) emitFunc(fd *js.FuncDecl, kind, parent string, isExported bool) {
	if fd.Name == nil {
		return
	}
	name := string(fd.Name.Data)
	startByte, endByte, ok := w.locateFunc(name)
	if !ok {
		return
	}
	qn := w.qnFor(name, parent)
	w.appendSymbol(ExtractedSymbol{
		Name: name, QualifiedName: qn, Kind: kind, Parent: parent,
		StartByte: startByte, EndByte: endByte,
		StartLine:  offsetToLine(w.lineOffsets, startByte),
		EndLine:    offsetToLine(w.lineOffsets, endByte),
		Signature:  w.signatureFromSource(startByte),
		IsExported: isExported,
	})
}

func (w *jsASTWalker) emitClass(cd *js.ClassDecl, isExported bool) {
	if cd.Name == nil {
		return
	}
	name := string(cd.Name.Data)
	startByte, endByte, ok := w.locateClass(name)
	if !ok {
		return
	}
	qn := w.qnFor(name, "")
	w.appendSymbol(ExtractedSymbol{
		Name: name, QualifiedName: qn, Kind: "Class",
		StartByte: startByte, EndByte: endByte,
		StartLine:  offsetToLine(w.lineOffsets, startByte),
		EndLine:    offsetToLine(w.lineOffsets, endByte),
		Signature:  w.signatureFromSource(startByte),
		IsExported: isExported,
	})
	// Walk methods inside the class body. Save+restore the cursor so
	// per-method searches stay scoped to the class's byte range.
	// Class methods inherit the class's exported-ness.
	w.classStack = append(w.classStack, qn)
	saveCursor := w.cursor
	w.cursor = startByte
	for _, el := range cd.List {
		if el.Method == nil {
			continue
		}
		methodName := propertyNameToString(el.Method.Name.PropertyName)
		if methodName == "" {
			continue
		}
		ms, me, mok := w.locateMethodInRange(methodName, startByte, endByte)
		if !mok {
			continue
		}
		w.appendSymbol(ExtractedSymbol{
			Name:          methodName,
			QualifiedName: qn + "." + methodName,
			Kind:          "Method",
			Parent:        qn,
			StartByte:     ms, EndByte: me,
			StartLine:  offsetToLine(w.lineOffsets, ms),
			EndLine:    offsetToLine(w.lineOffsets, me),
			Signature:  w.signatureFromSource(ms),
			IsExported: isExported,
		})
	}
	w.classStack = w.classStack[:len(w.classStack)-1]
	w.cursor = saveCursor
	if endByte > w.cursor {
		w.cursor = endByte
	}
}

// emitVarDecl emits one symbol per binding in a VarDecl. The kind
// depends on the initializer: `const f = () => {}` and
// `const f = function() {}` are Functions (the binding is named, the
// value is a function expression — that's how JS gives a function
// its name today). Everything else is a Variable. This is the
// arrow-function / function-expression de-double-emit (#266 follow-up):
// without it, the same symbol would surface twice (Variable + Function)
// or, worse, only as Variable and miss callers entirely.
//
// `isExported` flags top-level decls reached via emitExport so dead-
// code analysis doesn't flag them. Plain `const x = …` at module scope
// is unexported per ES2015 module semantics.
func (w *jsASTWalker) emitVarDecl(vd *js.VarDecl, isExported bool) {
	for _, be := range vd.List {
		name := bindingName(be.Binding)
		if name == "" {
			continue
		}
		kind := "Variable"
		if isFunctionInit(be.Default) {
			kind = "Function"
		}
		sb, eb, ok := w.locateVar(name)
		if !ok {
			continue
		}
		bindingQN := w.qnFor(name, "")
		w.appendSymbol(ExtractedSymbol{
			Name: name, QualifiedName: bindingQN,
			Kind:      kind,
			StartByte: sb, EndByte: eb,
			StartLine:  offsetToLine(w.lineOffsets, sb),
			EndLine:    offsetToLine(w.lineOffsets, eb),
			Signature:  w.signatureFromSource(sb),
			IsExported: isExported,
		})
		// Descend into object-literal initializers so patterns like
		// `const handlers = { onClick: ..., onChange: () => {} }` get
		// their methods extracted. Without this, `export const X =
		// {…}` (the modern config-object pattern: ESLint flat config,
		// Vue options, React reducers, redux slices) silently drops
		// every nested method. The regex extractor accidentally caught
		// `name: function` and `name: () =>` shapes here; AST without
		// this descent would regress on those — flip-blocking.
		//
		// Save+restore cursor: locateVar moved it to the END of the
		// binding statement, but locateObjectMember scans forward
		// from cursor, so we need to rewind to the binding start to
		// find the inner methods. Same pattern as emitClass.
		if be.Default != nil {
			savedCursor := w.cursor
			w.cursor = sb
			w.walkExprForObjectMethods(be.Default, bindingQN, 0)
			if w.cursor < savedCursor {
				w.cursor = savedCursor
			}
		}
	}
}

// isFunctionInit reports whether a binding's initializer is a function
// expression: arrow function, function expression, or either wrapped
// in parens. Matters for emitVarDecl: function-shaped initializers
// promote the binding's kind from Variable to Function so the symbol
// surfaces in the call graph.
func isFunctionInit(expr js.IExpr) bool {
	for expr != nil {
		switch e := expr.(type) {
		case *js.ArrowFunc:
			return true
		case *js.FuncDecl:
			return true
		case *js.GroupExpr:
			expr = e.X
			continue
		}
		return false
	}
	return false
}

func (w *jsASTWalker) emitImport(im *js.ImportStmt) {
	if len(im.Module) == 0 {
		return
	}
	// im.Module is the quoted module string — strip the quotes for a clean
	// edge target ("./foo" not "\"./foo\"").
	mod := unquoteJSString(im.Module)
	if mod == "" {
		return
	}
	w.edges = append(w.edges, ExtractedEdge{
		ToName: mod, Kind: "IMPORTS", Confidence: 1.0,
	})
}

// emitExport handles the inner declaration of an `export` statement —
// `export const X = ...`, `export function f() {}`, `export class C {}`.
// Re-emits the underlying decl so it surfaces with the right kind. Bare
// re-exports (`export { x }`) without a Decl don't introduce new symbols.
func (w *jsASTWalker) emitExport(ex *js.ExportStmt, insideFunc bool) {
	if ex.Decl == nil {
		return
	}
	switch d := ex.Decl.(type) {
	case *js.FuncDecl:
		if !insideFunc {
			w.emitFunc(d, "Function", "", true)
		}
	case *js.ClassDecl:
		if !insideFunc {
			w.emitClass(d, true)
		}
	case *js.VarDecl:
		if !insideFunc {
			w.emitVarDecl(d, true)
		}
	}
}

// appendSymbol stamps complexity from the source range, then appends.
// Caller controls IsExported — pre-#266-followup this method
// unconditionally set IsExported=true, but ES2015+ module semantics
// say "only `export`-prefixed decls are exported", and non-exported
// helpers should surface in dead_code. emitExport sets IsExported=true
// on its callees; everyone else passes false (the struct's zero value).
func (w *jsASTWalker) appendSymbol(s ExtractedSymbol) {
	s.Complexity = estimateComplexity(w.source[s.StartByte:min(s.EndByte, len(w.source))])
	w.symbols = append(w.symbols, s)
}

func (w *jsASTWalker) qnFor(name, parent string) string {
	mod := moduleQN(w.relPath, ".")
	if parent != "" {
		return parent + "." + name
	}
	return mod + "." + name
}

// signatureFromSource returns a single-line signature snippet capped at
// 200 chars. Mirrors the regex extractor's signature shape.
func (w *jsASTWalker) signatureFromSource(off int) string {
	end := off
	for end < len(w.source) && w.source[end] != '\n' && end-off < 200 {
		end++
	}
	return strings.TrimSpace(string(w.source[off:end]))
}

// ─────────────────────────────────────────────────────────────────────
// Locators — re-find the AST-identified declaration in source bytes.
// ─────────────────────────────────────────────────────────────────────

// regexFor returns a cached compiled regex for the given pattern.
func (w *jsASTWalker) regexFor(pattern string) *regexp.Regexp {
	if re, ok := w.regexCache[pattern]; ok {
		return re
	}
	re := regexp.MustCompile(pattern)
	w.regexCache[pattern] = re
	return re
}

func (w *jsASTWalker) locateFunc(name string) (int, int, bool) {
	// `function NAME(`, optionally `function*`, optionally preceded by `async`.
	//
	// #826: inter-token whitespace is `[ \t]`, not `\s` — `\s` spans
	// newlines, so `function\nsplitName()` would land startByte on the
	// `function` keyword with the name a line below, and findBraceBlock
	// (seeing a newline before the param list) would return a ~8-byte
	// span instead of the whole body. A function declaration's keyword,
	// name, and `(` are a single-line shape; matching only spaces/tabs
	// between them keeps the located span honest. Same `\s`-spans-
	// newlines fix as #809's locateMethodInRange. The rare genuine
	// `function`-on-its-own-line case is skipped rather than mis-spanned
	// — a dropped symbol is safer than confidently-wrong byte offsets.
	pattern := `\bfunction[ \t]*\*?[ \t]*` + regexp.QuoteMeta(name) + `[ \t]*\(`
	loc := w.regexFor(pattern).FindIndex(w.source[w.cursor:])
	if loc == nil {
		return 0, 0, false
	}
	startByte := w.cursor + loc[0]
	// Walk back over an `async ` modifier and `export ` keyword for
	// signature capture.
	startByte = trimBackToDeclKeyword(w.source, startByte, "async", "export")
	endByte := findBlockEnd(w.source, startByte, '{')
	w.cursor = endByte
	return startByte, endByte, true
}

func (w *jsASTWalker) locateClass(name string) (int, int, bool) {
	pattern := `\bclass\s+` + regexp.QuoteMeta(name) + `\b`
	loc := w.regexFor(pattern).FindIndex(w.source[w.cursor:])
	if loc == nil {
		return 0, 0, false
	}
	startByte := w.cursor + loc[0]
	startByte = trimBackToDeclKeyword(w.source, startByte, "export")
	endByte := findBlockEnd(w.source, startByte, '{')
	w.cursor = endByte
	return startByte, endByte, true
}

func (w *jsASTWalker) locateVar(name string) (int, int, bool) {
	// `const NAME =`, `let NAME =`, `var NAME =`, or `(const|let|var) NAME ;`
	// (declared-but-not-initialised).
	pattern := `\b(?:const|let|var)\s+` + regexp.QuoteMeta(name) + `\b`
	loc := w.regexFor(pattern).FindIndex(w.source[w.cursor:])
	if loc == nil {
		return 0, 0, false
	}
	startByte := w.cursor + loc[0]
	startByte = trimBackToDeclKeyword(w.source, startByte, "export")
	// Variables don't always have a brace block — scan to end of statement.
	endByte := findStatementEnd(w.source, startByte)
	w.cursor = endByte
	return startByte, endByte, true
}

// locateMethodInRange searches for a class-body method by name within
// [classStart, classEnd]. Restricts the regex to the class body so two
// classes with same-named methods don't collide.
func (w *jsASTWalker) locateMethodInRange(name string, classStart, classEnd int) (int, int, bool) {
	if classStart >= classEnd || classEnd > len(w.source) {
		return 0, 0, false
	}
	// Method shape inside a class body: optional `static`/`async`/`*`
	// then `NAME(`. Skip the `constructor` / `class` keywords that
	// would falsely match. Every inter-token whitespace class is
	// `[ \t]`, NOT `\s` — `\s` includes `\n`, so blank lines before
	// the method let the match start a line (or more) early, landing
	// startByte on a stray newline and giving a zero-width span (#809).
	pattern := `(?m)^[ \t]*(?:static[ \t]+)?(?:async[ \t]+)?\*?[ \t]*` + regexp.QuoteMeta(name) + `[ \t]*\(`
	body := w.source[max(w.cursor, classStart):classEnd]
	loc := w.regexFor(pattern).FindIndex(body)
	if loc == nil {
		return 0, 0, false
	}
	startByte := max(w.cursor, classStart) + loc[0]
	// Skip leading whitespace so the captured signature starts with the keyword/name.
	for startByte < classEnd && (w.source[startByte] == ' ' || w.source[startByte] == '\t') {
		startByte++
	}
	endByte := findBlockEnd(w.source, startByte, '{')
	if endByte > classEnd {
		endByte = classEnd
	}
	w.cursor = endByte
	return startByte, endByte, true
}

// ─────────────────────────────────────────────────────────────────────
// Utilities
// ─────────────────────────────────────────────────────────────────────

// bindingName extracts the bound identifier from a BindingElement's
// IBinding. Handles plain `Var` bindings (the common case); destructuring
// patterns return "" (skipped — would need recursion to enumerate).
func bindingName(b js.IBinding) string {
	if b == nil {
		return ""
	}
	if v, ok := b.(*js.Var); ok && v != nil {
		return string(v.Data)
	}
	return ""
}

// propertyNameToString returns the literal name of a class method.
// Computed (`[expr]: ...`) and string-literal property names return ""
// to skip — not relevant for the V1 scope.
func propertyNameToString(p js.PropertyName) string {
	if p.Computed != nil {
		return ""
	}
	data := p.Literal.Data
	if len(data) == 0 {
		return ""
	}
	// String/numeric literals carry quotes/sigils; only emit on
	// identifier-shaped names to avoid emitting Method "'foo'".
	if data[0] == '"' || data[0] == '\'' || data[0] == '`' {
		return ""
	}
	return string(data)
}

// unquoteJSString strips matching surrounding quotes from a tdewolff
// module-string literal. Returns "" if the input doesn't look quoted.
func unquoteJSString(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	q := b[0]
	if q != '"' && q != '\'' && q != '`' {
		return ""
	}
	if b[len(b)-1] != q {
		return ""
	}
	return string(b[1 : len(b)-1])
}

// trimBackToDeclKeyword walks `startByte` backwards to absorb leading
// modifiers like `async` and `export ` so the captured signature
// includes them. Stops at the first non-keyword token.
func trimBackToDeclKeyword(source []byte, startByte int, keywords ...string) int {
	for {
		lineStart := startByte
		for lineStart > 0 && source[lineStart-1] != '\n' {
			lineStart--
		}
		// Look at whitespace-trimmed prefix of [lineStart, startByte].
		prefix := strings.TrimLeftFunc(string(source[lineStart:startByte]), func(r rune) bool {
			return r == ' ' || r == '\t'
		})
		matched := false
		for _, kw := range keywords {
			if strings.HasPrefix(prefix, kw+" ") || strings.HasPrefix(prefix, kw+"\t") {
				// Move startByte to the keyword position.
				startByte = lineStart + (len(source[lineStart:startByte]) - len(prefix))
				matched = true
				break
			}
		}
		if !matched {
			return startByte
		}
	}
}

// findStatementEnd scans forward to the end of a JS statement, honouring
// braces / brackets / parens / strings / template literals / line and
// block comments. Returns the offset just past the terminating `;` or
// the implicit-semicolon-insertion point at line end.
func findStatementEnd(source []byte, start int) int {
	depth := 0
	i := start
	for i < len(source) {
		c := source[i]
		switch c {
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
			if depth < 0 {
				return i
			}
		case '"', '\'':
			i = skipJSString(source, i, c)
			continue
		case '`':
			i = skipJSTemplate(source, i)
			continue
		case '/':
			if i+1 < len(source) && source[i+1] == '/' {
				for i < len(source) && source[i] != '\n' {
					i++
				}
				continue
			}
			if i+1 < len(source) && source[i+1] == '*' {
				i += 2
				for i+1 < len(source) && !(source[i] == '*' && source[i+1] == '/') {
					i++
				}
				if i+1 < len(source) {
					i += 2
				}
				continue
			}
		case ';':
			if depth == 0 {
				return i + 1
			}
		case '\n':
			if depth == 0 && !continuesNextLine(source, i) {
				return i
			}
		}
		i++
	}
	return len(source)
}

// continuesNextLine reports whether the statement continues past the
// newline at source[nl]. JS ASI does NOT insert a semicolon when the
// next non-blank line starts with a continuation token — a method
// chain (`.`), optional chaining (`?.`), or a ternary arm (`?` / `:`).
// Without this, `findStatementEnd` truncated `const x = items\n .filter(…)`
// to just the first line (#825). Operator-at-end-of-line continuations
// (`x +\n y`) remain a residual.
func continuesNextLine(source []byte, nl int) bool {
	i := nl + 1
	for i < len(source) && (source[i] == ' ' || source[i] == '\t' || source[i] == '\r' || source[i] == '\n') {
		i++
	}
	if i >= len(source) {
		return false
	}
	switch source[i] {
	case '.', '?', ':':
		return true
	}
	return false
}

func skipJSString(source []byte, start int, quote byte) int {
	i := start + 1
	for i < len(source) {
		if source[i] == '\\' {
			i += 2
			continue
		}
		if source[i] == quote {
			return i + 1
		}
		if source[i] == '\n' {
			// Unterminated — bail so we don't consume the rest of the file.
			return i + 1
		}
		i++
	}
	return len(source)
}

func skipJSTemplate(source []byte, start int) int {
	i := start + 1
	for i < len(source) {
		if source[i] == '\\' {
			i += 2
			continue
		}
		if source[i] == '`' {
			return i + 1
		}
		// `${...}` — recurse over the embedded expression.
		if source[i] == '$' && i+1 < len(source) && source[i+1] == '{' {
			depth := 1
			i += 2
			for i < len(source) && depth > 0 {
				switch source[i] {
				case '{':
					depth++
				case '}':
					depth--
				}
				i++
			}
			continue
		}
		i++
	}
	return len(source)
}
