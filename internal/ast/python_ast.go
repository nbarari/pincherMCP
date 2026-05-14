package ast

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Python AST extractor. Shells out to a CPython 3 interpreter running an
// embedded helper script that uses the stdlib `ast` module to parse the
// file and emit a JSON summary. The Go side unmarshals that into the
// standard FileResult shape.
//
// Default-on when a working CPython 3 is on PATH (mirrors the JS-AST
// convention since v0.20.0). Falls back to the regex extractor on parse
// failure, no working interpreter, or `PINCHER_DISABLE_PY_AST=1`.

//go:embed python_extract.py
var pythonExtractScript string

// pyASTTimeout caps the wall-clock spent on a single file's Python AST run.
// CPython's ast.parse is fast; this only guards against pathological inputs
// or a hung subprocess. The regex fallback runs on timeout.
const pyASTTimeout = 10 * time.Second

// pyProbeTimeout bounds the one-time interpreter-probe subprocess.
const pyProbeTimeout = 5 * time.Second

// pyASTEnabled reads the opt-out env var on every call so tests can flip
// the flag with t.Setenv without re-registering the extractor.
//
// Resolution order:
//  1. PINCHER_DISABLE_PY_AST=1 → false (explicit opt-out wins)
//  2. no working CPython 3     → false (transparent fallback to regex)
//  3. otherwise                → true  (default-on)
func pyASTEnabled() bool {
	if os.Getenv("PINCHER_DISABLE_PY_AST") == "1" {
		return false
	}
	return pythonCommand() != nil
}

// PythonAvailable reports whether a working CPython 3 interpreter was
// found. Exported so tests in other packages (internal/index) can guard
// Python-AST-dependent cases with the same probe the extractor uses,
// rather than a bare exec.LookPath that a non-functional shim passes.
func PythonAvailable() bool {
	return pythonCommand() != nil
}

var (
	pyLookupOnce sync.Once
	pyCmd        []string // [binaryPath, extraArgs...] to invoke CPython 3; nil if none works
)

// pythonCommand resolves a working CPython 3 invocation, cached for the
// process lifetime. It probes candidates in order — `python3` (the POSIX
// convention), then `python`, then the Windows `py -3` launcher — and
// VERIFIES each actually executes CPython, not just that the name
// resolves on PATH.
//
// The verification is load-bearing on Windows: `python3.exe` there is
// frequently a Microsoft Store stub that resolves via exec.LookPath but
// prints an install prompt and exits non-zero instead of running Python.
// A bare LookPath check mis-detects that stub as "Python available", and
// every file then silently falls back to the regex extractor — and the
// test skip guards mis-fire the same way, running (and failing) instead
// of skipping. Probing with a trivial `-c` script and checking the
// output is the only honest signal.
func pythonCommand() []string {
	pyLookupOnce.Do(func() {
		candidates := [][]string{
			{"python3"},
			{"python"},
			{"py", "-3"},
		}
		for _, c := range candidates {
			path, err := exec.LookPath(c[0])
			if err != nil {
				continue
			}
			args := append(append([]string{}, c[1:]...), "-c", "import sys; sys.stdout.write('pinchok')")
			ctx, cancel := context.WithTimeout(context.Background(), pyProbeTimeout)
			out, runErr := exec.CommandContext(ctx, path, args...).Output()
			cancel()
			if runErr == nil && string(out) == "pinchok" {
				pyCmd = append([]string{path}, c[1:]...)
				return
			}
		}
	})
	return pyCmd
}

// pythonSymbolJSON / pythonEdgeJSON / pythonResponse mirror the JSON shape
// emitted by python_extract.py. Keep them in sync with that script.
type pythonSymbolJSON struct {
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"`
	Parent        string `json:"parent"`
	Signature     string `json:"signature"`
	Docstring     string `json:"docstring"`
	IsExported    bool   `json:"is_exported"`
	IsTest        bool   `json:"is_test"`
	StartByte     int    `json:"start_byte"`
	EndByte       int    `json:"end_byte"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
}

type pythonEdgeJSON struct {
	FromQN     string  `json:"from_qn"`
	ToName     string  `json:"to_name"`
	Kind       string  `json:"kind"`
	Confidence float64 `json:"confidence"`
}

type pythonResponse struct {
	Symbols []pythonSymbolJSON `json:"symbols"`
	Edges   []pythonEdgeJSON   `json:"edges"`
	Module  string             `json:"module"`
	// Error is set by the script when ast.parse raises SyntaxError. Non-empty
	// means Go falls back to the regex extractor.
	Error string `json:"error,omitempty"`
}

func (r *pythonResponse) toFileResult() *FileResult {
	syms := make([]ExtractedSymbol, len(r.Symbols))
	for i, s := range r.Symbols {
		syms[i] = ExtractedSymbol{
			Name:          s.Name,
			QualifiedName: s.QualifiedName,
			Kind:          s.Kind,
			StartByte:     s.StartByte,
			EndByte:       s.EndByte,
			StartLine:     s.StartLine,
			EndLine:       s.EndLine,
			Signature:     s.Signature,
			Docstring:     s.Docstring,
			Parent:        s.Parent,
			IsExported:    s.IsExported,
			IsTest:        s.IsTest,
		}
	}
	edges := make([]ExtractedEdge, len(r.Edges))
	for i, e := range r.Edges {
		edges[i] = ExtractedEdge{
			FromQN:     e.FromQN,
			ToName:     e.ToName,
			Kind:       e.Kind,
			Confidence: e.Confidence,
		}
	}
	return &FileResult{Symbols: syms, Edges: edges, Module: r.Module}
}

// extractPythonAST shells out to python3 with the embedded helper script.
// Returns (result, true) on success, (nil, false) on any failure — timeout,
// non-zero exit, JSON parse error, or Python SyntaxError in the source.
// The dispatch fn falls back to extractPython on false.
func extractPythonAST(src []byte, relPath string) (*FileResult, bool) {
	pyCmd := pythonCommand()
	if pyCmd == nil {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), pyASTTimeout)
	defer cancel()

	args := append(append([]string{}, pyCmd[1:]...), "-c", pythonExtractScript, relPath)
	cmd := exec.CommandContext(ctx, pyCmd[0], args...)
	cmd.Stdin = bytes.NewReader(src)
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}

	var resp pythonResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, false
	}
	if resp.Error != "" {
		return nil, false
	}
	return resp.toFileResult(), true
}
