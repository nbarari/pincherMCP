package ast

import (
	"os"
	"strings"
	"testing"
)

// #1626 v0.87: persistent Python subprocess. The daemon-mode opt-in
// is the bulk of the perf win; these tests pin the contract that
// daemon-mode extraction produces the same result shape as the
// one-shot path. Skip when no CPython 3 is available (mirrors the
// existing python_ast_test.go pattern).

func TestPythonRunner_Daemon_ExtractsSimpleModule_1626(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("no working CPython 3 on PATH")
	}
	src := []byte(`"""tiny module."""

def hello(name):
    return "hi " + name

class Greeter:
    def greet(self, who):
        return hello(who)
`)
	resp, ok := defaultPythonRunner.extract("tiny.py", src)
	if !ok {
		t.Fatal("daemon extract returned ok=false on a known-good Python source")
	}
	if len(resp.Symbols) == 0 {
		t.Fatal("daemon returned zero symbols on a non-empty Python module")
	}
	// Expect at least Module + hello function + Greeter class.
	gotKinds := make(map[string]bool)
	for _, s := range resp.Symbols {
		gotKinds[s.Kind] = true
	}
	for _, want := range []string{"Module", "Function", "Class"} {
		if !gotKinds[want] {
			t.Errorf("daemon output missing %q kind; got kinds: %v", want, gotKinds)
		}
	}
}

func TestPythonRunner_Daemon_HandlesMultipleRequests_1626(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("no working CPython 3 on PATH")
	}
	// Send three requests back-to-back through the same runner —
	// the perf win comes from amortising spawn across many calls,
	// so it's critical that the second + third requests don't get
	// dropped or framing-corrupted by the first.
	sources := []struct {
		path string
		src  string
		want string
	}{
		{"a.py", "def alpha(): pass\n", "alpha"},
		{"b.py", "def beta(): pass\n", "beta"},
		{"c.py", "def gamma(): pass\n", "gamma"},
	}
	for _, c := range sources {
		resp, ok := defaultPythonRunner.extract(c.path, []byte(c.src))
		if !ok {
			t.Fatalf("daemon extract %q returned ok=false", c.path)
		}
		found := false
		for _, s := range resp.Symbols {
			if s.Name == c.want {
				found = true
				break
			}
		}
		if !found {
			names := make([]string, 0, len(resp.Symbols))
			for _, s := range resp.Symbols {
				names = append(names, s.Name)
			}
			t.Errorf("daemon extract %q: missing symbol %q; got names: %s",
				c.path, c.want, strings.Join(names, ", "))
		}
	}
}

func TestPythonRunner_Daemon_SyntaxError_ReturnsNotOK_1626(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("no working CPython 3 on PATH")
	}
	// Bad Python source — daemon should respond with an error JSON,
	// extract should return ok=false, runner should stay alive for
	// subsequent good requests.
	bad := []byte("def broken(:\n    pass\n")
	if _, ok := defaultPythonRunner.extract("broken.py", bad); ok {
		t.Error("expected ok=false on syntax error; got ok=true")
	}
	// Recovery: a good request after a parser-error must still work.
	good := []byte("def good(): pass\n")
	if _, ok := defaultPythonRunner.extract("good.py", good); !ok {
		t.Errorf("daemon should survive a syntax error and process the next request; got ok=false")
	}
}

// Tiny smoke for the env-var opt-in surface: extractPythonAST routes
// through the daemon when PINCHER_PYTHON_AST_DAEMON=1, and the
// resulting FileResult has the same shape as the one-shot path.
func TestExtractPythonAST_DaemonRouting_1626(t *testing.T) {
	if !PythonAvailable() {
		t.Skip("no working CPython 3 on PATH")
	}
	t.Setenv("PINCHER_PYTHON_AST_DAEMON", "1")
	src := []byte("def thing(): return 42\n")
	result, ok := extractPythonAST(src, "thing.py")
	if !ok {
		t.Fatal("extractPythonAST returned ok=false with daemon enabled")
	}
	if len(result.Symbols) < 2 { // Module + Function
		t.Errorf("daemon path returned %d symbols; expected at least 2", len(result.Symbols))
	}
	// Sanity check the confidence override that toFileResult applies.
	if result.ConfidenceOverride != 1.0 {
		t.Errorf("ConfidenceOverride = %v, want 1.0", result.ConfidenceOverride)
	}
	_ = os.Getenv // touch import in case the file ever loses other usages
}
