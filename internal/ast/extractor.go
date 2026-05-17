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
	// Fields is populated for struct (Class) symbols: map of
	// field name → field type expression as a Go-syntax string
	// (e.g. {"stdin": "io.Writer", "cmd": "*exec.Cmd"}). Empty for
	// non-struct symbols. Used by the #423 resolver to follow
	// `recv.field.method` call patterns — look up the receiver's
	// struct, find the field's type, resolve the method on that type.
	// Embedded fields (no name in source) are keyed by their type's
	// last segment (e.g. `sync.Mutex` → key "Mutex").
	Fields map[string]string
	// InterfaceMethods is populated for Interface symbols: the names
	// of methods the interface declares (e.g. ["eval"] for
	// `type whereExpr interface { eval(...) bool }`). Used by the
	// #493 cheap heuristic to mark project-internal methods that
	// satisfy an interface as not-dead — without this, every
	// concrete `eval` would be flagged dead_code because the only
	// caller goes through interface dispatch and the static graph
	// can't see it.
	InterfaceMethods []string
}

// ExtractedEdge is a raw call/import relationship found during extraction.
type ExtractedEdge struct {
	FromQN string
	ToName string // may be short name; resolved by indexer against symbol table
	Kind   string // CALLS|IMPORTS|INHERITS|IMPLEMENTS
	// FromFile is the source file that produced this candidate. The
	// extractor leaves it empty; the indexer stamps it from the
	// per-file goroutine context before deferral, and LoadPendingEdges
	// carries it back through persistence (#487). Used by the resolver
	// to disambiguate FromQN when multiple symbols share it — most
	// commonly `main.main` across `package main` subcommand dirs.
	FromFile   string
	Confidence float64
	// ReceiverType is set when this edge was extracted from inside a
	// Go method body — the method's receiver type expression (e.g.
	// "*Supervisor" for `func (s *Supervisor) M()`). Empty for edges
	// from plain functions or non-Go languages. The #423 resolver
	// uses it to disambiguate field-shaped ToName like "stdin.Write"
	// by intersecting with the struct's Fields map.
	ReceiverType string
	// BaseType is set on a Go READS edge whose source AST node was a
	// non-package selector `base.Sel` where `base` is a local variable,
	// parameter, or receiver whose declared type the extractor could
	// resolve. It holds that type as written, stripped of leading `*`
	// and `[]` (e.g. "ast.ExtractedEdge" for `for _, e := range pending`
	// over a `pending []ast.ExtractedEdge` param). The #760 resolver
	// uses it to tell a struct-field read (`e.Confidence`) from a
	// function-value reference (`w.defaultDo`): if BaseType names a
	// project struct that has a field of the READS edge's ToName, the
	// binding pass must NOT emit a false CALLS edge to a same-named
	// Method. Empty when the base's type couldn't be resolved — the
	// binding pass then keeps its pre-#760 heuristic behaviour.
	BaseType string
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

	// ConfidenceOverride lets a dispatcher (langAdapter for Python /
	// future JS-AST) declare that this FileResult was produced by a
	// higher-quality extractor than the langAdapter's registered
	// confidence. When non-zero, ExtractWithModule uses this value as
	// the baseline for per-symbol composition instead of the adapter's
	// Confidence(). #944: pre-fix the Python AST dispatcher returned
	// symbols stamped at the regex-tier 0.85 → ~0.975, so callers had
	// no signal that AST extraction had actually run.
	ConfidenceOverride float64
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
	// #944: dispatcher-level override. When a langAdapter routes to a
	// higher-quality extractor at runtime (Python AST, future JS AST)
	// the FileResult declares the actual extractor's confidence so the
	// adapter's static-registered value doesn't downgrade AST output.
	if result.ConfidenceOverride > 0 {
		conf = result.ConfidenceOverride
	}
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
		// Same convention as the JS adapter below: registered confidence
		// stays at 0.85 so the regex fallback is honest about its accuracy.
		// When pyASTEnabled() returns true and the CPython subprocess
		// succeeds, the symbols are AST-exact; on parse failure, missing
		// python3, or PINCHER_DISABLE_PY_AST=1 the regex path runs.
		confidence: 0.85,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			if pyASTEnabled() {
				if r, ok := extractPythonAST(s, p); ok {
					return r
				}
			}
			return extractPython(s, p)
		},
	})
	Register(&langAdapter{
		primary: "JavaScript", aliases: []string{"JSX"},
		exts: map[string]string{
			".js": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript",
			".jsx": "JSX",
		},
		// The dispatcher stamps this confidence on every emitted symbol.
		// When PINCHER_EXPERIMENTAL_JS_AST=1 is set and the AST extractor
		// succeeds, the symbols are AST-exact; otherwise they fall back
		// to the regex path's 0.85. We keep the registered confidence at
		// 0.85 so the regex fallback is honest about its accuracy. When
		// the flag flips default-on (planned for v0.14.0 after a clean
		// two-cycle bake), we'll bump this to 1.0.
		confidence: 0.85,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			// #266: AST extraction behind PINCHER_EXPERIMENTAL_JS_AST=1.
			// The AST path eliminates false positives that the regex
			// extractor surfaced on dogfooded JS (#259/#260/#261); on
			// parse failure or with the flag unset, the regex path runs.
			if jsASTEnabled() {
				if r, ok := extractJavaScriptAST(s, p); ok {
					return r
				}
			}
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
	// #1161 v0.63: Lua / Elixir / Zig promoted from stub-tier (0.0) to
	// regex-tier (0.70). The remaining stubs are languages with
	// significantly harder regex-tier representation (Haskell indentation-
	// sensitive, Scala mixed-paradigm, Dart/R requiring more nuanced
	// detection); decide-or-defer documented in their issues under v0.63.
	Register(&langAdapter{
		primary:    "Scala",
		exts:       map[string]string{".scala": "Scala", ".sc": "Scala"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractScala(s, p)
		},
	})
	Register(&langAdapter{
		primary:    "Lua",
		exts:       map[string]string{".lua": "Lua"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractLua(s, p)
		},
	})
	Register(&langAdapter{
		primary:    "Zig",
		exts:       map[string]string{".zig": "Zig"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractZig(s, p)
		},
	})
	Register(&langAdapter{
		primary:    "Elixir",
		exts:       map[string]string{".ex": "Elixir", ".exs": "Elixir"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractElixir(s, p)
		},
	})
	Register(stubAdapter("Haskell", ".hs"))
	Register(&langAdapter{
		primary:    "Dart",
		exts:       map[string]string{".dart": "Dart"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractDart(s, p)
		},
	})
	// Bash is registered separately by bashExtractor in bash.go (real parser).
	Register(&langAdapter{
		primary:    "R",
		exts:       map[string]string{".r": "R", ".R": "R"},
		confidence: 0.70,
		fn: func(s []byte, _, p string, _ ExtractOptions) *FileResult {
			return extractR(s, p)
		},
	})
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
	//
	// #764: importPkgs is the set of package identifiers usable as a
	// selector base in this file (`db` in `db.CorpusCode`). extractGoReads
	// uses it to emit a *qualified* ToName for package selectors so they
	// resolve via lookupQN instead of flattening to a bare `CorpusCode`
	// read that the package-scoped name fallback would (correctly) drop.
	importPkgs := make(map[string]bool)
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
		// Record the in-code identifier for this import. Explicit alias
		// wins; otherwise the conventional name = last path segment
		// (correct for the vast majority; packages whose name differs
		// from the last segment just don't get the qualified-emit
		// optimisation and fall back to the old bare-read behaviour).
		switch {
		case imp.Name == nil:
			if i := strings.LastIndex(path, "/"); i >= 0 {
				importPkgs[path[i+1:]] = true
			} else {
				importPkgs[path] = true
			}
		case imp.Name.Name != "_" && imp.Name.Name != ".":
			importPkgs[imp.Name.Name] = true
		}
	}

	// #1134 v0.67: pre-pass over the file collecting `struct T { Field
	// X }` → fieldType maps so the in-body type-inference can resolve
	// `for _, x := range receiver.Field` patterns. Without this, an
	// element variable iterated from a struct-field slice has no known
	// type, BaseType="" on its later dotted-name READS, and the binding
	// pass false-binds the bare name to any project Method with the
	// same name (the #1134 repro). Per-file scope is sufficient — same-
	// file struct types resolve immediately; cross-file structs stay
	// unknown (a follow-up if real-world data shows it's worth the
	// added cross-file probe).
	fileStructFields := map[string]map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				continue
			}
			fm := map[string]string{}
			for _, f := range st.Fields.List {
				typeStr := goTypeToString(f.Type)
				if typeStr == "" {
					continue
				}
				for _, name := range f.Names {
					fm[name.Name] = typeStr
				}
			}
			if len(fm) > 0 {
				fileStructFields[ts.Name.Name] = fm
			}
		}
	}

	// Walk top-level declarations
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := goFuncToSymbol(d, fset, source, lineOffsets, pkg, isTest)
			result.Symbols = append(result.Symbols, sym)

			// Extract calls from function body
			if d.Body != nil {
				// #423: thread the receiver type through so each CALLS
				// edge can carry it. Empty for plain functions; the
				// resolver only acts on it when present.
				receiverType := ""
				if d.Recv != nil && len(d.Recv.List) > 0 {
					receiverType = goTypeToString(d.Recv.List[0].Type)
				}
				calls := extractGoCalls(d.Body, sym.QualifiedName, receiverType)
				result.Edges = append(result.Edges, calls...)
				// #247 #3: identifier references for READS edges. Walks
				// the same body — costs an extra ast.Inspect pass per
				// function, dwarfed by the parser cost itself.
				reads := extractGoReads(d, sym.QualifiedName, importPkgs, fileStructFields)
				result.Edges = append(result.Edges, reads...)
			}

		case *ast.GenDecl:
			syms := goGenDeclToSymbols(d, fset, source, lineOffsets, pkg)
			result.Symbols = append(result.Symbols, syms...)
			// #576: walk identifier references inside top-level
			// var-decl initializer expressions so function values bound
			// via composite literals (`var X = T{Field: fn}`) surface
			// READS edges. The resolveReads binding pass (#565) then
			// converts those whose target is a Function/Method into
			// CALLS edges so dead_code stops false-flagging the bound
			// function. Same shape as the in-body extractGoReads
			// walker; uses fileModuleQN as the anchor since the
			// reference lives at file scope, not in a function body.
			if d.Tok == token.VAR || d.Tok == token.CONST {
				edges := extractGoFileLevelReads(d, fileModuleQN, importPkgs)
				result.Edges = append(result.Edges, edges...)
			}
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
			var fields map[string]string
			var ifaceMethods []string
			switch t := sp.Type.(type) {
			case *ast.StructType:
				kind = "Class"
				// #423: capture field-name → field-type so the resolver
				// can follow `recv.field.method` calls.
				fields = extractGoStructFields(t)
			case *ast.InterfaceType:
				kind = "Interface"
				// #493: capture interface method names so dead_code
				// can mark project-internal methods that satisfy an
				// interface as not-dead. Cheap heuristic — name match
				// only, no full method-set comparison.
				ifaceMethods = extractGoInterfaceMethods(t)
			}
			doc := ""
			if d.Doc != nil {
				doc = strings.TrimSpace(d.Doc.Text())
			}
			syms = append(syms, ExtractedSymbol{
				Name:             sp.Name.Name,
				QualifiedName:    pkg + "." + sp.Name.Name,
				Kind:             kind,
				StartByte:        startPos.Offset,
				EndByte:          endPos.Offset,
				StartLine:        startPos.Line,
				EndLine:          endPos.Line,
				Docstring:        doc,
				IsExported:       ast.IsExported(sp.Name.Name),
				Fields:           fields,
				InterfaceMethods: ifaceMethods,
			})
		case *ast.ValueSpec:
			// #247 #3: package-level `var` and `const` declarations as
			// Variable symbols. One symbol per name (so `var a, b int`
			// produces two). Required for READS edge resolution — the
			// resolver only persists READS when the target is a Variable.
			// Without these symbols, no inbound trace would ever surface
			// references to package vars; that's the gap #247 #3 fixes.
			//
			// const declarations also extract as Variable (no separate
			// Constant kind in the registered enum). The user-facing
			// benefit is "find references to this name" which works
			// regardless of var-vs-const distinction.
			if d.Tok != token.VAR && d.Tok != token.CONST {
				continue
			}
			doc := ""
			if d.Doc != nil {
				doc = strings.TrimSpace(d.Doc.Text())
			}
			specStart := fset.Position(sp.Pos())
			specEnd := fset.Position(sp.End())
			for _, name := range sp.Names {
				if name == nil || name.Name == "_" {
					continue
				}
				syms = append(syms, ExtractedSymbol{
					Name:          name.Name,
					QualifiedName: pkg + "." + name.Name,
					Kind:          "Variable",
					StartByte:     specStart.Offset,
					EndByte:       specEnd.Offset,
					StartLine:     specStart.Line,
					EndLine:       specEnd.Line,
					Docstring:     doc,
					IsExported:    ast.IsExported(name.Name),
				})
			}
		}
	}
	return syms
}

func extractGoCalls(body *ast.BlockStmt, callerQN, receiverType string) []ExtractedEdge {
	var edges []ExtractedEdge
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := goCalleeToString(call.Fun)
		if callee != "" {
			edges = append(edges, ExtractedEdge{
				FromQN:       callerQN,
				ToName:       callee,
				Kind:         "CALLS",
				Confidence:   0.7, // unresolved, lower confidence
				ReceiverType: receiverType,
			})
		}
		return true
	})
	return edges
}

// extractGoReads emits READS and WRITES edges for identifiers
// referenced inside a function body. The post-pass resolution drops
// references that don't match a known package-level Variable symbol,
// which is the natural filter without doing scope analysis at
// extraction time. Local variables, parameters, types, and function
// names all surface here and get dropped at resolve-time.
//
// Edge attribution:
//   - WRITES: identifier appears as the LHS of an assignment (`Cache =
//     ...`) or in an inc/dec statement (`Counter++`). Short-var-decls
//     (`x := ...`) are LOCAL declarations, not writes-to-package-vars,
//     so they are excluded — emitting WRITES on them would produce
//     false-positive cross-function edges via name collision.
//   - READS: every other identifier reference in the body, including
//     the RHS of assignments. A `Cache = Cache + 1` produces both
//     a WRITES and a READS for `Cache`, which is the correct model.
//   - Pure write (`Cache = make(...)`): WRITES only — the LHS Ident
//     is not walked as a read because the AssignStmt branch consumes
//     LHS expressions before recursing into RHS.
//
// One edge per (name, kind) per function (deduped here so a var read
// 50 times emits one READS, not 50). Confidence 0.5 — lower than
// CALLS (0.7) because over-emission is expected and resolution is
// what filters.
//
// #247 #3: enables `trace inbound name=Cache` to surface every
// function reading or writing a package-level var. The READS / WRITES
// split lets refactor planners ask the narrower question — who
// modifies this vs who only observes it.
// extractGoFileLevelReads emits READS edges for identifier references
// inside a top-level GenDecl initializer. Mirrors extractGoReads but
// operates on `*ast.GenDecl` (var/const blocks) rather than function
// bodies.
//
// #576 motivation: the binding pass (#565) converts READS edges whose
// target resolves to a Function/Method into low-confidence CALLS edges
// so dead_code stops flagging the bound function. extractGoReads only
// runs over `FuncDecl.Body`, so a function bound at file scope —
// `var X = T{Field: fn}`, the canonical "registry of handlers" pattern
// — was invisible. This walker plugs the gap.
//
// One edge per name (deduped) per declaration block. Confidence 0.5,
// matching the in-body walker. Resolution drops anything not pointing
// at a known Function/Method/Variable symbol, so type-name idents
// inside the composite literal (`Target` in `var X = Target{...}`)
// and short package-qualified refs are filtered out at resolve time.
func extractGoFileLevelReads(d *ast.GenDecl, callerQN string, importPkgs map[string]bool) []ExtractedEdge {
	if d == nil {
		return nil
	}
	seen := make(map[string]bool)
	var edges []ExtractedEdge
	emit := func(name string) {
		if isPredeclaredOrBlank(name) || seen[name] {
			return
		}
		seen[name] = true
		edges = append(edges, ExtractedEdge{
			FromQN:     callerQN,
			ToName:     name,
			Kind:       "READS",
			Confidence: 0.5,
		})
	}
	// #764: same imported-package-selector rule as extractGoReads — emit
	// `db.CorpusCode` qualified rather than flattening to bare `db` +
	// `CorpusCode`, so resolveReads' package-scoped fallback doesn't drop
	// a legitimate cross-package file-level read.
	var inspect func(n ast.Node) bool
	inspect = func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.Ident:
			emit(e.Name)
			return false
		case *ast.SelectorExpr:
			if base, ok := e.X.(*ast.Ident); ok && importPkgs[base.Name] {
				emit(base.Name + "." + e.Sel.Name)
				return false
			}
		}
		return true
	}
	for _, spec := range d.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for _, val := range vs.Values {
			ast.Inspect(val, inspect)
		}
	}
	return edges
}

func extractGoReads(d *ast.FuncDecl, callerQN string, importPkgs map[string]bool, fileStructFields map[string]map[string]string) []ExtractedEdge {
	if d == nil || d.Body == nil {
		return nil
	}
	body := d.Body
	seenReads := make(map[string]int) // name → index into edges
	seenWrites := make(map[string]bool)
	var edges []ExtractedEdge

	// varTypes maps a local identifier (receiver, parameter, or in-body
	// declared variable) to its declared type as written. Seeded from
	// the receiver + parameter lists, then extended by a pre-pass over
	// the body for `var x T`, `x := T{}` / `x := &T{}`, and slice-range
	// variables. Used only to stamp ExtractedEdge.BaseType on selector
	// READS edges (#760). Lexical-scope-imperfect (no shadowing
	// tracking), but the resolver acts on BaseType only when it
	// positively confirms a struct field — a stale entry can miss a
	// suppression, never manufacture a false edge.
	varTypes := make(map[string]string)
	noteVarType := func(name, typ string) {
		if name == "" || name == "_" || typ == "" {
			return
		}
		varTypes[name] = typ
	}
	if d.Recv != nil {
		for _, f := range d.Recv.List {
			for _, n := range f.Names {
				noteVarType(n.Name, goTypeToString(f.Type))
			}
		}
	}
	if d.Type != nil && d.Type.Params != nil {
		for _, f := range d.Type.Params.List {
			for _, n := range f.Names {
				noteVarType(n.Name, goTypeToString(f.Type))
			}
		}
	}
	// Pre-pass: collect in-body local-variable → declared-type bindings
	// before the read/write walk so a selector `base.Sel` anywhere in
	// the body can look up `base`'s type. ast.Inspect visits in source
	// order, so a `pending := []T{}` is recorded before a later
	// `for _, e := range pending` can derive `e`'s element type.
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.ValueSpec:
			if s.Type != nil {
				for _, name := range s.Names {
					noteVarType(name.Name, goTypeToString(s.Type))
				}
			}
			for i, val := range s.Values {
				if i < len(s.Names) {
					if ct := compositeLitType(val); ct != "" {
						noteVarType(s.Names[i].Name, ct)
					}
				}
			}
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE && len(s.Lhs) == len(s.Rhs) {
				for i, lhs := range s.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						if ct := compositeLitType(s.Rhs[i]); ct != "" {
							noteVarType(id.Name, ct)
						}
					}
				}
			}
		case *ast.RangeStmt:
			if s.Tok == token.DEFINE && s.Value != nil {
				if vid, ok := s.Value.(*ast.Ident); ok {
					switch xe := s.X.(type) {
					case *ast.Ident:
						if elem := sliceElem(varTypes[xe.Name]); elem != "" {
							noteVarType(vid.Name, elem)
						}
					case *ast.SelectorExpr:
						// #1134 v0.67: `for _, x := range receiver.Field`.
						// Resolve `receiver`'s type via varTypes (set
						// from parameter/receiver lists at function
						// entry), look up `Field`'s declared type in
						// the file-level struct table, then derive the
						// element type. Without this, the element var
						// has no known type and bare-name READS on its
						// fields false-bind to project Methods of the
						// same name.
						if base, ok := xe.X.(*ast.Ident); ok {
							baseType := structBase(varTypes[base.Name])
							if fm := fileStructFields[baseType]; fm != nil {
								if fieldType := fm[xe.Sel.Name]; fieldType != "" {
									if elem := sliceElem(fieldType); elem != "" {
										noteVarType(vid.Name, elem)
									}
								}
							}
						}
					}
				}
			}
		}
		return true
	})

	emitWrite := func(name string) {
		if isPredeclaredOrBlank(name) || seenWrites[name] {
			return
		}
		seenWrites[name] = true
		edges = append(edges, ExtractedEdge{
			FromQN:     callerQN,
			ToName:     name,
			Kind:       "WRITES",
			Confidence: 0.5,
		})
	}
	emitRead := func(name, baseType string) {
		if isPredeclaredOrBlank(name) {
			return
		}
		if idx, ok := seenReads[name]; ok {
			// A later typed read upgrades an earlier untyped one so the
			// #760 resolver gets the disambiguating type even when the
			// first occurrence of this name was a bare identifier.
			if baseType != "" && edges[idx].BaseType == "" {
				edges[idx].BaseType = baseType
			}
			return
		}
		seenReads[name] = len(edges)
		edges = append(edges, ExtractedEdge{
			FromQN:     callerQN,
			ToName:     name,
			Kind:       "READS",
			Confidence: 0.5,
			BaseType:   baseType,
		})
	}

	// walkRead recursively walks any expression tree as a read context —
	// every Ident inside is a READS, with two exceptions.
	//
	// (1) The call subject of a CallExpr is NOT a read. extractGoCalls
	// already emits a CALLS edge for it; walking it here as a read would
	// emit the trailing selector component as a bare identifier —
	// `strings.Index(...)` would emit a read of `Index` that false-binds
	// to any project Method named `Index` via the binding pass (#758).
	// So for a call we walk only the receiver side of a selector-call
	// (`strings` in `strings.Index`) and the arguments.
	//
	// (2) A non-call SelectorExpr whose base is an imported package
	// (`db.CorpusCode`) emits the *qualified* name `db.CorpusCode` as a
	// single read (#764), so resolveReads resolves it via lookupQN
	// instead of flattening to a bare `CorpusCode` that the package-
	// scoped name fallback would drop. Non-package selectors
	// (`w.defaultDo` as a value) still emit their bare `.Sel` so #565
	// function-value bindings keep working.
	var walkRead func(n ast.Node)
	// #1062: forward-declared so walkRead can hand FuncLit bodies off
	// to the statement walker (defined below) without a forward-ref
	// build error.
	var walk func(n ast.Node)
	walkRead = func(n ast.Node) {
		if n == nil {
			return
		}
		switch e := n.(type) {
		case *ast.Ident:
			emitRead(e.Name, "")
		case *ast.CallExpr:
			if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
				walkRead(sel.X)
			}
			// #1062: immediately-invoked / go / defer FuncLit — Fun is
			// the literal itself, not a name. extractGoCalls doesn't
			// emit a CALLS edge for it (no callable name), so we have
			// to walk the body here or the closure's reads vanish.
			if fl, ok := e.Fun.(*ast.FuncLit); ok && fl.Body != nil {
				walk(fl.Body)
			}
			// Bare-Ident call subject (`Foo()`) is skipped — extractGoCalls
			// owns it. Only the args are reads.
			for _, arg := range e.Args {
				walkRead(arg)
			}
		case *ast.FuncLit:
			// #1062: function-literal body — `go func(){...}()`,
			// `defer func(){...}()`, callback args, and closures
			// assigned to locals all parse as FuncLit. Pre-fix the
			// walker stopped here, so READS/WRITES inside the body
			// were invisible (sessionFlushFast at server.go:450 was
			// flagged dead-code by `dead_code`). Hand the body off to
			// the statement walker so AssignStmt/IncDecStmt/etc. fire
			// correctly inside the closure — using ast.Inspect alone
			// would dump every LHS as a bare read.
			if e.Body != nil {
				walk(e.Body)
			}
		case *ast.SelectorExpr:
			if base, ok := e.X.(*ast.Ident); ok && importPkgs[base.Name] {
				// Imported-package selector — emit qualified, don't
				// recurse into the package ident itself.
				emitRead(base.Name+"."+e.Sel.Name, "")
				return
			}
			// Receiver/struct selector — preserve the legacy shape:
			// walk the base as a read, emit the trailing `.Sel` bare.
			// #760: when the base is a local whose declared type we
			// resolved, stamp it onto the `.Sel` READS edge so the
			// binding pass can tell `e.Confidence` (struct-field read)
			// from `w.defaultDo` (function-value reference).
			baseType := ""
			if base, ok := e.X.(*ast.Ident); ok {
				baseType = structBase(varTypes[base.Name])
			}
			walkRead(e.X)
			emitRead(e.Sel.Name, baseType)
		default:
			ast.Inspect(n, func(child ast.Node) bool {
				if child == n {
					return true
				}
				switch c := child.(type) {
				case *ast.Ident:
					emitRead(c.Name, "")
					return false
				case *ast.CallExpr:
					walkRead(c)
					return false
				case *ast.SelectorExpr:
					// Route selectors back through walkRead so the #764
					// imported-package-qualified-emit rule applies even
					// when the selector is nested inside another expr.
					walkRead(c)
					return false
				}
				return true
			})
		}
	}

	// Custom recursive walker that recognises AssignStmt and IncDecStmt
	// at the *statement* level so we don't double-walk LHS expressions
	// as reads. Unrecognised nodes fall through to walkRead which
	// emits READS for every Ident underneath. Variable forward-declared
	// above (#1062) so walkRead can refer to it.
	walk = func(n ast.Node) {
		if n == nil {
			return
		}
		switch v := n.(type) {
		case *ast.AssignStmt:
			// Short-var-decl introduces locals, not writes to existing
			// vars. The LHS names are local declarations; skip writes
			// emission and walk LHS as reads (covers cases like
			// `x, err := f()` where you might want err visible).
			isWrite := v.Tok != token.DEFINE
			for _, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && isWrite {
					emitWrite(id.Name)
				} else {
					walkRead(lhs)
				}
			}
			for _, rhs := range v.Rhs {
				walkRead(rhs)
			}
		case *ast.IncDecStmt:
			if id, ok := v.X.(*ast.Ident); ok {
				emitWrite(id.Name)
			} else {
				walkRead(v.X)
			}
		case *ast.BlockStmt:
			for _, stmt := range v.List {
				walk(stmt)
			}
		case *ast.IfStmt:
			walk(v.Init)
			walkRead(v.Cond)
			walk(v.Body)
			walk(v.Else)
		case *ast.ForStmt:
			walk(v.Init)
			walkRead(v.Cond)
			walk(v.Post)
			walk(v.Body)
		case *ast.RangeStmt:
			// `for k, v := range x` — k/v are local; only Key/Value
			// when assignment-style (`=`) count as writes.
			isWrite := v.Tok != token.DEFINE && v.Tok != token.ILLEGAL
			if v.Key != nil {
				if id, ok := v.Key.(*ast.Ident); ok && isWrite {
					emitWrite(id.Name)
				} else {
					walkRead(v.Key)
				}
			}
			if v.Value != nil {
				if id, ok := v.Value.(*ast.Ident); ok && isWrite {
					emitWrite(id.Name)
				} else {
					walkRead(v.Value)
				}
			}
			walkRead(v.X)
			walk(v.Body)
		case *ast.SwitchStmt:
			walk(v.Init)
			walkRead(v.Tag)
			walk(v.Body)
		case *ast.TypeSwitchStmt:
			walk(v.Init)
			walk(v.Assign)
			walk(v.Body)
		case *ast.SelectStmt:
			walk(v.Body)
		case *ast.CaseClause:
			for _, e := range v.List {
				walkRead(e)
			}
			for _, stmt := range v.Body {
				walk(stmt)
			}
		case *ast.CommClause:
			walk(v.Comm)
			for _, stmt := range v.Body {
				walk(stmt)
			}
		case *ast.LabeledStmt:
			walk(v.Stmt)
		default:
			// Expression statements, return statements, defer, go,
			// declarations, etc — walk every Ident underneath as a read.
			walkRead(n)
		}
	}
	walk(body)
	return edges
}

// compositeLitType returns the type of a composite-literal expression
// (`T{...}` or `&T{...}`) as written — used by extractGoReads's #760
// pre-pass to bind a `:=`-declared local to its struct type. Returns ""
// for any other expression (call results, conversions, etc. — those
// would need full type inference, which the resolver's positive-only
// confirmation makes optional).
func compositeLitType(e ast.Expr) string {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	if cl, ok := e.(*ast.CompositeLit); ok && cl.Type != nil {
		return goTypeToString(cl.Type)
	}
	return ""
}

// structBase strips a leading `*` from a type string and returns it
// only when what remains is a bare (optionally package-qualified)
// identifier — the shape the #760 struct-field lookup can use. Slices,
// maps, channels, funcs, and anonymous struct/interface literals return
// "" (the resolver then keeps its pre-#760 heuristic for that read).
func structBase(t string) string {
	t = strings.TrimPrefix(t, "*")
	if t == "" || strings.ContainsAny(t, " \t[]{}()*<>-/") {
		return ""
	}
	return t
}

// sliceElem returns the element type of a slice type string (`[]T` → `T`),
// "" for non-slice types. Lets the #760 pre-pass derive a range
// variable's type from the collection it ranges over.
func sliceElem(t string) string {
	if rest, ok := strings.CutPrefix(t, "[]"); ok {
		return rest
	}
	return ""
}

// isPredeclaredOrBlank skips identifiers that would never resolve to
// a project-level Variable symbol — saves pending-edge memory at the
// extraction stage. Centralised so the read/write paths share the
// same exclusion list and don't drift.
func isPredeclaredOrBlank(name string) bool {
	switch name {
	case "_", "true", "false", "nil", "iota":
		return true
	}
	return false
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

// extractGoStructFields walks a struct type's field list and returns
// a map of field name → field type expression as a Go-syntax string
// (#423). Used by the resolver to follow `recv.field.method` calls:
// for `s.stdin.Write()` inside `func (s *Supervisor) ...`, look up
// `*Supervisor`'s field `stdin`, find its type `io.Writer`, then
// resolve `Write` against `io.Writer`'s methods.
//
// Embedded fields (no name in source — `sync.Mutex`, `*log.Logger`)
// are keyed by their type's last identifier segment so calls like
// `s.Mutex.Lock()` resolve.
//
// Returns nil for an empty / nil field list — keeps the symbol's
// JSON shape clean (omitempty).
func extractGoStructFields(st *ast.StructType) map[string]string {
	if st == nil || st.Fields == nil || len(st.Fields.List) == 0 {
		return nil
	}
	fields := map[string]string{}
	for _, f := range st.Fields.List {
		if f == nil || f.Type == nil {
			continue
		}
		typeStr := goTypeToString(f.Type)
		if len(f.Names) == 0 {
			// Embedded — use the type's last identifier segment as the
			// field name (Go's promoted-field naming rule).
			if shortName := embeddedFieldName(f.Type); shortName != "" {
				fields[shortName] = typeStr
			}
			continue
		}
		for _, name := range f.Names {
			if name == nil || name.Name == "" || name.Name == "_" {
				continue
			}
			fields[name.Name] = typeStr
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// extractGoInterfaceMethods walks an interface type's method list and
// returns the names of declared methods (#493). Skips embedded
// interfaces (those have no name in source — they're another type
// expression) since the cheap heuristic is name-match only and the
// embedded interface's methods get captured when *that* interface is
// extracted in its own TypeSpec. Returns nil for empty interfaces so
// the symbol's JSON shape stays clean.
func extractGoInterfaceMethods(it *ast.InterfaceType) []string {
	if it == nil || it.Methods == nil || len(it.Methods.List) == 0 {
		return nil
	}
	names := make([]string, 0, len(it.Methods.List))
	for _, f := range it.Methods.List {
		if f == nil {
			continue
		}
		// Embedded interface (`io.Reader`): no Names; skip.
		if len(f.Names) == 0 {
			continue
		}
		// Method element: f.Type is *ast.FuncType, f.Names has one
		// entry holding the method's name.
		if _, isFunc := f.Type.(*ast.FuncType); !isFunc {
			continue
		}
		for _, name := range f.Names {
			if name == nil || name.Name == "" || name.Name == "_" {
				continue
			}
			names = append(names, name.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// embeddedFieldName returns the field name an embedded struct field
// gets (Go's "promoted field" rule): the last identifier segment of
// the type expression. `sync.Mutex` → "Mutex"; `*log.Logger` →
// "Logger"; `io.Reader` → "Reader". Returns "" for unsupported
// shapes (generics with type params, function types).
func embeddedFieldName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return embeddedFieldName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		// Generic embedding: `Foo[T]` → "Foo".
		return embeddedFieldName(t.X)
	}
	return ""
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
	// varRE matches top-level value declarations (#261). Emitted as
	// Variable symbols. Only consulted when funcRE didn't match the
	// same line — otherwise an arrow `const x = () => …` would
	// double-emit (one Function + one Variable for the same name).
	// Optional: extractors that don't supply this skip the variable
	// emission entirely.
	varRE *regexp.Regexp
	// scopeRE matches scope-container syntax that should set Parent for
	// inner symbols but should NOT itself emit a Class symbol — the
	// canonical case is Rust's `impl Type { ... }` and `impl Trait for
	// Type { ... }` blocks (#1183 v0.67 scope-tracking). The named
	// group `name` is the type that becomes Parent for methods inside.
	// Optional: extractors that don't supply this skip scope-only tracking.
	scopeRE *regexp.Regexp
}

func (rx *regexExtractor) extract(source []byte, relPath, language string, opts extractOpts) *FileResult {
	result := &FileResult{}
	lines := splitLines(source)
	lineOffsets := buildLineOffsets(source)

	var currentClass string
	var currentClassEnd int

	// #1375: track the most-recent top-level function so Variable
	// declarations inside its body get a scoped QN
	// (`<func_qn>.<var_name>`) instead of colliding at the module
	// level. Pre-fix, every `const res = await fetch(...)` inside
	// any sibling function in a Next.js App Router page.tsx file
	// produced the same QN (`app.<route>.page.res`) → the dedup
	// guard dropped all but one → real symbols silently disappeared
	// from the index. Same shape applies to TypeScript / JavaScript
	// regex extraction — any language sharing the regexExtractor.
	//
	// Tracked separately from currentClass because:
	//   - A Method already gets its enclosing-class QN via the
	//     classRE path above; Variables inside class methods would
	//     be doubly nested if we conflated the two.
	//   - Functions don't nest (we don't descend into them), so
	//     this single most-recent-function tracker is enough.
	var currentFuncQN string
	var currentFuncEnd int

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		lineStart := 0
		if lineIdx < len(lineOffsets) {
			lineStart = lineOffsets[lineIdx]
		}

		// Track class scope for method qualified names
		if rx.classRE != nil {
			if m := rx.classRE.FindStringSubmatch(line); m != nil {
				name := namedGroup(rx.classRE, m, "name")
				if name != "" {
					endByte := blockEnd(source, lineStart, opts)
					endLine := offsetToLine(lineOffsets, endByte)
					// Swift/C# classRE parent groups use a `[..., ]*`
					// char class (to allow multi-inheritance lists), so
					// the capture greedily eats the trailing space
					// before `{`. Trim so `parent` is "Base", not
					// "Base " (#817).
					parent := strings.TrimSpace(namedGroup(rx.classRE, m, "parent"))
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

		// #1183 v0.67: scope-only container (Rust impl blocks). Sets
		// currentClass to the receiver type so inner methods get the
		// right Parent + qualified name, but emits NO Class symbol —
		// impl is a syntactic grouping, not a type definition. Runs
		// AFTER classRE so a true struct/class on the same line wins.
		if rx.scopeRE != nil && currentClass == "" {
			if m := rx.scopeRE.FindStringSubmatch(line); m != nil {
				name := namedGroup(rx.scopeRE, m, "name")
				if name != "" {
					endByte := blockEnd(source, lineStart, opts)
					endLine := offsetToLine(lineOffsets, endByte)
					currentClass = name
					currentClassEnd = endLine
				}
			}
		}

		// Interface
		if rx.interfaceRE != nil {
			if m := rx.interfaceRE.FindStringSubmatch(line); m != nil {
				name := namedGroup(rx.interfaceRE, m, "name")
				if name != "" {
					endByte := blockEnd(source, lineStart, opts)
					endLine := offsetToLine(lineOffsets, endByte)
					// #819: scope the interface like a class so its
					// member declarations come out as Method/parent=Iface
					// rather than top-level Function/parent="".
					currentClass = name
					currentClassEnd = endLine
					qn := moduleQN(relPath, opts.modSep) + opts.modSep + name
					result.Symbols = append(result.Symbols, ExtractedSymbol{
						Name:          name,
						QualifiedName: qn,
						Kind:          "Interface",
						StartByte:     lineStart,
						EndByte:       endByte,
						StartLine:     lineNum,
						EndLine:       endLine,
						IsExported:    isExported(name, opts.exportedFn),
					})
				}
			}
		}

		// Enum
		if rx.enumRE != nil {
			if m := rx.enumRE.FindStringSubmatch(line); m != nil {
				name := namedGroup(rx.enumRE, m, "name")
				if name != "" {
					endByte := blockEnd(source, lineStart, opts)
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
		funcMatched := false
		if funcPattern != nil {
			if m := funcPattern.FindStringSubmatch(line); m != nil {
				name := namedGroup(funcPattern, m, "name")
				if name != "" {
					endByte := blockEnd(source, lineStart, opts)
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
					funcMatched = true
					// #1375: remember this function's scope so any
					// Variables (`const x = ...`) the varRE finds
					// inside its body get a properly scoped QN
					// instead of colliding at the module level.
					// Functions in this regex tier don't nest
					// (we don't descend into them), so the most-
					// recent-function tracker is sufficient — a
					// later function on a later line simply
					// overwrites it.
					if currentClass == "" {
						currentFuncQN = qn
						currentFuncEnd = endLine
					}
					// #858: per-file CALLS pass. Scan this function's body
					// for C-family call sites so non-Go corpora get an
					// edge graph instead of zero edges.
					if opts.extractCalls {
						result.Edges = append(result.Edges,
							regexCallScan(source[lineStart:min(endByte, len(source))], qn)...)
					}
				}
			}
		}

		// Variable (#261). Only consulted when the line wasn't already
		// claimed as a Function/Method — otherwise `const x = () =>
		// …` would double-emit. The varRE typically anchors at line
		// start with a `const|let|var` keyword, so block-internal
		// statements (loop variables, function locals) also surface
		// as Variables. That's the right call: the alternative
		// (line-start-only) under-emits real top-level constants in
		// indented modules, and the extra noise is filterable via
		// `kind=Variable` searches.
		if !funcMatched && rx.varRE != nil {
			if m := rx.varRE.FindStringSubmatch(line); m != nil {
				name := namedGroup(rx.varRE, m, "name")
				if name != "" {
					endByte := blockEnd(source, lineStart, opts)
					endLine := offsetToLine(lineOffsets, endByte)
					sig := strings.TrimSpace(line)
					if len(sig) > 200 {
						sig = sig[:200]
					}
					// #1375: scope Variables declared inside a
					// function body to that function's QN so sibling
					// functions each declaring `const res = ...` get
					// distinct QNs (`<module>.GET.res` vs
					// `<module>.POST.res`) instead of colliding at the
					// module level (`<module>.res` × N → all but one
					// silently dropped by the qualified_name_collision
					// guard).
					//
					// Parent stamping mirrors the Method case above —
					// callers consuming `parent` can drill from the
					// containing function to its locals.
					parent := ""
					qn := moduleQN(relPath, opts.modSep) + opts.modSep + name
					if currentFuncQN != "" && lineNum <= currentFuncEnd {
						parent = currentFuncQN
						qn = currentFuncQN + opts.modSep + name
					}
					exported := strings.Contains(line, "export")
					result.Symbols = append(result.Symbols, ExtractedSymbol{
						Name:          name,
						QualifiedName: qn,
						Kind:          "Variable",
						StartByte:     lineStart,
						EndByte:       endByte,
						StartLine:     lineNum,
						EndLine:       endLine,
						Signature:     sig,
						Parent:        parent,
						IsExported:    exported,
					})
				}
			}
		}

		// Imports
		if rx.importRE != nil {
			if m := rx.importRE.FindStringSubmatch(line); m != nil {
				imp := namedGroup(rx.importRE, m, "path")
				if imp == "" {
					imp = namedGroup(rx.importRE, m, "name")
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

// regexCallRE matches a C-family call site: an identifier immediately
// followed by `(`. Used by the regex extractor's per-file CALLS pass
// (#858) for languages whose opts set extractCalls. Deliberately
// permissive — the indexer's per-file resolver drops any ToName that
// doesn't match a symbol in the same file, so a keyword or macro that
// slips past regexCallKeywords resolves to nothing and vanishes rather
// than becoming a false edge.
var regexCallRE = regexp.MustCompile(`\b(?P<name>[A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// regexCallKeywords are C-family control-flow / operator keywords that
// are followed by `(` but are not calls. A superset across C / C++ /
// C# / Java / Rust — an entry irrelevant to one language is harmless
// (those names just never appear as a call site there).
var regexCallKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true,
	"return": true, "sizeof": true, "catch": true, "do": true,
	"else": true, "case": true, "defined": true, "alignof": true,
	"typeof": true, "decltype": true, "static_assert": true,
	"_Static_assert": true, "throw": true, "new": true, "delete": true,
	"and": true, "or": true, "not": true,
}

// regexCallScan scans a Function/Method body for C-family call sites and
// emits CALLS edges from fromQN (#858). body is the symbol's full source
// span; everything before the first `{` (the signature — return type,
// name, parameter types) is skipped so the function's own declaration
// doesn't self-match. Targets are bare names; the indexer resolves them
// per-file and drops misses, so over-emission here is bounded — it can
// never produce a cross-file false edge. Confidence 0.6: below AST CALLS
// (0.7), an honest regex-tier signal. Per-name dedup keeps a hot helper
// called ten times from emitting ten identical edges.
func regexCallScan(body []byte, fromQN string) []ExtractedEdge {
	// Skip past the signature so the function's own name doesn't
	// self-match. C-family bodies open with `{`; the byte after `{`
	// is the body's first content. Ruby (and other end-keyword
	// languages) have no `{` — the signature is the first line and
	// the body begins on the next line, so fall back to "skip past
	// the first newline." #1159 v0.62: extended fallback so Ruby's
	// per-file CALLS pass works the same as the {-bodied languages.
	open := bytes.IndexByte(body, '{')
	if open < 0 {
		nl := bytes.IndexByte(body, '\n')
		if nl < 0 {
			return nil
		}
		open = nl
	}
	var edges []ExtractedEdge
	seen := map[string]bool{}
	for _, m := range regexCallRE.FindAllSubmatch(body[open:], -1) {
		name := string(m[1])
		if regexCallKeywords[name] || seen[name] {
			continue
		}
		seen[name] = true
		edges = append(edges, ExtractedEdge{
			FromQN:     fromQN,
			ToName:     name,
			Kind:       "CALLS",
			Confidence: 0.6,
		})
	}
	return edges
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
	// endKeyword selects the `end`-keyword block finder instead of the
	// brace matcher / 80-line indentation fallback. Ruby/Elixir close
	// def/class/module/do with `end`, not a brace — without this every
	// such symbol got an 80-line span clamped to EOF (#805).
	endKeyword bool
	// extractCalls turns on the per-file CALLS pass (#858): each
	// Function/Method body is scanned for C-family `name(` call sites
	// and CALLS edges are emitted. Off by default — only set for
	// languages whose call syntax `regexCallScan` can read. Without it,
	// regex-tier languages produce a zero-edge graph (trace / dead_code
	// silently empty).
	extractCalls bool
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

// JS function-name regex (#259 + #260 fixes).
//
//   - #259: the arrow-function branch must require `=>` after the
//     parameter list — without it, expressions like
//     `const x = (a.b || c).method(...)` falsely match as Function
//     symbols (the regex sees `const NAME = (`). The
//     `(?:[^()]|\([^)]*\))*` span tolerates one level of nested
//     parens in the parameter list, which covers `(a = foo())` and
//     most real arrow signatures.
//   - #260: object-literal methods `name: function(...) {...}` and
//     `name: async function(...) {...}` and `name: (args) => {...}`
//     each emit a Function symbol. Idiomatic in LuCI views, Vue 2
//     `methods:` blocks, AMD modules, jQuery plugin tables, JSON-RPC
//     handler dispatch — the highest-volume miss in regex-era JS.
//
// Trade-off: regex-only fix. Two levels deep paren nesting in
// parameter defaults still falls through; ES6 shorthand methods
// (`name(args) {…}` inside an object literal) are deliberately NOT
// matched because the pattern collides with arbitrary call
// expressions. The full fix is a JS AST extractor (#266); these
// patches close 80% of the gap until that lands.
var jsRE = &regexExtractor{
	funcRE: regexp.MustCompile(
		`(?m)(?:^|\s)(?:export\s+)?(?:async\s+)?function\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)|` +
			`(?m)(?:^|\s)(?:export\s+)?(?:const|let|var)\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?\((?:[^()]|\([^)]*\))*\)\s*=>|` +
			`(?m)^\s*(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*:\s*(?:async\s+)?function\s*\(|` +
			`(?m)^\s*(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*:\s*(?:async\s*)?\((?:[^()]|\([^)]*\))*\)\s*=>`),
	// #261: top-level const/let/var emit Variable symbols. Caught
	// only when funcRE didn't already claim the line as Function —
	// see the !funcMatched gate in regexExtractor.extract.
	varRE: regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:const|let|var)\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*=`),
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

// TS shares the JS arrow-function bug (#259) and the
// object-literal-method gap (#260); same patches applied here. TS
// arrow signatures may also carry a return-type annotation before
// `=>` (e.g. `(a, b): string => …`); the optional `:\s*TYPE` group
// covers simple type names + generics + array/index forms. Function-
// typed returns (`(): (x: number) => number => …`) still fall
// through silently.
//
// #1158 (v0.61): methodRE added so class methods extract as Method
// symbols with Parent = enclosing class. Before this, the TS
// extractor emitted Class symbols but the methods inside were
// invisible — `class Cart { add(item: Item): void { ... } }` produced
// only `Class:Cart`, no `Method:Cart.add`. The pipeline's
// currentClass scope tracking (extractor.go:1438) routes
// inside-class matches to methodRE when set, falling back to funcRE
// otherwise — so this regex needs to match the method shape NOT
// covered by the existing funcRE alternations (no `function` keyword,
// no arrow). Captures:
//   - optional `public`/`private`/`protected`/`readonly` modifier
//   - optional `static`
//   - optional `async`
//   - optional `*` for generators
//   - method name (identifier; constructors land as `constructor`)
//   - opening paren
// Excludes lines that start with `function`/`const`/`let`/`var`/`=`/
// `if`/`for`/`while`/`switch` (already-handled or definitely-not-method).
// Foundational piece of the v0.61 TS receiver-type stack — without
// Method symbols there is nothing for the resolver to bind X.method
// calls to in later releases (#1158).
var tsRE = &regexExtractor{
	funcRE: regexp.MustCompile(
		`(?m)(?:^|\s)(?:export\s+)?(?:async\s+)?function\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)|` +
			`(?m)(?:^|\s)(?:export\s+)?(?:const|let|var)\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s*)?\((?:[^()]|\([^)]*\))*\)\s*(?::\s*[\w<>\[\]\s,|&'"]+\s*)?=>|` +
			`(?m)^\s*(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*:\s*(?:async\s+)?function\s*\(|` +
			`(?m)^\s*(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*:\s*(?:async\s*)?\((?:[^()]|\([^)]*\))*\)\s*(?::\s*[\w<>\[\]\s,|&'"]+\s*)?=>`),
	methodRE: regexp.MustCompile(
		`(?m)^\s*(?:public\s+|private\s+|protected\s+|readonly\s+)?(?:static\s+)?(?:async\s+)?\*?\s*(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*\(`),
	// #261: top-level const/let/var emit Variable symbols (TS parity
	// with JS).
	varRE: regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:const|let|var)\s+(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*(?::[^=]+)?=`),
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
		// #1158 v0.61: opt in to the per-file CALLS pass. TS's `name(`
		// call syntax matches the regexCallRE shape (C-family); same-
		// file calls resolve, cross-file calls drop until the v0.61
		// receiver-type resolver lands. Same shape as C #858 — gives
		// TS users a real edge graph instead of zero edges.
		extractCalls: true,
	}
	result := tsRE.extract(source, relPath, "TypeScript", opts)
	// #1158 v0.61: the new methodRE matches `name(` at line start, which
	// catches control-flow statements like `if (cond) {` / `for (i = 0; ...)`
	// when those appear inside a class body. Filter them out by name.
	result.Symbols = dropTSKeywordFalsePositives(result.Symbols)
	// #1208 v0.66 DOGFOOD: drop TS function-overload SIGNATURES, keep
	// only the implementation. A TS overload set looks like:
	//
	//   export function law(a: string): string;
	//   export function law(a: number): number;
	//   export function law(a: any): any { return a; }
	//
	// Pre-fix the regex emitted three Function symbols all named `law`
	// with identical qualified_name → qualified_name_collision diagnostic
	// fired. The first two are declaration-only signatures (no body,
	// line ends with `;`); only the third is the real implementation.
	// Same shape as dropCForwardDecls.
	result.Symbols = dropTSOverloadSignatures(result.Symbols, source)
	return result
}

// dropTSOverloadSignatures removes Function symbols whose source position
// is a `function name(...): T;` overload signature rather than a
// `function name(...): T { ... }` implementation. The signature-only
// form has no body to fetch and collides on qualified_name with the
// matching implementation (#1208). Detection walks forward from the
// symbol's StartByte, tracking parenthesis depth across the parameter
// list AND any return-type annotation, then inspects the first
// non-whitespace, non-comment character after the type annotation
// closes. `;` → overload signature (drop). `{` → implementation
// (keep). `=>` → arrow function (keep — the regex shouldn't have
// matched but the harmless case is to keep). Anything else is treated
// as an implementation to err on the side of keeping symbols.
//
// Multi-line signatures (parameters and return types on separate
// lines) walk until a top-level `;` or `{` is found.
func dropTSOverloadSignatures(syms []ExtractedSymbol, source []byte) []ExtractedSymbol {
	if len(syms) == 0 {
		return syms
	}
	out := syms[:0]
	for _, s := range syms {
		// Only candidate-drop Function symbols whose declaration uses
		// the `function` keyword. Arrow-function consts (`const x =
		// (...) => expr;`) and object-property arrows match funcRE's
		// other alternatives — their `;` terminates the EXPRESSION,
		// not a signature; the signature-vs-impl distinction doesn't
		// apply.
		if s.Kind == "Function" && tsSignatureLineUsesFunctionKeyword(s.Signature) && isTSOverloadSignature(source, s.StartByte) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// tsSignatureLineUsesFunctionKeyword returns true when the captured
// source line uses the `function` keyword form (the only TS shape that
// has a signature-vs-implementation distinction). Arrow consts and
// object-property arrows match funcRE's other alternatives — they're
// always implementations.
func tsSignatureLineUsesFunctionKeyword(sig string) bool {
	// The regex captures lines like `export function name(...)`,
	// `async function name(...)`, or `function name(...)`. Substring
	// search is sufficient — `function` is a reserved word so it
	// can't appear as an identifier where it'd false-match.
	return strings.Contains(sig, "function ") || strings.HasPrefix(strings.TrimSpace(sig), "function(")
}

// isTSOverloadSignature returns true when the function symbol starting
// at off is a `function name(...): T;` overload signature with no body.
// Walks parens to skip past `(...)` and `: Type` (including `Type<A,B>`
// generics) then checks whether the first body-position token is `;`.
func isTSOverloadSignature(source []byte, off int) bool {
	// Find the first `(` to start parameter-list traversal.
	i := off
	for i < len(source) && source[i] != '(' {
		if source[i] == '\n' && i-off > 200 {
			// Defensive: signature lines are short; if we walked
			// past 200 bytes without a `(`, the symbol probably
			// isn't a function declaration. Treat as
			// non-signature so we keep it.
			return false
		}
		i++
	}
	if i >= len(source) {
		return false
	}
	depth := 0
	for ; i < len(source); i++ {
		switch source[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				// Param list closed; scan past optional `: ReturnType`
				// (with possible generics like `Foo<A,B>` or
				// `Promise<{ a: number }>`) until we hit `;` (overload)
				// or `{` (implementation) at depth 0.
				return scanForTSBodyOrSemicolon(source, i+1)
			}
		}
	}
	return false
}

// scanForTSBodyOrSemicolon walks from `start` past optional whitespace,
// return-type annotation, and generic args, and returns true iff the
// first body-position character is `;` (overload signature). `{`
// indicates an implementation. `=>` is treated as not-a-signature
// (caller already established this is a `function` declaration; the
// `=>` case shouldn't happen but errs on safe-keep). Stops at end of
// source.
func scanForTSBodyOrSemicolon(source []byte, start int) bool {
	depth := 0    // generic / object-type brace depth
	bracket := 0  // square-bracket depth
	for i := start; i < len(source); i++ {
		c := source[i]
		// Skip line comments.
		if c == '/' && i+1 < len(source) && source[i+1] == '/' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		// Skip block comments.
		if c == '/' && i+1 < len(source) && source[i+1] == '*' {
			i += 2
			for i+1 < len(source) && !(source[i] == '*' && source[i+1] == '/') {
				i++
			}
			i++ // step past the closing `/`
			continue
		}
		if c == '<' {
			depth++
			continue
		}
		if c == '>' && depth > 0 {
			depth--
			continue
		}
		if c == '[' {
			bracket++
			continue
		}
		if c == ']' && bracket > 0 {
			bracket--
			continue
		}
		if depth > 0 || bracket > 0 {
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		// Top-level interesting character.
		if c == ';' {
			return true
		}
		if c == '{' {
			return false
		}
		// Anything else (letters, `:`, `,`, `|`, `&`, identifier chars
		// for a return-type expression) keeps scanning.
	}
	// Reached EOF without a `;` or `{` — treat as implementation
	// rather than over-drop on truncated files.
	return false
}

// tsReservedKeywords is the set of TS/JS reserved words and common
// control-flow keywords that the methodRE accidentally captures when
// they appear inside class bodies. None can legally be a TS method
// name (the language reserves them at the parser level), so dropping
// them at extraction is safe. Intentionally narrow — only the
// keywords actually observable as funcRE/methodRE captures in
// real-world TS code. `constructor` IS a valid method name in TS so
// it's NOT in this list.
var tsReservedKeywords = map[string]struct{}{
	"if":       {},
	"else":     {},
	"for":      {},
	"while":    {},
	"do":       {},
	"switch":   {},
	"case":     {},
	"default":  {},
	"break":    {},
	"continue": {},
	"return":   {},
	"throw":    {},
	"try":      {},
	"catch":    {},
	"finally":  {},
	"typeof":   {},
	"instanceof": {},
	"new":      {},
	"delete":   {},
	"void":     {},
	"yield":    {},
	"await":    {},
}

// dropTSKeywordFalsePositives filters Function/Method symbols whose
// Name is a TS reserved word per tsReservedKeywords. Kept Function-
// or-Method-only so a Class/Interface that happens to share a name
// with a keyword (unusual but parseable in some TS idioms) survives.
func dropTSKeywordFalsePositives(syms []ExtractedSymbol) []ExtractedSymbol {
	if len(syms) == 0 {
		return syms
	}
	out := syms[:0]
	for _, s := range syms {
		if s.Kind == "Function" || s.Kind == "Method" {
			if _, isKeyword := tsReservedKeywords[s.Name]; isKeyword {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

var rustRE = &regexExtractor{
	// #1183 v0.67: leading `\s*` so indented impl-block methods match.
	// Pre-fix, `^(?:pub...)fn name` required the declaration at column 0,
	// dropping every fn inside `impl Type { ... }` blocks. Same shape as
	// the PHP regex which already allows indentation.
	funcRE:      regexp.MustCompile(`(?m)^\s*(?:pub(?:\(.*?\))?\s+)?(?:async\s+)?fn\s+(?P<name>[a-z_][a-z0-9_]*)`),
	classRE:     regexp.MustCompile(`(?m)^(?:pub(?:\(.*?\))?\s+)?struct\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	interfaceRE: regexp.MustCompile(`(?m)^(?:pub(?:\(.*?\))?\s+)?trait\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	enumRE:      regexp.MustCompile(`(?m)^(?:pub(?:\(.*?\))?\s+)?enum\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	importRE:    regexp.MustCompile(`(?m)^use\s+(?P<path>[a-zA-Z0-9_:]+)`),
	// #1183 v0.67: impl blocks. Both forms — `impl Type { ... }` and
	// `impl Trait for Type { ... }` — set the receiver type (`Type`,
	// in the second form the second capture) as the scope so inner
	// methods get Parent=Type and the correct QN. Generic parameters
	// on the type are tolerated (`impl<T> Vec<T>` extracts `Vec`).
	// The two alternations: the "for-form" first so its match wins
	// when both could fire on `impl Trait for Type<X>`.
	scopeRE: regexp.MustCompile(`(?m)^impl(?:<[^>]*>)?\s+(?:[A-Z][A-Za-z0-9_]*(?:<[^>]*>)?\s+for\s+)?(?P<name>[A-Z][A-Za-z0-9_]*)(?:<[^>]*>)?\s*\{?`),
}

// extractRust: 'pub' keyword marks exports; approximated here as always-exported.
//
// #1159 v0.62: enable per-file CALLS pass. Pre-fix, every Rust
// project's edge graph was empty — `trace`/`dead_code`/`neighborhood`
// returned the #858 edge-graph-empty warning. Rust's `name(` call
// syntax matches regexCallRE; same-file calls resolve, cross-file
// calls drop until the v0.62 Rust receiver-type resolver lands.
// Macro invocations `name!(...)` aren't `name(...)` so they don't
// false-match.
func extractRust(source []byte, relPath string) *FileResult {
	opts := simpleOpts("::", '{')
	opts.extractCalls = true
	return rustRE.extract(source, relPath, "Rust", opts)
}

var javaRE = &regexExtractor{
	// The return-type token allows an optional generic arg list and
	// array brackets — `(?:\w+\s+)+` alone dropped every method
	// returning `List<String>` / `int[]` because the `<...>`/`[]`
	// breaks the bare-word run before any whitespace (#823). Nested
	// generics (`Map<String,List<Int>>`) are still a residual: the
	// `<[^>]*>` stops at the first `>`.
	funcRE:      regexp.MustCompile(`(?m)^\s*(?:public|private|protected)?\s*(?:static\s+)?(?:final\s+)?(?:[\w.]+(?:<[^>]*>)?(?:\[\])?\s+)+(?P<name>[A-Za-z][A-Za-z0-9_]*)\s*\(`),
	classRE:     regexp.MustCompile(`(?m)^(?:public\s+)?(?:abstract\s+)?(?:final\s+)?class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s+extends\s+(?P<parent>[A-Z][A-Za-z0-9_]*))?`),
	interfaceRE: regexp.MustCompile(`(?m)^(?:public\s+)?interface\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	enumRE:      regexp.MustCompile(`(?m)^(?:public\s+)?enum\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	importRE:    regexp.MustCompile(`(?m)^import\s+(?P<path>[a-zA-Z0-9_.]+)`),
}

func extractJava(source []byte, relPath string) *FileResult {
	opts := extractOpts{
		modSep:    ".",
		blockChar: '{',
		// #820: Java visibility is keyword-based (`public`/`private`),
		// not capitalization. The old rule (name starts uppercase)
		// reported every lowercase method — i.e. ~every Java method —
		// as not-exported, which made `dead_code` flag them all.
		// Regex-tier extraction can't reliably parse the modifier, so
		// match the other regex extractors (PHP/Rust/C#/Kotlin/Swift,
		// all `return true`): conservatively treat symbols as exported
		// rather than mislabel public ones.
		exportedFn: func(name string) bool { return true },
		isTest:     func(name string) bool { return false },
		// #1159 v0.62: enable per-file CALLS pass. Java's `name(` call
		// syntax matches regexCallRE; same-file calls resolve. Cross-
		// file resolution is still a v0.62+ resolver-side task.
		extractCalls: true,
	}
	return javaRE.extract(source, relPath, "Java", opts)
}

var rubyRE = &regexExtractor{
	funcRE:  regexp.MustCompile(`(?m)^\s*def\s+(?P<name>[a-zA-Z_][a-zA-Z0-9_?!]*)`),
	classRE: regexp.MustCompile(`(?m)^class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s*<\s*(?P<parent>[A-Z][A-Za-z0-9_:]*))?`),
}

func extractRuby(source []byte, relPath string) *FileResult {
	opts := simpleOpts("::", 0)
	opts.endKeyword = true // Ruby closes def/class/module/do with `end` (#805)
	// #1159 v0.62: per-file CALLS pass — regexCallScan falls back to
	// "skip past first newline" when there is no `{`, so Ruby's
	// end-keyword block bodies work the same as C-family.
	opts.extractCalls = true
	return rubyRE.extract(source, relPath, "Ruby", opts)
}

// phpRE patterns lead with `^\s*` so indented class methods match —
// the regex extractor feeds these one line at a time, so `\s` cannot
// span newlines here. Without it, every method inside a PHP class was
// silently dropped (#813).
var phpRE = &regexExtractor{
	funcRE:  regexp.MustCompile(`(?m)^\s*(?:public|private|protected)?\s*(?:static\s+)?function\s+(?P<name>[a-zA-Z_][a-zA-Z0-9_]*)`),
	classRE: regexp.MustCompile(`(?m)^\s*(?:abstract\s+)?class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s+extends\s+(?P<parent>[A-Z][A-Za-z0-9_]*))?`),
}

func extractPHP(source []byte, relPath string) *FileResult {
	opts := simpleOpts("\\", '{')
	// #1159 v0.62: per-file CALLS pass — parallel to Rust/Java/TS/C.
	opts.extractCalls = true
	return phpRE.extract(source, relPath, "PHP", opts)
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
//
// #1204 v0.66 DOGFOOD: the leading `[ \t]*` was wrong. extractCBareMacros
// runs FindAllSubmatchIndex over the whole source, so any indented
// SCREAM_CASE(arg, ...) inside a function body got lifted to a Symbol.
// Unreal Engine corpora repeat `UE_LOG(LogTemp, ...)` 4-11 times per
// file, each landing as a `LogTemp` Symbol and triggering a
// qualified_name_collision per file. Restricting to column 0 keeps the
// intended bare-prefix-declaration case (EXPORT_SYMBOL, MODULE_PARM,
// DEFINE_LOG_CATEGORY) — those land at column 0 in real code — while
// dropping the indented call-site overextraction. rewriteCMacroSymbols
// is unaffected: funcRE itself is column-0-only, so the line it inspects
// is already column 0 and the regex still matches without the
// permissive whitespace prefix.
var cMacroRE = regexp.MustCompile(
	`(?m)^(?:static\s+)?(?P<macro>[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)\s*\(\s*(?P<arg>[A-Za-z_][A-Za-z0-9_]*)`)

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
// Each pass is independently testable; the order matters because:
//   - rewriteCMacro must run BEFORE the central disambiguator so
//     DEVICE_ATTR and friends get their proper per-instance names
//     rather than colliding on the macro name.
//   - extractCBareMacros must run AFTER dropForwardDecls so the bare
//     macro pass doesn't re-emit names just removed.
func extractC(source []byte, relPath string) *FileResult {
	// #858: opt into the per-file CALLS pass. C's `name(` call syntax is
	// exactly what regexCallScan reads; same-file calls resolve, cross-
	// file calls drop (the regex-tier per-file resolution policy).
	cOpts := simpleOpts("::", '{')
	cOpts.extractCalls = true
	result := cRE.extract(source, relPath, "C", cOpts)
	rewriteCMacroSymbols(result, source, relPath)
	result.Symbols = dropCForwardDecls(result.Symbols, source)
	// #1148: drop symbols whose name is a C reserved keyword. The
	// `(?:\w+\s+)+name\s*\(` regex shape occasionally captures inside
	// expressions like `size_t n = sizeof(struct foo)` — `sizeof` lifts
	// to a Function symbol per occurrence, three matches in one file
	// collide on the qualified name, and `pincher doctor` reports
	// `qualified_name_collision`. Same shape for `struct`, `typeof`,
	// `offsetof`. These are C reserved keywords that can NEVER be
	// function names in valid C, so dropping them at extraction is
	// safe and eliminates the dominant collision class on kernel/
	// embedded corpora.
	result.Symbols = dropCKeywordFalsePositives(result.Symbols)
	result.Symbols = append(result.Symbols, extractCBareMacros(source, relPath, result.Symbols)...)
	return result
}

// cReservedKeywords is the set of C99/C11 reserved keywords that the
// regex extractor can accidentally lift to a Function symbol. They
// are syntactically illegal as function names, so dropping them is
// safe regardless of context. Limited to keywords actually observed
// in #1148's kernel-corpus collision repro; intentionally NOT the
// full C keyword list — we don't want to silently drop a legal
// function named `int_func` because someone half-thinks `int` is a
// reserved word elsewhere. Each entry must be a name the C compiler
// itself rejects as an identifier.
var cReservedKeywords = map[string]struct{}{
	"sizeof":   {},
	"offsetof": {},
	"typeof":   {},
	"struct":   {},
	"union":    {},
	"enum":     {},
	"if":       {},
	"else":     {},
	"for":      {},
	"while":    {},
	"do":       {},
	"switch":   {},
	"case":     {},
	"default":  {},
	"break":    {},
	"continue": {},
	"return":   {},
	"goto":     {},
}

// dropCKeywordFalsePositives filters out Function symbols whose Name
// is a C reserved keyword (per cReservedKeywords). The regex can
// emit these when an expression like `n = sizeof(struct foo)` happens
// to satisfy the column-0 `<types> name(` shape after tokenization.
// Kind != Function symbols pass through (Class/Setting are extracted
// from struct/typedef contexts and have separate handling).
func dropCKeywordFalsePositives(syms []ExtractedSymbol) []ExtractedSymbol {
	if len(syms) == 0 {
		return syms
	}
	out := syms[:0]
	for _, s := range syms {
		if s.Kind == "Function" {
			if _, isKeyword := cReservedKeywords[s.Name]; isKeyword {
				continue
			}
		}
		out = append(out, s)
	}
	return out
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
		// cMacroRE has TWO named groups (macro + arg); resolve arg by
		// name. Pre-#811 this had to bypass extractGroup, which ignored
		// the name argument; namedGroup now honours it.
		arg := namedGroup(cMacroRE, m, "arg")
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
	// #1324 v0.71: also dedupe by Function symbol NAME. funcRE captures
	// `static int probe(struct platform_device *pdev) { ... }` at line N;
	// then `EXPORT_SYMBOL(probe);` further down emits the macro form via
	// this pass with the same `arg = "probe"`, producing QN
	// `<path>::probe` twice — the start-byte dedup above doesn't catch
	// it because the two symbols live at different offsets. Result was
	// a per-file `qualified_name_collision` diagnostic on every Linux-
	// driver-shaped C file (kernel idioms typically pair `static int
	// probe(...) { ... } EXPORT_SYMBOL(probe);`). Skipping the macro
	// emission when a Function with the same name was already extracted
	// keeps the export-style macros (where there's no in-file definition,
	// the macro IS the symbol of record) and drops the duplicate-of-real-
	// function case.
	takenNames := make(map[string]struct{}, len(existing))
	for _, s := range existing {
		if s.Kind == "Function" {
			takenNames[s.Name] = struct{}{}
		}
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
		if _, dupName := takenNames[arg]; dupName {
			// #1324: the real function definition is the symbol of
			// record; this macro is just an export annotation.
			continue
		}

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

var csRE = &regexExtractor{
	funcRE:      regexp.MustCompile(`(?m)^\s*(?:public|private|protected|internal)?\s*(?:static\s+)?(?:async\s+)?(?:\w+\s+)+(?P<name>[A-Za-z][A-Za-z0-9_]*)\s*\(`),
	classRE:     regexp.MustCompile(`(?m)^(?:\s*)(?:public|private|internal)?\s*(?:abstract|sealed)?\s*class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s*:\s*(?P<parent>[A-Z][A-Za-z0-9_, ]*))?`),
	interfaceRE: regexp.MustCompile(`(?m)^(?:\s*)(?:public)?\s*interface\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
}

func extractCSharp(source []byte, relPath string) *FileResult {
	opts := simpleOpts(".", '{')
	// #1159 v0.62: per-file CALLS pass — parallel to Rust/Java/TS/C.
	opts.extractCalls = true
	return csRE.extract(source, relPath, "C#", opts)
}

var kotlinRE = &regexExtractor{
	// #1183 v0.67 (Kotlin parallel to Rust impl): `fun Type.method()`
	// is Kotlin's extension-function syntax. Pre-fix the regex captured
	// `Type` as the function name (since `Type` is the first identifier
	// after `fun`), emitting fake "String" / "List" / "Map" / etc
	// symbols and silently dropping the real method name. The optional
	// `(?:[A-Z][A-Za-z0-9_]*(?:<[^>]*>)?\.)?` segment skips the
	// receiver-type prefix (lowercase first char excluded to avoid
	// false-eating valid lowercase method names that happen to be
	// preceded by `fun `). Generics on the receiver tolerated.
	funcRE: regexp.MustCompile(`(?m)^\s*(?:public|private|internal|protected)?\s*(?:suspend\s+)?fun\s+(?:[A-Z][A-Za-z0-9_]*(?:<[^>]*>)?\.)?(?P<name>[a-zA-Z][a-zA-Z0-9_]*)`),
	classRE: regexp.MustCompile(`(?m)^(?:open\s+)?(?:data\s+)?class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\(|:|\s)`),
}

func extractKotlin(source []byte, relPath string) *FileResult {
	opts := simpleOpts(".", '{')
	// #1159 v0.62: per-file CALLS pass — parallel to Rust/Java/TS/C.
	opts.extractCalls = true
	return kotlinRE.extract(source, relPath, "Kotlin", opts)
}

var swiftRE = &regexExtractor{
	funcRE:      regexp.MustCompile(`(?m)^\s*(?:public|private|internal|open)?\s*(?:static\s+)?func\s+(?P<name>[a-zA-Z][a-zA-Z0-9_]*)`),
	classRE:     regexp.MustCompile(`(?m)^(?:public\s+)?(?:final\s+)?class\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s*:\s*(?P<parent>[A-Z][A-Za-z0-9_, ]*))?`),
	interfaceRE: regexp.MustCompile(`(?m)^(?:public\s+)?protocol\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
	// #1183 v0.67 (Swift parallel to Rust impl): `extension Type {}` and
	// `extension Type: Protocol {}` add methods to an existing type.
	// Pre-fix those methods emitted as Function with no Parent —
	// dead_code / trace couldn't bind them to the extended type's
	// method set. Generic parameters tolerated (`extension Array<T>`).
	scopeRE: regexp.MustCompile(`(?m)^(?:public\s+)?extension\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:<[^>]*>)?(?:\s*:\s*[A-Z][A-Za-z0-9_, ]*)?\s*\{?`),
}

func extractSwift(source []byte, relPath string) *FileResult {
	opts := simpleOpts(".", '{')
	// #1159 v0.62: per-file CALLS pass — parallel to Rust/Java/TS/C.
	opts.extractCalls = true
	return swiftRE.extract(source, relPath, "Swift", opts)
}

// #1161 v0.63 — Lua / Elixir / Zig regex-tier extractors promoted
// from stub. Patterns are deliberately minimal: function-definition
// keyword + name capture. Block-end heuristics fall through to the
// regex pipeline's blockEnd() — Lua uses `end`-keyword closing
// (similar to Ruby), Zig uses `{`, Elixir uses `end`.

var luaRE = &regexExtractor{
	// Lua: `function name(...)`, `local function name(...)`,
	// `function obj:method(...)`, `function ns.name(...)`.
	// Capture the trailing identifier (after any module / receiver
	// prefix) so the symbol surfaces with its callable name.
	funcRE: regexp.MustCompile(`(?m)^\s*(?:local\s+)?function\s+(?:[A-Za-z_][A-Za-z0-9_.]*[.:])?(?P<name>[A-Za-z_][A-Za-z0-9_]*)\s*\(`),
}

func extractLua(source []byte, relPath string) *FileResult {
	opts := simpleOpts(".", 0)
	opts.endKeyword = true // Lua closes function/if/for/while with `end`.
	// CALLS pass shares regexCallScan's newline-fallback (#1159 v0.62
	// Ruby work) so Lua's end-keyword bodies parse the same.
	opts.extractCalls = true
	return luaRE.extract(source, relPath, "Lua", opts)
}

var zigRE = &regexExtractor{
	// Zig: `fn name(...)`, `pub fn name(...)`, `export fn name(...)`.
	funcRE:  regexp.MustCompile(`(?m)^\s*(?:pub\s+|export\s+|extern\s+)?fn\s+(?P<name>[A-Za-z_][A-Za-z0-9_]*)\s*\(`),
	classRE: regexp.MustCompile(`(?m)^\s*(?:pub\s+)?const\s+(?P<name>[A-Z][A-Za-z0-9_]*)\s*=\s*struct`),
}

func extractZig(source []byte, relPath string) *FileResult {
	opts := simpleOpts(".", '{')
	opts.extractCalls = true
	return zigRE.extract(source, relPath, "Zig", opts)
}

// #1161 v0.63 round 2 — Scala / Dart / R regex-tier extractors.

var scalaRE = &regexExtractor{
	// Scala: `def name(...)`, `def name[T](...)`. Modifiers may include
	// `private`/`protected`/`override`/`final`/`abstract`/`implicit`. Methods
	// inside class/object/trait scope land via the existing pipeline's
	// classRE-tracked currentClass when funcRE matches.
	funcRE:      regexp.MustCompile(`(?m)^\s*(?:private(?:\[\w+\])?\s+|protected\s+|override\s+|final\s+|abstract\s+|implicit\s+|sealed\s+)*def\s+(?P<name>[A-Za-z_][A-Za-z0-9_]*)`),
	classRE:     regexp.MustCompile(`(?m)^\s*(?:sealed\s+)?(?:abstract\s+)?(?:final\s+)?(?:case\s+)?(?:class|object|trait)\s+(?P<name>[A-Z][A-Za-z0-9_]*)(?:\s+extends\s+(?P<parent>[A-Z][A-Za-z0-9_]*))?`),
	interfaceRE: regexp.MustCompile(`(?m)^\s*trait\s+(?P<name>[A-Z][A-Za-z0-9_]*)`),
}

func extractScala(source []byte, relPath string) *FileResult {
	opts := simpleOpts(".", '{')
	opts.extractCalls = true
	return scalaRE.extract(source, relPath, "Scala", opts)
}

var dartRE = &regexExtractor{
	// Dart's function shape is C-family: optional modifiers + return-type
	// tokens + name + `(`. Differentiation from Java is permissive — Dart
	// allows top-level functions without classes, so we treat the regex
	// the same way as TS's methodRE (modifier-permissive at line start)
	// but require either a type token before the name OR a static
	// modifier. The trailing `\(` anchor reduces false-positives.
	funcRE:      regexp.MustCompile(`(?m)^\s*(?:static\s+|external\s+|abstract\s+)?(?:async\s+)?(?:[A-Za-z_$][A-Za-z0-9_$<>?,\s]*?\s+)?(?P<name>[A-Za-z_$][A-Za-z0-9_$]*)\s*\(`),
	classRE:     regexp.MustCompile(`(?m)^\s*(?:abstract\s+)?class\s+(?P<name>[A-Z][A-Za-z0-9_$]*)(?:\s+extends\s+(?P<parent>[A-Z][A-Za-z0-9_$]*))?`),
	interfaceRE: regexp.MustCompile(`(?m)^\s*(?:abstract\s+)?(?:mixin|interface)\s+(?P<name>[A-Z][A-Za-z0-9_$]*)`),
	enumRE:      regexp.MustCompile(`(?m)^\s*enum\s+(?P<name>[A-Z][A-Za-z0-9_$]*)`),
}

func extractDart(source []byte, relPath string) *FileResult {
	opts := simpleOpts(".", '{')
	opts.extractCalls = true
	result := dartRE.extract(source, relPath, "Dart", opts)
	// Dart's funcRE permissive shape catches `if (cond) {` and similar
	// control-flow false positives the same way TS's methodRE did
	// (#1158). Reuse the TS keyword filter — the lists are equivalent
	// (Dart inherits JS/TS control-flow vocabulary).
	result.Symbols = dropTSKeywordFalsePositives(result.Symbols)
	return result
}

var rRE = &regexExtractor{
	// R: `name <- function(...)`, `name = function(...)`. R lacks a
	// dedicated keyword; assignment-style function definitions are
	// the convention.
	funcRE: regexp.MustCompile(`(?m)^\s*(?P<name>[A-Za-z_.][A-Za-z0-9_.]*)\s*(?:<-|=)\s*function\s*\(`),
}

func extractR(source []byte, relPath string) *FileResult {
	opts := simpleOpts(".", '{')
	opts.extractCalls = true
	return rRE.extract(source, relPath, "R", opts)
}

var elixirRE = &regexExtractor{
	// Elixir: `def name`, `defp name`, `defmacro name`. Optional
	// argument list with parens is captured implicitly by the
	// blockEnd() pass.
	funcRE:  regexp.MustCompile(`(?m)^\s*(?:def|defp|defmacro)\s+(?P<name>[A-Za-z_][A-Za-z0-9_?!]*)`),
	classRE: regexp.MustCompile(`(?m)^\s*defmodule\s+(?P<name>[A-Z][A-Za-z0-9_.]*)\s*do`),
}

func extractElixir(source []byte, relPath string) *FileResult {
	opts := simpleOpts(".", 0)
	opts.endKeyword = true // Elixir closes def / defmodule with `end`.
	opts.extractCalls = true
	return elixirRE.extract(source, relPath, "Elixir", opts)
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
// blockEnd dispatches to the right block-end finder for the language:
// the `end`-keyword scanner for Ruby-style languages, the brace matcher
// / indentation fallback otherwise.
func blockEnd(source []byte, startOffset int, opts extractOpts) int {
	if opts.endKeyword {
		return findEndKeywordBlock(source, startOffset)
	}
	return findBlockEnd(source, startOffset, opts.blockChar)
}

// findEndKeywordBlock finds the byte offset just past the `end` keyword
// that closes a Ruby-style block opened at startOffset (the byte offset
// of the start of the def/class/module line). Ruby closes blocks with
// `end`, not a brace — the brace matcher and the 80-line indentation
// fallback both mis-span these, so every Ruby symbol got an 80-line
// span clamped to EOF (#805).
//
// Heuristic: the closing `end` is the first later line whose
// indentation (in leading-whitespace characters) matches the opener's
// and whose first token is `end`. Conventional Ruby indentation makes
// this reliable; a malformed or un-indented file falls back to EOF.
func findEndKeywordBlock(source []byte, startOffset int) int {
	if startOffset >= len(source) {
		return len(source)
	}
	openIndent := 0
	for i := startOffset; i < len(source) && (source[i] == ' ' || source[i] == '\t'); i++ {
		openIndent++
	}
	// Advance past the opening line.
	i := startOffset
	for i < len(source) && source[i] != '\n' {
		i++
	}
	for i < len(source) {
		i++ // step over '\n' to the next line start
		ls := i
		indent := 0
		for ls < len(source) && (source[ls] == ' ' || source[ls] == '\t') {
			ls++
			indent++
		}
		if indent == openIndent && ls+3 <= len(source) && string(source[ls:ls+3]) == "end" {
			after := ls + 3
			// `end` must be a whole token, not a prefix of `endpoint` etc.
			if after >= len(source) || source[after] == '\n' || source[after] == '\r' ||
				source[after] == ' ' || source[after] == '\t' || source[after] == '#' {
				e := after
				for e < len(source) && source[e] != '\n' {
					e++
				}
				return e
			}
		}
		for i < len(source) && source[i] != '\n' {
			i++
		}
	}
	return len(source)
}

// findIndentBlock finds the byte offset just past the last line of an
// indentation-delimited block (Python def/class) opened at startOffset
// — the byte offset of the start of the opening line. The block runs
// to the line before the first later non-blank line whose
// leading-whitespace indentation is <= the opener's. Blank lines and
// comment-only lines do not terminate the block (they routinely appear
// inside a function body). A block with no dedent line runs to EOF —
// correct for the last def in a file (#807).
func findIndentBlock(source []byte, startOffset int) int {
	if startOffset >= len(source) {
		return len(source)
	}
	openIndent := 0
	for i := startOffset; i < len(source) && (source[i] == ' ' || source[i] == '\t'); i++ {
		openIndent++
	}
	// Advance past the opening line; lastEnd tracks the end of the last
	// line confirmed to belong to the block.
	i := startOffset
	for i < len(source) && source[i] != '\n' {
		i++
	}
	lastEnd := i
	for i < len(source) {
		i++ // step over '\n' to the next line start
		ls := i
		indent := 0
		for ls < len(source) && (source[ls] == ' ' || source[ls] == '\t') {
			ls++
			indent++
		}
		lineEnd := ls
		for lineEnd < len(source) && source[lineEnd] != '\n' {
			lineEnd++
		}
		blank := ls >= lineEnd // nothing but whitespace on the line
		if !blank && indent <= openIndent {
			// Dedent: this line belongs to an enclosing scope.
			return lastEnd
		}
		if !blank {
			lastEnd = lineEnd // a deeper-indented line is part of the body
		}
		i = lineEnd
	}
	return len(source)
}

func findBlockEnd(source []byte, startOffset int, blockChar byte) int {
	if startOffset >= len(source) {
		return len(source)
	}
	if blockChar == 0 {
		// Indentation-delimited (Python): the block runs until the first
		// later non-blank line whose indentation is <= the opener's.
		// #807: this used to be "just return 80 lines worth of bytes",
		// so every Python def/class got an 80-line span clamped to EOF
		// and symbol/context returned wildly wrong source.
		return findIndentBlock(source, startOffset)
	}
	// Brace-delimited: find the matching close character.
	var closeChar byte
	switch blockChar {
	case '{':
		// Brace-bodied declarations (function/class) get a bounded
		// opener search — see findBraceBlock (#809).
		return findBraceBlock(source, startOffset)
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

// findBraceBlock spans a brace-bodied declaration (function, class,
// interface, enum) opened at startOffset to the byte just past its
// matching `}`.
//
// The opener search is bounded. The old code scanned forward for the
// first `{` anywhere after startOffset with no upper bound — so an
// expression-bodied or body-less declaration (Kotlin/Swift
// `fun double(x) = x * 2`, an abstract method) latched onto the *next*
// sibling's brace and swallowed its whole block (#809). A declaration's
// `{` can only legitimately appear on the declaration line or on a
// continuation line still inside the parameter parentheses, so: while
// the body has not opened, track paren depth and treat a newline at
// paren depth 0 as end-of-declaration — there is no braced body, return
// end of that line. `{`/`}` inside the signature parens (default-value
// lambdas) are ignored; only a `{` at paren depth 0 opens the body.
//
// #816: a newline at paren depth 0 is NOT always end-of-declaration —
// a wrapped return type (`-> Type` on its own line) or a Rust `where`
// clause continues the signature, and the braced body is reached only
// past it. declContinuationAt peeks the next non-blank line before the
// newline is treated as a terminator; a `where` clause whose body spans
// several lines holds the scan open until the `{` (or a `;`).
//
// declContinuationAt peeks past the newline at nlIdx to decide whether
// the declaration continues onto the next non-blank line.
func declContinuationAt(source []byte, nlIdx int) (cont, isWhere bool) {
	j := nlIdx + 1
	for j < len(source) {
		// Skip leading whitespace on this line.
		for j < len(source) && (source[j] == ' ' || source[j] == '\t' || source[j] == '\r') {
			j++
		}
		if j >= len(source) {
			return false, false
		}
		if source[j] == '\n' { // blank line — look at the line after
			j++
			continue
		}
		c := source[j]
		if c == '{' {
			return true, false
		}
		if c == '-' && j+1 < len(source) && source[j+1] == '>' {
			return true, false // wrapped return type on its own line
		}
		// `where` as a leading word (followed by a non-identifier char).
		if c == 'w' && j+5 <= len(source) && string(source[j:j+5]) == "where" {
			if j+5 == len(source) || !isIdentByte(source[j+5]) {
				return true, true
			}
		}
		return false, false
	}
	return false, false
}

// isIdentByte reports whether b can appear inside an identifier — used
// to confirm a keyword match is whole-word, not a prefix.
func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

func findBraceBlock(source []byte, startOffset int) int {
	depth := 0
	parenDepth := 0
	started := false
	inWhereClause := false
	for i := startOffset; i < len(source); i++ {
		c := source[i]
		if !started {
			switch c {
			case '(':
				parenDepth++
			case ')':
				if parenDepth > 0 {
					parenDepth--
				}
			case '{':
				if parenDepth == 0 {
					depth = 1
					started = true
				}
			case ';':
				if parenDepth == 0 {
					return i + 1 // bodyless declaration (trait method, abstract decl)
				}
			case '\n':
				if parenDepth == 0 {
					if inWhereClause {
						continue // where-clause body line; keep scanning for `{` or `;`
					}
					contDecl, isWhere := declContinuationAt(source, i)
					if !contDecl {
						return i // declaration ended with no braced body
					}
					if isWhere {
						inWhereClause = true
					}
				}
			}
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
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
// namedGroup returns the first non-empty capture group named `name`, or
// "" if no such group participated in the match.
//
// It replaces the old extractGroup, which ignored its name argument and
// returned the first non-empty *positional* subgroup — so for a regex
// like classRE (groups: name, parent), asking for "parent" handed back
// the "name" group whenever the superclass clause was absent, leaving
// every superclass-less class with parent == its own name (#811).
//
// "First non-empty with this name" (rather than SubexpIndex's strict
// "first index with this name") is deliberate: funcRE patterns are an
// alternation of branches that each carry a `name` group, and only the
// matched branch's group is populated. Resolving by name keeps that
// working while still distinguishing `name` from `parent`.
func namedGroup(re *regexp.Regexp, m []string, name string) string {
	for i, gname := range re.SubexpNames() {
		if gname == name && i < len(m) && m[i] != "" {
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
