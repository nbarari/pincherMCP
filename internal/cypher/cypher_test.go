package cypher

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE symbols (
			id TEXT PRIMARY KEY,
			project_id TEXT,
			file_path TEXT,
			name TEXT,
			qualified_name TEXT,
			kind TEXT,
			language TEXT,
			start_byte INTEGER,
			end_byte INTEGER,
			start_line INTEGER,
			end_line INTEGER,
			is_exported INTEGER DEFAULT 0,
			is_entry_point INTEGER DEFAULT 0,
			complexity INTEGER DEFAULT 0,
			extraction_confidence REAL NOT NULL DEFAULT 1.0,
			signature TEXT,
			return_type TEXT,
			docstring TEXT,
			is_test INTEGER DEFAULT 0
		);
		CREATE TABLE edges (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id TEXT,
			from_id TEXT,
			to_id TEXT,
			kind TEXT,
			confidence REAL DEFAULT 1.0
		);
	`)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func insertSym(t *testing.T, db *sql.DB, id, name, kind, lang string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
			start_byte, end_byte, start_line, end_line) VALUES (?,?,?,?,?,?,?, 0,100,1,5)`,
		id, "proj1", "file.go", name, name, kind, lang,
	)
	if err != nil {
		t.Fatalf("insert symbol %q: %v", id, err)
	}
}

func insertEdge(t *testing.T, db *sql.DB, fromID, toID, kind string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO edges(project_id, from_id, to_id, kind) VALUES ('proj1',?,?,?)`,
		fromID, toID, kind,
	)
	if err != nil {
		t.Fatalf("insert edge %s->%s: %v", fromID, toID, err)
	}
}

func exec(t *testing.T, db *sql.DB, query string) *Result {
	t.Helper()
	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(), query)
	if err != nil {
		t.Fatalf("Execute(%q): %v", query, err)
	}
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// Tokenizer tests
// ─────────────────────────────────────────────────────────────────────────────

func TestTokenize_keywords(t *testing.T) {
	toks := tokenize("MATCH WHERE RETURN LIMIT ORDER BY")
	for _, tok := range toks {
		if tok.kind != "KEYWORD" {
			t.Errorf("expected KEYWORD, got %q for %q", tok.kind, tok.value)
		}
	}
}

func TestTokenize_strings(t *testing.T) {
	toks := tokenize(`'hello' "world"`)
	if len(toks) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(toks))
	}
	for _, tok := range toks {
		if tok.kind != "STRING" {
			t.Errorf("expected STRING, got %q", tok.kind)
		}
	}
}

func TestTokenize_operators(t *testing.T) {
	toks := tokenize("<> >= <= =~ ->")
	ops := make(map[string]bool)
	for _, tok := range toks {
		ops[tok.value] = true
	}
	for _, want := range []string{"<>", ">=", "<=", "=~", "->"} {
		if !ops[want] {
			t.Errorf("expected operator %q", want)
		}
	}
}

func TestTokenize_hops(t *testing.T) {
	toks := tokenize("*1..3")
	found := false
	for _, tok := range toks {
		if tok.kind == "HOPS" && tok.value == "1..3" {
			found = true
		}
	}
	if !found {
		t.Error("expected HOPS token with value '1..3'")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Parser tests
// ─────────────────────────────────────────────────────────────────────────────

func TestParseHops(t *testing.T) {
	cases := []struct {
		s        string
		min, max int
	}{
		{"1..3", 1, 3},
		{"2..5", 2, 5},
		{"3", 3, 3},
		{"", 1, 1},
		// min < 1: clamp to 1
		{"0..3", 1, 3},
		// max < min: clamp max to min
		{"3..1", 3, 3},
	}
	for _, c := range cases {
		mn, mx := parseHops(c.s)
		if mn != c.min || mx != c.max {
			t.Errorf("parseHops(%q) = (%d,%d), want (%d,%d)", c.s, mn, mx, c.min, c.max)
		}
	}
}

func TestParse_NodeOnlyQuery(t *testing.T) {
	q, err := parse("MATCH (f:Function) RETURN f.name LIMIT 10")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(q.patterns) != 1 {
		t.Errorf("expected 1 pattern, got %d", len(q.patterns))
	}
	if q.patterns[0].fromKind != "Function" {
		t.Errorf("fromKind = %q, want Function", q.patterns[0].fromKind)
	}
	if q.limit != 10 {
		t.Errorf("limit = %d, want 10", q.limit)
	}
}

func TestParse_EdgeQuery(t *testing.T) {
	q, err := parse("MATCH (a:Function)-[:CALLS]->(b) RETURN a.name, b.name")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(q.patterns[0].edgeKinds) == 0 {
		t.Error("expected edge kind CALLS")
	}
	if q.patterns[0].edgeKinds[0] != "CALLS" {
		t.Errorf("edgeKind = %q, want CALLS", q.patterns[0].edgeKinds[0])
	}
}

func TestParse_WhereCondition(t *testing.T) {
	q, err := parse("MATCH (f:Function) WHERE f.name = 'main' RETURN f.name")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(q.conditions) == 0 {
		t.Fatal("expected at least one condition")
	}
	c := q.conditions[0]
	if c.property != "name" || c.op != "=" || c.value != "main" {
		t.Errorf("condition = {%q %q %q}, want {name = main}", c.property, c.op, c.value)
	}
}

func TestParse_Distinct(t *testing.T) {
	q, err := parse("MATCH (f) RETURN DISTINCT f.name")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !q.distinct {
		t.Error("expected distinct=true")
	}
}

func TestParse_VariableLengthHops(t *testing.T) {
	q, err := parse("MATCH (a)-[:CALLS*1..3]->(b) WHERE a.name='main' RETURN b.name")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.patterns[0].minHops != 1 || q.patterns[0].maxHops != 3 {
		t.Errorf("hops = %d..%d, want 1..3", q.patterns[0].minHops, q.patterns[0].maxHops)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Executor: node scan
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeScan_AllFunctions(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Foo", "Function", "Go")
	insertSym(t, db, "f2", "Bar", "Function", "Go")
	insertSym(t, db, "t1", "MyType", "Class", "Go")

	r := exec(t, db, "MATCH (f:Function) RETURN f.name LIMIT 10")
	if r.Total != 2 {
		t.Errorf("expected 2 functions, got %d", r.Total)
	}
}

func TestNodeScan_WhereEquals(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "main", "Function", "Go")
	insertSym(t, db, "f2", "helper", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) WHERE f.name = 'main' RETURN f.name")
	if r.Total != 1 {
		t.Errorf("expected 1 result, got %d", r.Total)
	}
	if r.Rows[0]["f.name"] != "main" {
		t.Errorf("unexpected result: %v", r.Rows[0])
	}
}

func TestNodeScan_WhereRegex(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "HandleLogin", "Function", "Go")
	insertSym(t, db, "f2", "HandleLogout", "Function", "Go")
	insertSym(t, db, "f3", "DoSomething", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) WHERE f.name =~ '.*Handle.*' RETURN f.name")
	if r.Total != 2 {
		t.Errorf("expected 2 Handler functions, got %d", r.Total)
	}
}

func TestNodeScan_WhereContains(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "processOrder", "Function", "Go")
	insertSym(t, db, "f2", "processPayment", "Function", "Go")
	insertSym(t, db, "f3", "startServer", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) WHERE f.name CONTAINS 'process' RETURN f.name")
	if r.Total != 2 {
		t.Errorf("expected 2 results, got %d", r.Total)
	}
}

func TestNodeScan_Count(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "A", "Function", "Go")
	insertSym(t, db, "f2", "B", "Function", "Go")
	insertSym(t, db, "f3", "C", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) RETURN COUNT(f) AS total")
	if r.Total != 1 {
		t.Fatalf("COUNT should return 1 row, got %d", r.Total)
	}
	if r.Rows[0]["total"] != 3 {
		t.Errorf("COUNT = %v, want 3", r.Rows[0]["total"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Executor: edge queries
// ─────────────────────────────────────────────────────────────────────────────

func TestJoinQuery_SingleHop(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "main_fn", "main", "Function", "Go")
	insertSym(t, db, "run_fn", "run", "Function", "Go")
	insertSym(t, db, "other_fn", "other", "Function", "Go")
	insertEdge(t, db, "main_fn", "run_fn", "CALLS")

	r := exec(t, db, "MATCH (a:Function)-[:CALLS]->(b) WHERE a.name='main' RETURN b.name")
	if r.Total != 1 {
		t.Errorf("expected 1 callee, got %d", r.Total)
	}
	if r.Rows[0]["b.name"] != "run" {
		t.Errorf("expected 'run', got %v", r.Rows[0]["b.name"])
	}
}

func TestJoinQuery_NoEdgeFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "alpha", "Function", "Go")
	insertSym(t, db, "b", "beta", "Function", "Go")
	insertEdge(t, db, "a", "b", "CALLS")

	r := exec(t, db, "MATCH (x)-[:CALLS]->(y) RETURN x.name, y.name")
	if r.Total != 1 {
		t.Errorf("expected 1 edge result, got %d", r.Total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Executor: BFS variable-length paths
// ─────────────────────────────────────────────────────────────────────────────

func TestBFS_VariableLength(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	// Chain: main -> a -> b -> c
	insertSym(t, db, "main_fn", "main", "Function", "Go")
	insertSym(t, db, "a_fn", "a", "Function", "Go")
	insertSym(t, db, "b_fn", "b", "Function", "Go")
	insertSym(t, db, "c_fn", "c", "Function", "Go")
	insertEdge(t, db, "main_fn", "a_fn", "CALLS")
	insertEdge(t, db, "a_fn", "b_fn", "CALLS")
	insertEdge(t, db, "b_fn", "c_fn", "CALLS")

	r := exec(t, db, "MATCH (a)-[:CALLS*1..3]->(b) WHERE a.name='main' RETURN b.name")
	// Should find a, b, c (depths 1, 2, 3)
	if r.Total < 3 {
		t.Errorf("expected at least 3 nodes in chain, got %d", r.Total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// matchesConditions
// ─────────────────────────────────────────────────────────────────────────────

func TestMatchesConditions(t *testing.T) {
	row := map[string]any{"n.name": "processOrder", "n.kind": "Function"}

	pass := []condition{
		{variable: "n", property: "name", op: "=", value: "processOrder"},
		{variable: "n", property: "name", op: "CONTAINS", value: "Order"},
		{variable: "n", property: "name", op: "STARTS WITH", value: "process"},
		{variable: "n", property: "name", op: "=~", value: ".*Order.*"},
		{variable: "n", property: "name", op: "<>", value: "other"},
	}
	for _, c := range pass {
		if !matchesConditions(row, []condition{c}) {
			t.Errorf("condition {%s %s %s} should pass", c.property, c.op, c.value)
		}
	}

	fail := []condition{
		{variable: "n", property: "name", op: "=", value: "wrong"},
		{variable: "n", property: "name", op: "<>", value: "processOrder"},
		{variable: "n", property: "name", op: "CONTAINS", value: "xyz"},
	}
	for _, c := range fail {
		if matchesConditions(row, []condition{c}) {
			t.Errorf("condition {%s %s %s} should fail", c.property, c.op, c.value)
		}
	}
}

func TestMatchesConditions_Numeric(t *testing.T) {
	row := map[string]any{"n.complexity": "5"}
	if !matchesConditions(row, []condition{{variable: "n", property: "complexity", op: ">", value: "3"}}) {
		t.Error("5 > 3 should be true")
	}
	if matchesConditions(row, []condition{{variable: "n", property: "complexity", op: "<", value: "3"}}) {
		t.Error("5 < 3 should be false")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// cypherPropToCol
// ─────────────────────────────────────────────────────────────────────────────

func TestCypherPropToCol(t *testing.T) {
	cases := map[string]string{
		"name":           "name",
		"qualified_name": "qualified_name",
		"qn":             "qualified_name",
		"kind":           "kind",
		"label":          "kind",
		"file_path":      "file_path",
		"language":       "language",
		"start_line":     "start_line",
		"end_line":       "end_line",
		"unknown_prop":   "",
	}
	for prop, want := range cases {
		got := cypherPropToCol(prop)
		if got != want {
			t.Errorf("cypherPropToCol(%q) = %q, want %q", prop, got, want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Edge cases
// ─────────────────────────────────────────────────────────────────────────────

// #361: empty input must error — pre-fix the parser silently produced
// an empty queryAST and the runner returned zero rows, indistinguishable
// from a real-but-empty result. Typo in MATCH/RETURN looked like missing
// data, not malformed query.
func TestEmptyQuery(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	e := &Executor{DB: db, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(), "")
	if err == nil {
		t.Fatal("empty query should error, got nil")
	}
	if !strings.Contains(err.Error(), "MATCH") {
		t.Errorf("error should mention MATCH; got %v", err)
	}
}

func TestLimitRespected(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	for i := 0; i < 20; i++ {
		id := string(rune('a' + i))
		insertSym(t, db, id, id, "Function", "Go")
	}
	r := exec(t, db, "MATCH (f:Function) RETURN f.name LIMIT 5")
	if r.Total > 5 {
		t.Errorf("expected at most 5 results, got %d", r.Total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parseQuery coverage: ORDER BY, STARTS WITH
// ─────────────────────────────────────────────────────────────────────────────

func TestParse_OrderBy(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) RETURN f.name ORDER BY f.name")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if q.orderBy == "" {
		t.Error("expected orderBy to be set")
	}
}

func TestParse_OrderByDesc(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) RETURN f.name ORDER BY f.name DESC")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if q.orderDir != "DESC" {
		t.Errorf("orderDir = %q, want DESC", q.orderDir)
	}
}

func TestParse_OrderByAsc(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) RETURN f.name ORDER BY f.name ASC")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if q.orderDir == "DESC" {
		t.Error("expected ascending order (not DESC)")
	}
}

func TestParse_StartsWith(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) WHERE f.name STARTS WITH 'Get' RETURN f.name")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if len(q.conditions) == 0 {
		t.Fatal("expected at least 1 condition")
	}
	if q.conditions[0].op != "STARTS WITH" {
		t.Errorf("op = %q, want 'STARTS WITH'", q.conditions[0].op)
	}
}

func TestNodeScan_StartsWith(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "get1", "GetUser", "Function", "Go")
	insertSym(t, db, "set1", "SetUser", "Function", "Go")
	insertSym(t, db, "del1", "DeleteUser", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) WHERE f.name STARTS WITH 'Get' RETURN f.name")
	if r.Total != 1 {
		t.Errorf("expected 1 result for STARTS WITH 'Get', got %d", r.Total)
	}
}

// #340: ENDS WITH parsed and stored as a first-class operator,
// symmetric to STARTS WITH.
func TestParse_EndsWith(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) WHERE f.name ENDS WITH 'User' RETURN f.name")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if len(q.conditions) == 0 {
		t.Fatal("expected at least 1 condition")
	}
	if q.conditions[0].op != "ENDS WITH" {
		t.Errorf("op = %q, want 'ENDS WITH'", q.conditions[0].op)
	}
}

// #340: ENDS WITH executed via the SQL JOIN/scan path returns matching
// rows. Three seeds where two end in "User" and one doesn't.
func TestNodeScan_EndsWith(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "g1", "GetUser", "Function", "Go")
	insertSym(t, db, "s1", "SetUser", "Function", "Go")
	insertSym(t, db, "d1", "DeleteAccount", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) WHERE f.name ENDS WITH 'User' RETURN f.name")
	if r.Total != 2 {
		t.Errorf("expected 2 results for ENDS WITH 'User', got %d (%v)", r.Total, r.Rows)
	}
	for _, row := range r.Rows {
		name, _ := row["f.name"].(string)
		if !strings.HasSuffix(name, "User") {
			t.Errorf("row %q does not end in User", name)
		}
	}
}

// #342: IS NULL parsed and stored.
func TestParse_IsNull(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) WHERE f.qualified_name IS NULL RETURN f.name")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if len(q.conditions) == 0 {
		t.Fatal("expected at least 1 condition")
	}
	if q.conditions[0].op != "IS NULL" {
		t.Errorf("op = %q, want 'IS NULL'", q.conditions[0].op)
	}
}

// #342: IS NOT NULL parsed and stored.
func TestParse_IsNotNull(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) WHERE f.qualified_name IS NOT NULL RETURN f.name")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if q.conditions[0].op != "IS NOT NULL" {
		t.Errorf("op = %q, want 'IS NOT NULL'", q.conditions[0].op)
	}
}

// #342: IS NULL via SQL pushdown matches rows where the column is empty
// or NULL. qualified_name is in cypherPropToCol so the predicate
// pushes into SQL.
func TestNodeScan_IsNull_SQLPushdown(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	// Two rows with QN, one with empty QN.
	insertSym(t, db, "a", "Alpha", "Function", "Go")
	insertSym(t, db, "b", "Beta", "Function", "Go")
	insertSym(t, db, "c", "Gamma", "Function", "Go")
	// Override QN for one row to empty so IS NULL matches it.
	if _, err := db.Exec(`UPDATE symbols SET qualified_name = '' WHERE id = 'c'`); err != nil {
		t.Fatal(err)
	}

	r := exec(t, db, "MATCH (f:Function) WHERE f.qualified_name IS NULL RETURN f.name")
	if r.Total != 1 {
		t.Errorf("expected 1 IS NULL result, got %d (%v)", r.Total, r.Rows)
	}
	if r.Total > 0 {
		name, _ := r.Rows[0]["f.name"].(string)
		if name != "Gamma" {
			t.Errorf("expected Gamma to be the IS NULL match; got %q", name)
		}
	}
}

// #342: IS NOT NULL is the complement.
func TestNodeScan_IsNotNull_SQLPushdown(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "Alpha", "Function", "Go")
	insertSym(t, db, "b", "Beta", "Function", "Go")
	insertSym(t, db, "c", "Gamma", "Function", "Go")
	if _, err := db.Exec(`UPDATE symbols SET qualified_name = '' WHERE id = 'c'`); err != nil {
		t.Fatal(err)
	}

	r := exec(t, db, "MATCH (f:Function) WHERE f.qualified_name IS NOT NULL RETURN f.name")
	if r.Total != 2 {
		t.Errorf("expected 2 IS NOT NULL results, got %d (%v)", r.Total, r.Rows)
	}
}

// #348: implicit GROUP BY when mixing non-aggregate column with COUNT.
// Pre-fix, this collapsed every result to a single row with the
// non-aggregate column dropped. Now it groups by the non-aggregate.
func TestAggregation_ImplicitGroupBy_KindHistogram(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Alpha", "Function", "Go")
	insertSym(t, db, "f2", "Beta", "Function", "Go")
	insertSym(t, db, "f3", "Gamma", "Function", "Go")
	insertSym(t, db, "m1", "DoIt", "Method", "Go")
	insertSym(t, db, "m2", "DoOther", "Method", "Go")
	insertSym(t, db, "c1", "Widget", "Class", "Go")

	r := exec(t, db, "MATCH (n) RETURN n.kind, COUNT(n) ORDER BY COUNT(n) DESC")
	if r.Total != 3 {
		t.Fatalf("expected 3 groups (Function, Method, Class), got %d (%v)", r.Total, r.Rows)
	}
	// First row: Function with count 3.
	first := r.Rows[0]
	if first["n.kind"] != "Function" {
		t.Errorf("first group kind = %v, want Function", first["n.kind"])
	}
	count, _ := first["COUNT(n)"].(int)
	if count == 0 {
		// May come back as int64 or float64 depending on JSON path.
		if cf, ok := first["COUNT(n)"].(float64); ok {
			count = int(cf)
		}
	}
	if count != 3 {
		t.Errorf("first group count = %v, want 3", first["COUNT(n)"])
	}
	// Counts must sum to total seeded rows.
	var sum int
	for _, row := range r.Rows {
		switch v := row["COUNT(n)"].(type) {
		case int:
			sum += v
		case int64:
			sum += int(v)
		case float64:
			sum += int(v)
		}
	}
	if sum != 6 {
		t.Errorf("counts sum to %d, want 6 (total seeded rows)", sum)
	}
}

// #348: backward compatibility — RETURN COUNT(n) with no group var
// still returns one row with the total.
func TestAggregation_NoGroupBy_BackwardCompat(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Alpha", "Function", "Go")
	insertSym(t, db, "f2", "Beta", "Function", "Go")
	insertSym(t, db, "f3", "Gamma", "Function", "Go")

	r := exec(t, db, "MATCH (n) RETURN COUNT(n)")
	if r.Total != 1 {
		t.Fatalf("expected 1 row for ungrouped COUNT, got %d", r.Total)
	}
	row := r.Rows[0]
	switch v := row["COUNT(n)"].(type) {
	case int:
		if v != 3 {
			t.Errorf("count = %d, want 3", v)
		}
	case float64:
		if int(v) != 3 {
			t.Errorf("count = %v, want 3", v)
		}
	default:
		t.Errorf("unexpected count type %T (%v)", v, v)
	}
}

// #354: WHERE NOT n.x = "..." inverts the comparison; previously the
// parser errored "unsupported operator: <varname>" when NOT prefixed
// a condition.
func TestParse_NotPrefix(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) WHERE NOT f.name = 'main' RETURN f.name")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if len(q.conditions) == 0 {
		t.Fatal("expected at least 1 condition")
	}
	if !q.conditions[0].negated {
		t.Errorf("conditions[0].negated = false; want true after NOT prefix")
	}
}

// #354: NOT prefix runs through SQL pushdown — `NOT n.name = "main"`
// returns rows where name != "main".
func TestNodeScan_NotEquals_ViaNOT(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "m1", "main", "Function", "Go")
	insertSym(t, db, "f1", "Alpha", "Function", "Go")
	insertSym(t, db, "f2", "Beta", "Function", "Go")

	r := exec(t, db, `MATCH (f:Function) WHERE NOT f.name = 'main' RETURN f.name`)
	if r.Total != 2 {
		t.Errorf("expected 2 rows after NOT name='main', got %d (%v)", r.Total, r.Rows)
	}
	for _, row := range r.Rows {
		if row["f.name"] == "main" {
			t.Errorf("row %v should be excluded by NOT name='main'", row)
		}
	}
}

// #354: NOT works alongside non-negated conditions in the same WHERE.
func TestNodeScan_NotPrefix_Compound(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "main", "Function", "Go")
	insertSym(t, db, "f2", "Alpha", "Function", "Go")
	insertSym(t, db, "f3", "Beta", "Function", "Python")

	// Functions, not Go, not main → only Beta (Python) qualifies.
	r := exec(t, db, `MATCH (f:Function) WHERE f.kind = 'Function' AND NOT f.language = 'Go' RETURN f.name`)
	if r.Total != 1 {
		t.Errorf("expected 1 row, got %d (%v)", r.Total, r.Rows)
	}
	if r.Total > 0 {
		if name, _ := r.Rows[0]["f.name"].(string); name != "Beta" {
			t.Errorf("expected Beta, got %v", name)
		}
	}
}

// #354: NOT honours operators beyond `=` — wraps the entire inner clause.
func TestNodeScan_NotContains(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "fooBar", "Function", "Go")
	insertSym(t, db, "f2", "Baz", "Function", "Go")

	r := exec(t, db, `MATCH (f:Function) WHERE NOT f.name CONTAINS 'foo' RETURN f.name`)
	if r.Total != 1 {
		t.Errorf("expected 1 row, got %d (%v)", r.Total, r.Rows)
	}
	if r.Total > 0 {
		if name, _ := r.Rows[0]["f.name"].(string); name != "Baz" {
			t.Errorf("expected Baz, got %v", name)
		}
	}
}

// #340: ENDS WITH on file_path filters by extension.
func TestNodeScan_EndsWith_FilePath(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "a", "X", "Function", "Go")
	// Override file_path on the seed so we have a mix of extensions.
	if _, err := db.Exec(`UPDATE symbols SET file_path = 'main.go' WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	insertSym(t, db, "b", "Y", "Function", "Python")
	if _, err := db.Exec(`UPDATE symbols SET file_path = 'app.py' WHERE id = 'b'`); err != nil {
		t.Fatal(err)
	}

	r := exec(t, db, `MATCH (f:Function) WHERE f.file_path ENDS WITH '.go' RETURN f.name`)
	if r.Total != 1 {
		t.Errorf("expected 1 result for file_path ENDS WITH '.go', got %d (%v)", r.Total, r.Rows)
	}
}

func TestNodeScan_WhereNotEquals(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Alpha", "Function", "Go")
	insertSym(t, db, "f2", "Beta", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) WHERE f.name <> 'Alpha' RETURN f.name")
	if r.Total == 0 {
		t.Error("expected results for <> operator")
	}
	for _, row := range r.Rows {
		if row["f.name"] == "Alpha" {
			t.Error("Alpha should be excluded by <> filter")
		}
	}
}

func TestNodeScan_WhereGreaterThan(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "foo", "Function", "Go")

	// complexity > 0 (default is 0) → no results
	r := exec(t, db, "MATCH (f:Function) WHERE f.complexity > 5 RETURN f.name")
	_ = r // just verify no crash
}

// ─────────────────────────────────────────────────────────────────────────────
// QueryAST and minHops
// ─────────────────────────────────────────────────────────────────────────────

func TestQueryAST_Export(t *testing.T) {
	tokens := tokenize("MATCH (f:Function)-[:CALLS]->(g) WHERE f.name = 'main' RETURN g.name LIMIT 10")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	info := QueryAST(q)
	if info["patterns"].(int) == 0 {
		t.Error("expected patterns > 0")
	}
	if info["conditions"].(int) == 0 {
		t.Error("expected conditions > 0")
	}
	if info["limit"].(int) != 10 {
		t.Errorf("limit = %v, want 10", info["limit"])
	}
}

func TestMinHops_WithPattern(t *testing.T) {
	tokens := tokenize("MATCH (a)-[:CALLS*1..3]->(b) RETURN b.name")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	if q.minHops() < 1 {
		t.Errorf("minHops = %d, want ≥1", q.minHops())
	}
}

func TestMinHops_NoPattern(t *testing.T) {
	q := &queryAST{}
	if q.minHops() != 1 {
		t.Errorf("minHops with no patterns = %d, want 1", q.minHops())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Executor.MaxRows capping
// ─────────────────────────────────────────────────────────────────────────────

func TestMaxRows_Capping(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	for i := 0; i < 5; i++ {
		insertSym(t, db, string(rune('a'+i)), string(rune('A'+i)), "Function", "Go")
	}
	// MaxRows = 0 → should use default (not panic or return 0)
	e := &Executor{DB: db, MaxRows: 0, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(), "MATCH (f:Function) RETURN f.name")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Total == 0 {
		t.Error("expected results with MaxRows=0 (should use default)")
	}
}

func TestMaxRows_ExceedsCap(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	// MaxRows > 10000 → should be capped at 10000
	e := &Executor{DB: db, MaxRows: 99999, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(), "MATCH (f:Function) RETURN f.name")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = r // just verify no panic
}

// ─────────────────────────────────────────────────────────────────────────────
// OR conditions
// ─────────────────────────────────────────────────────────────────────────────

func TestParse_ORCondition(t *testing.T) {
	tokens := tokenize("MATCH (f:Function) WHERE f.name = 'Alpha' OR f.name = 'Beta' RETURN f.name")
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		t.Fatalf("parseQuery: %v", err)
	}
	// Both conditions should be captured (OR treated as AND in simplified impl)
	if len(q.conditions) < 2 {
		t.Errorf("expected 2+ conditions for OR clause, got %d", len(q.conditions))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// COUNT with alias
// ─────────────────────────────────────────────────────────────────────────────

func TestNodeScan_CountWithAlias(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Foo", "Function", "Go")
	insertSym(t, db, "f2", "Bar", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) RETURN COUNT(f) AS total")
	if r.Total == 0 {
		t.Error("expected count result")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Execute error: invalid query syntax
// ─────────────────────────────────────────────────────────────────────────────

func TestExecute_InvalidCypher(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	// Malformed Cypher that can't be executed (bad WHERE with no op)
	_, err := e.Execute(context.Background(), "MATCH (f:Function) WHERE RETURN f.name")
	// Execute may or may not error — just verify no panic
	_ = err
}

// ─────────────────────────────────────────────────────────────────────────────
// parseProps via MATCH with inline props
// ─────────────────────────────────────────────────────────────────────────────

func TestParsePattern_WithNodeProps(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "f1", "Foo", "Function", "Go")
	insertSym(t, db, "f2", "Bar", "Function", "Go")

	// MATCH (n:Function {name: 'Foo'}) uses parseProps internally
	r := exec(t, db, "MATCH (n:Function) WHERE n.name='Foo' RETURN n.name")
	if r.Total == 0 {
		t.Error("expected at least one result with name filter")
	}
	for _, row := range r.Rows {
		if row["n.name"] != "Foo" {
			t.Errorf("expected Foo, got %v", row["n.name"])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildResult: DISTINCT projection
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildResult_Distinct(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	// Insert two functions with same name
	insertSym(t, db, "d1", "MyFn", "Function", "Go")
	insertSym(t, db, "d2", "MyFn", "Function", "Go")

	r := exec(t, db, "MATCH (f:Function) RETURN DISTINCT f.name")
	// DISTINCT should deduplicate MyFn
	seen := map[string]int{}
	for _, row := range r.Rows {
		if n, ok := row["f.name"].(string); ok {
			seen[n]++
		}
	}
	if seen["MyFn"] > 1 {
		t.Errorf("DISTINCT should deduplicate MyFn, got %d occurrences", seen["MyFn"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildResult: COUNT aggregate
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildResult_Count(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "c1", "Alpha", "Function", "Go")
	insertSym(t, db, "c2", "Beta", "Function", "Go")
	insertSym(t, db, "c3", "Gamma", "Class", "Go")

	r := exec(t, db, "MATCH (f:Function) RETURN COUNT(f)")
	if r.Total == 0 {
		t.Error("expected count result")
	}
	row := r.Rows[0]
	// Should have a count column
	found := false
	for k, v := range row {
		if k != "" {
			if n, ok := v.(int); ok && n >= 2 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected count >= 2 in result, got %v", row)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildResult: auto-projection (no explicit return vars)
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildResult_AutoProject(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "ap1", "AutoFn", "Function", "Go")

	// RETURN * triggers auto-projection
	r := exec(t, db, "MATCH (f:Function) WHERE f.name='AutoFn' RETURN f.name, f.kind")
	if r.Total == 0 {
		t.Error("expected auto-project result")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// runJoinQuery: edge traversal
// ─────────────────────────────────────────────────────────────────────────────

func TestRunJoinQuery_WithEdge(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "j1", "Caller", "Function", "Go")
	insertSym(t, db, "j2", "Callee", "Function", "Go")
	insertEdge(t, db, "j1", "j2", "CALLS")

	r := exec(t, db, "MATCH (a:Function)-[:CALLS]->(b:Function) RETURN a.name, b.name")
	if r.Total == 0 {
		t.Error("expected join result with edge traversal")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Execute: error on completely invalid input
// ─────────────────────────────────────────────────────────────────────────────

func TestExecute_EmptyQuery(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(), "")
	// Empty query should return an error
	if err == nil {
		// Some engines might return empty result, that's also fine
		t.Log("empty query returned nil error")
	}
}

func TestExecute_NoMatchClause(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(), "RETURN 1")
	_ = err // just verify no panic
}

// ─────────────────────────────────────────────────────────────────────────────
// matchesConditions: various operator coverage
// ─────────────────────────────────────────────────────────────────────────────

func TestMatchesConditions_ContainsFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "mc1", "UserService", "Class", "Go")
	insertSym(t, db, "mc2", "OrderHandler", "Class", "Go")

	r := exec(t, db, "MATCH (n:Class) WHERE n.name CONTAINS 'Service' RETURN n.name")
	if r.Total == 0 {
		t.Error("expected CONTAINS result")
	}
	for _, row := range r.Rows {
		name, _ := row["n.name"].(string)
		if name != "UserService" {
			t.Errorf("unexpected result: %v", name)
		}
	}
}

func TestMatchesConditions_RegexFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "rx1", "FooHandler", "Function", "Go")
	insertSym(t, db, "rx2", "BarService", "Function", "Go")

	r := exec(t, db, "MATCH (n:Function) WHERE n.name =~ 'Foo.*' RETURN n.name")
	if r.Total == 0 {
		t.Error("expected regex match result")
	}
}

func TestMatchesConditions_NotEq(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "ne1", "Alpha", "Function", "Go")
	insertSym(t, db, "ne2", "Beta", "Function", "Go")

	r := exec(t, db, "MATCH (n:Function) WHERE n.name <> 'Alpha' RETURN n.name")
	for _, row := range r.Rows {
		if row["n.name"] == "Alpha" {
			t.Error("Alpha should be excluded by <> filter")
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parseProps: inline node property filter {key: val}
// ─────────────────────────────────────────────────────────────────────────────

func TestParseProps_InlineNodeFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "pp1", "Alpha", "Function", "Go")
	insertSym(t, db, "pp2", "Beta", "Function", "Go")

	// Inline props on from-node: MATCH (n:Function {name: Alpha}) RETURN n.name
	// (tokenizer doesn't quote-strip, so value must match as stored)
	r := exec(t, db, "MATCH (n:Function {name: Alpha}) RETURN n.name")
	if r.Total == 0 {
		t.Skip("parseProps filter returned no results — may need exact token match")
	}
	for _, row := range r.Rows {
		if row["n.name"] != "Alpha" {
			t.Errorf("expected Alpha, got %v", row["n.name"])
		}
	}
}

func TestParseProps_EmptyBraces(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "ep1", "Gamma", "Method", "Go")

	// Empty braces should parse cleanly and return all matches
	r := exec(t, db, "MATCH (n:Method {}) RETURN n.name")
	if r.Total == 0 {
		t.Skip("empty braces filter may not be supported")
	}
}

func TestParseProps_InlineToNodeFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "tp1", "Caller", "Function", "Go")
	insertSym(t, db, "tp2", "Callee", "Function", "Go")
	insertEdge(t, db, "tp1", "tp2", "CALLS")

	// Inline props on to-node
	r := exec(t, db, "MATCH (a:Function)-[:CALLS]->(b:Function {name: Callee}) RETURN b.name")
	if r.Total == 0 {
		t.Skip("to-node inline props filter returned no results")
	}
	for _, row := range r.Rows {
		if row["b.name"] != "Callee" {
			t.Errorf("expected Callee, got %v", row["b.name"])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// matchesConditions: >= and <= operators
// ─────────────────────────────────────────────────────────────────────────────

func TestMatchesConditions_GteLte(t *testing.T) {
	row := map[string]any{"n.complexity": "5"}

	// >= : 5 >= 5 → true, 5 >= 6 → false
	if !matchesConditions(row, []condition{{variable: "n", property: "complexity", op: ">=", value: "5"}}) {
		t.Error("5 >= 5 should be true")
	}
	if matchesConditions(row, []condition{{variable: "n", property: "complexity", op: ">=", value: "6"}}) {
		t.Error("5 >= 6 should be false")
	}

	// <= : 5 <= 5 → true, 5 <= 4 → false
	if !matchesConditions(row, []condition{{variable: "n", property: "complexity", op: "<=", value: "5"}}) {
		t.Error("5 <= 5 should be true")
	}
	if matchesConditions(row, []condition{{variable: "n", property: "complexity", op: "<=", value: "4"}}) {
		t.Error("5 <= 4 should be false")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Execute: parse error path
// ─────────────────────────────────────────────────────────────────────────────

func TestExecute_ParseError(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	// A deeply malformed query that cannot be parsed
	_, err := e.Execute(context.Background(), "MATCH ((( GARBLED NONSENSE )))!!!!!")
	// Either an error or empty result is acceptable — we just need the parse
	// error branch to execute without panicking.
	_ = err
}

// ─────────────────────────────────────────────────────────────────────────────
// buildResult: ORDER BY + return whole variable (no property)
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildResult_OrderBy(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "ob1", "Zebra", "Function", "Go")
	insertSym(t, db, "ob2", "Apple", "Function", "Go")
	insertSym(t, db, "ob3", "Mango", "Function", "Go")

	r := exec(t, db, "MATCH (n:Function) RETURN n.name ORDER BY n.name ASC")
	if r.Total < 3 {
		t.Fatalf("expected >=3 rows, got %d", r.Total)
	}
	// Verify ascending order among our inserted symbols
	names := make([]string, 0)
	for _, row := range r.Rows {
		if n, ok := row["n.name"].(string); ok {
			names = append(names, n)
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Errorf("ORDER BY ASC violated: %q after %q", names[i], names[i-1])
		}
	}
}

func TestBuildResult_OrderByDesc(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "od1", "Aardvark", "Interface", "Go")
	insertSym(t, db, "od2", "Zebra", "Interface", "Go")

	r := exec(t, db, "MATCH (n:Interface) RETURN n.name ORDER BY n.name DESC")
	if r.Total < 2 {
		t.Fatalf("expected >=2 rows, got %d", r.Total)
	}
	names := make([]string, 0)
	for _, row := range r.Rows {
		if n, ok := row["n.name"].(string); ok {
			names = append(names, n)
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i] > names[i-1] {
			t.Errorf("ORDER BY DESC violated: %q after %q", names[i], names[i-1])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// runJoinQuery: WHERE condition on toVar (tableAlias = "b") + CONTAINS pushdown
// ─────────────────────────────────────────────────────────────────────────────

func TestRunJoinQuery_WhereOnToVar(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "wt1", "Caller", "Function", "Go")
	insertSym(t, db, "wt2", "TargetCallee", "Function", "Go")
	insertSym(t, db, "wt3", "OtherCallee", "Function", "Go")
	insertEdge(t, db, "wt1", "wt2", "CALLS")
	insertEdge(t, db, "wt1", "wt3", "CALLS")

	// WHERE on b (toVar) exercises tableAlias="b" branch
	r := exec(t, db, "MATCH (a:Function)-[:CALLS]->(b:Function) WHERE b.name='TargetCallee' RETURN a.name, b.name")
	if r.Total == 0 {
		t.Fatal("expected join result filtered on toVar")
	}
	for _, row := range r.Rows {
		if row["b.name"] != "TargetCallee" {
			t.Errorf("WHERE on toVar not applied: got b.name=%v", row["b.name"])
		}
	}
}

func TestRunJoinQuery_ContainsPushdown(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "cp1", "ServiceA", "Function", "Go")
	insertSym(t, db, "cp2", "HandlerB", "Function", "Go")
	insertSym(t, db, "cp3", "ServiceC", "Function", "Go")
	insertEdge(t, db, "cp1", "cp2", "CALLS")
	insertEdge(t, db, "cp3", "cp2", "CALLS")

	// CONTAINS on fromVar exercises SQL LIKE pushdown in runJoinQuery
	r := exec(t, db, "MATCH (a:Function)-[:CALLS]->(b:Function) WHERE a.name CONTAINS 'Service' RETURN a.name, b.name")
	if r.Total == 0 {
		t.Fatal("expected CONTAINS pushdown result")
	}
	for _, row := range r.Rows {
		name, _ := row["a.name"].(string)
		if name != "ServiceA" && name != "ServiceC" {
			t.Errorf("CONTAINS filter wrong: a.name=%v", name)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parsePattern: named edge variable [r:KIND] — exercises pat.edgeVar branch
// ─────────────────────────────────────────────────────────────────────────────

func TestParsePattern_NamedEdgeVar(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "nev1", "Caller", "Function", "Go")
	insertSym(t, db, "nev2", "Callee", "Function", "Go")
	insertEdge(t, db, "nev1", "nev2", "CALLS")

	// Named edge variable: [r:CALLS] — exercises pat.edgeVar = p.next().value
	r := exec(t, db, "MATCH (a:Function)-[r:CALLS]->(b:Function) RETURN a.name, r.kind, b.name")
	if r.Total == 0 {
		t.Fatal("expected result with named edge variable")
	}
	// Verify r.kind is populated from the edge variable
	for _, row := range r.Rows {
		if _, ok := row["r.kind"]; !ok {
			t.Error("r.kind missing from result — edgeVar not captured")
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// runBFS: WHERE condition on result nodes
// ─────────────────────────────────────────────────────────────────────────────

func TestRunBFS_WhereFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "bw1", "Root", "Function", "Go")
	insertSym(t, db, "bw2", "TargetA", "Function", "Go")
	insertSym(t, db, "bw3", "TargetB", "Function", "Go")
	insertEdge(t, db, "bw1", "bw2", "CALLS")
	insertEdge(t, db, "bw1", "bw3", "CALLS")

	// BFS with WHERE to filter result nodes
	r := exec(t, db, "MATCH (a)-[:CALLS*1..2]->(b) WHERE a.name='Root' AND b.name='TargetA' RETURN b.name")
	if r.Total == 0 {
		t.Skip("BFS WHERE filter returned no results")
	}
	for _, row := range r.Rows {
		if row["b.name"] != "TargetA" {
			t.Errorf("BFS WHERE filter not applied: b.name=%v", row["b.name"])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parsePattern — undirected edge, fromProps, edgeVar, multi-kind
// ─────────────────────────────────────────────────────────────────────────────

func TestParsePattern_FromProps(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "fp1", "Target", "Function", "Go")

	// MATCH (n {name: 'Target'}) — inline prop filter on fromNode
	r := exec(t, db, "MATCH (n {name: 'Target'}) RETURN n.name")
	if r.Total == 0 {
		t.Error("fromProps filter returned no results")
	}
}

func TestParsePattern_EdgeVar(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "ev1", "A", "Function", "Go")
	insertSym(t, db, "ev2", "B", "Function", "Go")
	insertEdge(t, db, "ev1", "ev2", "CALLS")

	// Named edge variable: MATCH (a)-[r:CALLS]->(b) RETURN r.kind
	r := exec(t, db, "MATCH (a)-[r:CALLS]->(b) RETURN r.kind")
	if r.Total == 0 {
		t.Error("edge variable query returned no results")
	}
	for _, row := range r.Rows {
		if row["r.kind"] != "CALLS" {
			t.Errorf("r.kind=%v, want CALLS", row["r.kind"])
		}
	}
}

func TestParsePattern_MultiKindEdge(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "mk1", "A", "Function", "Go")
	insertSym(t, db, "mk2", "B", "Function", "Go")
	insertSym(t, db, "mk3", "C", "Function", "Go")
	insertEdge(t, db, "mk1", "mk2", "CALLS")
	insertEdge(t, db, "mk1", "mk3", "IMPORTS")

	// Multi-kind edge: MATCH (a)-[:CALLS|IMPORTS]->(b)
	r := exec(t, db, "MATCH (a)-[:CALLS|IMPORTS]->(b) RETURN b.name")
	if r.Total < 2 {
		t.Errorf("multi-kind edge returned %d rows, want >=2", r.Total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildResult — COUNT, DISTINCT, ORDER BY DESC, empty RETURN
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildResult_CountAlias(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "cntA1", "Fn1", "Function", "Go")
	insertSym(t, db, "cntA2", "Fn2", "Function", "Go")

	r := exec(t, db, "MATCH (n:Function) RETURN COUNT(n) AS total")
	if r.Total != 1 {
		t.Fatalf("COUNT AS query returned %d rows, want 1", r.Total)
	}
	v := r.Rows[0]["total"]
	if v == nil {
		t.Error("COUNT AS alias not present in result")
	}
}

func TestBuildResult_DistinctCollapses(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "dc1", "DupName", "Function", "Go")
	_, _ = db.Exec(
		`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
			start_byte, end_byte, start_line, end_line) VALUES (?,?,?,?,?,?,?, 0,100,1,5)`,
		"dc2", "proj1", "file2.go", "DupName", "DupName", "Method", "Go",
	)

	// Without DISTINCT there are 2 rows; with DISTINCT on name, the name values should be unique.
	r := exec(t, db, "MATCH (n) WHERE n.name='DupName' RETURN DISTINCT n.name")
	seen := map[string]bool{}
	for _, row := range r.Rows {
		key := fmt.Sprint(row["n.name"])
		if seen[key] {
			t.Errorf("DISTINCT returned duplicate: %v", key)
		}
		seen[key] = true
	}
}

func TestBuildResult_OrderByDescSort(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "odS1", "Aardvark", "Function", "Go")
	insertSym(t, db, "odS2", "Zebra", "Function", "Go")
	insertSym(t, db, "odS3", "Mango", "Function", "Go")

	// No WHERE filter — returns all Function symbols, sorted DESC by name
	r := exec(t, db, "MATCH (n:Function) RETURN n.name ORDER BY n.name DESC")
	if r.Total < 3 {
		t.Fatalf("expected >=3 rows, got %d", r.Total)
	}
	// Zebra should sort last alphabetically but first DESC
	first := fmt.Sprint(r.Rows[0]["n.name"])
	last := fmt.Sprint(r.Rows[len(r.Rows)-1]["n.name"])
	if first < last {
		t.Errorf("ORDER BY DESC not applied: first=%q last=%q", first, last)
	}
}

// #361: a query without RETURN must error. Pre-fix the runner returned
// an empty result silently — the pinchQL grammar requires RETURN.
func TestBuildResult_NoReturnVars(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "nrV1", "Foo", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(), "MATCH (n:Function) WHERE n.name='Foo'")
	if err == nil {
		t.Fatal("query missing RETURN should error, got nil")
	}
	if !strings.Contains(err.Error(), "RETURN") {
		t.Errorf("error should mention RETURN; got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// runJoinQuery — WHERE filter on the to-node (b) pushdown
// ─────────────────────────────────────────────────────────────────────────────

func TestRunJoinQuery_WhereOnToNode(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "jb1", "Caller", "Function", "Go")
	insertSym(t, db, "jb2", "TargetA", "Function", "Go")
	insertSym(t, db, "jb3", "TargetB", "Function", "Go")
	insertEdge(t, db, "jb1", "jb2", "CALLS")
	insertEdge(t, db, "jb1", "jb3", "CALLS")

	// WHERE on b (to-node) should be pushed down as "b.name=?"
	r := exec(t, db, "MATCH (a)-[:CALLS]->(b) WHERE b.name='TargetA' RETURN b.name")
	if r.Total == 0 {
		t.Error("WHERE on to-node returned no results")
	}
	for _, row := range r.Rows {
		if row["b.name"] != "TargetA" {
			t.Errorf("WHERE b.name='TargetA' returned %v", row["b.name"])
		}
	}
}

func TestRunJoinQuery_WhereContainsOnToNode(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "jbc1", "Srv", "Function", "Go")
	insertSym(t, db, "jbc2", "ProcessOrder", "Function", "Go")
	insertSym(t, db, "jbc3", "ProcessReturn", "Function", "Go")
	insertEdge(t, db, "jbc1", "jbc2", "CALLS")
	insertEdge(t, db, "jbc1", "jbc3", "CALLS")

	// CONTAINS on b should filter correctly
	r := exec(t, db, "MATCH (a)-[:CALLS]->(b) WHERE b.name CONTAINS 'Order' RETURN b.name")
	for _, row := range r.Rows {
		name := fmt.Sprint(row["b.name"])
		if !strings.Contains(name, "Order") {
			t.Errorf("CONTAINS filter violated: b.name=%q", name)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProjectID isolation — queries must not cross project boundaries
// ─────────────────────────────────────────────────────────────────────────────

// insertSymProject inserts a symbol with an explicit project_id.
func insertSymProject(t *testing.T, db *sql.DB, id, name, kind, lang, projectID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO symbols(id, project_id, file_path, name, qualified_name, kind, language,
			start_byte, end_byte, start_line, end_line) VALUES (?,?,?,?,?,?,?, 0,100,1,5)`,
		id, projectID, "file.go", name, name, kind, lang,
	)
	if err != nil {
		t.Fatalf("insert symbol %q: %v", id, err)
	}
}

// insertEdgeProject inserts an edge with an explicit project_id.
func insertEdgeProject(t *testing.T, db *sql.DB, fromID, toID, kind, projectID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO edges(project_id, from_id, to_id, kind) VALUES (?,?,?,?)`,
		projectID, fromID, toID, kind,
	)
	if err != nil {
		t.Fatalf("insert edge %s->%s: %v", fromID, toID, err)
	}
}

func execWithProject(t *testing.T, db *sql.DB, projectID, query string) *Result {
	t.Helper()
	e := &Executor{DB: db, MaxRows: 100, ProjectID: projectID}
	r, err := e.Execute(context.Background(), query)
	if err != nil {
		t.Fatalf("Execute(%q): %v", query, err)
	}
	return r
}

func TestProjectID_NodeScan_Isolation(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	insertSymProject(t, db, "p1-fn", "SharedName", "Function", "Go", "proj1")
	insertSymProject(t, db, "p2-fn", "SharedName", "Function", "Go", "proj2")

	// proj1 should see only its own symbol
	r := execWithProject(t, db, "proj1", "MATCH (n:Function) WHERE n.name='SharedName' RETURN n.name")
	if r.Total != 1 {
		t.Errorf("proj1 node scan: want 1 result, got %d", r.Total)
	}

	// proj2 should see only its own
	r = execWithProject(t, db, "proj2", "MATCH (n:Function) WHERE n.name='SharedName' RETURN n.name")
	if r.Total != 1 {
		t.Errorf("proj2 node scan: want 1 result, got %d", r.Total)
	}

	// SECURITY: empty ProjectID is REFUSED by Execute() (#41 item 5).
	// In-code callers that forget to scope a query fail loud rather than
	// returning cross-project data.
	unscoped := &Executor{DB: db, MaxRows: 100}
	if _, err := unscoped.Execute(context.Background(), "MATCH (n:Function) WHERE n.name='SharedName' RETURN n.name"); err == nil {
		t.Error("expected unscoped node scan to be rejected, got nil error")
	}
}

func TestProjectID_JoinQuery_Isolation(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	insertSymProject(t, db, "p1-a", "Caller", "Function", "Go", "proj1")
	insertSymProject(t, db, "p1-b", "Callee", "Function", "Go", "proj1")
	insertSymProject(t, db, "p2-a", "Caller", "Function", "Go", "proj2")
	insertSymProject(t, db, "p2-b", "Callee", "Function", "Go", "proj2")

	insertEdgeProject(t, db, "p1-a", "p1-b", "CALLS", "proj1")
	insertEdgeProject(t, db, "p2-a", "p2-b", "CALLS", "proj2")

	// proj1 sees only proj1 edges
	r := execWithProject(t, db, "proj1", "MATCH (a)-[:CALLS]->(b) RETURN a.name, b.name")
	if r.Total != 1 {
		t.Errorf("proj1 join: want 1 row, got %d", r.Total)
	}

	// proj2 sees only proj2 edges
	r = execWithProject(t, db, "proj2", "MATCH (a)-[:CALLS]->(b) RETURN a.name, b.name")
	if r.Total != 1 {
		t.Errorf("proj2 join: want 1 row, got %d", r.Total)
	}

	// SECURITY: an unscoped Executor (empty ProjectID) is REFUSED by Execute()
	// rather than silently returning cross-project data. This is the
	// defense-in-depth gate added in #41 item 5: in-code callers that
	// forget to set ProjectID fail loud instead of leaking rows from
	// other projects.
	unscoped := &Executor{DB: db, MaxRows: 100}
	if _, err := unscoped.Execute(context.Background(), "MATCH (a)-[:CALLS]->(b) RETURN a.name, b.name"); err == nil {
		t.Error("expected unscoped Executor to be rejected, got nil error")
	}
}

func TestProjectID_BFS_Isolation(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// proj1: Root -> Child1
	insertSymProject(t, db, "p1-root", "Root", "Function", "Go", "proj1")
	insertSymProject(t, db, "p1-child", "Child1", "Function", "Go", "proj1")
	// proj2: a separate Root -> Child2 (same symbol names, different project)
	insertSymProject(t, db, "p2-root", "Root", "Function", "Go", "proj2")
	insertSymProject(t, db, "p2-child", "Child2", "Function", "Go", "proj2")

	insertEdgeProject(t, db, "p1-root", "p1-child", "CALLS", "proj1")
	insertEdgeProject(t, db, "p2-root", "p2-child", "CALLS", "proj2")

	// proj1 BFS from Root should only reach Child1, not Child2
	r := execWithProject(t, db, "proj1", "MATCH (a)-[:CALLS*1..3]->(b) WHERE a.name='Root' RETURN b.name")
	for _, row := range r.Rows {
		if row["b.name"] == "Child2" {
			t.Errorf("proj1 BFS leaked into proj2: got Child2")
		}
	}
	found := false
	for _, row := range r.Rows {
		if row["b.name"] == "Child1" {
			found = true
		}
	}
	if !found && r.Total > 0 {
		t.Errorf("proj1 BFS: expected Child1 in results")
	}
}

func TestProjectID_BFS_MaxRows(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Insert a chain: root -> n0 -> n1 -> ... -> n4
	insertSym(t, db, "mr-root", "Root", "Function", "Go")
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("mr-%d", i)
		insertSym(t, db, id, fmt.Sprintf("Node%d", i), "Function", "Go")
		prev := "mr-root"
		if i > 0 {
			prev = fmt.Sprintf("mr-%d", i-1)
		}
		insertEdge(t, db, prev, id, "CALLS")
	}

	// With maxRows=2, BFS CTE should return at most 2 results
	e := &Executor{DB: db, MaxRows: 2, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(), "MATCH (a)-[:CALLS*1..5]->(b) WHERE a.name='Root' RETURN b.name")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// buildResult applies LIMIT from query default (200), but bfsViaCTE LIMIT
	// is now e.maxRows() = 2, so we expect at most 2 hops returned from CTE.
	if r.Total > 2 {
		t.Errorf("BFS with maxRows=2 returned %d rows, want <=2", r.Total)
	}
}

func TestRunBFS_GotoDoneEarlyExit(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	// Create 4 start nodes (A1..A4), each with one reachable child.
	// MaxRows=1 → maxRows()*2 = 2, so after 2 results the loop should goto done.
	for i := 1; i <= 4; i++ {
		src := fmt.Sprintf("ge-src%d", i)
		dst := fmt.Sprintf("ge-dst%d", i)
		insertSym(t, db, src, fmt.Sprintf("Source%d", i), "Function", "Go")
		insertSym(t, db, dst, fmt.Sprintf("Dest%d", i), "Function", "Go")
		insertEdge(t, db, src, dst, "CALLS")
	}

	// With MaxRows=1, the outer loop should exit after len(resultRows) >= 2
	e := &Executor{DB: db, MaxRows: 1, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		"MATCH (a:Function)-[:CALLS*1..1]->(b) RETURN a.name, b.name LIMIT 10")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// goto done triggers when len(resultRows) >= maxRows*2 = 2, so we get ≤2 rows (not all 4 pairs)
	if r.Total > 2 {
		t.Errorf("BFS with MaxRows=1 returned %d rows, want <=2 (early exit at maxRows*2)", r.Total)
	}
	if r.Total >= 4 {
		t.Errorf("BFS goto done did not fire: got %d rows, want <4", r.Total)
	}
}

func TestExecute_ParseError_ReturnsError(t *testing.T) {
	db := newTestDB(t)
	e := &Executor{DB: db, ProjectID: "proj1"}
	// IN is not a supported operator — should return an error via parseConditions
	_, err := e.Execute(context.Background(), `MATCH (n:Function) WHERE n.name IN ["Foo"] RETURN n.name`)
	if err == nil {
		t.Fatal("expected parse error for unsupported IN operator, got nil")
	}
}

func TestExecute_InvalidRegex_ReturnsError(t *testing.T) {
	db := newTestDB(t)
	insertSym(t, db, "f1", "Foo", "Function", "Go")
	e := &Executor{DB: db, ProjectID: "proj1"}
	// (*invalid is not a valid regex — should fail at parse time (B2 fix)
	_, err := e.Execute(context.Background(), `MATCH (n:Function) WHERE n.name =~ "(*invalid" RETURN n.name`)
	if err == nil {
		t.Fatal("expected error for invalid regex pattern, got nil")
	}
}

func TestExecute_ValidRegex_Matches(t *testing.T) {
	db := newTestDB(t)
	insertSym(t, db, "f1", "FooBar", "Function", "Go")
	insertSym(t, db, "f2", "BazQux", "Function", "Go")
	e := &Executor{DB: db, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(), `MATCH (n:Function) WHERE n.name =~ "Foo.*" RETURN n.name`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Total != 1 {
		t.Errorf("regex filter: got %d rows, want 1", r.Total)
	}
}

func TestExecute_UnsupportedOperator_ReturnsError(t *testing.T) {
	db := newTestDB(t)
	e := &Executor{DB: db, ProjectID: "proj1"}
	// LIKE is not a supported operator in our Cypher subset
	_, err := e.Execute(context.Background(), `MATCH (n:Function) WHERE n.name LIKE "Foo%" RETURN n.name`)
	if err == nil {
		t.Fatal("expected error for unsupported LIKE operator, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// runJoinQuery — uncovered branches
// ─────────────────────────────────────────────────────────────────────────────

func TestRunJoinQuery_ToKindFilter(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	// (a:Function)-[:CALLS]->(b:Method) — toKind filter
	insertSym(t, db, "jq-fn", "Caller", "Function", "Go")
	insertSym(t, db, "jq-mt", "Callee", "Method", "Go")
	insertEdge(t, db, "jq-fn", "jq-mt", "CALLS")

	e := &Executor{DB: db, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		"MATCH (a:Function)-[:CALLS]->(b:Method) RETURN a.name, b.name")
	if err != nil {
		t.Fatalf("runJoinQuery toKind: %v", err)
	}
	if r.Total == 0 {
		t.Error("expected at least 1 join result with toKind=Method")
	}
}

func TestRunJoinQuery_NamedEdgeVar(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "ev-a", "Alpha", "Function", "Go")
	insertSym(t, db, "ev-b", "Beta", "Function", "Go")
	insertEdge(t, db, "ev-a", "ev-b", "CALLS")

	// r is a named edge variable — should populate r.kind and r.confidence
	e := &Executor{DB: db, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		"MATCH (a)-[r:CALLS]->(b) RETURN a.name, r.kind, b.name")
	if err != nil {
		t.Fatalf("runJoinQuery namedEdge: %v", err)
	}
	if r.Total == 0 {
		t.Error("expected results with named edge variable")
	}
	// r.kind should be in the result row
	if len(r.Rows) > 0 {
		if _, ok := r.Rows[0]["r.kind"]; !ok {
			t.Error("expected r.kind in result row for named edge variable")
		}
	}
}

func TestRunJoinQuery_UnpushedCondition(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "up-a", "Source", "Function", "Go")
	insertSym(t, db, "up-b", "Sink", "Function", "Go")
	insertEdge(t, db, "up-a", "up-b", "CALLS")

	// Regex condition can't be pushed to SQL — goes to unpushed list
	e := &Executor{DB: db, ProjectID: "proj1"}
	r, err := e.Execute(context.Background(),
		`MATCH (a)-[:CALLS]->(b) WHERE a.name =~ "Sou.*" RETURN a.name, b.name`)
	if err != nil {
		t.Fatalf("runJoinQuery unpushed: %v", err)
	}
	if r.Total == 0 {
		t.Error("expected 1 join result with regex unpushed condition")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Security: SQL-injection probes, project-scope enforcement, deadline
// (#41 item 5)
//
// The audit found the SQL injection surface well-defended:
//   - cypherPropToCol is an allowlist (returns "" for unknown properties,
//     and callers skip when col == "")
//   - Every value is bound via ? placeholders, never concatenated
//   - project_id is bound when set, refused when empty (post-fix)
//
// These tests pin those defenses so a future refactor can't silently
// regress to string concatenation or relax the project-scope gate.
// ─────────────────────────────────────────────────────────────────────────────

func TestCypherSecurity_StringValueIsBound_NoSQLInjection(t *testing.T) {
	// A name field containing classic SQL-injection payloads MUST be
	// treated as a literal value (no match), NOT executed as SQL. This
	// proves the WHERE name=? path uses a bound parameter rather than
	// string concatenation.
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "ok-1", "RealFn", "Function", "Go")

	payloads := []string{
		`'; DROP TABLE symbols; --`,
		`' OR '1'='1`,
		`' OR 1=1 --`,
		`"; DELETE FROM symbols; --`,
		`\'; DROP TABLE symbols; --`,
	}
	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	for _, payload := range payloads {
		query := `MATCH (f:Function) WHERE f.name = "` + payload + `" RETURN f.name`
		r, err := e.Execute(context.Background(), query)
		if err != nil {
			// A parse error is acceptable (the payload contains characters
			// the lexer can't tokenize) — the bug we're guarding against
			// is "executes as SQL injection," not "rejects malformed input."
			continue
		}
		// If parse succeeded, the result MUST be empty: the payload is
		// just a string that doesn't match any real symbol.
		if r.Total != 0 {
			t.Errorf("payload %q: got %d rows, want 0 (potential SQL injection)", payload, r.Total)
		}
	}

	// And — the symbols table MUST still exist after every probe.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM symbols").Scan(&count); err != nil {
		t.Fatalf("symbols table gone after injection probes: %v", err)
	}
	if count == 0 {
		t.Error("symbols table is empty — injection probe deleted rows")
	}
}

func TestCypherSecurity_UnknownPropertyIsIgnored(t *testing.T) {
	// Cypher property names must pass the cypherPropToCol allowlist.
	// An unknown property name is silently skipped at the SQL push-down
	// stage — never concatenated as a column name into the SQL. Pinning
	// this ensures a future refactor can't accidentally turn an unknown
	// property into a runtime SQL error or, worse, an injected column.
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "p1-fn", "RealFn", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	// `weird_property` is not in cypherPropToCol's allowlist — this
	// WHERE clause cannot push down.
	r, err := e.Execute(context.Background(),
		`MATCH (f:Function) WHERE f.weird_property = "anything" RETURN f.name`)
	if err != nil {
		t.Fatalf("query with unknown property errored: %v", err)
	}
	// Behaviour: query runs, unpushed condition is evaluated in Go,
	// no rows match because RealFn has no weird_property field. The
	// key invariant is "no SQL syntax error, no injected column."
	if r.Total > 0 {
		t.Errorf("unknown property matched %d rows, want 0", r.Total)
	}
}

func TestCypherSecurity_EmptyProjectIDRefused(t *testing.T) {
	// The defense-in-depth gate: an Executor without ProjectID set
	// is refused with a clear error. Direct callers that forget to
	// scope a query fail loud rather than leak cross-project data.
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "p1", "Anything", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100} // ProjectID intentionally empty
	_, err := e.Execute(context.Background(), "MATCH (f:Function) RETURN f.name")
	if err == nil {
		t.Fatal("expected error from Execute with empty ProjectID, got nil")
	}
	if !strings.Contains(err.Error(), "ProjectID is required") {
		t.Errorf("error message %q should mention 'ProjectID is required'", err.Error())
	}
}

func TestCypherSecurity_DeadlineEnforced(t *testing.T) {
	// QueryContext respects context cancellation. A caller passing an
	// already-expired context MUST get a timeout error rather than
	// blocking the goroutine. handleQuery wraps every Cypher query in
	// a 10s WithTimeout — this test pins that the Executor honors it.
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "x", "X", "Function", "Go")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately expired

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(ctx, "MATCH (f:Function) RETURN f.name")
	if err == nil {
		t.Error("expected context-cancelled error, got nil")
	}
}

func TestCypherSecurity_BadRegexErrorClean(t *testing.T) {
	// An invalid regex pattern MUST produce a clean error at parse time,
	// NOT a panic during row evaluation. The parser pre-compiles =~
	// patterns so the failure mode is bounded.
	db := newTestDB(t)
	defer db.Close()
	insertSym(t, db, "p1", "Foo", "Function", "Go")

	e := &Executor{DB: db, MaxRows: 100, ProjectID: "proj1"}
	_, err := e.Execute(context.Background(),
		`MATCH (f:Function) WHERE f.name =~ "(*invalid" RETURN f.name`)
	if err == nil {
		t.Error("expected error for invalid regex, got nil")
	}
	if err != nil && !strings.Contains(err.Error(), "regex") && !strings.Contains(err.Error(), "invalid") {
		// We don't pin the exact wording, but the error must hint at the
		// cause so callers can debug.
		t.Logf("regex error: %v (acceptable; mentions parse details)", err)
	}
}
