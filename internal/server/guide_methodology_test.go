package server

import (
	"strings"
	"testing"
)

// #615: methodology questions ("how do I find what calls a private
// function") were extracting category nouns ("private") as the hint,
// then templating useless `search query="private"` recommendations.
// Fix: visibility/category nouns are now stop words so the hint
// extractor falls through to the actual subject (or empty).
//
// These tests pin the new stop-word coverage. They don't assert the
// hint is "" — that's a higher bar that requires methodology-shape
// detection (deferred). They DO assert the bad word doesn't survive
// as the discriminator.

func TestTaskHintFromString_DropsVisibilityCategoryNouns(t *testing.T) {
	cases := map[string]string{
		// task → forbidden hint (must NOT be the result)
		"how do I find what calls a private function":  "private",
		"list every public method":                     "public",
		"audit unexported helpers":                     "unexported",
		"find every exported symbol with no callers":   "exported",
		"show internal globals":                        "internal",
		"survey global variables":                      "global",
		"clean up the stub extractors":                 "stub",
		"static analysis tools":                        "static",
		"dynamic dispatch sites":                       "dynamic",
	}
	for task, forbidden := range cases {
		t.Run(task, func(t *testing.T) {
			got := taskHintFromString(task)
			if got == forbidden {
				t.Errorf("taskHintFromString(%q) = %q; bare category noun should not survive as the hint", task, got)
			}
		})
	}
}

func TestTaskHintFromString_KeepsRealSubjectAlongsideCategoryNoun(t *testing.T) {
	// When a real subject is present, dropping the category noun should
	// still leave the subject. e.g. "find every exported MCP handler"
	// should yield "MCP handler" or similar — not "exported".
	cases := map[string]string{
		"find every exported MCP handler":          "MCP handler",
		"list public registerTools entries":        "registerTools entries",
		"audit private cypherPropToCol callers":    "cypherPropToCol callers",
	}
	for task, want := range cases {
		t.Run(task, func(t *testing.T) {
			got := taskHintFromString(task)
			if !strings.Contains(strings.ToLower(got), strings.ToLower(strings.Split(want, " ")[0])) {
				t.Errorf("taskHintFromString(%q) = %q; expected to retain real subject like %q",
					task, got, want)
			}
		})
	}
}
