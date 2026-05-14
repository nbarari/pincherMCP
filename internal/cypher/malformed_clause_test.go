package cypher

import (
	"context"
	"strings"
	"testing"
)

// #748: the parser's `default: p.next()` case silently skipped any
// unrecognized token at clause position. A typo'd clause keyword —
// `WERE` for `WHERE` — therefore dropped the entire filter, degrading
// `MATCH (n:Function) WERE n.name = "Open" RETURN n.name` into
// `MATCH (n:Function) RETURN n.name`, which returned every function as
// if they were real matches. The parser now rejects the malformed
// token instead of swallowing the clause.
func TestExecute_TypodWhereKeyword_RejectedNotSilentlyDropped(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "s1", "Open", "Function", "Go")
	insertSym(t, db, "s2", "Close", "Function", "Go")
	insertSym(t, db, "s3", "Read", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (n:Function) WERE n.name = "Open" RETURN n.name`)
	if err == nil {
		t.Fatal("expected a parse error for the typo'd WHERE keyword; got nil (clause silently dropped → all rows returned)")
	}
	if !strings.Contains(err.Error(), "WERE") {
		t.Errorf("error should name the offending token %q; got %q", "WERE", err.Error())
	}
	if !strings.Contains(err.Error(), "WHERE") {
		t.Errorf("error should suggest the intended keyword WHERE (did-you-mean); got %q", err.Error())
	}
}

// Other top-level junk tokens are rejected even when they aren't a
// keyword near-miss — no silent skip.
func TestExecute_UnexpectedTopLevelToken_Rejected(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "s1", "Open", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (n:Function) garbagetoken RETURN n.name`)
	if err == nil {
		t.Fatal("expected a parse error for the unexpected top-level token; got nil")
	}
	if !strings.Contains(err.Error(), "garbagetoken") {
		t.Errorf("error should name the offending token; got %q", err.Error())
	}
}

// Well-formed queries are unaffected — the rejection only fires on
// genuinely unexpected tokens.
func TestExecute_WellFormedQuery_StillParses(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "s1", "Open", "Function", "Go")
	insertSym(t, db, "s2", "Close", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) WHERE n.name = "Open" RETURN n.name ORDER BY n.name LIMIT 10`)
	if err != nil {
		t.Fatalf("well-formed query must still parse: %v", err)
	}
	if len(r.Rows) != 1 {
		t.Errorf("rows=%d, want 1 (only Open)", len(r.Rows))
	}
}

// editDistanceAtMost1 unit coverage — the did-you-mean engine.
func TestEditDistanceAtMost1(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"WERE", "WHERE", true},   // insertion
		{"WHRE", "WHERE", true},   // insertion
		{"WHEREE", "WHERE", true}, // deletion
		{"WHARE", "WHERE", true},  // substitution
		{"WHERE", "WHERE", true},  // equal
		{"RETRUN", "RETURN", false}, // transposition = distance 2
		{"FOO", "WHERE", false},
		{"", "WHERE", false},
	}
	for _, c := range cases {
		if got := editDistanceAtMost1(c.a, c.b); got != c.want {
			t.Errorf("editDistanceAtMost1(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
