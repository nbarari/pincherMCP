package ast

import (
	"strings"
	"testing"
)

// Tests for buildGoSignature (#245). The renderer must reconstruct a
// signature byte-equivalent to the source declaration for every shape
// of params and returns Go allows. Pre-fix bug: grouped same-type
// returns like `(calls, tokensUsed, tokensSaved int64, costAvoided
// float64, err error)` collapsed to `(int64, float64, error)` because
// the renderer emitted one entry per Field instead of len(Field.Names).

// TestBuildGoSignature_ShapeMatrix is table-driven so each newly-
// discovered shape becomes one row, not a new test function. Every
// row asserts the rendered Signature matches the input exactly
// (modulo the parser's normalisation, e.g. `func ` prefix).
func TestBuildGoSignature_ShapeMatrix(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "single-unnamed-return",
			src:  `package p; func f() error { return nil }`,
			want: "func f() error",
		},
		{
			name: "no-return",
			src:  `package p; func f() {}`,
			want: "func f()",
		},
		{
			name: "single-named-return-needs-parens",
			src:  `package p; func f() (x int) { return }`,
			want: "func f() (x int)",
		},
		{
			name: "two-unnamed-returns",
			src:  `package p; func f() (int, error) { return 0, nil }`,
			want: "func f() (int, error)",
		},
		{
			name: "grouped-same-type-returns",
			src:  `package p; func f() (a, b, c int) { return }`,
			want: "func f() (a, b, c int)",
		},
		{
			name: "five-named-three-typed-returns",
			src:  `package p; func GetAllTimeSavings() (calls, tokensUsed, tokensSaved int64, costAvoided float64, err error) { return }`,
			want: "func GetAllTimeSavings() (calls, tokensUsed, tokensSaved int64, costAvoided float64, err error)",
		},
		{
			name: "grouped-named-params",
			src:  `package p; func f(a, b int, c string) {}`,
			want: "func f(a, b int, c string)",
		},
		{
			name: "single-unnamed-param-no-leading-space",
			src:  `package p; func f(int) {}`,
			want: "func f(int)",
		},
		{
			name: "multiple-unnamed-params",
			src:  `package p; func f(int, string) {}`,
			want: "func f(int, string)",
		},
		{
			name: "method-with-grouped-named-returns",
			src:  `package p; type S struct{}; func (s *S) M() (x, y string) { return }`,
			want: "func (*S) M() (x, y string)",
		},
		{
			name: "no-params-no-returns",
			src:  `package p; func init() {}`,
			want: "func init()",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Extract([]byte(tc.src), "Go", "p/p.go")
			if result == nil {
				t.Fatal("Extract returned nil")
			}
			// Find the function/method symbol — there's always exactly one
			// per case (TypeSpecs in `S` cases come through as separate
			// Class symbols and are filtered by kind here).
			var got string
			for _, s := range result.Symbols {
				if s.Kind == "Function" || s.Kind == "Method" {
					got = s.Signature
					break
				}
			}
			if got != tc.want {
				t.Errorf("signature mismatch\n  src:  %s\n  got:  %q\n  want: %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestBuildGoSignature_NamesAndArityRecoverable is the regression gate
// for the specific bug call-out in #245: an LLM caller reading the
// signature must be able to recover the correct arity and parameter
// names. Without name preservation, the displayed arity is wrong and
// downstream code generation fails at compile time.
func TestBuildGoSignature_NamesAndArityRecoverable(t *testing.T) {
	src := `package db
func (s *Store) GetAllTimeSavings() (calls, tokensUsed, tokensSaved int64, costAvoided float64, err error) { return }`
	result := Extract([]byte(src), "Go", "db/db.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}

	var sig string
	for _, s := range result.Symbols {
		if s.Name == "GetAllTimeSavings" {
			sig = s.Signature
			break
		}
	}
	if sig == "" {
		t.Fatal("GetAllTimeSavings symbol not extracted")
	}

	// Each named return must be present in the rendered signature so
	// a caller can reconstruct the call site.
	for _, name := range []string{"calls", "tokensUsed", "tokensSaved", "costAvoided", "err"} {
		if !strings.Contains(sig, name) {
			t.Errorf("signature missing return name %q:\n  %s", name, sig)
		}
	}

	// Three type tokens must appear: int64, float64, error.
	for _, ty := range []string{"int64", "float64", "error"} {
		if !strings.Contains(sig, ty) {
			t.Errorf("signature missing type %q:\n  %s", ty, sig)
		}
	}
}
