package ast

import (
	"bytes"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// bashExtractor parses Bash / sh scripts via mvdan.cc/sh/v3/syntax — a pure-Go
// shell parser used by shfmt. Emits one Function symbol per top-level
// function declaration (`name() { ... }` POSIX style and `function name { ... }`
// reserved-word style), with exact byte offsets pulled from the parser's
// position info.
//
// Confidence is 1.0 (real AST parser, not regex).
//
// Registered for .sh and .bash. Replaces the stub adapter that previously
// registered Bash as a "detected but not extracted" language.
type bashExtractor struct{}

func (b *bashExtractor) Languages() []string { return []string{"Bash"} }
func (b *bashExtractor) Extensions() map[string]string {
	return map[string]string{
		".sh":   "Bash",
		".bash": "Bash",
	}
}
func (b *bashExtractor) Confidence() float64 { return 1.0 }

func (b *bashExtractor) Extract(source []byte, _, relPath string, _ ExtractOptions) *FileResult {
	result := &FileResult{}

	base := filepath.Base(relPath)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	result.Module = base

	if len(source) == 0 {
		return result
	}

	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(bytes.NewReader(source), relPath)
	if err != nil && file == nil {
		// Parse failed irrecoverably — return empty.
		return result
	}

	sourceLen := len(source)

	// First pass: emit Function symbols. We need the set of in-file
	// function names before the second pass so CALLS edges only fire
	// when the callee is locally defined (cross-file calls drop until
	// the cross-file resolver supports Bash). #1341 v0.71.
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil {
			return true
		}
		fn, ok := node.(*syntax.FuncDecl)
		if !ok {
			return true
		}
		if fn.Name == nil || fn.Name.Value == "" {
			return true
		}

		startByte := int(fn.Pos().Offset())
		endByte := int(fn.End().Offset())
		if endByte > sourceLen {
			endByte = sourceLen
		}
		if startByte >= endByte {
			return true
		}

		name := fn.Name.Value
		sig := name + "()"
		if fn.RsrvWord {
			sig = "function " + name
			if fn.Parens {
				sig += "()"
			}
		}

		result.Symbols = append(result.Symbols, ExtractedSymbol{
			Name:          name,
			QualifiedName: result.Module + "." + name,
			Kind:          "Function",
			StartByte:     startByte,
			EndByte:       endByte,
			StartLine:     int(fn.Pos().Line()),
			EndLine:       int(fn.End().Line()),
			Signature:     sig,
			// By Bash convention, names beginning with "_" are treated as
			// internal helpers; everything else is callable from outside.
			IsExported: !strings.HasPrefix(name, "_"),
		})
		return true
	})

	// Second pass: edges. #1341 v0.71. Two shapes from the same AST
	// walk that the existing Function pass already covers:
	//
	//   - CALLS: a CallExpr whose first word is a literal matching one
	//     of the file's Function names. Scope (FromName) is the
	//     enclosing function, or "" for top-level invocations. We
	//     deliberately limit to in-file functions so a stray `ls`
	//     command-line tool reference doesn't synthesize a CALLS to a
	//     non-existent symbol — same policy regex-tier languages use.
	//   - IMPORTS: a CallExpr whose first word is the literal `source`
	//     or `.` (the POSIX include keywords). The second word is the
	//     path; relative paths are passed to the cross-file resolver
	//     as-is.
	//
	// Manual recursive descent rather than syntax.Walk so we can
	// maintain a function-scope stack — Walk's nil-after-children
	// signal doesn't carry parent identity, making stack pop ambiguous.
	defined := make(map[string]struct{}, len(result.Symbols))
	for _, s := range result.Symbols {
		defined[s.Name] = struct{}{}
	}
	emitEdges(file.Stmts, defined, "", result.Module, result)

	return result
}

// emitEdges walks Bash statements emitting CALLS and IMPORTS edges
// per #1341 v0.71. scope is the enclosing function's name ("" for
// top-level statements). module is the FileResult.Module value used to
// build qualified names matching the FuncDecl emit path.
func emitEdges(stmts []*syntax.Stmt, defined map[string]struct{}, scope, module string, result *FileResult) {
	for _, st := range stmts {
		if st == nil {
			continue
		}
		emitEdgesCmd(st.Cmd, defined, scope, module, result)
	}
}

func emitEdgesCmd(cmd syntax.Command, defined map[string]struct{}, scope, module string, result *FileResult) {
	switch c := cmd.(type) {
	case *syntax.CallExpr:
		if len(c.Args) == 0 {
			return
		}
		first := bashFirstLiteral(c.Args[0])
		if first == "" {
			return
		}
		// IMPORTS: `source X` or `. X` (POSIX include).
		if first == "source" || first == "." {
			if len(c.Args) >= 2 {
				target := bashFirstLiteral(c.Args[1])
				if target != "" {
					result.Edges = append(result.Edges, ExtractedEdge{
						FromQN:     bashEdgeFromName(scope, module),
						ToName:     target,
						Kind:       "IMPORTS",
						Confidence: 1.0,
					})
				}
			}
			return
		}
		// CALLS: in-file function name only. Cross-file / external
		// commands are intentionally not edge-resolved at extraction
		// — that's the cross-file resolver's job and Bash isn't there
		// yet.
		if _, ok := defined[first]; ok {
			result.Edges = append(result.Edges, ExtractedEdge{
				FromQN:     bashEdgeFromName(scope, module),
				ToName:     module + "." + first,
				Kind:       "CALLS",
				Confidence: 1.0,
			})
		}
	case *syntax.FuncDecl:
		// Descend into the function body with scope = this function.
		if c.Name == nil || c.Body == nil {
			return
		}
		emitEdgesCmd(c.Body.Cmd, defined, c.Name.Value, module, result)
	case *syntax.Block:
		emitEdges(c.Stmts, defined, scope, module, result)
	case *syntax.IfClause:
		emitEdges(c.Cond, defined, scope, module, result)
		emitEdges(c.Then, defined, scope, module, result)
		if c.Else != nil {
			emitEdgesCmd(c.Else, defined, scope, module, result)
		}
	case *syntax.WhileClause:
		emitEdges(c.Cond, defined, scope, module, result)
		emitEdges(c.Do, defined, scope, module, result)
	case *syntax.ForClause:
		emitEdges(c.Do, defined, scope, module, result)
	case *syntax.CaseClause:
		for _, item := range c.Items {
			emitEdges(item.Stmts, defined, scope, module, result)
		}
	case *syntax.Subshell:
		emitEdges(c.Stmts, defined, scope, module, result)
	}
}

// bashFirstLiteral returns the literal string of a Word's first part
// when it is a plain literal — `foo`, `./script.sh`. Returns "" when
// the word starts with a quote / param expansion / arithmetic / etc.,
// since those don't represent a static callee/path.
func bashFirstLiteral(w *syntax.Word) string {
	if w == nil || len(w.Parts) == 0 {
		return ""
	}
	if lit, ok := w.Parts[0].(*syntax.Lit); ok {
		return lit.Value
	}
	// SglQuoted is a literal string in single quotes — `'./lib.sh'`.
	if sq, ok := w.Parts[0].(*syntax.SglQuoted); ok {
		return sq.Value
	}
	// DblQuoted with a single literal child: `"./lib.sh"`. Skip when
	// there's interpolation inside — that's not a static path.
	if dq, ok := w.Parts[0].(*syntax.DblQuoted); ok {
		if len(dq.Parts) == 1 {
			if lit, ok := dq.Parts[0].(*syntax.Lit); ok {
				return lit.Value
			}
		}
	}
	return ""
}

// bashEdgeFromName returns the qualified name to use as edge FromName.
// Top-level invocations (scope == "") return "" — the indexer treats
// that as the file-scope edge attachment point (matches the convention
// jinja / yaml extractors use for IMPORTS).
func bashEdgeFromName(scope, module string) string {
	if scope == "" {
		return ""
	}
	return module + "." + scope
}

func init() {
	Register(&bashExtractor{})
}
