// Package cypher implements pinchQL — pincher's lightweight graph
// query language. The package name is "cypher" for git-blame
// continuity (the language was originally documented as "Cypher-like");
// the user-facing name is "pinchQL" since #206. The grammar is
// Cypher-shaped but deliberately a pragmatic subset, not a moving
// target trying to track Neo4j.
//
// Supported pinchQL subset:
//
//	MATCH (n:Kind) RETURN n.name
//	MATCH (n:Kind) WHERE n.name = 'x' RETURN n.name, n.file_path
//	MATCH (a:Function)-[:CALLS]->(b:Function) RETURN a.name, b.name LIMIT 20
//	MATCH (a)-[:CALLS*1..3]->(b) WHERE a.name = 'main' RETURN b.name
//	MATCH (a)-[:CALLS]->(b) WHERE a.name =~ '.*Handler.*' RETURN b.name
//	MATCH (a)-[:CALLS]->(b) ORDER BY a.name LIMIT 10
//
// The engine translates pinchQL patterns to SQLite queries against
// the pincherMCP schema (symbols + edges tables). Single-hop patterns
// are fused into a single JOIN for sub-millisecond execution.
package cypher

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Result holds the tabular output of a Cypher query.
type Result struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Total   int              `json:"total"`
}

// Executor runs Cypher queries against a SQLite database.
type Executor struct {
	DB        *sql.DB
	MaxRows   int    // 0 = default (200)
	ProjectID string // if set, all queries are scoped to this project
}

// Execute parses and executes a Cypher query.
// Execute parses and executes a Cypher query.
//
// SECURITY: rejects empty ProjectID. The runNodeScan / runJoinQuery / runBFS
// paths only append `project_id=?` to the SQL when ProjectID is non-empty,
// so a caller forgetting to set it would get cross-project results.
// Refusing here is defense-in-depth — handleQuery (the MCP entrypoint)
// already enforces a non-empty project via mustProject, but in-code
// callers might construct an Executor directly. The constraint is
// announced explicitly at the boundary so misuse fails loudly instead
// of silently leaking cross-project data.
func (e *Executor) Execute(ctx context.Context, query string) (*Result, error) {
	if e.ProjectID == "" {
		return nil, fmt.Errorf("cypher: ProjectID is required (refusing to run cross-project query)")
	}
	q, err := parse(query)
	if err != nil {
		return nil, fmt.Errorf("cypher parse: %w", err)
	}
	return e.run(ctx, q)
}

// symCols is the canonical SELECT column list for the symbols table.
// Keep in sync with db.symSelectFrom and cypher.symRow.
const symCols = `id, project_id, file_path, name, qualified_name, kind, language,
	start_byte, end_byte, start_line, end_line, is_exported, is_entry_point, complexity,
	extraction_confidence`

// inPlaceholders returns a comma-separated "?,?,..." string for n items.
func inPlaceholders(n int) string {
	s := strings.Repeat("?,", n)
	return s[:len(s)-1]
}

// ─────────────────────────────────────────────────────────────────────────────
// Query AST
// ─────────────────────────────────────────────────────────────────────────────

type queryAST struct {
	patterns   []pattern   // MATCH clauses
	conditions []condition // WHERE clauses
	returnVars []returnVar // RETURN items
	orderBy    string
	orderDir   string // ASC | DESC
	limit      int
	distinct   bool
}

type pattern struct {
	fromVar   string
	fromKind  string
	fromProps map[string]string
	edgeVar   string
	edgeKinds []string
	minHops   int
	maxHops   int
	toVar     string
	toKind    string
	toProps   map[string]string
	directed  bool // -> vs -
}

type condition struct {
	variable  string
	property  string
	op        string // = <> > < >= <= =~ CONTAINS STARTS_WITH ENDS_WITH IS_NULL IS_NOT_NULL
	value     string
	negated   bool   // #354: WHERE NOT n.x = ... — invert the comparison result
	connector string // #358: "AND" or "OR" — connects this condition to the running result. First condition is "" (start).
}

type returnVar struct {
	variable string
	property string // "" = return whole node
	alias    string
	fn       string // COUNT | ""
}

// ─────────────────────────────────────────────────────────────────────────────
// Parser
// ─────────────────────────────────────────────────────────────────────────────

func parse(query string) (*queryAST, error) {
	p := &parser{tokens: tokenize(query), pos: 0}
	return p.parseQuery()
}

type parser struct {
	tokens []token
	pos    int
}

type token struct {
	kind  string // KEYWORD IDENT NUMBER STRING PUNCT
	value string
}

func tokenize(s string) []token {
	var tokens []token
	i := 0
	for i < len(s) {
		c := s[i]
		// Skip whitespace
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		// String literal
		if c == '\'' || c == '"' {
			j := i + 1
			for j < len(s) && s[j] != c {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			tokens = append(tokens, token{kind: "STRING", value: s[i+1 : j]})
			i = j + 1
			continue
		}
		// Punctuation: ( ) [ ] { } : , . * + - > < = ! ~ |
		punct := "()[]{}:,.*+-><=!~|"
		if strings.ContainsRune(punct, rune(c)) {
			// Handle multi-char operators: ->, <>, >=, <=, =~, *n..m
			if c == '-' && i+1 < len(s) && s[i+1] == '>' {
				tokens = append(tokens, token{kind: "PUNCT", value: "->"})
				i += 2
				continue
			}
			if (c == '<' && i+1 < len(s) && s[i+1] == '>') ||
				(c == '>' && i+1 < len(s) && s[i+1] == '=') ||
				(c == '<' && i+1 < len(s) && s[i+1] == '=') ||
				(c == '=' && i+1 < len(s) && s[i+1] == '~') {
				tokens = append(tokens, token{kind: "PUNCT", value: string(s[i : i+2])})
				i += 2
				continue
			}
			// Variable-length: *1..3
			if c == '*' {
				j := i + 1
				hops := ""
				for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
					hops += string(s[j])
					j++
				}
				tokens = append(tokens, token{kind: "HOPS", value: hops})
				i = j
				continue
			}
			tokens = append(tokens, token{kind: "PUNCT", value: string(c)})
			i++
			continue
		}
		// Number
		if c >= '0' && c <= '9' {
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			tokens = append(tokens, token{kind: "NUMBER", value: s[i:j]})
			i = j
			continue
		}
		// Identifier or keyword
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			j := i
			for j < len(s) && ((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z') ||
				(s[j] >= '0' && s[j] <= '9') || s[j] == '_') {
				j++
			}
			word := s[i:j]
			kind := "IDENT"
			upper := strings.ToUpper(word)
			switch upper {
			case "MATCH", "WHERE", "RETURN", "ORDER", "BY", "LIMIT", "DISTINCT",
				"AND", "OR", "NOT", "CONTAINS", "STARTS", "ENDS", "WITH", "ASC", "DESC",
				"COUNT", "IN", "IS", "NULL", "TRUE", "FALSE":
				kind = "KEYWORD"
				word = upper
			}
			tokens = append(tokens, token{kind: kind, value: word})
			i = j
			continue
		}
		i++ // skip unknown
	}
	return tokens
}

func (p *parser) peek() token {
	if p.pos >= len(p.tokens) {
		return token{kind: "EOF"}
	}
	return p.tokens[p.pos]
}

func (p *parser) next() token {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) skip(val string) {
	if p.peek().value == val {
		p.pos++
	}
}

func (p *parser) parseQuery() (*queryAST, error) {
	q := &queryAST{limit: 200}

	for p.pos < len(p.tokens) {
		t := p.peek()
		switch t.value {
		case "MATCH":
			p.next()
			pat, err := p.parsePattern()
			if err != nil {
				return nil, err
			}
			q.patterns = append(q.patterns, pat)

		case "WHERE":
			p.next()
			conds, err := p.parseConditions()
			if err != nil {
				return nil, err
			}
			q.conditions = append(q.conditions, conds...)

		case "RETURN":
			p.next()
			if p.peek().value == "DISTINCT" {
				q.distinct = true
				p.next()
			}
			vars, err := p.parseReturn()
			if err != nil {
				return nil, err
			}
			q.returnVars = vars

		case "ORDER":
			p.next()
			p.skip("BY")
			q.orderBy = p.next().value
			if p.peek().value == "." {
				p.next()
				q.orderBy += "." + p.next().value
			}
			if p.peek().value == "DESC" {
				q.orderDir = "DESC"
				p.next()
			} else if p.peek().value == "ASC" {
				p.next()
			}

		case "LIMIT":
			p.next()
			if p.peek().kind == "NUMBER" {
				n, _ := strconv.Atoi(p.next().value)
				q.limit = n
			}

		default:
			p.next() // skip unknown tokens
		}
	}
	return q, nil
}

func (p *parser) parsePattern() (pattern, error) {
	pat := pattern{minHops: 1, maxHops: 1, directed: true}

	// (fromVar:FromKind {prop:val})
	p.skip("(")
	pat.fromVar = p.next().value
	if p.peek().value == ":" {
		p.next()
		pat.fromKind = p.next().value
	}
	if p.peek().value == "{" {
		pat.fromProps = p.parseProps()
	}
	p.skip(")")

	// Optional edge: -[r:KIND]-> or -[:KIND*1..3]->
	if p.peek().value == "-" || p.peek().value == "<" {
		p.next() // consume -
		if p.peek().value == "[" {
			p.next()
			if p.peek().kind == "IDENT" {
				pat.edgeVar = p.next().value
			}
			if p.peek().value == ":" {
				p.next()
				for p.peek().kind == "IDENT" {
					pat.edgeKinds = append(pat.edgeKinds, p.next().value)
					if p.peek().value == "|" {
						p.next()
					} else {
						break
					}
				}
			}
			if p.peek().kind == "HOPS" {
				t := p.next()
				pat.minHops, pat.maxHops = parseHops(t.value)
			}
			p.skip("]")
		}
		// consume -> (tokenizer emits it as a two-char token after "]")
		switch p.peek().value {
		case "->":
			p.next()
			pat.directed = true
		case "-":
			p.next()
			if p.peek().value == ">" {
				p.next()
				pat.directed = true
			}
		}

		// (toVar:ToKind)
		if p.peek().value == "(" {
			p.next()
			pat.toVar = p.next().value
			if p.peek().value == ":" {
				p.next()
				pat.toKind = p.next().value
			}
			if p.peek().value == "{" {
				pat.toProps = p.parseProps()
			}
			p.skip(")")
		}
	}
	return pat, nil
}

func (p *parser) parseProps() map[string]string {
	props := make(map[string]string)
	p.skip("{")
	for p.peek().value != "}" && p.peek().kind != "EOF" {
		key := p.next().value
		p.skip(":")
		val := p.next().value
		props[key] = val
		p.skip(",")
	}
	p.skip("}")
	return props
}

func (p *parser) parseConditions() ([]condition, error) {
	var conds []condition
	// Pending connector stamped on the next-parsed condition. First
	// condition has no connector (it's the start of the chain); each
	// subsequent gets the AND/OR token that preceded it. Pre-fix this
	// connector was discarded, silently converting every OR to AND (#358).
	pendingConnector := ""
	for {
		c, err := p.parseOneCondition()
		if err != nil {
			return nil, err
		}
		c.connector = pendingConnector
		conds = append(conds, c)
		next := p.peek().value
		if next != "AND" && next != "OR" {
			break
		}
		p.next() // consume AND/OR
		pendingConnector = next
	}
	return conds, nil
}

func (p *parser) parseOneCondition() (condition, error) {
	c := condition{}

	// #354: WHERE NOT n.x = ... — consume NOT prefix and flag the
	// condition for negation. Single-condition prefix only; compound
	// `NOT (a OR b)` requires paren-aware parsing that's out of scope.
	if p.peek().value == "NOT" {
		p.next()
		c.negated = true
	}

	varTok := p.next()
	c.variable = varTok.value

	if p.peek().value == "." {
		p.next()
		c.property = p.next().value
	}

	// Operator
	switch p.peek().value {
	case "=", "<>", ">", "<", ">=", "<=":
		c.op = p.next().value
		c.value = normalizeConditionValue(p.next())
	case "=~":
		c.op = p.next().value
		c.value = normalizeConditionValue(p.next())
		if _, err := regexp.Compile(c.value); err != nil {
			return c, fmt.Errorf("invalid regex pattern %q: %w", c.value, err)
		}
	case "CONTAINS":
		p.next()
		c.op = "CONTAINS"
		c.value = normalizeConditionValue(p.next())
	case "STARTS":
		p.next()
		p.skip("WITH")
		c.op = "STARTS WITH"
		c.value = normalizeConditionValue(p.next())
	case "ENDS":
		// #340: ENDS WITH as a first-class operator, symmetric to
		// STARTS WITH (#288). Same two-token shape — consume "WITH"
		// then the value literal.
		p.next()
		p.skip("WITH")
		c.op = "ENDS WITH"
		c.value = normalizeConditionValue(p.next())
	case "IS":
		// #342: IS NULL / IS NOT NULL. Common Cypher pattern for
		// finding rows with empty/absent properties (e.g. functions
		// without docstrings). No value literal — the operator IS
		// the predicate.
		p.next() // consume IS
		if p.peek().value == "NOT" {
			p.next() // consume NOT
			p.skip("NULL")
			c.op = "IS NOT NULL"
		} else {
			p.skip("NULL")
			c.op = "IS NULL"
		}
		c.value = ""
	case "!":
		// Detect `!=` (two-char op the tokenizer doesn't fuse) so the hint
		// catches the SQL-muscle-memory case before the generic fallback.
		if p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].value == "=" {
			return c, fmt.Errorf("unsupported operator: != — use <> ('name <> \"foo\"')")
		}
		return c, fmt.Errorf("unsupported operator: %s", p.peek().value)
	default:
		op := p.peek().value
		if hint, ok := operatorHint(op); ok {
			return c, fmt.Errorf("unsupported operator: %s — %s", op, hint)
		}
		return c, fmt.Errorf("unsupported operator: %s", op)
	}
	return c, nil
}

func (p *parser) parseReturn() ([]returnVar, error) {
	var vars []returnVar
	for {
		rv := returnVar{}
		t := p.peek()

		// COUNT(var)
		if t.value == "COUNT" {
			p.next()
			p.skip("(")
			rv.fn = "COUNT"
			rv.variable = p.next().value
			p.skip(")")
		} else {
			rv.variable = p.next().value
			if p.peek().value == "." {
				p.next()
				rv.property = p.next().value
			}
		}

		// AS alias
		if p.peek().value == "AS" || p.peek().kind == "KEYWORD" && p.peek().value == "AS" {
			p.next()
			rv.alias = p.next().value
		}

		vars = append(vars, rv)

		if p.peek().value != "," {
			break
		}
		p.next()

		// Stop at clause keywords
		next := p.peek().value
		if next == "WHERE" || next == "ORDER" || next == "LIMIT" || next == "MATCH" {
			break
		}
	}
	return vars, nil
}

// normalizeConditionValue lowercases the token value for boolean and
// null keywords so equality compares correctly against Go-formatted
// row values (#323). The tokenizer uppercases all keywords for parser
// convenience (so MATCH/WHERE/RETURN can be matched without case
// folding), but TRUE/FALSE/NULL are *literal values* — when they
// flow through to matchesConditions they're compared against
// `fmt.Sprint(boolValue)` which Go formats as lowercase. Without
// this normalisation, `WHERE n.is_exported = true` always returned
// 0 rows because `"true" != "TRUE"`.
//
// Non-keyword tokens (strings, identifiers, numbers) pass through
// unchanged — only the three known boolean/null literals get the
// case-fold.
func normalizeConditionValue(tok token) string {
	if tok.kind == "KEYWORD" {
		switch tok.value {
		case "TRUE", "FALSE", "NULL":
			return strings.ToLower(tok.value)
		}
	}
	return tok.value
}

// operatorHint maps common-mistake operator tokens to a one-line nudge
// toward the supported pinchQL spelling. Returns ("", false) when the
// token doesn't have a known suggestion (the caller falls back to the
// bare "unsupported operator" message).
func operatorHint(op string) (string, bool) {
	switch strings.ToUpper(op) {
	case "LIKE":
		return "use CONTAINS for substring (CONTAINS 'foo'), or STARTS WITH for prefix", true
	case "REGEXP", "RLIKE":
		return "use =~ ('name =~ \".*foo.*\"')", true
	case "STARTS_WITH":
		return "use STARTS WITH (two words, no underscore)", true
	// ENDS WITH is now a first-class operator (#340) — no hint needed.
	case "MATCHES":
		return "use =~ for regex match", true
	case "IN":
		// #321: IN multi-value membership isn't implemented yet —
		// the OR-of-equalities pattern is the documented workaround.
		return "IN is not supported; combine equality conditions with OR: 'n.kind = \"Function\" OR n.kind = \"Method\"'", true
	}
	return "", false
}

func parseHops(s string) (min, max int) {
	min, max = 1, 1
	if s == "" {
		return
	}
	parts := strings.Split(s, "..")
	if len(parts) == 2 {
		min, _ = strconv.Atoi(parts[0])
		max, _ = strconv.Atoi(parts[1])
	} else if len(parts) == 1 && parts[0] != "" {
		n, _ := strconv.Atoi(parts[0])
		min, max = n, n
	}
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	return
}

// ─────────────────────────────────────────────────────────────────────────────
// Executor: Cypher → SQL
// ─────────────────────────────────────────────────────────────────────────────

type symRow struct {
	ID                   string
	ProjectID            string
	FilePath             string
	Name                 string
	QualifiedName        string
	Kind                 string
	Language             string
	StartByte            int
	EndByte              int
	StartLine            int
	EndLine              int
	IsExported           bool
	IsEntryPoint         bool
	Complexity           int
	ExtractionConfidence float64
}

func (e *Executor) maxRows() int {
	if e.MaxRows <= 0 {
		return 200
	}
	if e.MaxRows > 10000 {
		return 10000
	}
	return e.MaxRows
}

func (e *Executor) run(ctx context.Context, q *queryAST) (*Result, error) {
	if len(q.patterns) == 0 {
		return &Result{Columns: []string{}, Rows: []map[string]any{}}, nil
	}

	pat := q.patterns[0]

	// Node-only query (no edge)
	if pat.toVar == "" {
		return e.runNodeScan(ctx, q, pat)
	}

	// Single-hop edge query — fuse into JOIN
	if pat.minHops == 1 && pat.maxHops == 1 {
		return e.runJoinQuery(ctx, q, pat)
	}

	// Variable-length — use BFS
	return e.runBFS(ctx, q, pat)
}

// runNodeScan handles: MATCH (n:Kind) WHERE ... RETURN ...
func (e *Executor) runNodeScan(ctx context.Context, q *queryAST, pat pattern) (*Result, error) {
	sqlQ := "SELECT " + symCols + " FROM symbols WHERE 1=1"
	var args []any

	if e.ProjectID != "" {
		sqlQ += " AND project_id=?"
		args = append(args, e.ProjectID)
	}
	if pat.fromKind != "" {
		sqlQ += " AND kind=?"
		args = append(args, pat.fromKind)
	}
	for k, v := range pat.fromProps {
		col := cypherPropToCol(k)
		if col != "" {
			sqlQ += " AND " + col + "=?"
			args = append(args, v)
		}
	}

	// Push down simple WHERE conditions
	var unpushed []condition
	// #358: SQL pushdown emits AND-joined WHERE clauses; if any condition
	// has an OR connector, all conditions must go through Go evaluation
	// (which handles boolean composition correctly). Otherwise an OR
	// condition would silently push to SQL as AND, recreating the bug.
	hasOr := conditionsHaveOr(q.conditions)
	for _, c := range q.conditions {
		if c.variable != pat.fromVar {
			continue
		}
		col := cypherPropToCol(c.property)
		if !hasOr && col != "" && (c.op == "=" || c.op == "CONTAINS" || c.op == "STARTS WITH" || c.op == "ENDS WITH" || c.op == "IS NULL" || c.op == "IS NOT NULL") {
			appendWhereOp(&sqlQ, &args, "", col, c)
		} else {
			unpushed = append(unpushed, c)
		}
	}

	// #308: skip the SQL LIMIT when the query is aggregating
	// (COUNT projection). The pre-fix path clamped the row scan to
	// `max_rows * 2` even for COUNT queries, so `RETURN COUNT(n)`
	// silently returned the clamp instead of the cardinality.
	// Non-aggregating queries keep the safety clamp so a runaway
	// query can't drag the entire symbols table into memory.
	if !hasAggregation(q) {
		sqlQ += " LIMIT ?"
		args = append(args, e.maxRows()*2)
	}

	rows, err := e.DB.QueryContext(ctx, sqlQ, args...)
	if err != nil {
		return nil, fmt.Errorf("node scan: %w", err)
	}
	defer rows.Close()

	reCache := make(map[string]*regexp.Regexp)
	var nodes []map[string]any
	for rows.Next() {
		n, err := scanSymRow(rows)
		if err != nil {
			return nil, err
		}
		m := symRowToMap(pat.fromVar, n)
		// Apply unpushed conditions in Go
		if !matchesConditionsWithCache(m, unpushed, reCache) {
			continue
		}
		nodes = append(nodes, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return buildResult(nodes, q)
}

// conditionsHaveOr reports whether any condition is OR-joined to the
// previous one. SQL pushdown emits AND-joined WHERE clauses; if any
// connector is OR, all conditions must be evaluated in Go to honor
// boolean composition (#358).
func conditionsHaveOr(conds []condition) bool {
	for _, c := range conds {
		if c.connector == "OR" {
			return true
		}
	}
	return false
}

// hasAggregation reports whether any RETURN variable in q is an
// aggregation (currently only COUNT). Aggregating queries must scan
// the full match set so the COUNT reflects cardinality, not the
// safety LIMIT (#308).
func hasAggregation(q *queryAST) bool {
	for _, rv := range q.returnVars {
		if rv.fn == "COUNT" {
			return true
		}
	}
	return false
}

// runJoinQuery handles: MATCH (a:Kind)-[:EDGE]->(b:Kind) WHERE ... RETURN ...
// This is the performance-critical hot path — fused into one SQL JOIN.
func (e *Executor) runJoinQuery(ctx context.Context, q *queryAST, pat pattern) (*Result, error) {
	// Build edge type filter
	edgeFilter := ""
	var edgeArgs []any
	if len(pat.edgeKinds) > 0 {
		edgeFilter = " AND e.kind IN (" + inPlaceholders(len(pat.edgeKinds)) + ")"
		for _, k := range pat.edgeKinds {
			edgeArgs = append(edgeArgs, k)
		}
	}

	sqlQ := `SELECT
		a.id, a.project_id, a.file_path, a.name, a.qualified_name, a.kind, a.language,
		a.start_byte, a.end_byte, a.start_line, a.end_line, a.is_exported, a.is_entry_point, a.complexity,
		a.extraction_confidence,
		b.id, b.project_id, b.file_path, b.name, b.qualified_name, b.kind, b.language,
		b.start_byte, b.end_byte, b.start_line, b.end_line, b.is_exported, b.is_entry_point, b.complexity,
		b.extraction_confidence,
		e.kind, e.confidence
		FROM edges e
		JOIN symbols a ON a.id = e.from_id
		JOIN symbols b ON b.id = e.to_id
		WHERE 1=1` + edgeFilter

	var args []any
	args = append(args, edgeArgs...)

	if e.ProjectID != "" {
		sqlQ += " AND a.project_id=?"
		args = append(args, e.ProjectID)
	}
	if pat.fromKind != "" {
		sqlQ += " AND a.kind=?"
		args = append(args, pat.fromKind)
	}
	if pat.toKind != "" {
		sqlQ += " AND b.kind=?"
		args = append(args, pat.toKind)
	}

	// Push down WHERE conditions
	var unpushed []condition
	hasOr := conditionsHaveOr(q.conditions)
	for _, c := range q.conditions {
		tableAlias := "a"
		if c.variable == pat.toVar {
			tableAlias = "b"
		} else if c.variable != pat.fromVar {
			unpushed = append(unpushed, c)
			continue
		}
		col := cypherPropToCol(c.property)
		if !hasOr && col != "" && (c.op == "=" || c.op == "CONTAINS" || c.op == "STARTS WITH" || c.op == "ENDS WITH" || c.op == "IS NULL" || c.op == "IS NOT NULL") {
			appendWhereOp(&sqlQ, &args, tableAlias+".", col, c)
		} else {
			unpushed = append(unpushed, c)
		}
	}

	// #308: same skip-when-aggregating treatment as runNodeScan.
	if !hasAggregation(q) {
		sqlQ += " LIMIT ?"
		args = append(args, e.maxRows()*2)
	}

	rows, err := e.DB.QueryContext(ctx, sqlQ, args...)
	if err != nil {
		return nil, fmt.Errorf("join query: %w", err)
	}
	defer rows.Close()

	reCache := make(map[string]*regexp.Regexp)
	var resultRows []map[string]any
	for rows.Next() {
		aNode, bNode, edgeKind, conf, err := scanJoinRow(rows)
		if err != nil {
			return nil, err
		}
		m := make(map[string]any)
		for k, v := range symRowToMap(pat.fromVar, aNode) {
			m[k] = v
		}
		for k, v := range symRowToMap(pat.toVar, bNode) {
			m[k] = v
		}
		if pat.edgeVar != "" {
			m[pat.edgeVar+".kind"] = edgeKind
			m[pat.edgeVar+".confidence"] = conf
		}
		if !matchesConditionsWithCache(m, unpushed, reCache) {
			continue
		}
		resultRows = append(resultRows, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return buildResult(resultRows, q)
}

// bfsHop is one reachable node found by the recursive CTE traversal.
type bfsHop struct {
	node  *symRow
	depth int
}

// runBFS handles variable-length path queries: MATCH (a)-[:CALLS*1..3]->(b)
//
// Implementation: one SQL recursive CTE per start node — collapses the old
// N×depth×width round-trip loop into a single query per start node.
// UNION ALL + depth < maxHops guarantees termination even in cyclic graphs.
// GROUP BY id + MIN(depth) returns each reachable node once at its shortest depth.
func (e *Executor) runBFS(ctx context.Context, q *queryAST, pat pattern) (*Result, error) {
	// Find start nodes
	startQ := "SELECT " + symCols + " FROM symbols WHERE 1=1"
	var startArgs []any

	if e.ProjectID != "" {
		startQ += " AND project_id=?"
		startArgs = append(startArgs, e.ProjectID)
	}
	if pat.fromKind != "" {
		startQ += " AND kind=?"
		startArgs = append(startArgs, pat.fromKind)
	}
	for _, c := range q.conditions {
		if c.variable != pat.fromVar {
			continue
		}
		col := cypherPropToCol(c.property)
		if col != "" && c.op == "=" {
			startQ += " AND " + col + "=?"
			startArgs = append(startArgs, c.value)
		}
	}
	startQ += " LIMIT 100"

	sRows, err := e.DB.QueryContext(ctx, startQ, startArgs...)
	if err != nil {
		return nil, err
	}
	defer sRows.Close()

	var startNodes []*symRow
	for sRows.Next() {
		n, err := scanSymRow(sRows)
		if err != nil {
			return nil, err
		}
		startNodes = append(startNodes, n)
	}
	sRows.Close()

	edgeKinds := pat.edgeKinds
	if len(edgeKinds) == 0 {
		edgeKinds = []string{"CALLS"}
	}

	maxDepth := pat.maxHops
	if maxDepth > 10 {
		maxDepth = 10
	}

	reCache := make(map[string]*regexp.Regexp)
	var resultRows []map[string]any
	for _, start := range startNodes {
		hops, err := e.bfsViaCTE(ctx, start.ID, edgeKinds, pat.minHops, maxDepth, e.ProjectID, e.maxRows())
		if err != nil {
			return nil, fmt.Errorf("bfs traversal from %q: %w", start.ID, err)
		}
		for _, hop := range hops {
			m := make(map[string]any)
			for k, v := range symRowToMap(pat.fromVar, start) {
				m[k] = v
			}
			for k, v := range symRowToMap(pat.toVar, hop.node) {
				m[k] = v
			}
			m["_hop"] = hop.depth
			if !matchesConditionsWithCache(m, q.conditions, reCache) {
				continue
			}
			resultRows = append(resultRows, m)
			if len(resultRows) >= e.maxRows()*2 {
				goto done
			}
		}
	}
done:
	return buildResult(resultRows, q)
}

// bfsViaCTE uses a single recursive CTE to find all nodes reachable from startID
// within [minHops, maxHops] steps along edges of the given kinds.
// This replaces the old Go BFS loop that issued one SQL call per node per depth.
func (e *Executor) bfsViaCTE(ctx context.Context, startID string, kinds []string, minHops, maxHops int, projectID string, maxRows int) ([]bfsHop, error) {
	in := inPlaceholders(len(kinds))

	projectFilter := ""
	if projectID != "" {
		projectFilter = " AND e.project_id = ?"
	}

	// UNION ALL + WHERE depth < maxHops terminates even on cyclic graphs.
	// GROUP BY id + MIN(depth) returns each reachable node once at shortest path.
	cteQ := `WITH RECURSIVE reach(id, depth) AS (
		SELECT ?, 0
		UNION ALL
		SELECT e.to_id, r.depth + 1
		FROM reach r
		JOIN edges e ON e.from_id = r.id AND e.kind IN (` + in + `)` + projectFilter + `
		WHERE r.depth < ?
	)
	SELECT s.id, s.project_id, s.file_path, s.name, s.qualified_name, s.kind, s.language,
		s.start_byte, s.end_byte, s.start_line, s.end_line, s.is_exported, s.is_entry_point, s.complexity,
		s.extraction_confidence, MIN(r.depth) AS min_depth
	FROM reach r
	JOIN symbols s ON s.id = r.id
	WHERE r.depth >= ? AND r.id != ?
	GROUP BY s.id
	ORDER BY min_depth
	LIMIT ?`

	args := []any{startID}
	for _, k := range kinds {
		args = append(args, k)
	}
	if projectID != "" {
		args = append(args, projectID)
	}
	args = append(args, maxHops, minHops, startID, maxRows)

	rows, err := e.DB.QueryContext(ctx, cteQ, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hops []bfsHop
	for rows.Next() {
		var n symRow
		var isExp, isEntry int64
		var depth int
		if err := rows.Scan(
			&n.ID, &n.ProjectID, &n.FilePath, &n.Name, &n.QualifiedName, &n.Kind, &n.Language,
			&n.StartByte, &n.EndByte, &n.StartLine, &n.EndLine, &isExp, &isEntry, &n.Complexity,
			&n.ExtractionConfidence, &depth,
		); err != nil {
			return nil, err
		}
		n.IsExported = isExp != 0
		n.IsEntryPoint = isEntry != 0
		hops = append(hops, bfsHop{node: &n, depth: depth})
	}
	return hops, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// Result projection
// ─────────────────────────────────────────────────────────────────────────────

func buildResult(allRows []map[string]any, q *queryAST) (*Result, error) {
	// Project RETURN columns
	var cols []string
	hasCount := false
	for _, rv := range q.returnVars {
		if rv.fn == "COUNT" {
			col := rv.alias
			if col == "" {
				col = "COUNT(" + rv.variable + ")"
			}
			cols = append(cols, col)
			hasCount = true
		} else {
			col := rv.variable
			if rv.property != "" {
				col = rv.variable + "." + rv.property
			}
			if rv.alias != "" {
				col = rv.alias
			}
			cols = append(cols, col)
		}
	}

	if len(cols) == 0 {
		// Auto-project all keys from first row
		if len(allRows) > 0 {
			for k := range allRows[0] {
				if !strings.HasPrefix(k, "_") {
					cols = append(cols, k)
				}
			}
			sort.Strings(cols)
		}
	}

	if hasCount {
		// #348: implicit GROUP BY when mixing non-aggregate columns with COUNT.
		// Standard Cypher/SQL semantics — `RETURN n.kind, COUNT(n)` should
		// group by n.kind and emit one row per kind, not collapse to a single
		// total row that silently drops the n.kind column. Pre-fix path treated
		// the presence of any COUNT as "single-row total" regardless of the
		// projection shape.
		var groupVars []returnVar
		var aggVars []returnVar
		for _, rv := range q.returnVars {
			if rv.fn == "COUNT" {
				aggVars = append(aggVars, rv)
			} else {
				groupVars = append(groupVars, rv)
			}
		}

		// No group-by columns → existing single-row total path. Backward
		// compatible: `RETURN COUNT(n)` still returns one row.
		if len(groupVars) == 0 {
			total := len(allRows)
			row := map[string]any{}
			for _, rv := range aggVars {
				col := rv.alias
				if col == "" {
					col = "COUNT(" + rv.variable + ")"
				}
				row[col] = total
			}
			return &Result{Columns: cols, Rows: []map[string]any{row}, Total: 1}, nil
		}

		// Group rows by the tuple of group-var values. The key is fmt.Sprint
		// of the tuple — the same approach `q.distinct` uses for row dedup
		// (line 1071), so behaviour is consistent.
		type groupBucket struct {
			values map[string]any
			count  int
		}
		groups := map[string]*groupBucket{}
		var groupOrder []string // preserve first-seen order so unORDERed output is deterministic
		for _, row := range allRows {
			values := map[string]any{}
			for _, rv := range groupVars {
				key := rv.variable
				if rv.property != "" {
					key = rv.variable + "." + rv.property
				}
				outCol := key
				if rv.alias != "" {
					outCol = rv.alias
				}
				values[outCol] = row[key]
			}
			groupKey := fmt.Sprint(values)
			b, ok := groups[groupKey]
			if !ok {
				b = &groupBucket{values: values}
				groups[groupKey] = b
				groupOrder = append(groupOrder, groupKey)
			}
			b.count++
		}

		// Emit one row per group, with each agg's count.
		grouped := make([]map[string]any, 0, len(groups))
		for _, gk := range groupOrder {
			b := groups[gk]
			out := make(map[string]any, len(groupVars)+len(aggVars))
			for k, v := range b.values {
				out[k] = v
			}
			for _, rv := range aggVars {
				col := rv.alias
				if col == "" {
					col = "COUNT(" + rv.variable + ")"
				}
				out[col] = b.count
			}
			grouped = append(grouped, out)
		}

		// ORDER BY + LIMIT apply to grouped rows (not the underlying scan).
		// Mirrors the non-aggregate path below.
		if q.orderBy != "" {
			desc := q.orderDir == "DESC"
			sort.SliceStable(grouped, func(i, j int) bool {
				return cypherLessThan(grouped[i][q.orderBy], grouped[j][q.orderBy], desc)
			})
		}
		limit := q.limit
		if limit <= 0 {
			limit = 200
		}
		if len(grouped) > limit {
			grouped = grouped[:limit]
		}
		return &Result{Columns: cols, Rows: grouped, Total: len(grouped)}, nil
	}

	// Project rows
	var projected []map[string]any
	seen := map[string]bool{}
	for _, row := range allRows {
		pr := make(map[string]any, len(cols))
		for i, rv := range q.returnVars {
			var val any
			if rv.property != "" {
				key := rv.variable + "." + rv.property
				val = row[key]
			} else {
				// Return all properties for the variable
				val = row[rv.variable+".name"]
			}
			pr[cols[i]] = val
		}
		if len(q.returnVars) == 0 {
			pr = row
		}
		if q.distinct {
			key := fmt.Sprint(pr)
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		projected = append(projected, pr)
	}

	// ORDER BY. #313: when both values are numeric, compare them
	// numerically. The pre-fix path always stringified via
	// fmt.Sprint, which sorted "1004" before "126" (lex). Numeric
	// columns (start_line, end_line, complexity) are the typical
	// ORDER BY target so the silent wrongness was easy to hit.
	if q.orderBy != "" {
		desc := q.orderDir == "DESC"
		sort.SliceStable(projected, func(i, j int) bool {
			return cypherLessThan(projected[i][q.orderBy], projected[j][q.orderBy], desc)
		})
	}

	// LIMIT
	limit := q.limit
	if limit <= 0 {
		limit = 200
	}
	if len(projected) > limit {
		projected = projected[:limit]
	}

	return &Result{Columns: cols, Rows: projected, Total: len(projected)}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Scan helpers
// ─────────────────────────────────────────────────────────────────────────────

// cypherLessThan compares two values for ORDER BY. Numeric on both
// sides → numeric compare; otherwise stringify and compare
// lexicographically. `desc` flips the result. Mixed-type rows fall
// to the string path (rare in practice — the same column is
// usually all-numeric or all-string).
func cypherLessThan(a, b any, desc bool) bool {
	af, aok := toFloatForOrderBy(a)
	bf, bok := toFloatForOrderBy(b)
	if aok && bok {
		if desc {
			return af > bf
		}
		return af < bf
	}
	as := fmt.Sprint(a)
	bs := fmt.Sprint(b)
	if desc {
		return as > bs
	}
	return as < bs
}

// toFloatForOrderBy returns (n, true) when v is one of the integer
// or floating-point shapes pincher's symRow / map projections might
// carry. Returns (_, false) for strings, nil, or anything else so
// the caller falls back to string compare.
func toFloatForOrderBy(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func scanSymRow(rows *sql.Rows) (*symRow, error) {
	var n symRow
	var isExp, isEntry int64
	if err := rows.Scan(
		&n.ID, &n.ProjectID, &n.FilePath, &n.Name, &n.QualifiedName, &n.Kind, &n.Language,
		&n.StartByte, &n.EndByte, &n.StartLine, &n.EndLine, &isExp, &isEntry, &n.Complexity,
		&n.ExtractionConfidence,
	); err != nil {
		return nil, err
	}
	n.IsExported = isExp != 0
	n.IsEntryPoint = isEntry != 0
	return &n, nil
}

func scanJoinRow(rows *sql.Rows) (a, b *symRow, edgeKind string, conf float64, err error) {
	a = &symRow{}
	b = &symRow{}
	var isExpA, isEntryA, isExpB, isEntryB int64
	err = rows.Scan(
		&a.ID, &a.ProjectID, &a.FilePath, &a.Name, &a.QualifiedName, &a.Kind, &a.Language,
		&a.StartByte, &a.EndByte, &a.StartLine, &a.EndLine, &isExpA, &isEntryA, &a.Complexity,
		&a.ExtractionConfidence,
		&b.ID, &b.ProjectID, &b.FilePath, &b.Name, &b.QualifiedName, &b.Kind, &b.Language,
		&b.StartByte, &b.EndByte, &b.StartLine, &b.EndLine, &isExpB, &isEntryB, &b.Complexity,
		&b.ExtractionConfidence,
		&edgeKind, &conf,
	)
	a.IsExported = isExpA != 0
	a.IsEntryPoint = isEntryA != 0
	b.IsExported = isExpB != 0
	b.IsEntryPoint = isEntryB != 0
	return
}

func symRowToMap(varName string, n *symRow) map[string]any {
	prefix := varName + "."
	return map[string]any{
		prefix + "id":             n.ID,
		prefix + "name":           n.Name,
		prefix + "qualified_name": n.QualifiedName,
		prefix + "kind":           n.Kind,
		prefix + "language":       n.Language,
		prefix + "file_path":      n.FilePath,
		prefix + "start_line":     n.StartLine,
		prefix + "end_line":       n.EndLine,
		prefix + "start_byte":     n.StartByte,
		prefix + "end_byte":       n.EndByte,
		prefix + "is_exported":            n.IsExported,
		prefix + "is_entry_point":         n.IsEntryPoint,
		prefix + "complexity":             n.Complexity,
		prefix + "extraction_confidence":  n.ExtractionConfidence,
	}
}

// appendWhereOp appends a SQL condition for a pushed-down Cypher WHERE clause.
// prefix is "" for single-table queries or "alias." for JOIN queries.
func appendWhereOp(sqlQ *string, args *[]any, prefix, col string, c condition) {
	// Build the inner clause first (without the leading " AND "), then
	// wrap with " AND NOT (...)" or " AND " depending on c.negated (#354).
	var inner string
	switch c.op {
	case "=":
		inner = prefix + col + "=?"
		*args = append(*args, c.value)
	case "CONTAINS":
		inner = prefix + col + " LIKE ?"
		*args = append(*args, "%"+c.value+"%")
	case "STARTS WITH":
		inner = prefix + col + " LIKE ?"
		*args = append(*args, c.value+"%")
	case "ENDS WITH":
		// #340: SQL pushdown for the suffix-match family.
		inner = prefix + col + " LIKE ?"
		*args = append(*args, "%"+c.value)
	case "IS NULL":
		// #342: NULL OR empty. SQLite's Go driver maps NULL TEXT to "".
		inner = "(" + prefix + col + " IS NULL OR " + prefix + col + " = '')"
	case "IS NOT NULL":
		inner = "(" + prefix + col + " IS NOT NULL AND " + prefix + col + " <> '')"
	default:
		return
	}
	if c.negated {
		*sqlQ += " AND NOT (" + inner + ")"
	} else {
		*sqlQ += " AND " + inner
	}
}

// cypherPropToCol maps a Cypher property name to a SQL column name.
func cypherPropToCol(prop string) string {
	switch prop {
	case "name":
		return "name"
	case "qualified_name", "qn":
		return "qualified_name"
	case "kind", "label":
		return "kind"
	case "file_path":
		return "file_path"
	case "language":
		return "language"
	case "start_line":
		return "start_line"
	case "end_line":
		return "end_line"
	default:
		return ""
	}
}

// matchesConditions applies remaining (non-SQL-pushed) conditions in Go,
// supporting regex (=~) and numeric comparisons.
// reCache is an optional map for caching compiled regexes across calls to
// avoid recompiling the same pattern for every row.
func matchesConditions(row map[string]any, conds []condition) bool {
	return matchesConditionsWithCache(row, conds, nil)
}

func matchesConditionsWithCache(row map[string]any, conds []condition, reCache map[string]*regexp.Regexp) bool {
	if len(conds) == 0 {
		return true
	}
	// #358: walk left-to-right honouring AND/OR connectors. No operator
	// precedence (paren parsing not yet implemented), so `a OR b AND c`
	// evaluates as `(a OR b) AND c`. Document this as a known limitation.
	result := evalCondition(row, conds[0], reCache)
	if conds[0].negated {
		result = !result
	}
	for _, c := range conds[1:] {
		matched := evalCondition(row, c, reCache)
		if c.negated {
			matched = !matched
		}
		switch c.connector {
		case "OR":
			result = result || matched
		default: // "AND" or "" (treat as AND for safety)
			result = result && matched
		}
	}
	return result
}

// evalCondition returns true iff the row satisfies the un-negated form
// of c. Caller XORs with c.negated for #354 NOT semantics.
func evalCondition(row map[string]any, c condition, reCache map[string]*regexp.Regexp) bool {
	key := c.variable + "." + c.property
	actual := fmt.Sprint(row[key])

	switch c.op {
	case "=":
		return actual == c.value
	case "<>":
		return actual != c.value
	case "=~":
		var re *regexp.Regexp
		if reCache != nil {
			re = reCache[c.value]
		}
		if re == nil {
			var err error
			re, err = regexp.Compile(c.value)
			if err != nil {
				return false
			}
			if reCache != nil {
				reCache[c.value] = re
			}
		}
		return re.MatchString(actual)
	case "CONTAINS":
		return strings.Contains(actual, c.value)
	case "STARTS WITH":
		return strings.HasPrefix(actual, c.value)
	case "ENDS WITH":
		return strings.HasSuffix(actual, c.value)
	case "IS NULL":
		// Empty string OR Go nil-interface map miss — both treated as NULL (#342).
		raw, present := row[key]
		return !present || raw == nil || actual == "" || actual == "<nil>"
	case "IS NOT NULL":
		raw, present := row[key]
		return present && raw != nil && actual != "" && actual != "<nil>"
	case ">", "<", ">=", "<=":
		an, aerr := strconv.ParseFloat(actual, 64)
		bn, berr := strconv.ParseFloat(c.value, 64)
		if aerr != nil || berr != nil {
			return false
		}
		switch c.op {
		case ">":
			return an > bn
		case "<":
			return an < bn
		case ">=":
			return an >= bn
		case "<=":
			return an <= bn
		}
	}
	return false
}

// QueryAST exposes minimal fields for external tests.
func QueryAST(q *queryAST) map[string]any {
	return map[string]any{
		"patterns":   len(q.patterns),
		"conditions": len(q.conditions),
		"returns":    len(q.returnVars),
		"limit":      q.limit,
	}
}

// minHops is referenced below to avoid unused variable warnings.
func (q *queryAST) minHops() int {
	if len(q.patterns) == 0 {
		return 1
	}
	return q.patterns[0].minHops
}
