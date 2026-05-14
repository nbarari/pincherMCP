package ast

import (
	"reflect"
	"testing"
)

func TestPythonImportCandidates_AbsoluteWithSrcLayout(t *testing.T) {
	got := PythonImportCandidates(
		"zelosmcp.config.ServerSpec",
		"src/zelosmcp/foo/bar.py",
		[]string{"src", ""},
	)
	want := []string{
		"zelosmcp.config.ServerSpec",
		"src.zelosmcp.config.ServerSpec",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPythonImportCandidates_AbsoluteFlatLayout(t *testing.T) {
	// Flat layout: only the empty root → just the identity.
	got := PythonImportCandidates("os", "myproj/main.py", []string{""})
	want := []string{"os"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPythonImportCandidates_AbsoluteMultiRoot(t *testing.T) {
	// Monorepo with packages in both src/ and lib/.
	got := PythonImportCandidates(
		"foo.bar",
		"src/foo/x.py",
		[]string{"src", "lib", ""},
	)
	want := []string{
		"foo.bar",
		"src.foo.bar",
		"lib.foo.bar",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPythonImportCandidates_RelativeSingleDot(t *testing.T) {
	// `from . import x` from src/pkg/sub/mod.py → src.pkg.sub.x
	got := PythonImportCandidates(".x", "src/pkg/sub/mod.py", []string{"src", ""})
	want := []string{"src.pkg.sub.x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPythonImportCandidates_RelativeDoubleDot(t *testing.T) {
	// `from ..parent import baz` from src/pkg/sub/mod.py → src.pkg.parent.baz
	got := PythonImportCandidates("..parent.baz", "src/pkg/sub/mod.py", nil)
	want := []string{"src.pkg.parent.baz"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPythonImportCandidates_RelativeBareDot(t *testing.T) {
	// `from . import x` with no module — to_name is ".x".
	got := PythonImportCandidates(".x", "pkg/m.py", nil)
	want := []string{"pkg.x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPythonImportCandidates_RelativeEscapesRoot(t *testing.T) {
	// Three dots from a one-level-deep file pops too far; return no candidates
	// rather than an invalid prefix.
	got := PythonImportCandidates("...x", "pkg/m.py", nil)
	if got != nil {
		t.Errorf("expected nil for over-deep relative import, got %v", got)
	}
}

func TestPythonImportCandidates_Empty(t *testing.T) {
	if got := PythonImportCandidates("", "x.py", nil); got != nil {
		t.Errorf("expected nil for empty toName, got %v", got)
	}
}
