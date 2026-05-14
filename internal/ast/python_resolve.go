package ast

import (
	"path/filepath"
	"strings"
)

// PythonImportCandidates expands a Python import `to_name` into the list of
// dotted qualified names the resolver should try against the symbol table.
// Used by the indexer's resolveImports to bridge Python's module-path import
// format and pincher's file-path-derived QN format.
//
// Inputs:
//
//	toName       — the to_name field from the IMPORTS pending edge, as
//	               emitted by python_extract.py. Absolute imports are dotted
//	               ("zelosmcp.config.ServerSpec"); relative imports keep
//	               their leading dots (".sib.helper", "..parent.baz").
//	fromFile     — the importing file's project-relative path
//	               ("src/zelosmcp/foo/bar.py"). Required for relative-import
//	               resolution; ignored for absolute imports.
//	sourceRoots  — the per-project source-root prefixes from
//	               PythonSourceRoots (e.g. ["src", ""] for a src-layout repo).
//
// Output: ordered candidate list. The caller tries each against the symbol
// table and accepts the first hit. Order is deterministic: identity first,
// then each non-empty source-root prefix appended.
//
// Relative imports return a single candidate (or none, if the dot count
// escapes the project root). Source roots don't apply to relative imports
// — they're already resolved against the importing file's path.
func PythonImportCandidates(toName, fromFile string, sourceRoots []string) []string {
	if toName == "" {
		return nil
	}

	if strings.HasPrefix(toName, ".") {
		if c, ok := resolveRelativeImport(toName, fromFile); ok {
			return []string{c}
		}
		return nil
	}

	out := []string{toName}
	for _, root := range sourceRoots {
		if root == "" {
			continue // identity already in out[0]
		}
		dotted := strings.ReplaceAll(root, "/", ".")
		out = append(out, dotted+"."+toName)
	}
	return out
}

// resolveRelativeImport turns a leading-dot import like "..parent.baz"
// into an absolute QN by counting dots, climbing that many directory
// levels from fromFile, and appending the rest.
//
// Returns ok=false when the dot count escapes the project root — the
// pending edge will stay unresolved, matching CPython's ImportError.
func resolveRelativeImport(toName, fromFile string) (string, bool) {
	level := 0
	for level < len(toName) && toName[level] == '.' {
		level++
	}
	rest := toName[level:]

	// CPython: `from . import x` means import x from current package, i.e.
	// the directory containing fromFile. Each extra dot pops one more
	// directory level.
	dir := filepath.ToSlash(filepath.Dir(fromFile))
	if dir == "." {
		dir = ""
	}
	parts := []string{}
	if dir != "" {
		parts = strings.Split(dir, "/")
	}

	// level=1 keeps the file's directory; each extra dot pops one segment.
	pops := level - 1
	if pops > len(parts) {
		return "", false
	}
	parts = parts[:len(parts)-pops]

	dotted := strings.Join(parts, ".")
	switch {
	case dotted == "" && rest == "":
		return "", false
	case dotted == "":
		return rest, true
	case rest == "":
		return dotted, true
	default:
		return dotted + "." + rest, true
	}
}
