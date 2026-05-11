package server

import (
	"reflect"
	"testing"
)

func TestParseHunkHeader_StandardForm(t *testing.T) {
	cases := []struct {
		header     string
		wantStart  int
		wantCount  int
	}{
		{"@@ -10,5 +20,7 @@", 20, 7},
		{"@@ -10 +20 @@", 20, 1},                        // count omitted defaults to 1
		{"@@ -10,5 +20,0 @@", 20, 0},                    // pure deletion
		{"@@ -10,5 +20,7 @@ func handleChanges() {", 20, 7}, // trailing context
		{"@@ -1 +1 @@", 1, 1},
	}
	for _, tc := range cases {
		gotStart, gotCount := parseHunkHeader(tc.header)
		if gotStart != tc.wantStart || gotCount != tc.wantCount {
			t.Errorf("parseHunkHeader(%q) = (%d, %d); want (%d, %d)",
				tc.header, gotStart, gotCount, tc.wantStart, tc.wantCount)
		}
	}
}

func TestParseHunkHeader_Malformed_ReturnsZero(t *testing.T) {
	cases := []string{
		"@@ no plus marker @@",
		"+++ b/file.go", // not a hunk header
		"",
		"@@ -10,5 @@",
	}
	for _, c := range cases {
		s, n := parseHunkHeader(c)
		if s != 0 {
			t.Errorf("parseHunkHeader(%q) start=%d; want 0 on malformed", c, s)
		}
		_ = n
	}
}

func TestParseGitDiffHunks_SingleFileMultipleHunks(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\nindex abc..def 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -10,3 +10,5 @@\n+new line\n+new line\n@@ -50,1 +52,2 @@\n+another\n"
	got := parseGitDiffHunks(diff)
	want := map[string][][2]int{
		"foo.go": {{10, 14}, {52, 53}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGitDiffHunks = %v; want %v", got, want)
	}
}

func TestParseGitDiffHunks_MultipleFiles(t *testing.T) {
	diff := "--- a/a.go\n+++ b/a.go\n@@ -1,1 +1,2 @@\n+x\n--- a/b.go\n+++ b/b.go\n@@ -100,1 +100,3 @@\n+y\n+z\n"
	got := parseGitDiffHunks(diff)
	want := map[string][][2]int{
		"a.go": {{1, 2}},
		"b.go": {{100, 102}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGitDiffHunks multi-file = %v; want %v", got, want)
	}
}

func TestParseGitDiffHunks_PureDeletion_StillEmits(t *testing.T) {
	// `+20,0` means "0 new lines starting at 20" — a pure deletion.
	// We still want a single-line range so a function losing one line
	// shows up.
	diff := "+++ b/foo.go\n@@ -10,2 +20,0 @@\n-removed\n-removed\n"
	got := parseGitDiffHunks(diff)
	want := map[string][][2]int{
		"foo.go": {{20, 20}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pure-deletion hunks = %v; want %v", got, want)
	}
}

func TestSymbolOverlapsHunks_Inside(t *testing.T) {
	// Symbol at lines 10-30; hunk at lines 15-20 (entirely inside).
	if !symbolOverlapsHunks(10, 30, [][2]int{{15, 20}}) {
		t.Errorf("hunk fully inside symbol must overlap")
	}
}

func TestSymbolOverlapsHunks_StraddlesStart(t *testing.T) {
	// Symbol at 50-100; hunk at 40-60 (overlaps start).
	if !symbolOverlapsHunks(50, 100, [][2]int{{40, 60}}) {
		t.Errorf("hunk overlapping symbol start must overlap")
	}
}

func TestSymbolOverlapsHunks_StraddlesEnd(t *testing.T) {
	if !symbolOverlapsHunks(50, 100, [][2]int{{90, 120}}) {
		t.Errorf("hunk overlapping symbol end must overlap")
	}
}

func TestSymbolOverlapsHunks_Disjoint(t *testing.T) {
	// Symbol at 50-100; hunks at 10-20 and 200-210 — neither overlaps.
	if symbolOverlapsHunks(50, 100, [][2]int{{10, 20}, {200, 210}}) {
		t.Errorf("disjoint hunks must NOT overlap")
	}
}

func TestSymbolOverlapsHunks_EmptyHunks(t *testing.T) {
	if symbolOverlapsHunks(50, 100, nil) {
		t.Errorf("nil hunks must NOT overlap (no edits in this file)")
	}
	if symbolOverlapsHunks(50, 100, [][2]int{}) {
		t.Errorf("empty hunks must NOT overlap")
	}
}

func TestSymbolOverlapsHunks_AdjacentNotOverlap(t *testing.T) {
	// Symbol ends at 100; next hunk starts at 101. The two ranges are
	// adjacent but disjoint — should NOT match.
	if symbolOverlapsHunks(50, 100, [][2]int{{101, 110}}) {
		t.Errorf("adjacent ranges must NOT overlap")
	}
}
