package cypher

import (
	"context"
	"strings"
	"testing"
)

// #1130: pre-fix, LIMIT silently dropped any value strconv.Atoi couldn't
// parse — `LIMIT 1.5` (float) returned 0 rows, no warning, no error.
// `LIMIT -1` returned a generic "unexpected token '-'" parser error
// with no LIMIT-aware guidance (user might have come from SQL where
// LIMIT -1 sometimes means "unlimited"). Both should produce a
// LIMIT-aware error rather than silent zero or a generic message.

func TestExecute_LimitFloat_ErrorsWithGuidance(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "x", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name LIMIT 1.5`)
	if err == nil {
		t.Fatal("expected error for LIMIT 1.5; got nil (silent zero rows is the pre-fix bug)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "LIMIT") {
		t.Errorf("error should name LIMIT for context; got: %v", err)
	}
	if !strings.Contains(msg, "integer") {
		t.Errorf("error should explain the integer requirement; got: %v", err)
	}
	if !strings.Contains(msg, "1.5") {
		t.Errorf("error should echo the bad value; got: %v", err)
	}
}

func TestExecute_LimitNegative_ErrorsWithGuidance(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "x", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name LIMIT -1`)
	if err == nil {
		t.Fatal("expected error for LIMIT -1; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "LIMIT") {
		t.Errorf("error should name LIMIT for context (not generic 'unexpected token'); got: %v", err)
	}
	if !strings.Contains(msg, "negative") {
		t.Errorf("error should explicitly mention negative LIMIT is unsupported; got: %v", err)
	}
	// Remediation: user might think LIMIT -1 means "unlimited" (some SQL
	// dialects) — should suggest LIMIT 0 or omitting LIMIT.
	if !strings.Contains(msg, "0") {
		t.Errorf("error should mention LIMIT 0 as the suppress-rows alternative; got: %v", err)
	}
}

func TestExecute_LimitJunkToken_ErrorsWithGuidance(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "x", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name LIMIT abc`)
	if err == nil {
		t.Fatal("expected error for LIMIT abc; got nil (silent default-limit fallthrough)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "LIMIT") {
		t.Errorf("error should name LIMIT for context; got: %v", err)
	}
	if !strings.Contains(msg, "integer") {
		t.Errorf("error should explain the integer requirement; got: %v", err)
	}
}

// Control: valid integer LIMIT still works.
func TestExecute_LimitIntegerStillWorks(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "a", "Function", "Go")
	insertSym(t, db, "f2", "b", "Function", "Go")
	insertSym(t, db, "f3", "c", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name LIMIT 2`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 2 {
		t.Errorf("expected 2 rows; got %d", len(r.Rows))
	}
}

// Control: LIMIT 0 still means "explicit zero rows" (not the bug path).
func TestExecute_LimitZeroStillMeansZeroRows(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "a", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (n:Function) RETURN n.name LIMIT 0`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(r.Rows) != 0 {
		t.Errorf("LIMIT 0 should yield 0 rows; got %d", len(r.Rows))
	}
}
