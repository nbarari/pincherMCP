package server

import "testing"

// #937: contains("diff") substring-matched "difference" / "different",
// so a meta question like "what's the difference between symbol and
// context" misrouted to shapeReview and recommended the `changes`
// tool. Word-boundary the "diff" keyword the same way #784
// word-bounded "extract" to avoid catching "extraction" / "extractor".

func TestClassifyTaskShape_DiffSubstringDoesNotTriggerReview(t *testing.T) {
	t.Parallel()
	// Tasks containing "diff" as a substring of a larger word should
	// NOT route to shapeReview just because of the substring.
	cases := map[string]guideShape{
		"what's the difference between symbol and context": shapeUnknown,
		"explain the differences between code and config corpus": shapeUnderstand,
		"differentiate the kind=Method from kind=Function":       shapeUnknown,
	}
	for task, want := range cases {
		t.Run(task, func(t *testing.T) {
			if got := classifyTaskShape(task); got != want {
				t.Errorf("classifyTaskShape(%q) = %v, want %v", task, got, want)
			}
		})
	}
}

func TestClassifyTaskShape_BareDiffStillTriggersReview(t *testing.T) {
	t.Parallel()
	// The actual "diff" word still routes to shapeReview — we
	// haven't broken the original intent.
	cases := []string{
		"review my diff before commit",
		"what's in this diff",
		"show the diff for the auth changes",
	}
	for _, task := range cases {
		t.Run(task, func(t *testing.T) {
			if got := classifyTaskShape(task); got != shapeReview {
				t.Errorf("classifyTaskShape(%q) = %v, want shapeReview", task, got)
			}
		})
	}
}
