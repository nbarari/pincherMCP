package server

import (
	"strings"
	"testing"
)

// #902: when the language filter excluded everything but a case-
// normalised form of the user's language WOULD match, the advisory
// must teach the case fix rather than recommending "drop the filter
// entirely" — dropping the filter over-broadens to all languages.

func TestVerifyEmptySearchCause_LanguageWrongCase_SuggestsCanonicalCase(t *testing.T) {
	t.Parallel()
	// The relaxer returns rows for the canonical "JavaScript" but zero
	// for the user's typed "JaVaScRiPt" (which would be the case-
	// sensitive filter at the DB layer).
	relaxer := &fakeRelaxer{counts: map[string]int{
		"handleSearch||JavaScript|": 1, // matches with canonical case
		"handleSearch|||":           1, // matches without any language
	}}
	cause, steps, ok := verifyEmptySearchCause(
		"handleSearch", "", "JaVaScRiPt", "", 0, 0, relaxer.relax(),
	)
	if !ok {
		t.Fatal("verifier returned ok=false; want a verified cause")
	}
	if !strings.Contains(cause, `language="JaVaScRiPt"`) {
		t.Errorf("cause must name the user's input case, got: %q", cause)
	}
	if !strings.Contains(cause, "wrong case") {
		t.Errorf("cause must explicitly say wrong case, got: %q", cause)
	}
	if !strings.Contains(cause, `"JavaScript"`) {
		t.Errorf("cause must name the canonical form, got: %q", cause)
	}
	// Next step should re-invoke with the CANONICAL language, not drop
	// the filter — dropping would surface non-JavaScript matches too.
	if len(steps) == 0 {
		t.Fatal("steps empty")
	}
	argsStr := steps[0]["args"]
	if !strings.Contains(argsStr, `"language":"JavaScript"`) {
		t.Errorf("next_step must suggest the canonical case in args; got: %q", argsStr)
	}
	if strings.Contains(argsStr, `"language":"JaVaScRiPt"`) {
		t.Errorf("next_step must not echo the user's wrong case; got: %q", argsStr)
	}
}

// When the canonical case STILL doesn't match (i.e. the user's
// language is the wrong language entirely, not a case typo), fall
// back to the legacy "drop the filter" advisory.
func TestVerifyEmptySearchCause_LanguageUnknownLanguage_FallsBackToDrop(t *testing.T) {
	t.Parallel()
	relaxer := &fakeRelaxer{counts: map[string]int{
		"handleSearch|||": 2, // matches without any language
		// No counts for "BogusLang" or its case variants — both probes return 0.
	}}
	cause, steps, ok := verifyEmptySearchCause(
		"handleSearch", "", "BogusLang", "", 0, 0, relaxer.relax(),
	)
	if !ok {
		t.Fatal("verifier returned ok=false")
	}
	if !strings.Contains(cause, "drop the language filter") {
		t.Errorf("unknown-language cause should fall back to drop-filter advice; got: %q", cause)
	}
	if len(steps) == 0 {
		t.Fatal("steps empty")
	}
	if strings.Contains(steps[0]["args"], `"language"`) {
		t.Errorf("fallback next_step must drop the language; got: %q", steps[0]["args"])
	}
}

// Unit test for the case-normaliser. Covers all known languages plus
// the unknown-fallback case.
func TestCanonicalLanguageCase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"javascript", "JavaScript"},
		{"JaVaScRiPt", "JavaScript"},
		{"JAVASCRIPT", "JavaScript"},
		{"JavaScript", "JavaScript"}, // already canonical, returned as-is
		{"go", "Go"},
		{"GO", "Go"},
		{"python", "Python"},
		{"c#", "C#"},
		{"C++", "C++"},
		{"typescript", "TypeScript"},
		{"BogusLang", ""}, // unknown
		{"", ""},          // empty
	}
	for _, c := range cases {
		got := canonicalLanguageCase(c.in)
		if got != c.want {
			t.Errorf("canonicalLanguageCase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
