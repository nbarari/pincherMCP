package ast

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/nikolalohinski/gonja/builtins"
	"github.com/nikolalohinski/gonja/builtins/statements"
	"github.com/nikolalohinski/gonja/config"
	"github.com/nikolalohinski/gonja/nodes"
	"github.com/nikolalohinski/gonja/parser"
	"github.com/nikolalohinski/gonja/tokens"
)

// jinjaParseTimeout caps gonja parser/lexer execution per file.
// gonja's lexer can deadlock on certain truncated inputs (e.g. `{% block`
// without a closing `%}`) — its lexer runs as a goroutine reading
// state from an internal channel and never reaches EOF when the
// closing token is missing. The timeout protects the indexer goroutine
// from hanging on a single malformed file.
//
// 2 seconds is generous for any realistic template (most Ansible
// templates are <50 KB and parse in <10ms). The trade-off is that one
// pathological file delays its containing index batch by up to 2s;
// since extraction is already concurrent across files, this is fine.
const jinjaParseTimeout = 2 * time.Second

// jinjaExtractor parses Jinja2 templates (Ansible / Salt / generic
// Jinja2) via github.com/nikolalohinski/gonja's parser. Pure Go,
// closes #70 — homelab/SRE/platform repos with Ansible were previously
// half-blind to pincher because `.j2` files weren't recognised as
// source.
//
// Symbol shape:
//
//	{% macro name(args) %}    →  Function           qn=<module>.name
//	{% block name %}          →  Block              qn=<module>.name
//	{% set var = expr %}      →  Setting            qn=<module>.var
//	{% extends "parent.j2" %} →  IMPORTS edge       to_name=parent.j2
//	{% include "child.j2"  %} →  IMPORTS edge       to_name=child.j2
//	{% import "lib.j2" as l %}→  IMPORTS edge       to_name=lib.j2
//
// Confidence is 1.0 (real parser, not regex).
//
// **Two real caveats** documented inline:
//
//  1. gonja's `nodes.Walk()` is incomplete (returns "Unknown type %T"
//     for most node types). We walk manually with a type-switch — the
//     AST is well-typed and exported; only the convenience walker is
//     partial.
//  2. gonja's parser doesn't validate Ansible/Salt custom filters
//     (`default`, `to_json`, `b64encode`, `salt.module.func()`, etc.)
//     because the unknown-filter check is commented out in
//     parser/filters.go. So we don't need to register filter stubs —
//     parsing succeeds and the AST contains a generic FilterCall
//     node we can ignore at extract time.
//
// Routes to the code corpus via ClassifyCorpus's default rule
// ("everything else") — Jinja2 carries logic (control flow, macros)
// closer to code than to static config, and Ansible-aware queries
// (#71) want it adjacent to the Go/Python/etc. code corpus.
type jinjaExtractor struct{}

func (j *jinjaExtractor) Languages() []string { return []string{"Jinja2"} }
func (j *jinjaExtractor) Extensions() map[string]string {
	return map[string]string{
		".j2":     "Jinja2",
		".jinja":  "Jinja2",
		".jinja2": "Jinja2",
	}
}
func (j *jinjaExtractor) Confidence() float64 { return 1.0 }

func (j *jinjaExtractor) Extract(source []byte, _, relPath string, _ ExtractOptions) (result *FileResult) {
	module := jinjaModuleName(relPath)
	result = &FileResult{Module: module}
	if len(source) == 0 {
		return result
	}

	// Defensive recover: gonja shouldn't panic on any input, but a
	// malformed file shouldn't take down the indexer goroutine. Partial
	// result is better than a crash. Same pattern as the HCL/Markdown
	// extractors.
	defer func() {
		if r := recover(); r != nil {
			if result == nil {
				result = &FileResult{Module: module}
			}
		}
	}()

	tpl, err := parseJinja(string(source), relPath)
	if err != nil || tpl == nil {
		return result
	}

	// Macros and Blocks are exposed as maps on the Template directly
	// (gonja indexes them at parse time for execution-side lookup).
	// Use the maps as the canonical source — the Nodes slice may also
	// contain them but the maps guarantee no duplicates.
	for _, m := range jinjaSortedMacros(tpl.Macros) {
		result.Symbols = append(result.Symbols, jinjaMacroSymbol(m, source, module))
	}
	for _, name := range jinjaSortedBlockNames(tpl.Blocks) {
		w := tpl.Blocks[name]
		if w == nil {
			continue
		}
		result.Symbols = append(result.Symbols, jinjaBlockSymbol(name, w, source, module))
	}

	// Walk Nodes for {% set %} statements + {% extends/include/import %}
	// edges. The walker is bespoke because gonja's nodes.Walk() is
	// incomplete (see caveat 1 in the type godoc).
	jinjaWalkNodes(tpl.Nodes, source, module, relPath, result)

	return result
}

// parseJinja runs gonja's parser on the source and returns the
// resulting template AST. Builds a parser with the registered builtin
// statement set so {% block %}, {% include %}, etc. parse correctly.
//
// TemplateParser is set to a no-op that returns an empty template —
// for include / extends, we only want the filename, not the parsed
// content of the referenced file. This lets the extractor work without
// a real loader pointing at the project's filesystem.
//
// Wrapped in a goroutine + timeout because gonja's lexer can deadlock
// on truncated inputs (see jinjaParseTimeout godoc). On timeout the
// lexer goroutine leaks until its scheduler-time runs out, but the
// indexer's calling goroutine returns cleanly and continues to the
// next file.
func parseJinja(source, name string) (*nodes.Template, error) {
	type result struct {
		tpl *nodes.Template
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("gonja parser panic: %v", r)}
			}
		}()
		stream := tokens.Lex(source)
		p := parser.NewParser(name, config.DefaultConfig, stream)
		p.Statements = builtins.Statements
		p.TemplateParser = func(name string) (*nodes.Template, error) {
			// No-op: extractor doesn't need to recurse into included
			// templates. They'll be extracted on their own when the
			// indexer walks them.
			return &nodes.Template{Name: name, Nodes: []nodes.Node{}}, nil
		}
		tpl, err := p.Parse()
		ch <- result{tpl: tpl, err: err}
	}()
	select {
	case r := <-ch:
		return r.tpl, r.err
	case <-time.After(jinjaParseTimeout):
		return nil, fmt.Errorf("gonja parse timeout after %s", jinjaParseTimeout)
	}
}

func jinjaMacroSymbol(m *nodes.Macro, source []byte, module string) ExtractedSymbol {
	tok := m.Position()
	startByte, endByte := jinjaWrapperByteRange(source, tok, m.Wrapper)
	startLine, endLine := jinjaTokenLines(tok, m.Wrapper)

	args := make([]string, 0, len(m.Kwargs))
	for _, p := range m.Kwargs {
		if p == nil || p.Key == nil {
			continue
		}
		args = append(args, p.Key.String())
	}
	sig := fmt.Sprintf("{%% macro %s(%s) %%}", m.Name, strings.Join(args, ", "))

	return ExtractedSymbol{
		Name:          m.Name,
		QualifiedName: module + "." + m.Name,
		Kind:          "Function",
		StartByte:     startByte,
		EndByte:       endByte,
		StartLine:     startLine,
		EndLine:       endLine,
		Signature:     sig,
		// Jinja convention: macros prefixed with `_` are internal helpers.
		IsExported: !strings.HasPrefix(m.Name, "_"),
	}
}

func jinjaBlockSymbol(name string, w *nodes.Wrapper, source []byte, module string) ExtractedSymbol {
	tok := w.Position()
	startByte, endByte := jinjaWrapperByteRange(source, tok, w)
	startLine, endLine := jinjaTokenLines(tok, w)
	sig := fmt.Sprintf("{%% block %s %%}", name)
	return ExtractedSymbol{
		Name:          name,
		QualifiedName: module + "." + name,
		Kind:          "Block",
		StartByte:     startByte,
		EndByte:       endByte,
		StartLine:     startLine,
		EndLine:       endLine,
		Signature:     sig,
		IsExported:    !strings.HasPrefix(name, "_"),
	}
}

// jinjaWalkNodes recurses through gonja AST nodes, emitting Setting
// symbols for {% set %}, IMPORTS edges for {% extends/include/import %},
// and USES_VAR edges for {{ var_name }} output expressions (#1165).
// Macros and Blocks are handled separately via Template.Macros / .Blocks
// (gonja indexes them there); this walker handles the rest.
func jinjaWalkNodes(ns []nodes.Node, source []byte, module, relPath string, out *FileResult) {
	currentQN := module
	for _, n := range ns {
		switch v := n.(type) {
		case *nodes.StatementBlock:
			jinjaHandleStatement(v, source, currentQN, out)
		case *nodes.Wrapper:
			jinjaWalkNodes(v.Nodes, source, currentQN, relPath, out)
		case *nodes.Output:
			jinjaHandleOutput(v, currentQN, out)
		}
	}
}

// jinjaHandleOutput extracts USES_VAR edges from a {{ ... }} output
// expression. The leftmost identifier in the expression is treated as
// the referenced var name — handles plain {{ x }}, filtered {{ x | f }},
// attribute/index access {{ x.y }} / {{ x[0] }}, and calls {{ x() }}.
//
// The ternary form {{ a if cond else b }} contributes up to three
// references (a, cond, b) so resolve binds them all if their decls
// exist. Literal-only outputs ({{ 42 }}, {{ "hello" }}) contribute none.
//
// A small set of Jinja-reserved names (`loop`, `super`, `caller`, ...)
// is skipped at extraction time — they never bind to a Setting declaration
// and would otherwise dangle into the synthetic-external pool unnecessarily.
// Ansible builtin facts (`ansible_*`, `inventory_hostname`) are left to
// the resolver to drop — they may or may not have a counterpart in
// user-defined vars depending on the repo.
func jinjaHandleOutput(o *nodes.Output, parentQN string, out *FileResult) {
	if o == nil {
		return
	}
	for _, e := range []nodes.Expression{o.Expression, o.Condition, o.Alternative} {
		if e == nil {
			continue
		}
		name := jinjaBaseName(e)
		if !isUsefulJinjaVarName(name) {
			continue
		}
		out.Edges = append(out.Edges, ExtractedEdge{
			FromQN:     parentQN,
			ToName:     name,
			Kind:       "USES_VAR",
			Confidence: 1.0,
		})
	}
}

// jinjaBaseName returns the leftmost identifier in an expression node.
// Recurses through filters, attribute/index access, calls, and test
// expressions so the deepest base identifier is returned. Returns ""
// for literal-only expressions (no identifier to bind to a var decl).
func jinjaBaseName(e nodes.Expression) string {
	if e == nil {
		return ""
	}
	switch v := e.(type) {
	case *nodes.Name:
		if v.Name != nil {
			return v.Name.Val
		}
		return v.String()
	case *nodes.FilteredExpression:
		return jinjaBaseName(v.Expression)
	case *nodes.TestExpression:
		return jinjaBaseName(v.Expression)
	case *nodes.Getattr:
		if inner, ok := v.Node.(nodes.Expression); ok {
			return jinjaBaseName(inner)
		}
	case *nodes.Getitem:
		if inner, ok := v.Node.(nodes.Expression); ok {
			return jinjaBaseName(inner)
		}
	case *nodes.Call:
		if inner, ok := v.Func.(nodes.Expression); ok {
			return jinjaBaseName(inner)
		}
	case *nodes.Variable:
		if len(v.Parts) > 0 && v.Parts[0].Type == nodes.VarTypeIdent {
			return v.Parts[0].S
		}
	}
	return ""
}

// isUsefulJinjaVarName drops literal-keyword tokens and Jinja-internal
// loop/macro names that can never bind to a user-declared var.
func isUsefulJinjaVarName(name string) bool {
	if name == "" {
		return false
	}
	switch name {
	case "loop", "super", "self", "caller", "varargs", "kwargs",
		"true", "false", "none", "True", "False", "None", "_":
		return false
	}
	return true
}

// jinjaHandleStatement extracts a symbol/edge from a single
// {% statement %} block. Switches on the underlying Statement type to
// pull out the relevant data.
func jinjaHandleStatement(sb *nodes.StatementBlock, source []byte, parentQN string, out *FileResult) {
	switch s := sb.Stmt.(type) {
	case *statements.SetStmt:
		// {% set var = ... %} — record a Setting symbol named after the
		// target. Targets are usually *nodes.Name (simple var) but can
		// be Getattr/Getitem (target expression assignment); for those
		// we emit using the rendered string form, which is good enough
		// for snapshot stability.
		varName := jinjaTargetName(s.Target)
		if varName == "" {
			return
		}
		tok := sb.Position()
		startByte := tokenByte(tok)
		endByte := startByte + len(jinjaSetSignature(s))
		if endByte > len(source) {
			endByte = len(source)
		}
		out.Symbols = append(out.Symbols, ExtractedSymbol{
			Name:          varName,
			QualifiedName: parentQN + "." + varName,
			Kind:          "Setting",
			StartByte:     startByte,
			EndByte:       endByte,
			StartLine:     tok.Line,
			EndLine:       tok.Line,
			Signature:     jinjaSetSignature(s),
			IsExported:    !strings.HasPrefix(varName, "_"),
		})

	case *statements.IncludeStmt:
		if s.Filename != "" {
			out.Edges = append(out.Edges, ExtractedEdge{
				FromQN: parentQN, ToName: s.Filename,
				Kind: "IMPORTS", Confidence: 1.0,
			})
		}
	case *statements.ExtendsStmt:
		if s.Filename != "" {
			out.Edges = append(out.Edges, ExtractedEdge{
				FromQN: parentQN, ToName: s.Filename,
				Kind: "IMPORTS", Confidence: 1.0,
			})
		}
	case *statements.ImportStmt:
		if s.Filename != "" {
			out.Edges = append(out.Edges, ExtractedEdge{
				FromQN: parentQN, ToName: s.Filename,
				Kind: "IMPORTS", Confidence: 1.0,
			})
		}
	case *statements.FromImportStmt:
		if s.Filename != "" {
			out.Edges = append(out.Edges, ExtractedEdge{
				FromQN: parentQN, ToName: s.Filename,
				Kind: "IMPORTS", Confidence: 1.0,
			})
		}
	}
}

// jinjaTargetName extracts a printable name from a {% set %} target
// expression. The common case is `*nodes.Name` (a simple identifier);
// Getattr / Getitem fall back to the expression's String() form.
func jinjaTargetName(e nodes.Expression) string {
	if e == nil {
		return ""
	}
	if name, ok := e.(*nodes.Name); ok {
		if name.Name != nil {
			return name.Name.Val
		}
		return name.String()
	}
	return e.String()
}

func jinjaSetSignature(s *statements.SetStmt) string {
	target := jinjaTargetName(s.Target)
	return fmt.Sprintf("{%% set %s = ... %%}", target)
}

// jinjaModuleName turns "roles/web/templates/dhcp.j2" into "dhcp" — the
// filename without extension, which matches the convention used by
// the other extractors (Bash, HCL).
func jinjaModuleName(relPath string) string {
	base := filepath.Base(relPath)
	for _, ext := range []string{".j2", ".jinja", ".jinja2"} {
		if strings.HasSuffix(base, ext) {
			return base[:len(base)-len(ext)]
		}
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		return base[:i]
	}
	return base
}

// jinjaWrapperByteRange computes the byte range covering a wrapper's
// full text — from the opening tag's byte offset through the end of
// its last child node. Used for {% block %} and {% macro %} symbols
// so retrieving the symbol returns the entire block body, mirroring
// the YAML/HCL "key + nested value" semantics.
func jinjaWrapperByteRange(source []byte, openTok *tokens.Token, w *nodes.Wrapper) (start, end int) {
	start = tokenByte(openTok)
	end = start // worst case: just the opening tag
	if w == nil || len(w.Nodes) == 0 {
		// No body — make end point at the next end-of-line so retrieval
		// shows at least the tag itself.
		end = nextLineEnd(source, start)
		return
	}
	last := w.Nodes[len(w.Nodes)-1]
	if last == nil {
		end = nextLineEnd(source, start)
		return
	}
	end = nextLineEnd(source, tokenByte(last.Position()))
	if end <= start {
		end = nextLineEnd(source, start)
	}
	return
}

func jinjaTokenLines(tok *tokens.Token, w *nodes.Wrapper) (start, end int) {
	if tok == nil {
		return 1, 1
	}
	start = tok.Line
	end = start
	if w != nil && len(w.Nodes) > 0 {
		if last := w.Nodes[len(w.Nodes)-1]; last != nil {
			end = last.Position().Line
		}
	}
	return
}

func tokenByte(t *tokens.Token) int {
	if t == nil {
		return 0
	}
	return t.Pos
}

// nextLineEnd returns the byte offset of the newline ending the line
// that contains `off` (or len(source) if off is past EOF). Used to
// give symbol byte ranges a sensible right boundary even when we
// don't know the exact end of the construct.
func nextLineEnd(source []byte, off int) int {
	if off < 0 {
		off = 0
	}
	for off < len(source) && source[off] != '\n' {
		off++
	}
	return off
}

// jinjaSortedMacros returns the macros map sorted by source position
// (ascending). Stable iteration order matters for snapshot tests.
func jinjaSortedMacros(m map[string]*nodes.Macro) []*nodes.Macro {
	out := make([]*nodes.Macro, 0, len(m))
	for _, v := range m {
		if v != nil {
			out = append(out, v)
		}
	}
	// Sort by start byte for stable order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && tokenByte(out[j-1].Position()) > tokenByte(out[j].Position()); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// jinjaSortedBlockNames returns block names sorted by their wrapper's
// source position.
func jinjaSortedBlockNames(blocks nodes.BlockSet) []string {
	out := make([]string, 0, len(blocks))
	for k := range blocks {
		out = append(out, k)
	}
	// Insertion sort by source position of the wrapper.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := blocks[out[j-1]], blocks[out[j]]
			if a == nil || b == nil {
				break
			}
			if tokenByte(a.Position()) <= tokenByte(b.Position()) {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func init() {
	Register(&jinjaExtractor{})
}
