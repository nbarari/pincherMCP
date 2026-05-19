package ast

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// #1328 v0.71: JavaScript AST extractor went default-on in v0.20.0
// (#266), but the langAdapter still registered confidence=0.85 (the
// regex fallback's honest floor). Pre-fix, every AST-extracted JS
// symbol stamped 0.85 → leaving no way to distinguish AST output from
// regex output in min_confidence filters or `pincher health`'s parser
// label. Fix: extractJavaScriptAST returns ConfidenceOverride=1.0,
// mirroring Python's #944 pattern.

func TestJavaScriptAST_ConfidenceOverride_BoostsSymbols_1328(t *testing.T) {
	// Force the AST path (default, but be explicit for the test).
	t.Setenv("PINCHER_DISABLE_JS_AST", "")
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "")

	src := []byte(`
class Foo {
  bar() { return 1; }
}
function baz() { return 2; }
const Q = (x) => x + 1;
`)
	result := ExtractWithModule(src, "JavaScript", "src/mod.js", "")
	if len(result.Symbols) == 0 {
		t.Fatal("expected JS symbols; got none")
	}
	for _, s := range result.Symbols {
		// AST path stamps 1.0 baseline → Compose may add signal bumps,
		// but it should NOT come back at the regex-tier 0.975 ceiling.
		// Threshold 0.99 matches the Python AST confidence test.
		if s.ExtractionConfidence < 0.99 {
			t.Errorf("symbol %q: confidence = %v, want >=0.99 (AST-extracted)",
				s.Name, s.ExtractionConfidence)
		}
	}
}

// Negative control: with PINCHER_DISABLE_JS_AST=1, the dispatcher
// routes to the regex extractor and symbols stamp the regex-tier
// confidence (≤0.99). Pre-fix THIS was the universal behaviour even
// without the opt-out — now it's only the opt-out path.
func TestJavaScriptAST_DisableOptOut_KeepsRegexConfidence_1328(t *testing.T) {
	t.Setenv("PINCHER_DISABLE_JS_AST", "1")

	src := []byte("function foo() { return 1; }\n")
	result := ExtractWithModule(src, "JavaScript", "src/mod.js", "")
	if len(result.Symbols) == 0 {
		t.Skip("JS regex extractor returned no symbols on the fixture")
	}
	for _, s := range result.Symbols {
		if s.ExtractionConfidence > 0.99 {
			t.Errorf("opt-out path symbol %q: confidence = %v, expected ≤0.99 (regex tier)",
				s.Name, s.ExtractionConfidence)
		}
	}
}

// #1477 v0.84: parseJSWithRecovery logs at slog.Debug when the parser
// rejects source. Pre-fix the fallback to regex was silent — users
// debugging "why are my JS symbols at regex confidence" had no
// signal to trace. The Debug-level log makes the failure correlatable
// when PINCHER_LOG_LEVEL=debug; production stays quiet by default.
func TestParseJSWithRecovery_LogsParseFailureAtDebug(t *testing.T) {
	// Capture slog output at debug level.
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(originalLogger) })

	// Source that the JS parser will reject (incomplete statement).
	bad := []byte("function broken( {")
	_, ok := parseJSWithRecovery(bad)
	if ok {
		t.Fatal("parseJSWithRecovery returned ok=true on malformed input")
	}
	got := buf.String()
	if !strings.Contains(got, "pincher.ast.js.parse_failed") {
		t.Errorf("expected slog.Debug entry pincher.ast.js.parse_failed; got: %q", got)
	}
}

// JavaScriptASTEnabled mirrors PythonAvailable so /internal/server can
// upgrade the parser label at runtime. With both opt-out envs cleared,
// the default is true (post-v0.20.0).
func TestJavaScriptASTEnabled_DefaultsOn_1328(t *testing.T) {
	t.Setenv("PINCHER_DISABLE_JS_AST", "")
	t.Setenv("PINCHER_EXPERIMENTAL_JS_AST", "")
	if !JavaScriptASTEnabled() {
		t.Error("JavaScriptASTEnabled() = false with both env vars cleared; want true (default-on since v0.20.0)")
	}

	t.Setenv("PINCHER_DISABLE_JS_AST", "1")
	if JavaScriptASTEnabled() {
		t.Error("JavaScriptASTEnabled() = true with PINCHER_DISABLE_JS_AST=1; want false")
	}
}
