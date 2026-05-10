// Package ast provides multi-language symbol extraction with byte-offset recording.
//
// Each extracted symbol stores start_byte and end_byte alongside line numbers.
// This enables O(1) source retrieval at query time: one SQL lookup, one file seek,
// one read — no re-parsing, no line scanning.
//
// Extractors implement the Extractor interface (registry.go) and self-register
// in init(). Adding a new language is one file: implement Extractor and call
// Register(). The DetectLanguage / IsSourceFile / SupportedLanguages helpers
// consult the registry.
//
// Language support:
//   - Go:         go/ast + go/parser (precise byte offsets via token.Pos)
//   - YAML/JSON:  gopkg.in/yaml.v3 Node tree (Setting symbols with dotted paths)
//   - Bash:       mvdan.cc/sh/v3/syntax (the shfmt parser; Function symbols
//                 from POSIX and reserved-word style declarations)
//   - HCL:        github.com/hashicorp/hcl/v2/hclsyntax (Resource/DataSource/
//                 Module/Variable/Output/Local/Provider/Block symbols with
//                 prefixed Terraform-reference qualified names; covers .tf and
//                 .tfvars; recurses into nested blocks at any depth)
//   - Python:     regex patterns (function/class/method definitions)
//   - JavaScript: regex patterns (function/class/method/arrow definitions)
//   - TypeScript: regex patterns (extends JavaScript, adds interface/type)
//   - Rust:       regex patterns (fn/struct/enum/trait/impl)
//   - Java:       regex patterns (class/interface/method)
//   - Ruby, PHP, C, C++, C#, Kotlin, Swift: regex fallback
//
// Regex extractors cover ~80% of real-world symbols accurately. The plan for
// lifting them to confidence 1.0 favours per-language pure-Go AST libraries
// (esbuild, modernc/cc, gpython, etc.) over tree-sitter, to preserve pincher's
// pure-Go / tiny-binary invariants. Any backend plugs in via the Extractor
// interface without touching callers.
package ast

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
)

// ExtractedSymbol is the raw output of the AST extractor.
// It does NOT include project-level fields (project_id, file_hash) —
// those are added by the indexer.
type ExtractedSymbol struct {
	Name          string
	QualifiedName string
	Kind          string // Function|Method|Class|Interface|Enum|Type|Variable|Module
	StartByte     int
	EndByte       int
	StartLine     int
	EndLine       int
	Signature     string
	ReturnType    string
	Docstring     string
	Parent        string
	IsExported    bool
	IsTest        bool
	IsEntryPoint  bool
	Complexity    int
	// ExtractionConfidence is set by Extract() based on the parser used.
	// 1.0 = AST-exact (Go). 0.85 = stable regex (Python, TS, Rust, Java).
	// 0.70 = approximate regex (Ruby, PHP, C, C++, C#, Kotlin, Swift).
	ExtractionConfidence float64
}

// ExtractedEdge is a raw call/import relationship found during extraction.
type ExtractedEdge struct {
	FromQN     string
	ToName     string // may be short name; resolved by indexer against symbol table
	Kind       string // CALLS|IMPORTS|INHERITS|IMPLEMENTS
	Confidence float64
}

// FileResult holds all symbols and edges extracted from one file.
type FileResult struct {
	Symbols []ExtractedSymbol
	Edges   []ExtractedEdge
	Module  string // module/package name

	// QNCollisions maps a (qualified-name × kind) tuple that the regex
	// extractor produced more than once in this file → the original
	// occurrence count. Populated by `disambiguateDuplicates`. A non-empty
	// map means the extractor saw scope-blind duplicates (Python nested
	// `def`, TS function shadows, Rust #[cfg]-gated overloads) and made
	// the QNs unique by appending `~<line>` to the 2nd+ occurrences;
	// callers that want to track the underlying issue read this map.
	QNCollisions map[string]int
}

// Extract dispatches to the registered Extractor for the given language.
// source is the raw file content; language is the detected language string.
// relPath is the file path relative to the project root (used for qualified names).
// Each returned symbol has ExtractionConfidence stamped from the chosen extractor.
func Extract(source []byte, language, relPath string) *FileResult {
	return ExtractWithModule(source, language, relPath, "")
}

// ExtractWithModule is Extract with an optional module path prefix (e.g. the
// `module` line from go.mod). When set, the Go extractor strips it from
// intra-module import paths and emits Module-level symbols + IMPORTS edges
// keyed by within-module paths, enabling cross-file dependency queries.
// Pass "" to behave exactly like Extract.
func ExtractWithModule(source []byte, language, relPath, modulePath string) *FileResult {
	e := extractorFor(language)
	if e == nil {
		return &FileResult{}
	}
	result := e.Extract(source, language, relPath, ExtractOptions{ModulePath: modulePath})
	if result == nil {
		return &FileResult{}
	}
	// #115 safety net: every extractor gets duplicate-QN disambiguation,
	// regardless of whether it's regex/AST/goldmark/HCL. Scope-blind
	// regex passes (Python/TS/Rust/C) are the dominant source, but
	// Markdown sibling-heading collisions and YAML byte-range bugs hit
	// the same code path. Centralising here means a new extractor can't
	// forget to call disambiguateDuplicates.
	disambiguateDuplicates(result)
	conf := e.Confidence()
	for i := range result.Symbols {
		// Per-symbol composition (#34). In Phase 1 every signal contributes
		// 0 except BaseExtractor (and KindBaseline which falls back to
		// BaseExtractor), so Compose() returns conf — byte-identical to
		// today. Phase 2 populates the lookup tables and the snapshot diff
		// shifts intentionally.
		sigs := computeSignals(&result.Symbols[i], conf, relPath, source)
		result.Symbols[i].ExtractionConfidence = sigs.Compose()
	}
	return result
}

// langAdapter wraps a free per-language extract function in the Extractor
// interface. Useful for the existing extractGo / extractPython / ... helpers
// that pre-date the interface; new extractors should be full structs so they
// can carry per-extractor state (e.g. a cached parser instance).
type langAdapter struct {
	primary    string                                                                          // primary language name (e.g. "JavaScript")
	aliases    []string                                                                        // additional language names this extractor handles ("JSX")
	exts       map[string]string                                                               // extension → language name (e.g. {".jsx": "JSX"})
	filenames  map[string]string                                                               // exact basename → language (e.g. {"Makefile": "Makefile"})
	confidence float64                                                                         // 0.0–1.0
	fn         func(source []byte, language, relPath string, opts ExtractOptions) *FileResult // delegate
}

func (a *langAdapter) Languages() []string {
	out := make([]string, 0, 1+len(a.aliases))
	out = append(out, a.primary)
	out = append(out, a.aliases...)
	return out
}
func (a *langAdapter) Extensions() map[string]string { return a.exts }

// Filenames satisfies the optional FilenameExtractor interface. Adapters
// without filename-based detection set the field to nil; the registry
// gracefully treats nil as "no filename claims".
func (a *langAdapter) Filenames() map[string]string { return a.filenames }
func (a *langAdapter) Confidence() float64          { return a.confidence }
func (a *langAdapter) Extract(source []byte, language, relPath string, opts ExtractOptions) *FileResult {
	return a.fn(source, language, relPath, opts)
}

// stubAdapter registers a language as detected (so IsSourceFile returns true)
// but produces zero symbols. Used for languages pincher recognises but doesn't
// yet have an extractor for (Scala, Lua, Bash, Elixir, Haskell, Dart, Zig, R).
func stubAdapter(name string, exts ...string) *langAdapter {
	em := make(map[string]string, len(exts))
	for _, e := range exts {
		em[e] = name
	}
	return &langAdapter{
		primary: name, exts: em, confidence: 0,
		fn: func([]byte, string, string, ExtractOptions) *FileResult { return &FileResult{} },
	}
}

func init() {
	// AST-exact extractors (confidence 1.0)
	Register(&langAdapter{
		primary: "Go",
		exts:    map[string]string{".go": "Go"},
		confidence: 1.0,
		fn: func(s []byte, _, p string, o ExtractOptions) *FileResult {
			return extractGo(s, p, o.ModulePath)
		},
	})
	Register(&langAdapter{
		primary: "YAML", aliases: []string{"JSON"},
		exts: map[string]string{
			".yml": "YAML", ".yaml": "YAML",
			".json": "JSON",
		},
		confidence: 1.0,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractYAML(s, p)
		},
	})

	// Stable regex (confidence 0.85)
	Register(&langAdapter{
		primary: "Python",
		exts:    map[string]string{".py": "Python", ".pyw": "Python"},
		confidence: 0.85,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractPython(s, p)
		},
	})
	Register(&langAdapter{
		primary: "JavaScript", aliases: []string{"JSX"},
		exts: map[string]string{
			".js": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript",
			".jsx": "JSX",
		},
		confidence: 0.85,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractJavaScript(s, p)
		},
	})
	Register(&langAdapter{
		primary: "TypeScript", aliases: []string{"TSX"},
		exts: map[string]string{
			".ts":  "TypeScript",
			".tsx": "TSX",
		},
		confidence: 0.85,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractTypeScript(s, p)
		},
	})
	Register(&langAdapter{
		primary: "Rust",
		exts:    map[string]string{".rs": "Rust"},
		confidence: 0.85,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractRust(s, p)
		},
	})
	Register(&langAdapter{
		primary: "Java",
		exts:    map[string]string{".java": "Java"},
		confidence: 0.85,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractJava(s, p)
		},
	})

	// Approximate regex (confidence 0.70)
	Register(&langAdapter{
		primary: "Ruby",
		exts:    map[string]string{".rb": "Ruby", ".rake": "Ruby"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractRuby(s, p)
		},
	})
	Register(&langAdapter{
		primary: "PHP",
		exts:    map[string]string{".php": "PHP"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractPHP(s, p)
		},
	})
	Register(&langAdapter{
		primary: "C", aliases: []string{"C++"},
		exts: map[string]string{
			".c": "C", ".h": "C",
			".cpp": "C++", ".cxx": "C++", ".cc": "C++",
			".hpp": "C++", ".hh": "C++",
		},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractC(s, p)
		},
	})
	Register(&langAdapter{
		primary: "C#",
		exts:    map[string]string{".cs": "C#"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractCSharp(s, p)
		},
	})
	Register(&langAdapter{
		primary: "Kotlin",
		exts:    map[string]string{".kt": "Kotlin", ".kts": "Kotlin"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractKotlin(s, p)
		},
	})
	Register(&langAdapter{
		primary: "Swift",
		exts:    map[string]string{".swift": "Swift"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractSwift(s, p)
		},
	})

	// Makefile — regex-tier (#103). Detected by both extension (.mk/.mak)
	// and filename (Makefile / GNUmakefile / makefile). Filename detection
	// is the dominant case in real projects; extensions are a long tail.
	Register(&langAdapter{
		primary: "Makefile",
		exts: map[string]string{
			".mk":  "Makefile",
			".mak": "Makefile",
		},
		filenames: map[string]string{
			"Makefile":    "Makefile",
			"GNUmakefile": "Makefile",
			"makefile":    "Makefile", // common-on-case-insensitive-FS variant
		},
		confidence: 0.85,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractMakefile(s, p)
		},
	})

	// SQL — regex-tier (#102). Captures CREATE TABLE/VIEW (Class),
	// CREATE FUNCTION/PROCEDURE/TRIGGER (Function) across all dialects
	// (MySQL / Postgres / SQLite / MSSQL / Oracle). DML, ALTER, and
	// CREATE INDEX are deliberately out of scope.
	Register(&langAdapter{
		primary: "SQL",
		exts: map[string]string{
			".sql":  "SQL",
			".ddl":  "SQL",
		},
		confidence: 0.85,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractSQL(s, p)
		},
	})

	// Detected-but-no-extractor languages (confidence 0; FileResult always empty).
	// Preserves prior IsSourceFile behaviour while making the gap visible via
	// the registry's Confidence() == 0.
	Register(stubAdapter("Scala", ".scala"))
	Register(stubAdapter("Lua", ".lua"))
	Register(stubAdapter("Zig", ".zig"))
	Register(stubAdapter("Elixir", ".ex", ".exs"))
	Register(stubAdapter("Haskell", ".hs"))
	Register(stubAdapter("Dart", ".dart"))
	// Bash is registered separately by bashExtractor in bash.go (real parser).
	Register(stubAdapter("R", ".r"))
}

// ─────────────────────────────────────────────────────────────────────────────
// Go extractor — uses go/ast for precise byte offsets
// ─────────────────────────────────────────────────────────────────────────────

func extractGo(source []byte, relPath, modulePath string) *FileResult {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, relPath, source, parser.ParseComments)
	if err != nil {
		// Partial parse still yields symbols
	}
	if f == nil {
		return &FileResult{}
	}

	result := &FileResult{}
	if f.Name != nil {
		result.Module = f.Name.Name
	}

	lineOffsets := buildLineOffsets(source)
	isTest := strings.HasSuffix(relPath, "_test.go")

	// Track current package prefix for qualified names
	pkg := ""
	if f.Name != nil {
		pkg = f.Name.Name
	}

	// Within-module import path for this file's package (e.g. "internal/db").
	// Used as the qualified name of the Module symbol and as the FromQN of
	// IMPORTS edges, so they can be resolved across files by the indexer.
	fileModuleQN := filepath.ToSlash(filepath.Dir(relPath))
	if fileModuleQN == "." {
		fileModuleQN = pkg
	}

	// Emit a Module symbol for this file — gives IMPORTS edges a stable
	// endpoint to point at. One Module symbol per file; packages with
	// multiple files produce multiple Module rows, all sharing qualified_name.
	if f.Name != nil {
		pkgPos := fset.Position(f.Name.Pos())
		pkgEnd := fset.Position(f.Name.End())
		result.Symbols = append(result.Symbols, ExtractedSymbol{
			Name:          pkg,
			QualifiedName: fileModuleQN,
			Kind:          "Module",
			StartByte:     pkgPos.Offset,
			EndByte:       pkgEnd.Offset,
			StartLine:     pkgPos.Line,
			EndLine:       pkgEnd.Line,
			Signature:     "package " + pkg,
			IsExported:    true,
		})
	}

	// Extract top-level imports as edges from this file's Module symbol.
	// Intra-module imports are rewritten to within-module paths so they
	// resolve against other Module symbols. External imports keep their
	// full path and will stay unresolved (no matching symbol indexed).
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		toName := path
		if modulePath != "" && (path == modulePath || strings.HasPrefix(path, modulePath+"/")) {
			toName = strings.TrimPrefix(strings.TrimPrefix(path, modulePath), "/")
		}
		result.Edges = append(result.Edges, ExtractedEdge{
			FromQN:     fileModuleQN,
			ToName:     toName,
			Kind:       "IMPORTS",
			Confidence: 1.0,
		})
	}

	// Walk top-level declarations
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := goFuncToSymbol(d, fset, source, lineOffsets, pkg, isTest)
			result.Symbols = append(result.Symbols, sym)

			// Extract calls from function body
			if d.Body != nil {
				calls := extractGoCalls(d.Body, sym.QualifiedName)
				result.Edges = append(result.Edges, calls...)
			}

		case *ast.GenDecl:
			syms := goGenDeclToSymbols(d, fset, source, lineOffsets, pkg)
			result.Symbols = append(result.Symbols, syms...)
		}
	}

	return result
}

func goFuncToSymbol(d *ast.FuncDecl, fset *token.FileSet, source []byte, lineOffsets []int, pkg string, isTest bool) ExtractedSymbol {
	startPos := fset.Position(d.Pos())
	endPos := fset.Position(d.End())

	name := d.Name.Name
	kind := "Function"
	parent := ""
	qn := pkg + "." + name

	// Method if it has a receiver
	if d.Recv != nil && len(d.Recv.List) > 0 {
		kind = "Method"
		recv := d.Recv.List[0]
		recvType := goTypeToString(recv.Type)
		parent = pkg + "." + recvType
		qn = parent + "." + name
	}

	sig := buildGoSignature(d)
	retType := ""
	if d.Type.Results != nil {
		var parts []string
		for _, r := range d.Type.Results.List {
			parts = append(parts, goTypeToString(r.Type))
		}
		retType = strings.Join(parts, ", ")
	}

	doc := ""
	if d.Doc != nil {
		doc = strings.TrimSpace(d.Doc.Text())
	}

	isExported := ast.IsExported(name)
	isEntryPoint := name == "main" && pkg == "main"
	isTestFn := isTest || strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark")

	return ExtractedSymbol{
		Name:          name,
		QualifiedName: qn,
		Kind:          kind,
		StartByte:     startPos.Offset,
		EndByte:       endPos.Offset,
		StartLine:     startPos.Line,
		EndLine:       endPos.Line,
		Signature:     sig,
		ReturnType:    retType,
		Docstring:     doc,
		Parent:        parent,
		IsExported:    isExported,
		IsTest:        isTestFn,
		IsEntryPoint:  isEntryPoint,
		Complexity:    estimateComplexity(source[startPos.Offset:min(endPos.Offset, len(source))]),
	}
}

func goGenDeclToSymbols(d *ast.GenDecl, fset *token.FileSet, source []byte, lineOffsets []int, pkg string) []ExtractedSymbol {
	var syms []ExtractedSymbol
	for _, spec := range d.Specs {
		switch sp := spec.(type) {
		case *ast.TypeSpec:
			startPos := fset.Position(sp.Pos())
			endPos := fset.Position(sp.End())
			kind := "Type"
			switch sp.Type.(type) {
			case *ast.StructType:
				kind = "Class"
			case *ast.InterfaceType:
				kind = "Interface"
			}
			doc := ""
			if d.Doc != nil {
				doc = strings.TrimSpace(d.Doc.Text())
			}
			syms = append(syms, ExtractedSymbol{
				Name:          sp.Name.Name,
				QualifiedName: pkg + "." + sp.Name.Name,
				Kind:          kind,
				StartByte:     startPos.Offset,
				EndByte:       endPos.Offset,
				StartLine:     startPos.Line,
				EndLine:       endPos.Line,
				Docstring:     doc,
				IsExported:    ast.IsExported(sp.Name.Name),
			})
		}
	}
	return syms
}

func extractGoCalls(body *ast.BlockStmt, callerQN string) []ExtractedEdge {
	var edges []ExtractedEdge
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := goCalleeToString(call.Fun)
		if callee != "" {
			edges = append(edges, ExtractedEdge{
				FromQN:     callerQN,
				ToName:     callee,
				Kind:       "CALLS",
				Confidence: 0.7, // unresolved, lower confidence
			})
		}
		return true
	})
	return edges
}

func goCalleeToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return goCalleeToString(e.X) + "." + e.Sel.Name
	default:
		return ""
	}
}

func goTypeToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + goTypeToString(e.X)
	case *ast.SelectorExpr:
		return goTypeToString(e.X) + "." + e.Sel.Name
	case *ast.ArrayType:
		return "[]" + goTypeToString(e.Elt)
	case *ast.MapType:
		return "map[" + goTypeToString(e.Key) + "]" + goTypeToString(e.Value)
	default:
		return "?"
	}
}

func buildGoSignature(d *ast.FuncDecl) string {
	var sb strings.Builder
	sb.WriteString("func ")
	if d.Recv != nil && len(d.Recv.List) > 0 {
		sb.WriteString("(")
		sb.WriteString(goTypeToString(d.Recv.List[0].Type))
		sb.WriteString(") ")
	}
	sb.WriteString(d.Name.Name)
	sb.WriteString("(")
	if d.Type.Params != nil {
		writeGoFieldList(&sb, d.Type.Params.List)
	}
	sb.WriteString(")")
	if d.Type.Results != nil && len(d.Type.Results.List) > 0 {
		sb.WriteString(" ")
		// Parens are mandatory in Go when any return is named OR when
		// there are multiple returns. A single unnamed return like
		// `func f() error` is bare; a single named return like
		// `func f() (x int)` requires parens.
		needParens := len(d.Type.Results.List) > 1
		if !needParens {
			for _, r := range d.Type.Results.List {
				if len(r.Names) > 0 {
					needParens = true
					break
				}
			}
		}
		if needParens {
			sb.WriteString("(")
		}
		writeGoFieldList(&sb, d.Type.Results.List)
		if needParens {
			sb.WriteString(")")
		}
	}
	return sb.String()
}

// writeGoFieldList renders a *ast.FieldList in source-equivalent form.
// Each Field carries 0..N names and one type; grouped same-type fields
// like `func f(a, b, c int)` come through as a single Field with three
// names. The pre-fix renderer (#245) treated each Field as one entry
// regardless of name count, so a 5-named-1-typed return came back as
// a 1-typed signature (wrong arity). This walk emits the names then
// the type, restoring the source shape.
func writeGoFieldList(sb *strings.Builder, fields []*ast.Field) {
	for i, f := range fields {
		if i > 0 {
			sb.WriteString(", ")
		}
		for j, n := range f.Names {
			if j > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(n.Name)
		}
		// Only insert a separating space when names were emitted; an
		// unnamed param like `func f(int)` should render as `int`,
		// not ` int` (the pre-fix code wrote a leading space here).
		if len(f.Names) > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(goTypeToString(f.Type))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Regex-based extractors for other languages
// ─────────────────────────────────────────────────────────────────────────────

// regexExtractor holds pre-compiled patterns for a language.
type regexExtractor struct {
	funcRE      *regexp.Regexp
	classRE     *regexp.Regexp
	interfaceRE *regexp.Regexp
	methodRE    *regexp.Regexp
	importRE    *regexp.Regexp
	enumRE      *regexp.Regexp
}

func (rx *regexExtractor) extract(source []byte, relPath, language string, opts extractOpts) *FileResult {
	result := &FileResult{}
	lines := splitLines(source)
	lineOffsets := buildLineOffsets(source)

	var currentClass string
	var currentClassEnd int

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		lineStart := 0
		if lineIdx < len(lineOffsets) {
			lineStart = lineOffsets[lineIdx]
		}

		// Track class scope for method qualified names
		if rx.classRE != nil {
			if m := rx.classRE.FindStringSubmatch(line); m != nil {
				name := extractGroup(m, "name")
				if name != "" {
					endByte := findBlockEnd(source, lineStart, opts.blockChar)
					endLine := offsetToLine(lineOffsets, endByte)
					parent := extractGroup(m, "parent")
					currentClass = name
					currentClassEnd = endLine
					qn := moduleQN(relPath, opts.modSep) + opts.modSep + name
					result.Symbols = append(result.Symbols, ExtractedSymbol{
						Name:          name,
						QualifiedName: qn,
						Kind:          "Class",
						StartByte:     lineStart,
						EndByte:       endByte,
						StartLine:     lineNum,
						EndLine:       endLine,
						Parent:        parent,
						IsExported:    isExported(name, opts.exportedFn),
					})
				}
			}
		}

		// Reset class context when past its end
		if lineNum > currentClassEnd {
			currentClass = ""
		}

		// Interface
		if rx.interfaceRE != nil {
			if m := rx.interfaceRE.FindStringSubmatch(line); m != nil {
				name := extractGroup(m, "name")
				if name != "" {
					endByte := findBlockEnd(source, lineStart, opts.blockChar)
					qn := moduleQN(relPath, opts.modSep) + opts.modSep + name
					result.Symbols = append(result.Symbols, ExtractedSymbol{
						Name:          name,
						QualifiedName: qn,
						Kind:          "Interface",
						StartByte:     lineStart,
						EndByte:       endByte,
						StartLine:     lineNum,
						EndLine:       offsetToLine(lineOffsets, endByte),
						IsExported:    isExported(name, opts.exportedFn),
					})
				}
			}
		}

		// Enum
		if rx.enumRE != nil {
			if m := rx.enumRE.FindStringSubmatch(line); m != nil {
				name := extractGroup(m, "name")
				if name != "" {
					endByte := findBlockEnd(source, lineStart, opts.blockChar)
					qn := moduleQN(relPath, opts.modSep) + opts.modSep + name
					result.Symbols = append(result.Symbols, ExtractedSymbol{
						Name: name, QualifiedName: qn, Kind: "Enum",
						StartByte: lineStart, EndByte: endByte,
						StartLine: lineNum, EndLine: offsetToLine(lineOffsets, endByte),
					})
				}
			}
		}

		// Function / Method
		funcPattern := rx.funcRE
		if currentClass != "" && rx.methodRE != nil {
			funcPattern = rx.methodRE
		}
		if funcPattern != nil {
			if m := funcPattern.FindStringSubmatch(line); m != nil {
				name := extractGroup(m, "name")
				if name != "" {
					endByte := findBlockEnd(source, lineStart, opts.blockChar)
					endLine := offsetToLine(lineOffsets, endByte)
					sig := strings.TrimSpace(line)
					if len(sig) > 200 {
						sig = sig[:200]
					}

					kind := "Function"
					qn := moduleQN(relPath, opts.modSep) + opts.modSep + name
					parent := ""
					if currentClass != "" {
						kind = "Method"
						parent = moduleQN(relPath, opts.modSep) + opts.modSep + currentClass
						qn = parent + opts.modSep + name
					}

					result.Symbols = append(result.Symbols, ExtractedSymbol{
						Name:          name,
						QualifiedName: qn,
						Kind:          kind,
						StartByte:     lineStart,
						EndByte:       endByte,
						StartLine:     lineNum,
						EndLine:       endLine,
						Signature:     sig,
						Parent:        parent,
						IsExported:    isExported(name, opts.exportedFn),
						IsTest:        opts.isTest(name),
						Complexity:    estimateComplexity(source[lineStart:min(endByte, len(source))]),
					})
				}
			}
		}

		// Imports
		if rx.importRE != nil {
			if m := rx.importRE.FindStringSubmatch(line); m != nil {
				imp := extractGroup(m, "path")
				if imp == "" {
					imp = extractGroup(m, "name")
				}
				if imp != "" {
					result.Edges = append(result.Edges, ExtractedEdge{
						ToName: imp, Kind: "IMPORTS", Confidence: 1.0,
					})
				}
			}
		}
	}
	return result
}

// disambiguateDuplicates makes the (QN, kind) keys in result.Symbols unique
// by appending `~<startLine>` to the 2nd+ occurrence of any duplicate.
//
// Why: the regex extractors are scope-blind. A file with multiple
// `def helper():` inside different test functions, or several
// `function jsx(...)` polymorphic variants, or `#[cfg(...)]`-gated Rust
// `fn` overloads will produce ExtractedSymbol entries that all share the
// same QualifiedName. Pre-fix, those symbols collapsed at
// `BulkUpsertSymbols` (last-write-wins via `MakeSymbolID`) and N-1
// occurrences were silently lost (#115). Disambiguation by line keeps
// every symbol addressable while preserving the canonical first
// occurrence for callers that already search the un-suffixed QN.
//
// The pre-disambiguation collision count is recorded in
// `result.QNCollisions` so the existing #42 extraction-failure heuristic
// (`recordExtractionHeuristics`) can still surface the underlying
// regex-scope blindness — disambiguation hides the symbol-loss symptom,
// it doesn't pretend the regex extractor became smarter.
//
// Order-preserving: scans symbols in their original emission order, so
// the first occurrence keeps its plain QN. Determinism: same input
// always produces the same suffixed QNs because line numbers are stable.
func disambiguateDuplicates(result *FileResult) {
	if len(result.Symbols) <= 1 {
		return
	}
	type key struct{ qn, kind string }
	count := make(map[key]int, len(result.Symbols))
	for _, s := range result.Symbols {
		count[key{s.QualifiedName, s.Kind}]++
	}
	collisions := make(map[string]int)
	for k, n := range count {
		if n > 1 {
			collisions[k.qn] = n
		}
	}
	if len(collisions) == 0 {
		return
	}
	seen := make(map[key]int, len(collisions))
	for i := range result.Symbols {
		s := &result.Symbols[i]
		k := key{s.QualifiedName, s.Kind}
		if count[k] <= 1 {
			continue
		}
		seen[k]++
		if seen[k] == 1 {
			continue // first occurrence keeps the plain QN
		}
		s.QualifiedName = fmt.Sprintf("%s~%d", s.QualifiedName, s.StartLine)
	}
	result.QNCollisions = collisions
}

type extractOpts struct {
	modSep     string
	blockChar  byte
	exportedFn func(string) bool
	isTest     func(string) bool
}

// Language-specific extractors

var pyRE = &regexExtractor{
	funcRE:  regexp.MustCompile(`(?m)^[ \t]*def\s+(?P<name>[A-Za-z_][A-Za-z0-9_]*)\s*\(`),
	classRE: regexp.MustCompile(`(?m)^class\s+(?P<name>[A-Za-z_][A-Za-z0-9_]*)(?:\((?P<parent>[^)]*)\))?:`),
	importRE: regexp.MustCompile(
		`(?m)^(?:from\s+(?P<path>[.\w]+)\s+import|import\s+(?P<name>[.\w]+))`),
}

func extractPython(source []byte, relPath string) *FileResult {
	opts := extractOpts{
		modSep:    ".",
		blockChar: 0, // Python uses indentation, approximate via colon heuristic
		exportedFn: func(name string) bool {
			return !strings.HasPrefix(name, "_")
		},
		isTest: func(name string) bool {
			return strings.HasPrefix(name, "test_") || strings.HasPrefix(name, "Test")
		},
	}
	res := pyRE.extract(source, relPath, "Python", opts)
	// Derive module name from file path
	mod := strings.TrimSuffix(relPath, ".py")
	mod = strings.ReplaceAll(mod, "/", ".")
	mod = strings.ReplaceAll(mod, "\\", ".")
	res.Module = mod
	return res
}

var jsRE = &regexExtractor{
	funcRE: regexp.MustCompile(
		`(?m)(?:^|\s)(?:export\s+)?(?:async\s+)?function\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)|` +
			`(?m)(?:^|\s)(?:export\s+)?(?:const|let|var)\s+(?P<name2>[A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?\(`),
	classRE: regexp.MustCompile(`(?m)^(?:export\s+)?(?:default\s+)?class\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)(?:\s+extends\s+(?P<parent>[A-Za-z_$][A-Za-z0-9_$]*))?`),
	importRE: regexp.MustCompile(`(?m)^import\s+.*?from\s+['"](?P<path>[^'"]+)['"]`),
}

func extractJavaScript(source []byte, relPath string) *FileResult {
	opts := extractOpts{
		modSep:    ".",
		blockChar: '{',
		exportedFn: func(name string) bool {
			// Exported if the declaration has 'export' before it
			return true
		},
		isTest: func(name string) bool {
			return strings.HasPrefix(name, "test") || strings.HasPrefix(name, "spec")
		},
	}
	return jsRE.extract(source, relPath, "JavaScript", opts)
}

var tsRE = &regexExtractor{
	funcRE: regexp.MustCompile(
		`(?m)(?:^|\s)(?:export\s+)?(?:async\s+)?function\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)|` +
			`(?m)(?:^|\s)(?:export\s+)?(?:const|let|var)\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?\(`),
	classRE:     regexp.MustCompile(`(?m)^(?:export\s+)?(?:abstract\s+)?class\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)(?:\s+extends\s+(?P<parent>[A-Za-z_$][A-Za-z0-9_$]*))?`),
	interfaceRE: regexp.MustCompile(`(?m)^(?:export\s+)?interface\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)`),
	enumRE:      regexp.MustCompile(`(?m)^(?:export\s+)?(?:const\s+)?enum\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)`),
	importRE:    regexp.MustCompile(`(?m)^import\s+.*?from\s+['"](?P<path>[^'"]+)['"]`),
}

func extractTypeScript(source []byte, relPath string) *FileResult {
	opts := extractOpts{
		modSep:     ".",
		blockChar:  '{',
		exportedFn: func(name string) bool { return true },
		isTest: func(name string) bool {
			return strings.HasPrefix(name, "test") || strings.HasPrefix(name, "describe")
		},
	}
	return tsRE.extract(source, relPath, "TypeScript", opts)
}

var rustRE = &regexExtractor{
	funcRE:      regexp.MustCompile(`(?m)^(?:pub(?:\(.*?\))?\s+)?(?:async\s+)?fn\s+(?P<name>[a-z_][a-z0-9_]*)`),
	classRE:     regexp.MustCompile(`(?m)^(?:pub(?:\(.*?\))?\s+)?struct\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	interfaceRE: regexp.MustCompile(`(?m)^(?:pub(?:\(.*?\))?\s+)?trait\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	enumRE:      regexp.MustCompile(`(?m)^(?:pub(?:\(.*?\))?\s+)?enum\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	importRE:    regexp.MustCompile(`(?m)^use\s+(?P<path>[a-zA-Z0-9_:]+)`),
}

// extractRust: 'pub' keyword marks exports; approximated here as always-exported.
func extractRust(source []byte, relPath string) *FileResult {
	return rustRE.extract(source, relPath, "Rust", simpleOpts("::", '{'))
}

var javaRE = &regexExtractor{
	funcRE:      regexp.MustCompile(`(?m)^\s*(?:public|private|protected)?\s*(?:static\s+)?(?:final\s+)?(?:\w+\s+)+(?P<name>[A-Za-z][A-Za-z0-9_]*)\s*\(`),
	classRE:     regexp.MustCompile(`(?m)^(?:public\s+)?(?:abstract\s+)?(?:final\s+)?class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s+extends\s+(?P<parent>[A-Z][A-Za-z0-9_]*))?`),
	interfaceRE: regexp.MustCompile(`(?m)^(?:public\s+)?interface\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	enumRE:      regexp.MustCompile(`(?m)^(?:public\s+)?enum\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	importRE:    regexp.MustCompile(`(?m)^import\s+(?P<path>[a-zA-Z0-9_.]+)`),
}

func extractJava(source []byte, relPath string) *FileResult {
	opts := extractOpts{
		modSep:    ".",
		blockChar: '{',
		exportedFn: func(name string) bool {
			return len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
		},
		isTest: func(name string) bool { return false },
	}
	return javaRE.extract(source, relPath, "Java", opts)
}

var rubyRE = &regexExtractor{
	funcRE:  regexp.MustCompile(`(?m)^\s*def\s+(?P<name>[a-zA-Z_][a-zA-Z0-9_?!]*)`),
	classRE: regexp.MustCompile(`(?m)^class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s*<\s*(?P<parent>[A-Z][A-Za-z0-9_:]*))?`),
}

func extractRuby(source []byte, relPath string) *FileResult {
	return rubyRE.extract(source, relPath, "Ruby", simpleOpts("::", 0))
}

var phpRE = &regexExtractor{
	funcRE:  regexp.MustCompile(`(?m)^(?:public|private|protected)?\s*(?:static\s+)?function\s+(?P<name>[a-zA-Z_][a-zA-Z0-9_]*)`),
	classRE: regexp.MustCompile(`(?m)^(?:abstract\s+)?class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s+extends\s+(?P<parent>[A-Z][A-Za-z0-9_]*))?`),
}

func extractPHP(source []byte, relPath string) *FileResult {
	return phpRE.extract(source, relPath, "PHP", simpleOpts("\\", '{'))
}

var cRE = &regexExtractor{
	funcRE: regexp.MustCompile(`(?m)^(?:static\s+)?(?:inline\s+)?(?:\w+\s+)+(?P<name>[A-Za-z_][A-Za-z0-9_]*)\s*\(`),
}

// cMacroRE matches Linux-kernel-style declaration macros where the real
// symbol identity is the first argument inside the parens, not the macro
// name itself. Examples: `static DEVICE_ATTR(keys, ...)`, `MODULE_PARM(p)`,
// `EXPORT_SYMBOL(foo)`. Without this, every DEVICE_ATTR in a file collides
// because the regex captures `DEVICE_ATTR` for all of them — issue #69.
//
// Constraint: the macro name must be all-uppercase + digits + underscores
// AND at least one underscore (single-word ALL CAPS like `MAIN` are real
// function names in some embedded codebases). Two-letter all-caps like
// `IO` would also be ambiguous; the underscore requirement filters those.
var cMacroRE = regexp.MustCompile(
	`(?m)^[ \t]*(?:static\s+)?(?P<macro>[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)\s*\(\s*(?P<arg>[A-Za-z_][A-Za-z0-9_]*)`)

// extractC runs the regex extractor over a C source file, then applies
// three post-processing passes that the regex alone can't handle:
//
//  1. rewriteCMacroSymbols (#69/#73): SCREAM_CASE_MACRO(arg, ...) — name
//     the symbol after `arg`, not the macro.
//  2. dropCForwardDecls (#79 part 1): drop `name(args);` declarations.
//     The body's where the source lives; a decl-only symbol has nothing
//     useful to fetch and collides on QN with the matching definition.
//  3. extractCBareMacros (#74): emit Function symbols for column-0
//     bare-prefix macros (EXPORT_SYMBOL, MODULE_PARM_DESC) that funcRE
//     can't match because they have no preceding word.
//
// (#79 part 2's `dedupCSymbolsByQN` step has been retired: the centralised
// `disambiguateDuplicates` call in `ExtractWithModule` now handles
// `#ifdef`/`#else` branch collisions by suffixing the 2nd+ occurrence
// with `~<line>`, so both variants survive instead of one being dropped.
// dedupCSymbolsByQN is kept around for the per-extractor unit tests
// that drive it directly.)
//
// Each pass is independently testable; the order matters because:
//   - rewriteCMacro must run BEFORE the central disambiguator so
//     DEVICE_ATTR and friends get their proper per-instance names
//     rather than colliding on the macro name.
//   - extractCBareMacros must run AFTER dropForwardDecls so the bare
//     macro pass doesn't re-emit names just removed.
func extractC(source []byte, relPath string) *FileResult {
	result := cRE.extract(source, relPath, "C", simpleOpts("::", '{'))
	rewriteCMacroSymbols(result, source, relPath)
	result.Symbols = dropCForwardDecls(result.Symbols, source)
	result.Symbols = append(result.Symbols, extractCBareMacros(source, relPath, result.Symbols)...)
	return result
}

// rewriteCMacroSymbols post-processes regex output to replace macro-style
// symbol names with their first-argument identifiers. Touches Name and
// QualifiedName; byte ranges, kind, line numbers stay untouched.
//
// Why post-process rather than fix the funcRE: the regex pipeline assumes
// one capture group per match drives the symbol name, and shoehorning the
// macro alternative into the same regex produces unreadable patterns and
// risks regressing the common case. A second pass is cheap (we already
// hold all matched symbols in memory) and the logic stays auditable.
func rewriteCMacroSymbols(result *FileResult, source []byte, relPath string) {
	if result == nil || len(result.Symbols) == 0 {
		return
	}
	mod := moduleQN(relPath, "::")
	for i := range result.Symbols {
		sym := &result.Symbols[i]
		// Only consider Function symbols — Class/Interface aren't emitted
		// by the C extractor today, but be defensive.
		if sym.Kind != "Function" {
			continue
		}
		line := sourceLineAt(source, sym.StartByte)
		m := cMacroRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// extractGroup() in this file is a "first-non-empty-subgroup"
		// helper that pre-dates this fix; it does NOT honour the name
		// argument. cMacroRE has TWO named groups (macro + arg) so we
		// must look up arg by its real index.
		argIdx := cMacroRE.SubexpIndex("arg")
		if argIdx < 0 || argIdx >= len(m) {
			continue
		}
		arg := m[argIdx]
		if arg == "" {
			continue
		}
		// Rewrite name + QN. Old QN was `<mod>::DEVICE_ATTR`; new is
		// `<mod>::keys`. Signature stays as the original line so users
		// still see the macro shape in search results.
		sym.Name = arg
		sym.QualifiedName = mod + "::" + arg
	}
}

// sourceLineAt returns the line containing byte offset off. Returns "" if
// off is out of range.
func sourceLineAt(source []byte, off int) string {
	if off < 0 || off >= len(source) {
		return ""
	}
	start := off
	for start > 0 && source[start-1] != '\n' {
		start--
	}
	end := off
	for end < len(source) && source[end] != '\n' {
		end++
	}
	return string(source[start:end])
}

// dropCForwardDecls removes Function symbols whose source position is a
// `name(args);` forward declaration rather than a `name(args) { ... }`
// definition. The decl-only form has no body to fetch and collides on
// qualified_name with the matching definition (#79 part 1).
//
// Detection walks forward from the symbol's StartByte, tracking
// parenthesis depth, then inspects the first non-whitespace, non-comment
// character after the parameter list closes. `;` → forward decl (drop).
// `{` → definition (keep). Anything else (e.g. `__attribute__((...))`)
// is treated as a definition to err on the side of keeping symbols.
//
// Multi-line forward decls (parameters on separate lines) are handled
// because the scan tracks paren depth, not line-by-line.
func dropCForwardDecls(syms []ExtractedSymbol, source []byte) []ExtractedSymbol {
	out := syms[:0]
	for _, s := range syms {
		if s.Kind == "Function" && cIsForwardDecl(source, s.StartByte) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// cIsForwardDecl returns true if the C function declaration starting at
// `off` is a forward declaration (terminated by `;`, no body) rather
// than a definition (followed by `{`).
func cIsForwardDecl(source []byte, off int) bool {
	if off < 0 || off >= len(source) {
		return false
	}
	// Find the first `(` after off.
	i := off
	for i < len(source) && source[i] != '(' && source[i] != '\n' {
		i++
	}
	if i >= len(source) || source[i] != '(' {
		// No `(` on this line — bail. The regex extractor wouldn't have
		// matched without one, but be defensive.
		return false
	}

	// Walk through the parameter list, tracking paren depth.
	depth := 0
	for ; i < len(source); i++ {
		switch source[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				// Past the closing `)`. Find the next non-whitespace,
				// non-comment char.
				return cNextSignificantByteIs(source, i+1, ';')
			}
		}
	}
	return false // EOF inside parens — treat as definition (don't drop)
}

// cNextSignificantByteIs returns true if the next non-whitespace,
// non-comment character starting at `off` equals `want`. Skips
// `// line` and `/* block */` comments and ASCII whitespace. Returns
// false on EOF or any other character.
func cNextSignificantByteIs(source []byte, off int, want byte) bool {
	for i := off; i < len(source); i++ {
		c := source[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			continue
		case c == '/' && i+1 < len(source) && source[i+1] == '/':
			// Skip to end of line.
			for i < len(source) && source[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(source) && source[i+1] == '*':
			// Skip to */
			i += 2
			for i+1 < len(source) && !(source[i] == '*' && source[i+1] == '/') {
				i++
			}
			i++ // step past `/`
		case c == want:
			return true
		default:
			return false
		}
	}
	return false
}

// extractCBareMacros emits Function symbols for column-0 bare-prefix
// declaration macros (`EXPORT_SYMBOL(name);`, `MODULE_PARM_DESC(...)`)
// that the main funcRE can't match because they have no preceding word
// or `static`/`inline` keyword (#74). The first arg of the macro is the
// symbol name, mirroring the rewriteCMacroSymbols rule for the
// `static MACRO(...)` form.
//
// Skips lines whose start byte is already covered by an existing
// symbol — avoids double-emit when the same line was matched by the
// main extractor and rewriteCMacroSymbols renamed it.
func extractCBareMacros(source []byte, relPath string, existing []ExtractedSymbol) []ExtractedSymbol {
	if len(source) == 0 {
		return nil
	}

	taken := make(map[int]struct{}, len(existing))
	for _, s := range existing {
		taken[s.StartByte] = struct{}{}
	}

	mod := moduleQN(relPath, "::")
	argIdx := cMacroRE.SubexpIndex("arg")
	if argIdx < 0 {
		return nil
	}

	matches := cMacroRE.FindAllSubmatchIndex(source, -1)
	out := make([]ExtractedSymbol, 0, len(matches))
	for _, m := range matches {
		startByte := m[0]
		if _, alreadyEmitted := taken[startByte]; alreadyEmitted {
			continue
		}
		argStart, argEnd := m[2*argIdx], m[2*argIdx+1]
		if argStart < 0 || argEnd <= argStart {
			continue
		}
		arg := string(source[argStart:argEnd])

		// EndByte: scan forward from match end to the line's terminating
		// newline OR the matching `;`, whichever comes first. Bare-prefix
		// macros are typically single-line, so this is usually short.
		endByte := startByte
		for endByte < len(source) && source[endByte] != '\n' && source[endByte] != ';' {
			endByte++
		}
		if endByte < len(source) && source[endByte] == ';' {
			endByte++ // include the `;`
		}

		startLine := offsetToLineNumber(source, startByte)
		endLine := offsetToLineNumber(source, endByte)
		sig := strings.TrimSpace(sourceLineAt(source, startByte))
		out = append(out, ExtractedSymbol{
			Name:          arg,
			QualifiedName: mod + "::" + arg,
			Kind:          "Function",
			StartByte:     startByte,
			EndByte:       endByte,
			StartLine:     startLine,
			EndLine:       endLine,
			Signature:     sig,
			IsExported:    true,
		})
	}
	return out
}

// offsetToLineNumber returns the 1-indexed line number for byte offset
// off. O(off) — fine for our use case (one call per symbol).
func offsetToLineNumber(source []byte, off int) int {
	if off > len(source) {
		off = len(source)
	}
	line := 1
	for i := 0; i < off; i++ {
		if source[i] == '\n' {
			line++
		}
	}
	return line
}

// dedupCSymbolsByQN keeps the first symbol per qualified_name and drops
// duplicates. C's preprocessor permits multiple definitions of the same
// function name in mutually-exclusive `#ifdef` / `#else` branches; the
// regex extractor can't tell which branch the active build configures,
// so emitting both produces a qualified_name_collision (#79 part 2).
//
// The first occurrence wins. This is a heuristic — a real fix needs
// preprocessor awareness (modernc.org/cc/v4 is the documented next
// step). The user-visible improvement is that `pincher search` returns
// one canonical symbol per name rather than silently picking the last
// one via BulkUpsertSymbols' last-write-wins behaviour.
func dedupCSymbolsByQN(syms []ExtractedSymbol) []ExtractedSymbol {
	if len(syms) <= 1 {
		return syms
	}
	seen := make(map[string]struct{}, len(syms))
	out := make([]ExtractedSymbol, 0, len(syms))
	for _, s := range syms {
		if _, ok := seen[s.QualifiedName]; ok {
			continue
		}
		seen[s.QualifiedName] = struct{}{}
		out = append(out, s)
	}
	return out
}

var csRE = &regexExtractor{
	funcRE:      regexp.MustCompile(`(?m)^\s*(?:public|private|protected|internal)?\s*(?:static\s+)?(?:async\s+)?(?:\w+\s+)+(?P<name>[A-Za-z][A-Za-z0-9_]*)\s*\(`),
	classRE:     regexp.MustCompile(`(?m)^(?:\s*)(?:public|private|internal)?\s*(?:abstract|sealed)?\s*class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s*:\s*(?P<parent>[A-Z][A-Za-z0-9_, ]*))?`),
	interfaceRE: regexp.MustCompile(`(?m)^(?:\s*)(?:public)?\s*interface\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
}

func extractCSharp(source []byte, relPath string) *FileResult {
	return csRE.extract(source, relPath, "C#", simpleOpts(".", '{'))
}

var kotlinRE = &regexExtractor{
	funcRE:  regexp.MustCompile(`(?m)^\s*(?:public|private|internal|protected)?\s*(?:suspend\s+)?fun\s+(?P<name>[a-zA-Z][a-zA-Z0-9_]*)`),
	classRE: regexp.MustCompile(`(?m)^(?:open\s+)?(?:data\s+)?class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\(|:|\s)`),
}

func extractKotlin(source []byte, relPath string) *FileResult {
	return kotlinRE.extract(source, relPath, "Kotlin", simpleOpts(".", '{'))
}

var swiftRE = &regexExtractor{
	funcRE:      regexp.MustCompile(`(?m)^\s*(?:public|private|internal|open)?\s*(?:static\s+)?func\s+(?P<name>[a-zA-Z][a-zA-Z0-9_]*)`),
	classRE:     regexp.MustCompile(`(?m)^(?:public\s+)?(?:final\s+)?class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s*:\s*(?P<parent>[A-Z][A-Za-z0-9_, ]*))?`),
	interfaceRE: regexp.MustCompile(`(?m)^(?:public\s+)?protocol\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
}

func extractSwift(source []byte, relPath string) *FileResult {
	return swiftRE.extract(source, relPath, "Swift", simpleOpts(".", '{'))
}

// simpleOpts returns extractOpts for languages where every symbol is exported
// and there are no test-detection heuristics. Most non-Go extractors use this.
func simpleOpts(modSep string, blockChar byte) extractOpts {
	return extractOpts{
		modSep:     modSep,
		blockChar:  blockChar,
		exportedFn: func(string) bool { return true },
		isTest:     func(string) bool { return false },
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Utility functions
// ─────────────────────────────────────────────────────────────────────────────

// buildLineOffsets returns the byte offset of the start of each line.
func buildLineOffsets(source []byte) []int {
	offsets := []int{0}
	for i, b := range source {
		if b == '\n' && i+1 < len(source) {
			offsets = append(offsets, i+1)
		}
	}
	return offsets
}

// splitLines splits source into lines without allocating a giant string.
func splitLines(source []byte) []string {
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(source))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

// findBlockEnd finds the byte offset of the closing brace/dedent after startOffset.
// For brace-delimited languages (blockChar='{'), walks forward counting braces.
// For indent-delimited languages (blockChar=0), finds the next line with equal or less indent.
func findBlockEnd(source []byte, startOffset int, blockChar byte) int {
	if startOffset >= len(source) {
		return len(source)
	}
	if blockChar == 0 {
		// Indentation-based (Python): find next line with ≤ indent level
		// Simplified: just return 80 lines worth of bytes
		end := startOffset
		count := 0
		for end < len(source) && count < 80 {
			if source[end] == '\n' {
				count++
			}
			end++
		}
		return min(end, len(source))
	}
	// Brace-delimited: find the matching close character.
	var closeChar byte
	switch blockChar {
	case '{':
		closeChar = '}'
	case '(':
		closeChar = ')'
	case '[':
		closeChar = ']'
	default:
		// Unknown delimiter: scan to end of source.
		return len(source)
	}
	depth := 0
	started := false
	for i := startOffset; i < len(source); i++ {
		if source[i] == blockChar {
			depth++
			started = true
		} else if source[i] == closeChar {
			depth--
			if started && depth == 0 {
				return i + 1
			}
		}
	}
	return len(source)
}

// offsetToLine returns the 1-based line number for a byte offset.
func offsetToLine(lineOffsets []int, offset int) int {
	lo, hi := 0, len(lineOffsets)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if lineOffsets[mid] <= offset {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return hi + 1
}

// moduleQN derives a module/package qualified name prefix from a relative file path.
func moduleQN(relPath, sep string) string {
	// Strip extension
	base := relPath
	if idx := strings.LastIndex(base, "."); idx > 0 {
		base = base[:idx]
	}
	// Normalize path separators to the language separator
	base = strings.ReplaceAll(base, "/", sep)
	base = strings.ReplaceAll(base, "\\", sep)
	return base
}

// extractGroup extracts a named capture group from a regex match.
// Falls back to group 2 if "name" group is empty (for alternation patterns).
func extractGroup(m []string, name string) string {
	// Try to find the group named "name" — but regexp doesn't give us named group indices easily.
	// For simplicity, return the first non-empty group after the full match.
	for i := 1; i < len(m); i++ {
		if m[i] != "" {
			return m[i]
		}
	}
	return ""
}

// isExported checks if a name is exported according to the given rule.
func isExported(name string, fn func(string) bool) bool {
	if fn == nil {
		return len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
	}
	return fn(name)
}

// estimateComplexity counts branch keywords as a rough cyclomatic complexity proxy.
func estimateComplexity(body []byte) int {
	keywords := []string{"if ", "else ", "for ", "while ", "switch ", "case ", "catch ", "&&", "||"}
	count := 1
	s := string(body)
	for _, kw := range keywords {
		count += strings.Count(s, kw)
	}
	return count
}

// min returns the smaller of two ints.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
