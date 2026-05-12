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
	// Warnings is the list of non-fatal advisories produced while running
	// the query. The most common entry is "property X not recognized" —
	// see #473. Empty when the query is structurally clean. Callers MUST
	// surface this to the user / agent; the engine treats unknown
	// properties as undefined (falsy in comparisons), which silently
	// returns 0 rows on typos when no warning is shown.
	Warnings []string `json:"warnings,omitempty"`
}

// Executor runs Cypher queries against a SQLite database.
type Executor struct {
	DB        *sql.DB
	MaxRows   int    // 0 = default (200)
	ProjectID string // if set, all queries are scoped to this project

	// AllowAllProjects opts in to cross-project queries. The MCP
	// `query` handler sets this when the caller passes `project=*`,
	// matching the same opt-in shape `search` uses. Empty ProjectID
	// without this flag is rejected as defense-in-depth.
	AllowAllProjects bool
}

// Execute parses and executes a Cypher query.
//
// SECURITY: by default, rejects empty ProjectID. The runNodeScan /
// runJoinQuery / runBFS paths only append `project_id=?` to the SQL
// when ProjectID is non-empty, so a caller forgetting to set it would
// get cross-project results. Refusing here is defense-in-depth —
// handleQuery (the MCP entrypoint) already enforces a non-empty
// project via mustProject, but in-code callers might construct an
// Executor directly.
//
// AllowAllProjects=true is the explicit opt-in for cross-project
// queries, set by handleQuery when the caller passes `project=*`.
// In that mode an empty ProjectID is permitted and the SQL omits
// the project_id filter, returning rows from every indexed project.
func (e *Executor) Execute(ctx context.Context, query string) (*Result, error) {
	if e.ProjectID == "" && !e.AllowAllProjects {
		return nil, fmt.Errorf("cypher: ProjectID is required (refusing to run cross-project query; pass AllowAllProjects=true or project=* via the MCP handler to opt in)")
	}
	q, err := parse(query)
	if err != nil {
		return nil, fmt.Errorf("cypher parse: %w", err)
	}
	// #473: surface property names the engine doesn't know about as
	// non-fatal warnings. Unknown properties evaluate to undefined,
	// which makes comparisons falsy and returns 0 rows — without a
	// warning the agent walks away thinking "no matches" when the
	// real cause is a typo in the property name.
	warnings := collectUnknownPropertyWarnings(q)
	// #593: column-vs-column comparisons (`a.col <op> b.col`) parse
	// but evaluation returns false — surface a warning so the agent
	// knows the predicate isn't being honored. Same UX class as #473.
	warnings = append(warnings, collectCrossColumnWarnings(q)...)
	res, err := e.run(ctx, q)
	if err != nil {
		return res, err
	}
	if res != nil {
		res.Warnings = append(res.Warnings, warnings...)
		// #501: when the result set is empty AND the query filters on
		// an enum-shaped property (kind / language) with a value not
		// in the project, suggest valid values. Same failure-as-pedagogy
		// shape as #473 extended one layer down: not the property NAME
		// but the property VALUE. Skipped on non-empty results because
		// "you found rows AND we have a complaint about your filter"
		// would be noise — the agent isn't confused.
		if res.Total == 0 {
			res.Warnings = append(res.Warnings, e.collectUnknownEnumValueWarnings(ctx, q)...)
		}
	}
	return res, nil
}

// knownPropertyList is the human-readable enumeration used in the
// unknown-property warning text for NODE variables. Sourced from the
// cypherPropToCol switch — keep in sync if a new column is added there.
var knownPropertyList = []string{
	"id", "name", "qualified_name (qn)", "kind (label)", "file_path",
	"language", "start_line", "end_line", "start_byte", "end_byte",
	"complexity", "extraction_confidence (confidence)",
	"is_exported", "is_entry_point", "is_test",
	"signature", "return_type", "docstring",
}

// knownEdgePropertyList is the equivalent for EDGE variables.
// #612: pre-fix the unknown-property warning always showed node props
// even when the offending variable was bound to an edge — pointing the
// user at the wrong fix. Edge result rows currently carry `kind` and
// `confidence` (engine.go ~1680); future fields (source) get added
// here when surfaced to pinchQL.
var knownEdgePropertyList = []string{
	"kind", "confidence",
}

// collectUnknownPropertyWarnings walks the parsed query and returns one
// warning per distinct unknown property name. Touches WHERE conditions
// (including inside binaryExpr / notExpr trees), pattern inline match
// braces ({name:"x"}), and RETURN projections. Empty when every
// property is known. Sorted alphabetically for stable test output.
func collectUnknownPropertyWarnings(q *queryAST) []string {
	if q == nil {
		return nil
	}
	// #612: tag each unknown property with whether the variable on the
	// LHS was bound to an edge. Pre-fix the warning always recommended
	// node properties — useless when the user wrote `r.source` on a
	// `[r:CALLS]` edge.
	edgeVars := map[string]bool{}
	for _, pat := range q.patterns {
		if pat.edgeVar != "" {
			edgeVars[pat.edgeVar] = true
		}
	}
	type unknownRef struct {
		isEdge bool
	}
	unknown := map[string]unknownRef{}
	check := func(variable, prop string) {
		if prop == "" {
			return
		}
		isEdge := variable != "" && edgeVars[variable]
		if isEdge {
			if isKnownEdgeProperty(prop) {
				return
			}
		} else if cypherPropToCol(prop) != "" {
			return
		}
		// Don't downgrade an existing edge mark to node-only just because
		// the same property name appears on a node var elsewhere — keep
		// the more-specific edge warning when both apply.
		if existing, ok := unknown[prop]; !ok || (isEdge && !existing.isEdge) {
			unknown[prop] = unknownRef{isEdge: isEdge}
		}
	}
	// Flat WHERE conditions (the common AND-chain path).
	for _, c := range q.conditions {
		check(c.variable, c.property)
	}
	// Recursive WHERE tree (paren-grouped queries / NOT-groups).
	var walk func(w whereExpr)
	walk = func(w whereExpr) {
		switch e := w.(type) {
		case condExpr:
			check(e.c.variable, e.c.property)
		case binaryExpr:
			walk(e.left)
			walk(e.right)
		case notExpr:
			walk(e.inner)
		}
	}
	walk(q.where)
	// Inline pattern match braces: MATCH (n:Function {foo:"x"}).
	// Inline braces are always on node patterns (fromVar/toVar) — pinchQL
	// doesn't accept inline braces on edge declarations — so pass an
	// empty variable to default to the node warning.
	for _, pat := range q.patterns {
		for prop := range pat.fromProps {
			check(pat.fromVar, prop)
		}
		for prop := range pat.toProps {
			check(pat.toVar, prop)
		}
	}
	// RETURN projections — n.foo / r.foo references count too.
	for _, rv := range q.returnVars {
		check(rv.variable, rv.property)
	}
	if len(unknown) == 0 {
		return nil
	}
	names := make([]string, 0, len(unknown))
	for n := range unknown {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		propList := knownPropertyList
		kind := "node"
		if unknown[n].isEdge {
			propList = knownEdgePropertyList
			kind = "edge"
		}
		out = append(out, fmt.Sprintf(
			"property %q not recognized on %s variable; treated as undefined (always false in comparisons). Valid %s properties: %s.",
			n, kind, kind, strings.Join(propList, ", ")))
	}
	return out
}

// isKnownEdgeProperty reports whether prop is one of the edge-variable
// properties surfaced by the engine into result rows. Mirror of the
// cypherPropToCol switch but for edges. Kept tiny — when a new edge
// property gets exposed (e.g. `source` once #475's edges.source column
// is plumbed through to pinchQL projections), add it here AND in
// knownEdgePropertyList.
func isKnownEdgeProperty(prop string) bool {
	switch prop {
	case "kind", "confidence":
		return true
	}
	return false
}

// collectCrossColumnWarnings (#593) walks the WHERE tree for
// conditions whose RHS is a column reference (`a.col <op> b.col`).
// pinchQL doesn't currently support column-vs-column comparison —
// evaluation returns false for these, so the user gets 0 rows
// (consistent with #473 unknown-property handling). The warning
// names the offending clauses so the agent can rewrite them as
// literal comparisons or run the equivalent post-filter in their
// client. Sorted alphabetically for stable test output.
func collectCrossColumnWarnings(q *queryAST) []string {
	if q == nil {
		return nil
	}
	clauses := map[string]struct{}{}
	check := func(c condition) {
		if c.rhsProperty == "" {
			return
		}
		clause := fmt.Sprintf("%s.%s %s %s.%s",
			c.variable, c.property, c.op, c.rhsVariable, c.rhsProperty)
		clauses[clause] = struct{}{}
	}
	for _, c := range q.conditions {
		check(c)
	}
	var walk func(w whereExpr)
	walk = func(w whereExpr) {
		switch e := w.(type) {
		case condExpr:
			check(e.c)
		case binaryExpr:
			walk(e.left)
			walk(e.right)
		case notExpr:
			walk(e.inner)
		}
	}
	walk(q.where)
	if len(clauses) == 0 {
		return nil
	}
	names := make([]string, 0, len(clauses))
	for n := range clauses {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, fmt.Sprintf(
			"column-vs-column comparison %q is not supported in pinchQL — predicate ignored (returns false). Use literal values on the RHS, or post-filter the result set in your client.",
			n))
	}
	return out
}

// enumValuedProperties is the set of cypher property names whose values
// come from a finite, project-discoverable set. When a 0-row query
// filters on one of these with `=`, the engine looks up actual distinct
// values in the project and suggests them in a warning (#501).
//
// Limited to columns that are themselves a closed vocabulary on the
// symbols table (kind, language). corpus is computed by ClassifyCorpus
// at runtime, not stored as a column, and isn't a cypher property anyway.
var enumValuedProperties = map[string]bool{
	"kind":     true,
	"label":    true, // alias for kind in cypherPropToCol
	"language": true,
}

// collectUnknownEnumValueWarnings finds equality filters on enum-shaped
// properties whose values don't exist in the project's symbols table,
// and emits one warning per (property, value) pair. Called only when
// the query returned zero rows — non-empty results are taken as proof
// the filter value was at least one observed value.
//
// Each unique (property, value) becomes at most one DB query
// (`SELECT DISTINCT col FROM symbols WHERE project_id=?`); on a typical
// corpus the result is a 5-15-row list cached implicitly by SQLite.
func (e *Executor) collectUnknownEnumValueWarnings(ctx context.Context, q *queryAST) []string {
	if e == nil || e.DB == nil || q == nil {
		return nil
	}
	type probe struct{ prop, value string }
	probes := map[probe]bool{}
	consider := func(c condition) {
		if c.op != "=" || c.value == "" {
			return
		}
		if !enumValuedProperties[c.property] {
			return
		}
		col := cypherPropToCol(c.property)
		if col == "" {
			return
		}
		probes[probe{prop: col, value: c.value}] = true
	}
	for _, c := range q.conditions {
		consider(c)
	}
	var walk func(w whereExpr)
	walk = func(w whereExpr) {
		switch x := w.(type) {
		case condExpr:
			consider(x.c)
		case binaryExpr:
			walk(x.left)
			walk(x.right)
		case notExpr:
			walk(x.inner)
		}
	}
	walk(q.where)

	// MATCH pattern labels: `MATCH (n:Funtion)` is a typo'd kind that
	// behaves like `WHERE n.kind = 'Funtion'` — same silent-zero,
	// same pedagogy needed. fromKind/toKind feed the same `kind`
	// column, so probe them under the same enum machinery.
	for _, pat := range q.patterns {
		if pat.fromKind != "" {
			probes[probe{prop: "kind", value: pat.fromKind}] = true
		}
		if pat.toKind != "" {
			probes[probe{prop: "kind", value: pat.toKind}] = true
		}
	}

	if len(probes) == 0 {
		return nil
	}
	// Deterministic order for tests + readable warnings.
	keys := make([]probe, 0, len(probes))
	for p := range probes {
		keys = append(keys, p)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].prop != keys[j].prop {
			return keys[i].prop < keys[j].prop
		}
		return keys[i].value < keys[j].value
	})

	out := []string{}
	for _, p := range keys {
		known := e.distinctSymbolsColumn(ctx, p.prop)
		if known == nil {
			continue
		}
		if containsString(known, p.value) {
			continue
		}
		out = append(out, fmt.Sprintf(
			"WHERE %s = %q matched no symbols. Known %s values in this project: %s. (typo? wrong property — e.g. did you mean name = %q?)",
			p.prop, p.value, p.prop, strings.Join(known, ", "), p.value,
		))
	}
	return out
}

// distinctSymbolsColumn returns the project-scoped distinct values of
// a single column on the symbols table. col MUST be one of the
// hard-coded enum properties — it is interpolated unquoted into the
// SQL, so any caller that bypasses enumValuedProperties has supplied
// an injection vector. enumValuedProperties is a closed set so the
// guard is a structural invariant, not a runtime check.
//
// Cross-project (AllowAllProjects=true, ProjectID="") path drops the
// project_id filter — preserves parity with the rest of the engine.
//
// Returns nil on any DB error; callers treat nil as "skip the warning,
// don't false-positive on transient failures."
func (e *Executor) distinctSymbolsColumn(ctx context.Context, col string) []string {
	var rows *sql.Rows
	var err error
	if e.ProjectID != "" {
		rows, err = e.DB.QueryContext(ctx,
			"SELECT DISTINCT "+col+" FROM symbols WHERE project_id=? AND "+col+" != '' ORDER BY "+col,
			e.ProjectID)
	} else {
		rows, err = e.DB.QueryContext(ctx,
			"SELECT DISTINCT "+col+" FROM symbols WHERE "+col+" != '' ORDER BY "+col)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// containsString — pre-Go-1.21 generic shim; engine.go already targets
// the toolchain default in this repo (1.21+ has slices.Contains) but
// the cypher package's import surface is intentionally narrow, so a
// 5-line helper is cheaper than pulling in `slices`.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// symCols is the canonical SELECT column list for the symbols table.
// Keep in sync with db.symSelectFrom and cypher.symRow.
// #438: signature, return_type, docstring, is_test exposed so pinchQL
// WHERE clauses can address them — IS NULL / IS NOT NULL previously
// matched all-or-none because the row map didn't carry the column.
const symCols = `id, project_id, file_path, name, qualified_name, kind, language,
	start_byte, end_byte, start_line, end_line, is_exported, is_entry_point, complexity,
	extraction_confidence, signature, return_type, docstring, is_test`

// inPlaceholders returns a comma-separated "?,?,..." string for n items.
func inPlaceholders(n int) string {
	s := strings.Repeat("?,", n)
	return s[:len(s)-1]
}

// ─────────────────────────────────────────────────────────────────────────────
// Query AST
// ─────────────────────────────────────────────────────────────────────────────

type queryAST struct {
	patterns []pattern // MATCH clauses
	// conditions is the flat representation of WHERE, populated only when
	// the parsed tree is a left-leaning AND/OR chain of leaves. Reading
	// code that needs to introspect WHERE one-condition-at-a-time (or
	// drive SQL pushdown) uses this. Empty for paren-grouped queries —
	// callers fall through to `where` for those.
	conditions []condition
	// where is the canonical tree representation of WHERE (#362). Always
	// populated when WHERE is present. Required for queries with parens
	// or `NOT (...)` where flat ordering can't express the semantics.
	where      whereExpr
	returnVars []returnVar // RETURN items
	orderBy    string
	orderDir   string // ASC | DESC
	limit      int
	distinct   bool
}

// whereExpr is the recursive-descent parse tree for WHERE clauses.
// Three shapes — condExpr (leaf), binaryExpr (AND/OR), notExpr
// (group NOT). Single-condition NOT keeps using condition.negated
// to match the pre-#362 leaf-NOT semantics from #354.
type whereExpr interface {
	eval(row map[string]any, reCache map[string]*regexp.Regexp) bool
}

type condExpr struct{ c condition }

func (e condExpr) eval(row map[string]any, reCache map[string]*regexp.Regexp) bool {
	r := evalCondition(row, e.c, reCache)
	if e.c.negated {
		r = !r
	}
	return r
}

type binaryExpr struct {
	op          string // "AND" | "OR"
	left, right whereExpr
}

func (e binaryExpr) eval(row map[string]any, reCache map[string]*regexp.Regexp) bool {
	if e.op == "OR" {
		return e.left.eval(row, reCache) || e.right.eval(row, reCache)
	}
	return e.left.eval(row, reCache) && e.right.eval(row, reCache)
}

type notExpr struct{ inner whereExpr }

func (e notExpr) eval(row map[string]any, reCache map[string]*regexp.Regexp) bool {
	return !e.inner.eval(row, reCache)
}

// matchesWhere returns true if w matches row (or w is nil — no WHERE).
func matchesWhere(row map[string]any, w whereExpr, reCache map[string]*regexp.Regexp) bool {
	if w == nil {
		return true
	}
	return w.eval(row, reCache)
}

// flattenWhere returns the leaves in source order with connectors stamped
// per #358 semantics, iff the tree is a left-leaning AND/OR chain of
// condExpr leaves (no parens-induced asymmetric tree, no notExpr group).
// Returns nil otherwise. The boolean signals whether the flat list is
// authoritative; callers that need a non-flat tree fall back to
// queryAST.where.
func flattenWhere(w whereExpr) []condition {
	var leaves []condition
	var connectors []string
	if !collectFlatLeaves(w, &leaves, &connectors) {
		return nil
	}
	out := make([]condition, len(leaves))
	for i, l := range leaves {
		out[i] = l
		if i == 0 {
			out[i].connector = ""
		} else {
			out[i].connector = connectors[i-1]
		}
	}
	return out
}

func collectFlatLeaves(w whereExpr, leaves *[]condition, connectors *[]string) bool {
	switch e := w.(type) {
	case condExpr:
		*leaves = append(*leaves, e.c)
		return true
	case binaryExpr:
		// Left-leaning shape: left can be a chain, right must be a leaf.
		if !collectFlatLeaves(e.left, leaves, connectors) {
			return false
		}
		rc, ok := e.right.(condExpr)
		if !ok {
			return false
		}
		*leaves = append(*leaves, rc.c)
		*connectors = append(*connectors, e.op)
		return true
	}
	return false // notExpr (or future shapes) — non-flat by definition
}

// pushdownAllowed reports whether SQL pushdown is safe for this WHERE.
// Pushdown emits AND-joined WHERE clauses; anything richer (OR, group,
// NOT-group) must be evaluated in Go via matchesWhere. The gate is
// stricter than the old conditionsHaveOr — it also catches NOT-groups
// and asymmetric trees from parens.
func pushdownAllowed(q *queryAST) bool {
	return whereIsAndChainOfLeaves(q.where)
}

func whereIsAndChainOfLeaves(w whereExpr) bool {
	if w == nil {
		return true
	}
	switch e := w.(type) {
	case condExpr:
		return true
	case binaryExpr:
		if e.op != "AND" {
			return false
		}
		if _, ok := e.right.(condExpr); !ok {
			return false
		}
		return whereIsAndChainOfLeaves(e.left)
	}
	return false
}

// andChainFromConds builds a left-leaning AND tree from a flat slice of
// conditions. Used to wrap the post-pushdown leftover leaves so the row
// loop can use a single matchesWhere call regardless of pushdown mode.
func andChainFromConds(conds []condition) whereExpr {
	if len(conds) == 0 {
		return nil
	}
	var w whereExpr = condExpr{c: conds[0]}
	for _, c := range conds[1:] {
		w = binaryExpr{op: "AND", left: w, right: condExpr{c: c}}
	}
	return w
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
	// #593: when the user writes `WHERE a.col <op> b.col`, parseOneCondition
	// captures the RHS variable + property here so collectCrossColumnWarnings
	// can name them in the advisory and evaluation can return false instead
	// of falsely-true. Empty when the RHS is a literal.
	rhsVariable string
	rhsProperty string
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
	// limit=-1 = "no LIMIT clause; runner picks default". Distinct from
	// LIMIT 0, which the parser sets to 0 below and the runner honors as
	// "zero rows" (#360). Pre-fix the parser used 200 directly so an
	// explicit `LIMIT 0` was indistinguishable from "unset" and silently
	// returned the default.
	q := &queryAST{limit: -1}

	for p.pos < len(p.tokens) {
		t := p.peek()
		switch t.value {
		case "MATCH":
			p.next()
			pat, err := p.parsePattern()
			if err != nil {
				return nil, err
			}
			// #433: chained-edge patterns like (a)-[]->(b)-[]->(c)
			// used to silently parse the second edge as garbage and
			// return null `c.*` projections. Reject explicitly and
			// point at the variable-length workaround.
			if p.peek().value == "-" || p.peek().value == "<" {
				return nil, fmt.Errorf(
					"pinchQL: chained edge patterns ((a)-[]->(b)-[]->(c)) are not supported. " +
						"Use the variable-length form for fixed-length paths: (a)-[*2..2]->(c)")
			}
			q.patterns = append(q.patterns, pat)

		case "WHERE":
			p.next()
			where, err := p.parseWhere()
			if err != nil {
				return nil, err
			}
			// Multiple WHERE clauses (one per MATCH) AND together.
			if q.where == nil {
				q.where = where
			} else {
				q.where = binaryExpr{op: "AND", left: q.where, right: where}
			}
			// Populate q.conditions when the tree is a left-leaning AND/OR
			// chain — back-compat with code paths and tests that read the
			// flat representation. Paren-grouped queries leave it empty;
			// those callers must use q.where.
			if flat := flattenWhere(q.where); flat != nil {
				q.conditions = flat
			} else {
				q.conditions = nil
			}

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

		case "WITH":
			// #433: WITH is a real Cypher projection-pipeline keyword
			// pinchQL doesn't support, but the tokenizer marks it as a
			// keyword (so STARTS WITH / ENDS WITH work). Bare WITH at
			// top level used to fall through and silently consume the
			// rest of the chain, making `WITH n WHERE ...` look like
			// it ran the WHERE while in fact the WHERE never gated
			// anything. Reject explicitly.
			return nil, fmt.Errorf(
				"pinchQL: WITH clause is not supported. " +
					"Use a single MATCH ... WHERE ... RETURN; chain via subsequent calls")

		default:
			p.next() // skip unknown tokens
		}
	}
	// #361: validate the parsed query has the minimum shape pinchQL
	// requires. Pre-fix, an empty input or a query missing MATCH / RETURN
	// returned an empty result silently — typo in `MATCH` / `RETURN`
	// looked like missing data. Reject up front so the agent sees the
	// query is malformed, not the index.
	if len(q.patterns) == 0 {
		return nil, fmt.Errorf("pinchQL: query must contain a MATCH clause")
	}
	if len(q.returnVars) == 0 {
		return nil, fmt.Errorf("pinchQL: query must contain a RETURN clause")
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

// parseWhere parses a WHERE clause and returns the recursive-descent tree
// (#362). Grammar:
//
//	or:     and ('OR' and)*
//	and:    factor ('AND' factor)*
//	factor: 'NOT'? ('(' or ')' | leafCondition)
//
// Left-recursion in or/and means flat queries (no parens) produce
// left-leaning trees that flattenWhere can collapse back to []condition
// for back-compat with the pre-#362 q.conditions code paths.
func (p *parser) parseWhere() (whereExpr, error) {
	return p.parseOrExpr()
}

func (p *parser) parseOrExpr() (whereExpr, error) {
	left, err := p.parseAndExpr()
	if err != nil {
		return nil, err
	}
	for p.peek().value == "OR" {
		p.next()
		right, err := p.parseAndExpr()
		if err != nil {
			return nil, err
		}
		left = binaryExpr{op: "OR", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseAndExpr() (whereExpr, error) {
	left, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	for p.peek().value == "AND" {
		p.next()
		right, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		left = binaryExpr{op: "AND", left: left, right: right}
	}
	return left, nil
}

// parseFactor handles a single boolean atom: an optional NOT, then either
// a parenthesized sub-expression or a leaf condition. The two NOT shapes
// are distinguished by the next token after NOT:
//   - `NOT (` → group NOT; wraps the parsed sub-expression in notExpr.
//   - `NOT <ident>` → leaf NOT; flags condition.negated for #354 behaviour.
func (p *parser) parseFactor() (whereExpr, error) {
	negated := false
	if p.peek().value == "NOT" {
		p.next()
		negated = true
	}
	if p.peek().value == "(" {
		p.next()
		inner, err := p.parseOrExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().value != ")" {
			return nil, fmt.Errorf("expected ')' after WHERE sub-expression, got %q", p.peek().value)
		}
		p.next()
		if negated {
			return notExpr{inner: inner}, nil
		}
		return inner, nil
	}
	c, err := p.parseOneCondition()
	if err != nil {
		return nil, err
	}
	c.negated = negated
	return condExpr{c: c}, nil
}

func (p *parser) parseOneCondition() (condition, error) {
	c := condition{}

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
		// #593: detect column-vs-column shape (`a.col <op> b.col`).
		// When the RHS token is followed by `.<prop>`, the user wrote a
		// property reference instead of a literal. Capture both sides so
		// collectCrossColumnWarnings can name them; evaluation returns
		// false (consistent with the #473 unknown-property handling)
		// rather than the silently-always-true behavior pre-#593.
		if p.peek().value == "." {
			p.next()
			c.rhsVariable = c.value
			c.rhsProperty = p.next().value
			c.value = ""
		}
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
		// #431: when the parser sees a "naked" property reference
		// followed by something that ends the predicate — `)`, AND,
		// OR, RETURN, end-of-input — the user almost certainly meant
		// `n.is_exported` as a boolean shorthand for `= true`. Cypher
		// (Neo4j, Memgraph) supports this. We only honour it for
		// columns we know are bool-typed; anything else gets the
		// improved error below so the user knows what they're missing.
		if c.property != "" && isExpressionBoundary(p.peek()) {
			if isBoolCol(cypherPropToCol(c.property)) {
				c.op = "="
				c.value = "1"
				return c, nil
			}
			return c, fmt.Errorf(
				"WHERE %s.%s needs an operator — saw %q (expected =, <>, >, <, >=, <=, CONTAINS, STARTS WITH, ENDS WITH, IS NULL, IS NOT NULL, =~)",
				c.variable, c.property, op)
		}
		if hint, ok := operatorHint(op); ok {
			return c, fmt.Errorf("unsupported operator: %s — %s", op, hint)
		}
		return c, fmt.Errorf("unsupported operator: %s", op)
	}
	return c, nil
}

// isExpressionBoundary reports whether tok terminates a WHERE leaf —
// i.e. the parser would expect the next operand here. Used by the
// #431 naked-boolean check so we can distinguish "missing operator"
// from "operator typo".
func isExpressionBoundary(tok token) bool {
	if tok.kind == "EOF" {
		return true
	}
	switch tok.value {
	case ")", "AND", "OR", "RETURN", "ORDER", "LIMIT":
		return true
	}
	return false
}

func (p *parser) parseReturn() ([]returnVar, error) {
	var vars []returnVar
	for {
		rv := returnVar{}
		t := p.peek()

		// Aggregation: COUNT(var), or AVG/MIN/MAX/SUM(var.property).
		// COUNT keeps the count-rows-only shape (no property); the
		// numeric aggregators take a column reference because their
		// value depends on which property to summarise (#432).
		if isAggFn(t.value) {
			fn := strings.ToUpper(t.value)
			p.next()
			p.skip("(")
			rv.fn = fn
			rv.variable = p.next().value
			if p.peek().value == "." {
				p.next()
				rv.property = p.next().value
			}
			p.skip(")")
		} else {
			// #578: catch unknown function calls (typo'd or unsupported)
			// before they parse as bare variable refs and silently
			// evaluate to null. Pre-fix `RETURN LENGTH(f.docstring)`
			// rendered as a column named `LENGTH` with every row null —
			// no warning, no error, the caller's audit silently
			// returned wrong results. Same UX class as #473's typo'd
			// properties; same fix shape: surface a clear pinchQL
			// error naming the offender + the supported set.
			tok := p.next()
			if p.peek().value == "(" {
				return nil, fmt.Errorf("pinchQL: unknown function %q in RETURN. Supported: COUNT, AVG, MIN, MAX, SUM (aggregators only). For per-row computations use a property reference like %s.docstring instead.", tok.value, strings.ToLower(tok.value))
			}
			rv.variable = tok.value
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
		// #421: map boolean literals to SQLite's INTEGER form ("1" /
		// "0") so the SQL pushdown path (`is_entry_point=?` against
		// an INTEGER column) matches under SQLite affinity. The
		// in-Go fallback (evalCondition) handles the same equivalence
		// via boolCoerceEqual, so callers that bypass pushdown still
		// get the right answer.
		switch tok.value {
		case "TRUE":
			return "1"
		case "FALSE":
			return "0"
		case "NULL":
			return ""
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
	// #438: scanned as sql.NullString so an unset column stays nil
	// in symRowToMap rather than coercing to "". IS NULL in pinchQL
	// then evaluates against an absent value the way Cypher expects.
	Signature  sql.NullString
	ReturnType sql.NullString
	Docstring  sql.NullString
	IsTest     bool
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

	// Push down simple WHERE conditions. #358 + #362: pushdown is only
	// safe when the WHERE tree is a pure AND chain of leaves — anything
	// richer (OR, paren-grouped, NOT-group) goes through Go evaluation
	// against q.where so boolean composition is honoured exactly.
	canPush := pushdownAllowed(q)
	var unpushed []condition
	if canPush {
		for _, c := range q.conditions {
			// #593: cross-column comparisons demoted to in-Go (see
			// runJoinQuery for full rationale). Symmetric across both
			// scan paths.
			if c.rhsProperty != "" {
				unpushed = append(unpushed, c)
				continue
			}
			if c.variable != pat.fromVar {
				continue
			}
			col := cypherPropToCol(c.property)
			if col != "" && pushableOp(c.op) {
				appendWhereOp(&sqlQ, &args, "", col, c)
			} else {
				unpushed = append(unpushed, c)
			}
		}
	}
	var filter whereExpr
	if canPush {
		filter = andChainFromConds(unpushed)
	} else {
		// #430: try to push the full WHERE tree (OR / paren / NOT
		// included) as a SQL boolean. Falling through to in-Go
		// evaluation here was the bug — the SQL scan capped at
		// maxRows()*2 before any filter ran, so OR predicates against
		// large corpora returned far fewer rows than they should.
		prefixFor := func(v string) (string, bool) {
			if v == pat.fromVar {
				return "", true
			}
			return "", false
		}
		if expr, exprArgs, ok := whereExprToSQL(q.where, prefixFor); ok {
			sqlQ += " AND " + expr
			args = append(args, exprArgs...)
			filter = nil
		} else {
			filter = q.where
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
		args = append(args, scanLimitFor(e.maxRows(), filter))
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
		if !matchesWhere(m, filter, reCache) {
			continue
		}
		nodes = append(nodes, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return buildResult(nodes, q)
}

// hasAggregation reports whether any RETURN variable in q is an
// aggregation (currently only COUNT). Aggregating queries must scan
// the full match set so the COUNT reflects cardinality, not the
// safety LIMIT (#308).
func hasAggregation(q *queryAST) bool {
	for _, rv := range q.returnVars {
		if rv.fn != "" {
			return true
		}
	}
	return false
}

// isAggFn reports whether tok is an aggregator function name. The
// tokenizer uppercases known KEYWORD entries (COUNT) but leaves
// IDENT-shaped function names case-sensitive — accept either form
// so RETURN avg(n.complexity) and RETURN AVG(n.complexity) both
// parse (#432).
func isAggFn(tok string) bool {
	switch strings.ToUpper(tok) {
	case "COUNT", "AVG", "MIN", "MAX", "SUM":
		return true
	}
	return false
}

// aggColName returns the output column name for an aggregator
// projection — alias if the user wrote AS, otherwise the canonical
// `FN(var.prop)` shape mirroring the source.
func aggColName(rv returnVar) string {
	if rv.alias != "" {
		return rv.alias
	}
	col := rv.variable
	if rv.property != "" {
		col = rv.variable + "." + rv.property
	}
	return rv.fn + "(" + col + ")"
}

// computeAgg evaluates rv.fn over the given row set. COUNT counts
// rows (matches Cypher / SQL semantics). AVG / MIN / MAX / SUM
// extract rv.variable.rv.property and parse as float64. Non-numeric
// rows skip silently (mirrors SQLite behaviour). Returns nil when
// the row set is empty for AVG / MIN / MAX (SQL-style NULL); SUM
// returns 0.0 over an empty set, COUNT returns 0.
func computeAgg(rows []map[string]any, rv returnVar) any {
	if rv.fn == "COUNT" {
		return len(rows)
	}
	key := rv.variable
	if rv.property != "" {
		key = rv.variable + "." + rv.property
	}
	var nums []float64
	for _, row := range rows {
		raw, ok := row[key]
		if !ok || raw == nil {
			continue
		}
		f, err := strconv.ParseFloat(fmt.Sprint(raw), 64)
		if err != nil {
			continue
		}
		nums = append(nums, f)
	}
	if len(nums) == 0 {
		if rv.fn == "SUM" {
			return 0.0
		}
		return nil
	}
	switch rv.fn {
	case "SUM":
		var s float64
		for _, n := range nums {
			s += n
		}
		return s
	case "AVG":
		var s float64
		for _, n := range nums {
			s += n
		}
		return s / float64(len(nums))
	case "MIN":
		m := nums[0]
		for _, n := range nums[1:] {
			if n < m {
				m = n
			}
		}
		return m
	case "MAX":
		m := nums[0]
		for _, n := range nums[1:] {
			if n > m {
				m = n
			}
		}
		return m
	}
	return nil
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
		a.extraction_confidence, a.signature, a.return_type, a.docstring, a.is_test,
		b.id, b.project_id, b.file_path, b.name, b.qualified_name, b.kind, b.language,
		b.start_byte, b.end_byte, b.start_line, b.end_line, b.is_exported, b.is_entry_point, b.complexity,
		b.extraction_confidence, b.signature, b.return_type, b.docstring, b.is_test,
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

	// Push down WHERE conditions. #358 + #362: tree-aware pushdown gate —
	// only AND-chains-of-leaves push to SQL; richer trees are fully
	// evaluated in Go via q.where (see runNodeScan for rationale).
	canPush := pushdownAllowed(q)
	var unpushed []condition
	if canPush {
		for _, c := range q.conditions {
			// #593: cross-column comparisons (rhsProperty != "")
			// can't push to SQL — the RHS references another row,
			// not a literal. Pre-fix this branch pushed `a.lang <> ?`
			// with c.value="" (the empty fallback after parsing
			// stripped the RHS), which silently let rows through.
			// Demote to in-Go evaluation where evalCondition returns
			// false for these and the warning emitter names them.
			if c.rhsProperty != "" {
				unpushed = append(unpushed, c)
				continue
			}
			tableAlias := "a"
			if c.variable == pat.toVar {
				tableAlias = "b"
			} else if c.variable != pat.fromVar {
				unpushed = append(unpushed, c)
				continue
			}
			col := cypherPropToCol(c.property)
			if col != "" && pushableOp(c.op) {
				appendWhereOp(&sqlQ, &args, tableAlias+".", col, c)
			} else {
				unpushed = append(unpushed, c)
			}
		}
	}
	var filter whereExpr
	if canPush {
		filter = andChainFromConds(unpushed)
	} else {
		// #430: same OR-pushdown attempt as runNodeScan, with the JOIN
		// alias mapping (a. for fromVar, b. for toVar). When all leaves
		// are pushable, SQL handles OR natively and the LIMIT clamp
		// becomes safe again.
		prefixFor := func(v string) (string, bool) {
			if v == pat.fromVar {
				return "a.", true
			}
			if v == pat.toVar {
				return "b.", true
			}
			return "", false
		}
		if expr, exprArgs, ok := whereExprToSQL(q.where, prefixFor); ok {
			sqlQ += " AND " + expr
			args = append(args, exprArgs...)
			filter = nil
		} else {
			filter = q.where
		}
	}

	// #308: same skip-when-aggregating treatment as runNodeScan.
	if !hasAggregation(q) {
		sqlQ += " LIMIT ?"
		args = append(args, scanLimitFor(e.maxRows(), filter))
	}

	rows, err := e.DB.QueryContext(ctx, sqlQ, args...)
	if err != nil {
		return nil, fmt.Errorf("join query: %w", err)
	}
	defer rows.Close()

	reCache := make(map[string]*regexp.Regexp)
	var resultRows []map[string]any
	// #591: dedup multi-sourced edges by (from_id, to_id, kind). The
	// edges table stores one row per source tag (per_file /
	// resolve_pass / binding_pass) by design — each pass owns its
	// source-tagged subset so atomic replaces don't wipe sibling-source
	// edges. But the user-facing semantic is "this caller calls this
	// callee once," so the JOIN must collapse to one row per logical
	// edge. Keep the highest-confidence variant when sources disagree
	// (typically resolve_pass at 0.7 wins over binding_pass at 0.4).
	// Map index → row in resultRows for in-place upgrade when a higher-
	// confidence variant of an already-seen edge appears.
	seenEdge := map[string]int{} // key → index in resultRows
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
		if !matchesWhere(m, filter, reCache) {
			continue
		}
		key := aNode.ID + "\x00" + bNode.ID + "\x00" + edgeKind
		if idx, dup := seenEdge[key]; dup {
			// Already have a row for this logical edge — keep the
			// higher-confidence variant.
			if pat.edgeVar != "" {
				if existing, _ := resultRows[idx][pat.edgeVar+".confidence"].(float64); conf > existing {
					resultRows[idx] = m
				}
			}
			continue
		}
		seenEdge[key] = len(resultRows)
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
//
// #426: planner inverts walk direction when only the end predicate is
// selective. `MATCH (a)-[:CALLS*1..3]->(b) WHERE b.name="X"` with no
// fromVar predicate would otherwise enumerate up to 100 a-candidates
// and run a CTE per start, fanning out 3 hops each — 10s timeout on
// real corpora. With inversion the same query walks inbound from the
// single b match, completing in milliseconds.
func (e *Executor) runBFS(ctx context.Context, q *queryAST, pat pattern) (*Result, error) {
	inverted := shouldInvertBFS(q, pat)
	startVar := pat.fromVar
	startKind := pat.fromKind
	if inverted {
		startVar = pat.toVar
		startKind = pat.toKind
	}

	// Find start nodes
	startQ := "SELECT " + symCols + " FROM symbols WHERE 1=1"
	var startArgs []any

	if e.ProjectID != "" {
		startQ += " AND project_id=?"
		startArgs = append(startArgs, e.ProjectID)
	}
	if startKind != "" {
		startQ += " AND kind=?"
		startArgs = append(startArgs, startKind)
	}
	// Start-node prefilter pushes start-var equalities into SQL. Only safe
	// when pushdownAllowed(q) — otherwise an OR or paren-grouped WHERE
	// could incorrectly exclude valid start nodes (e.g.
	// `WHERE a.name='X' OR a.name='Y'` flat-pushed as ANDed equalities
	// returns zero start nodes). q.where still drives the per-row match.
	if pushdownAllowed(q) {
		for _, c := range q.conditions {
			if c.variable != startVar {
				continue
			}
			col := cypherPropToCol(c.property)
			if col != "" && c.op == "=" {
				startQ += " AND " + col + "=?"
				startArgs = append(startArgs, c.value)
			}
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
		hops, err := e.bfsViaCTE(ctx, start.ID, edgeKinds, pat.minHops, maxDepth, e.ProjectID, e.maxRows(), inverted)
		if err != nil {
			return nil, fmt.Errorf("bfs traversal from %q: %w", start.ID, err)
		}
		for _, hop := range hops {
			m := make(map[string]any)
			// #426: when inverted, the CTE walks inbound from the
			// toVar match — so each hop *is* a fromVar candidate.
			// Project results in original orientation regardless.
			if inverted {
				for k, v := range symRowToMap(pat.fromVar, hop.node) {
					m[k] = v
				}
				for k, v := range symRowToMap(pat.toVar, start) {
					m[k] = v
				}
			} else {
				for k, v := range symRowToMap(pat.fromVar, start) {
					m[k] = v
				}
				for k, v := range symRowToMap(pat.toVar, hop.node) {
					m[k] = v
				}
			}
			m["_hop"] = hop.depth
			if !matchesWhere(m, q.where, reCache) {
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

// shouldInvertBFS reports whether a variable-length MATCH (a)->(b) plan
// should walk inbound from b instead of outbound from a. Heuristic:
// invert when there is at least one constant predicate on toVar and no
// constant predicate on fromVar. Selectivity is not measured — the
// existence of a toVar predicate is a strong-enough signal because the
// uninverted plan otherwise fans out from up to 100 fromVar candidates
// (a 3-hop CALLS BFS hits the 10s deadline on a 2k-symbol corpus).
//
// Pushdown gates: only invert when the WHERE clause is a flat AND chain
// (pushdownAllowed). OR / paren-grouped WHERE clauses may reference
// fromVar implicitly even when no direct equality predicate appears.
func shouldInvertBFS(q *queryAST, pat pattern) bool {
	if pat.toVar == "" || pat.fromVar == "" {
		return false
	}
	if !pushdownAllowed(q) {
		return false
	}
	var hasToConst, hasFromConst bool
	for _, c := range q.conditions {
		col := cypherPropToCol(c.property)
		if col == "" {
			continue
		}
		if c.op != "=" {
			continue
		}
		switch c.variable {
		case pat.fromVar:
			hasFromConst = true
		case pat.toVar:
			hasToConst = true
		}
	}
	return hasToConst && !hasFromConst
}

// bfsViaCTE uses a single recursive CTE to find all nodes reachable from startID
// within [minHops, maxHops] steps along edges of the given kinds.
// This replaces the old Go BFS loop that issued one SQL call per node per depth.
//
// #426: inbound=true flips the recursive step from "follow from_id→to_id"
// to "follow to_id→from_id" so the planner can walk caller graphs in
// reverse without re-shaping the query.
func (e *Executor) bfsViaCTE(ctx context.Context, startID string, kinds []string, minHops, maxHops int, projectID string, maxRows int, inbound bool) ([]bfsHop, error) {
	in := inPlaceholders(len(kinds))

	projectFilter := ""
	if projectID != "" {
		projectFilter = " AND e.project_id = ?"
	}

	// recursive step: outbound walks e.from_id → e.to_id, inbound flips.
	recursiveStep := "SELECT e.to_id, r.depth + 1 FROM reach r JOIN edges e ON e.from_id = r.id"
	if inbound {
		recursiveStep = "SELECT e.from_id, r.depth + 1 FROM reach r JOIN edges e ON e.to_id = r.id"
	}

	// UNION ALL + WHERE depth < maxHops terminates even on cyclic graphs.
	// GROUP BY id + MIN(depth) returns each reachable node once at shortest path.
	cteQ := `WITH RECURSIVE reach(id, depth) AS (
		SELECT ?, 0
		UNION ALL
		` + recursiveStep + ` AND e.kind IN (` + in + `)` + projectFilter + `
		WHERE r.depth < ?
	)
	SELECT s.id, s.project_id, s.file_path, s.name, s.qualified_name, s.kind, s.language,
		s.start_byte, s.end_byte, s.start_line, s.end_line, s.is_exported, s.is_entry_point, s.complexity,
		s.extraction_confidence, s.signature, s.return_type, s.docstring, s.is_test, MIN(r.depth) AS min_depth
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
		var isExp, isEntry, isTest sql.NullInt64
		var depth int
		if err := rows.Scan(
			&n.ID, &n.ProjectID, &n.FilePath, &n.Name, &n.QualifiedName, &n.Kind, &n.Language,
			&n.StartByte, &n.EndByte, &n.StartLine, &n.EndLine, &isExp, &isEntry, &n.Complexity,
			&n.ExtractionConfidence, &n.Signature, &n.ReturnType, &n.Docstring, &isTest, &depth,
		); err != nil {
			return nil, err
		}
		n.IsExported = isExp.Int64 != 0
		n.IsEntryPoint = isEntry.Int64 != 0
		n.IsTest = isTest.Int64 != 0
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
	hasAgg := false
	for _, rv := range q.returnVars {
		if rv.fn != "" {
			cols = append(cols, aggColName(rv))
			hasAgg = true
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

	if hasAgg {
		// #348: implicit GROUP BY when mixing non-aggregate columns with COUNT.
		// Standard Cypher/SQL semantics — `RETURN n.kind, COUNT(n)` should
		// group by n.kind and emit one row per kind, not collapse to a single
		// total row that silently drops the n.kind column. Pre-fix path treated
		// the presence of any COUNT as "single-row total" regardless of the
		// projection shape.
		// #432 extends the same pattern to AVG/MIN/MAX/SUM.
		var groupVars []returnVar
		var aggVars []returnVar
		for _, rv := range q.returnVars {
			if rv.fn != "" {
				aggVars = append(aggVars, rv)
			} else {
				groupVars = append(groupVars, rv)
			}
		}

		// No group-by columns → single-row aggregate path. Backward
		// compatible: `RETURN COUNT(n)` still returns one row.
		if len(groupVars) == 0 {
			// #360: explicit LIMIT 0 short-circuits even the
			// single-row aggregate path. `RETURN COUNT(f) LIMIT 0`
			// is the SQL idiom for "validate the query, no result"
			// and must return zero rows here too.
			if q.limit == 0 {
				return &Result{Columns: cols, Rows: []map[string]any{}, Total: 0}, nil
			}
			row := map[string]any{}
			for _, rv := range aggVars {
				row[aggColName(rv)] = computeAgg(allRows, rv)
			}
			return &Result{Columns: cols, Rows: []map[string]any{row}, Total: 1}, nil
		}

		// Group rows by the tuple of group-var values. The key is fmt.Sprint
		// of the tuple — the same approach `q.distinct` uses for row dedup
		// (line 1071), so behaviour is consistent.
		type groupBucket struct {
			values map[string]any
			rows   []map[string]any
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
			b.rows = append(b.rows, row)
		}

		// Emit one row per group, with each agg evaluated on the
		// group's rows.
		grouped := make([]map[string]any, 0, len(groups))
		for _, gk := range groupOrder {
			b := groups[gk]
			out := make(map[string]any, len(groupVars)+len(aggVars))
			for k, v := range b.values {
				out[k] = v
			}
			for _, rv := range aggVars {
				out[aggColName(rv)] = computeAgg(b.rows, rv)
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
		// #360: only treat absent LIMIT (q.limit==-1) as "default". An
		// explicit LIMIT 0 must return zero rows; pre-fix the `<= 0`
		// guard collapsed it to the 200-row default.
		if limit < 0 {
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

	// LIMIT — see #360 note above; -1 means "no LIMIT clause", 0 means
	// "explicit zero rows".
	limit := q.limit
	if limit < 0 {
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
	var isExp, isEntry, isTest sql.NullInt64
	if err := rows.Scan(
		&n.ID, &n.ProjectID, &n.FilePath, &n.Name, &n.QualifiedName, &n.Kind, &n.Language,
		&n.StartByte, &n.EndByte, &n.StartLine, &n.EndLine, &isExp, &isEntry, &n.Complexity,
		&n.ExtractionConfidence, &n.Signature, &n.ReturnType, &n.Docstring, &isTest,
	); err != nil {
		return nil, err
	}
	n.IsExported = isExp.Int64 != 0
	n.IsEntryPoint = isEntry.Int64 != 0
	n.IsTest = isTest.Int64 != 0
	return &n, nil
}

func scanJoinRow(rows *sql.Rows) (a, b *symRow, edgeKind string, conf float64, err error) {
	a = &symRow{}
	b = &symRow{}
	var isExpA, isEntryA, isTestA, isExpB, isEntryB, isTestB sql.NullInt64
	err = rows.Scan(
		&a.ID, &a.ProjectID, &a.FilePath, &a.Name, &a.QualifiedName, &a.Kind, &a.Language,
		&a.StartByte, &a.EndByte, &a.StartLine, &a.EndLine, &isExpA, &isEntryA, &a.Complexity,
		&a.ExtractionConfidence, &a.Signature, &a.ReturnType, &a.Docstring, &isTestA,
		&b.ID, &b.ProjectID, &b.FilePath, &b.Name, &b.QualifiedName, &b.Kind, &b.Language,
		&b.StartByte, &b.EndByte, &b.StartLine, &b.EndLine, &isExpB, &isEntryB, &b.Complexity,
		&b.ExtractionConfidence, &b.Signature, &b.ReturnType, &b.Docstring, &isTestB,
		&edgeKind, &conf,
	)
	a.IsExported = isExpA.Int64 != 0
	a.IsEntryPoint = isEntryA.Int64 != 0
	a.IsTest = isTestA.Int64 != 0
	b.IsExported = isExpB.Int64 != 0
	b.IsEntryPoint = isEntryB.Int64 != 0
	b.IsTest = isTestB.Int64 != 0
	return
}

func symRowToMap(varName string, n *symRow) map[string]any {
	prefix := varName + "."
	m := map[string]any{
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
		prefix + "is_test":                n.IsTest,
		prefix + "complexity":             n.Complexity,
		prefix + "extraction_confidence":  n.ExtractionConfidence,
	}
	// #438: nullable text columns. Use nil rather than "" so
	// `WHERE n.docstring IS NULL` distinguishes unset from empty,
	// matching SQL/Cypher semantics. The in-Go evaluator's IS NULL
	// check tests for nil specifically.
	if n.Signature.Valid {
		m[prefix+"signature"] = n.Signature.String
	} else {
		m[prefix+"signature"] = nil
	}
	if n.ReturnType.Valid {
		m[prefix+"return_type"] = n.ReturnType.String
	} else {
		m[prefix+"return_type"] = nil
	}
	if n.Docstring.Valid {
		m[prefix+"docstring"] = n.Docstring.String
	} else {
		m[prefix+"docstring"] = nil
	}
	return m
}

// appendWhereOp appends a SQL condition for a pushed-down Cypher WHERE clause.
// prefix is "" for single-table queries or "alias." for JOIN queries.
func appendWhereOp(sqlQ *string, args *[]any, prefix, col string, c condition) {
	inner, leafArgs, ok := condLeafToSQL(prefix, col, c)
	if !ok {
		return
	}
	*args = append(*args, leafArgs...)
	if c.negated {
		// condLeafToSQL already wraps with NOT for paren/OR pushdown
		// callers; appendWhereOp owns its own NOT wrapping for the
		// AND-chain path. Strip the inner NOT and re-wrap with the AND
		// prefix expected by the legacy emit shape.
		stripped := strings.TrimPrefix(inner, "NOT (")
		stripped = strings.TrimSuffix(stripped, ")")
		*sqlQ += " AND NOT (" + stripped + ")"
		return
	}
	*sqlQ += " AND " + inner
}

// scanLimitFor picks the SQL LIMIT for the row scan. When SQL handles
// the entire WHERE (filter==nil), maxRows*2 is plenty — SQL filters
// before counting against the LIMIT. When in-Go filtering is still
// needed (e.g. an `=~` regex leaf, or any other non-pushable op),
// the LIMIT applies to the *unfiltered* row set, so a tight regex
// against a wide kind+project scan can return 0 just because the
// matching rows live past row 400 (#435 / sibling of #430, #434).
//
// 50× the user limit (capped at 10000) lets the scan reach the
// matching rows on real corpora (4000 symbols, 2000 functions)
// while still bounding memory if someone runs against a 1M-symbol
// project. Aggregating queries opt out of LIMIT entirely (#308).
func scanLimitFor(maxRows int, filter whereExpr) int {
	if filter == nil {
		return maxRows * 2
	}
	limit := maxRows * 50
	if limit > 10000 {
		limit = 10000
	}
	return limit
}

// pushableOp reports whether condLeafToSQL knows how to render this
// operator as SQL. Used by the AND-chain pushdown gate to decide
// whether to emit SQL or post-filter in Go. Keep in sync with
// condLeafToSQL's switch.
func pushableOp(op string) bool {
	switch op {
	case "=", "<>", ">", "<", ">=", "<=",
		"CONTAINS", "STARTS WITH", "ENDS WITH",
		"IS NULL", "IS NOT NULL":
		return true
	}
	return false
}

// isBoolCol reports whether col holds a SQLite INTEGER 0/1 backing
// a Go bool field. Equality binds against these get the "1"/"0" /
// "true"/"false" coercion (#421); the row scan would otherwise see
// a TEXT bind arg that SQLite affinity refuses to convert.
func isBoolCol(col string) bool {
	switch col {
	case "is_exported", "is_entry_point", "is_test":
		return true
	}
	return false
}

// coerceBoolLiteral maps "true" / "false" (any case, with or without
// quotes already stripped) to "1" / "0". Pass-through for anything
// else.
func coerceBoolLiteral(v string) string {
	switch strings.ToLower(v) {
	case "true":
		return "1"
	case "false":
		return "0"
	}
	return v
}

// isZeroValuePredicate reports whether a `col=val` predicate is
// asking "where the column is absent or empty" — in which case the
// SQL emitter wraps the comparison in `(IS NULL OR col=?)` so NULL
// rows match (#606). The cases are:
//
//   - val=="" on any column — the user wrote "" meaning "no value"
//   - bool-column false (val=="0" after coerceBoolLiteral) — same
//     intent: "where this flag is unset"
//
// Non-zero literals keep the strict `col=?` semantics (NULL stays
// excluded), matching the natural reading of `WHERE col="hello"`.
func isZeroValuePredicate(col, val string) bool {
	if val == "" {
		return true
	}
	if isBoolCol(col) && val == "0" {
		return true
	}
	return false
}

// condLeafToSQL returns the SQL fragment for a single leaf condition
// without any leading boolean-connector glue, plus its bind args. The
// fragment is wrapped with `NOT (...)` when c.negated is set so it
// drops directly into a paren/OR tree from whereExprToSQL.
//
// Returns ok=false for unsupported operators (`=~`) — callers fall
// back to in-Go evaluation. Comparison operators (`>`, `<`, `>=`,
// `<=`, `<>`) push as parameterised SQL — SQLite's column affinity
// coerces the bind arg to the column's declared type, so `n.start_line
// > "4000"` against an INTEGER column compares numerically (#434).
func condLeafToSQL(prefix, col string, c condition) (string, []any, bool) {
	var inner string
	var args []any
	val := c.value
	if isBoolCol(col) {
		val = coerceBoolLiteral(val)
	}
	switch c.op {
	case "=":
		// #606: treat `col=""` and `col=<falsy-bool>` as "no value
		// extracted OR explicit zero" so the canonical "find
		// undocumented APIs" / "exclude tests" queries match NULL
		// rows. SQL standard `col=?` returns false for NULL by
		// tri-state logic; users writing the predicate mean "where
		// the value is absent or empty" and expect NULL to match.
		// Same UX class as #473/#578/#591/#593 — silent wrong
		// answer otherwise.
		if isZeroValuePredicate(col, val) {
			inner = "(" + prefix + col + " IS NULL OR " + prefix + col + "=?)"
		} else {
			inner = prefix + col + "=?"
		}
		args = append(args, val)
	case "<>":
		// #434: include rows where the column is NULL when comparing
		// inequality. SQL's `col <> ?` is FALSE on NULL by tri-state
		// logic; the in-Go evaluator (`actual != c.value` after a
		// `fmt.Sprint(nil)` → "<nil>") returned TRUE for NULL rows,
		// so preserve that semantics.
		inner = "(" + prefix + col + " IS NULL OR " + prefix + col + "<>?)"
		args = append(args, val)
	case ">", "<", ">=", "<=":
		// #434: comparison-operator pushdown. SQLite affinity converts
		// the string bind arg to the column type, so a query like
		// `WHERE n.start_line > 4000` works against an INTEGER column.
		inner = prefix + col + c.op + "?"
		args = append(args, val)
	case "CONTAINS":
		inner = prefix + col + " LIKE ?"
		args = append(args, "%"+c.value+"%")
	case "STARTS WITH":
		inner = prefix + col + " LIKE ?"
		args = append(args, c.value+"%")
	case "ENDS WITH":
		// #340: SQL pushdown for the suffix-match family.
		inner = prefix + col + " LIKE ?"
		args = append(args, "%"+c.value)
	case "IS NULL":
		// #342: NULL OR empty. SQLite's Go driver maps NULL TEXT to "".
		inner = "(" + prefix + col + " IS NULL OR " + prefix + col + " = '')"
	case "IS NOT NULL":
		inner = "(" + prefix + col + " IS NOT NULL AND " + prefix + col + " <> '')"
	default:
		return "", nil, false
	}
	if c.negated {
		inner = "NOT (" + inner + ")"
	}
	return inner, args, true
}

// whereExprToSQL recursively builds a SQL boolean expression for a
// WHERE tree. Returns ok=false if any leaf has an unknown column,
// references a variable not in prefixFor, or uses an operator
// condLeafToSQL doesn't support.
//
// This is the #430 fix path: when pushdownAllowed=false (OR / paren),
// the full row scan was capped at maxRows()*2 BEFORE the in-Go OR
// filter ran — so on a 4000-symbol corpus, a `.js OR .jsx OR .ts`
// query whose .js matches sat past the 400-row clamp returned 0.
// Pushing the full tree (OR included) to SQL makes the LIMIT clamp
// safe again because SQL filters before counting against it.
func whereExprToSQL(w whereExpr, prefixFor func(string) (string, bool)) (string, []any, bool) {
	switch e := w.(type) {
	case condExpr:
		// #593: cross-column comparisons can't push to SQL — the RHS
		// references another row, not a literal. Fall through to
		// in-Go evalCondition which returns false for these.
		if e.c.rhsProperty != "" {
			return "", nil, false
		}
		prefix, ok := prefixFor(e.c.variable)
		if !ok {
			return "", nil, false
		}
		col := cypherPropToCol(e.c.property)
		if col == "" {
			return "", nil, false
		}
		return condLeafToSQL(prefix, col, e.c)
	case binaryExpr:
		ls, la, lok := whereExprToSQL(e.left, prefixFor)
		if !lok {
			return "", nil, false
		}
		rs, ra, rok := whereExprToSQL(e.right, prefixFor)
		if !rok {
			return "", nil, false
		}
		op := " AND "
		if e.op == "OR" {
			op = " OR "
		}
		out := make([]any, 0, len(la)+len(ra))
		out = append(out, la...)
		out = append(out, ra...)
		return "(" + ls + op + rs + ")", out, true
	case notExpr:
		s, a, ok := whereExprToSQL(e.inner, prefixFor)
		if !ok {
			return "", nil, false
		}
		return "NOT (" + s + ")", a, true
	}
	return "", nil, false
}

// cypherPropToCol maps a Cypher property name to a SQL column name.
// Returning "" suppresses SQL pushdown — the condition then falls back
// to in-Go evaluation against the row map. #412: that fallback was
// silently undercounting `WHERE n.id="X"` queries because the SQL scan
// LIMIT (#308 — `e.maxRows()*2`) cut the row set BEFORE the in-Go
// id filter could reject non-matching edges. Adding `id` here pushes
// the filter to SQL where the LIMIT becomes irrelevant.
func cypherPropToCol(prop string) string {
	switch prop {
	case "id":
		return "id"
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
	case "start_byte":
		return "start_byte"
	case "end_byte":
		return "end_byte"
	case "complexity":
		return "complexity"
	case "extraction_confidence", "confidence":
		return "extraction_confidence"
	case "is_exported":
		// #421: bool/int columns. SQLite affinity converts the bind
		// arg ("1", "0", "true", "false") to INTEGER 0/1, so the
		// pushed SQL `is_exported=?` matches regardless of how the
		// caller spelled the bool. Falls through to the in-Go path
		// for unsupported operators where boolCoerceEqual handles
		// the same equivalence.
		return "is_exported"
	case "is_entry_point":
		return "is_entry_point"
	case "is_test":
		return "is_test"
	case "signature":
		// #438: nullable TEXT columns. SQL pushdown of `IS NULL` /
		// `IS NOT NULL` / `=` works directly against the column.
		return "signature"
	case "return_type":
		return "return_type"
	case "docstring":
		return "docstring"
	default:
		return ""
	}
}

// matchesConditions evaluates a flat []condition slice — kept as a
// helper for tests that drive operator semantics directly. The live
// row-evaluation path is matchesWhere over the queryAST.where tree
// (#362); this helper covers the same ground for the AND/OR-only
// flat shape (where flattenWhere succeeds).
func matchesConditions(row map[string]any, conds []condition) bool {
	return matchesConditionsWithCache(row, conds, nil)
}

func matchesConditionsWithCache(row map[string]any, conds []condition, reCache map[string]*regexp.Regexp) bool {
	if len(conds) == 0 {
		return true
	}
	// Walks left-to-right honouring #358 AND/OR connectors. The tree
	// path (matchesWhere over queryAST.where) handles paren grouping
	// and group-NOT; this helper still gives the same answer when the
	// tree was a left-leaning flat chain (which flattenWhere checks).
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
	// #593: column-vs-column comparisons are unsupported. Return false
	// so the predicate filters everything out — consistent with the
	// #473 "unknown property → 0 rows + warning" handling. Without
	// this the comparison silently evaluates to true (RHS treated as
	// a literal that doesn't match any column value), inflating the
	// result set and confusing the agent.
	if c.rhsProperty != "" {
		return false
	}
	key := c.variable + "." + c.property
	actual := fmt.Sprint(row[key])

	switch c.op {
	case "=":
		// #606: NULL-as-zero. When the user writes `col=""` or a
		// falsy bool predicate, NULL rows must match — same logic
		// as the SQL emitter's isZeroValuePredicate path. Without
		// this the in-Go evaluation (used when the predicate can't
		// push to SQL, e.g. inside an OR tree with an unsupported
		// sibling) silently zero-rows the canonical "find
		// undocumented APIs" query.
		raw, present := row[key]
		isNullRow := !present || raw == nil || actual == "<nil>"
		if isNullRow {
			if c.value == "" {
				return true
			}
			// Bool columns: NULL coerces to false, so `col=false`
			// matches NULL.
			col := cypherPropToCol(c.property)
			if isBoolCol(col) && (c.value == "false" || c.value == "0") {
				return true
			}
		}
		if actual == c.value {
			return true
		}
		// #421 (bool col coercion) + #431 (naked-bool predicate)
		// both want "1" / "0" / "true" / "false" to compare equal
		// when the row holds a Go bool and the WHERE wrote any of
		// the four spellings. boolCoerceEqual handles both.
		return boolCoerceEqual(actual, c.value)
	case "<>":
		if actual == c.value {
			return false
		}
		return !boolCoerceEqual(actual, c.value)
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

// boolCoerceEqual reports whether two stringified values are equal
// under the "1" / "0" / "true" / "false" equivalence used by SQLite
// INTEGER columns scanned into Go bool. The Symbol struct's
// IsExported / IsEntryPoint fields are bool; fmt.Sprint on them
// yields "true" / "false". The caller's literal in WHERE could be
// any of "1", "true", "TRUE" (#323 normalises to "true"), or even
// "0"/"false" — all should resolve to the same equality (#421).
func boolCoerceEqual(a, b string) bool {
	an, aok := boolNorm(a)
	bn, bok := boolNorm(b)
	if !aok || !bok {
		return false
	}
	return an == bn
}

func boolNorm(s string) (bool, bool) {
	switch s {
	case "1", "true", "TRUE":
		return true, true
	case "0", "false", "FALSE":
		return false, true
	}
	return false, false
}

// minHops is referenced below to avoid unused variable warnings.
func (q *queryAST) minHops() int {
	if len(q.patterns) == 0 {
		return 1
	}
	return q.patterns[0].minHops
}
