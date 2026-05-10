package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// Integration test for #247 #3: package-level vars/consts extracted as
// Variable symbols + cross-file READS edges resolved against the
// project's symbol table.
//
// Shape of the test corpus:
//   limits.go : const MaxRetries = 3
//   config.go : var Cache map[string]int
//   handler.go: func Foo() { _ = Cache; _ = MaxRetries; helper() }
//               func helper() { return }
//
// Assertions:
//   - Cache and MaxRetries surface as Variable symbols.
//   - Foo has READS edges to Cache and MaxRetries (cross-file).
//   - Foo's call to helper() produces a CALLS edge, NOT a READS edge,
//     even though `helper` is also an Ident in Foo's body.
//   - helper itself doesn't get spurious READS to MaxRetries (it
//     doesn't reference it; only Foo does).

const fixtureLimits = `package svc

// MaxRetries caps retry attempts per request.
const MaxRetries = 3
`

const fixtureConfig = `package svc

// Cache holds in-memory entries.
var Cache map[string]int
`

const fixtureHandler = `package svc

// Foo reads both Cache and MaxRetries and calls helper.
func Foo() {
	_ = Cache
	_ = MaxRetries
	helper()
}

func helper() {
	return
}
`

func TestIndex_ReadsEdges_VariablesExtractedAsSymbols(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/limits.go", fixtureLimits)
	writeFile(t, dir, "svc/config.go", fixtureConfig)
	writeFile(t, dir, "svc/handler.go", fixtureHandler)
	pid := db.ProjectIDFromPath(dir)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	for _, name := range []string{"Cache", "MaxRetries"} {
		syms, err := store.GetSymbolsByName(pid, name, 5)
		if err != nil {
			t.Fatalf("GetSymbolsByName(%q): %v", name, err)
		}
		if len(syms) == 0 {
			t.Errorf("Variable symbol %q not extracted", name)
			continue
		}
		if syms[0].Kind != "Variable" {
			t.Errorf("%s.Kind = %q, want Variable (#247 #3 ValueSpec extraction)", name, syms[0].Kind)
		}
	}
}

func TestIndex_ReadsEdges_FooReadsCacheAndMaxRetries(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/limits.go", fixtureLimits)
	writeFile(t, dir, "svc/config.go", fixtureConfig)
	writeFile(t, dir, "svc/handler.go", fixtureHandler)
	_ = db.ProjectIDFromPath(dir) // referenced indirectly via the indexer

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	fooID := db.MakeSymbolID("svc/handler.go", "svc.Foo", "Function")
	cacheID := db.MakeSymbolID("svc/config.go", "svc.Cache", "Variable")
	maxID := db.MakeSymbolID("svc/limits.go", "svc.MaxRetries", "Variable")

	// Inbound edges to Cache must include Foo with READS.
	inboundCache, err := store.EdgesTo(cacheID, nil)
	if err != nil {
		t.Fatalf("EdgesTo Cache: %v", err)
	}
	if !hasEdge(inboundCache, fooID, "READS") {
		t.Errorf("expected READS edge Foo → Cache:\n  inbound: %v", inboundCache)
	}

	// Inbound to MaxRetries similarly.
	inboundMax, err := store.EdgesTo(maxID, nil)
	if err != nil {
		t.Fatalf("EdgesTo MaxRetries: %v", err)
	}
	if !hasEdge(inboundMax, fooID, "READS") {
		t.Errorf("expected READS edge Foo → MaxRetries:\n  inbound: %v", inboundMax)
	}
}

// helper() is called from Foo; the Ident `helper` shouldn't produce
// a spurious READS edge to itself or to anything else. Only the CALLS
// edge resolves (helper is a Function, not a Variable).
func TestIndex_ReadsEdges_FunctionCallNotResolvedAsRead(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/limits.go", fixtureLimits)
	writeFile(t, dir, "svc/config.go", fixtureConfig)
	writeFile(t, dir, "svc/handler.go", fixtureHandler)
	_ = db.ProjectIDFromPath(dir)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	helperID := db.MakeSymbolID("svc/handler.go", "svc.helper", "Function")
	inboundHelper, err := store.EdgesTo(helperID, nil)
	if err != nil {
		t.Fatalf("EdgesTo helper: %v", err)
	}
	for _, e := range inboundHelper {
		if e.Kind == "READS" {
			t.Errorf("Function call surfaced as READS edge (should be CALLS only): %v", e)
		}
	}
	// At least the CALLS edge from Foo should still be present —
	// READS extraction must not regress CALLS resolution.
	fooID := db.MakeSymbolID("svc/handler.go", "svc.Foo", "Function")
	if !hasEdge(inboundHelper, fooID, "CALLS") {
		t.Errorf("CALLS edge Foo → helper missing; READS extraction regressed CALLS resolution:\n  inbound: %v", inboundHelper)
	}
}

// helper() doesn't reference Cache/MaxRetries, so neither should
// surface in inbound edges from helper. Pin so a future regression
// in dedupe / scope / over-emission doesn't add cross-function
// false positives.
func TestIndex_ReadsEdges_HelperHasNoSpuriousReads(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/limits.go", fixtureLimits)
	writeFile(t, dir, "svc/config.go", fixtureConfig)
	writeFile(t, dir, "svc/handler.go", fixtureHandler)
	_ = db.ProjectIDFromPath(dir)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	helperID := db.MakeSymbolID("svc/handler.go", "svc.helper", "Function")
	outbound, err := store.EdgesFrom(helperID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom helper: %v", err)
	}
	for _, e := range outbound {
		if e.Kind == "READS" {
			t.Errorf("helper has unexpected outbound READS edge: %v", e)
		}
	}
}

// Repeated references to the same Variable from the same function
// must produce ONE READS edge, not N. This pins the seen[key] dedupe
// path in resolveReads — without it, a function with `_ = Cache; _ =
// Cache` would emit two duplicate edges and inflate trace fan-in.
// Also exercises the QN/name lookup-cache hit paths (second lookup of
// the same identifier returns the cached entry instead of re-querying).
func TestIndex_ReadsEdges_DedupesRepeatedReadsToSameTarget(t *testing.T) {
	const repeated = `package svc

// FooRepeats reads Cache three times — should still produce one
// READS edge to Cache.
func FooRepeats() {
	_ = Cache
	_ = Cache
	_ = Cache
}
`
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/config.go", fixtureConfig)
	writeFile(t, dir, "svc/repeats.go", repeated)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	cacheID := db.MakeSymbolID("svc/config.go", "svc.Cache", "Variable")
	inbound, err := store.EdgesTo(cacheID, nil)
	if err != nil {
		t.Fatalf("EdgesTo Cache: %v", err)
	}
	fooID := db.MakeSymbolID("svc/repeats.go", "svc.FooRepeats", "Function")
	count := 0
	for _, e := range inbound {
		if e.Kind == "READS" && e.FromID == fooID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("READS edge count from FooRepeats → Cache = %d, want 1 (dedup must collapse N references to one edge)", count)
	}
}

// hasEdge reports whether the slice contains an edge with the given
// FromID and Kind. Edges-to / edges-from queries return both endpoints
// in a uniform shape; the caller already knows the target side, so we
// only need to verify the other end + the kind.
func hasEdge(edges []db.Edge, otherEndID, kind string) bool {
	for _, e := range edges {
		if e.Kind == kind && (e.FromID == otherEndID || e.ToID == otherEndID) {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// WRITES edges (#247 #3 follow-up)
// ─────────────────────────────────────────────────────────────────────────────

// fixtureWritesPatterns covers every WRITES-emitting AST shape:
//   - Pure write `=`         → WRITES only
//   - Read-then-write `+= /= = X+1` → WRITES + READS (compound, OR
//                                       same-name-on-both-sides)
//   - IncDecStmt             → WRITES only
//   - Short var decl `:=`    → neither (introduces local, not a write)
const fixtureWritesPatterns = `package pkg

var Counter int
var Cache map[string]int
var Limit int

// PureWrite assigns to Cache; no read of Cache.
func PureWrite() {
	Cache = make(map[string]int)
}

// ReadAndWrite reads + writes Counter (compound expr).
func ReadAndWrite() {
	Counter = Counter + 1
}

// IncOnly increments Counter — should emit WRITES via IncDecStmt.
func IncOnly() {
	Counter++
}

// LocalOnly uses :=, which introduces a local; the package-level
// Counter must NOT see a WRITES edge from this function.
func LocalOnly() {
	Counter := 99
	_ = Counter
}

// ReadOnly reads Limit; no write to it.
func ReadOnly() int {
	return Limit
}
`

func TestIndex_WritesEdges_PureWriteEmitsOnlyWrites(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "pkg/file.go", fixtureWritesPatterns)
	pid := db.ProjectIDFromPath(dir)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}
	_ = pid

	purewriteID := db.MakeSymbolID("pkg/file.go", "pkg.PureWrite", "Function")
	cacheID := db.MakeSymbolID("pkg/file.go", "pkg.Cache", "Variable")

	outbound, err := store.EdgesFrom(purewriteID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom PureWrite: %v", err)
	}
	if !hasEdge(outbound, cacheID, "WRITES") {
		t.Errorf("PureWrite must emit WRITES → Cache:\n  outbound: %v", outbound)
	}
	for _, e := range outbound {
		if e.ToID == cacheID && e.Kind == "READS" {
			t.Errorf("PureWrite emitted spurious READS → Cache (LHS-only assign should not be a read): %v", e)
		}
	}
}

func TestIndex_WritesEdges_CompoundProducesBothKinds(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "pkg/file.go", fixtureWritesPatterns)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	rwID := db.MakeSymbolID("pkg/file.go", "pkg.ReadAndWrite", "Function")
	counterID := db.MakeSymbolID("pkg/file.go", "pkg.Counter", "Variable")

	outbound, err := store.EdgesFrom(rwID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom ReadAndWrite: %v", err)
	}
	if !hasEdge(outbound, counterID, "READS") {
		t.Errorf("ReadAndWrite must emit READS → Counter (RHS reference):\n  outbound: %v", outbound)
	}
	if !hasEdge(outbound, counterID, "WRITES") {
		t.Errorf("ReadAndWrite must emit WRITES → Counter (LHS assignment):\n  outbound: %v", outbound)
	}
}

func TestIndex_WritesEdges_IncDecStmtEmitsWrites(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "pkg/file.go", fixtureWritesPatterns)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	incID := db.MakeSymbolID("pkg/file.go", "pkg.IncOnly", "Function")
	counterID := db.MakeSymbolID("pkg/file.go", "pkg.Counter", "Variable")

	outbound, err := store.EdgesFrom(incID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom IncOnly: %v", err)
	}
	if !hasEdge(outbound, counterID, "WRITES") {
		t.Errorf("Counter++ must emit WRITES → Counter:\n  outbound: %v", outbound)
	}
}

// Short-var-decls (`:=`) introduce locals. The package-level Counter
// must NOT see WRITES from a function that locally shadows the name.
// Without this gate, refactors that rename Counter would mis-target
// `LocalOnly` even though it has no relation to the package var.
func TestIndex_WritesEdges_ShortVarDeclSkipsWrites(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "pkg/file.go", fixtureWritesPatterns)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	localID := db.MakeSymbolID("pkg/file.go", "pkg.LocalOnly", "Function")
	counterID := db.MakeSymbolID("pkg/file.go", "pkg.Counter", "Variable")

	outbound, err := store.EdgesFrom(localID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom LocalOnly: %v", err)
	}
	for _, e := range outbound {
		if e.ToID == counterID && e.Kind == "WRITES" {
			t.Errorf("short-var-decl `Counter := 99` produced a spurious WRITES → package-level Counter: %v", e)
		}
	}
}

// fixtureControlFlow exercises the walker's non-trivial AST branches:
// IfStmt (init+cond+else), ForStmt (init+cond+post), RangeStmt with
// both := and = forms, SwitchStmt + CaseClause, TypeSwitchStmt,
// SelectStmt + CommClause, LabeledStmt. Without exercise across these
// shapes, an extractGoReads regression in any one branch (eg. failing
// to recurse into a switch case body) would slip past the other
// fixture-driven tests because they all use straight-line code.
const fixtureControlFlow = `package pkg

var Cap int
var State int
var Items []int

// AllShapes bundles every control-flow shape so extractor walks each
// branch at least once. The expected READS/WRITES targets are pinned
// in the test below; we don't re-list them here so the fixture stays
// readable.
func AllShapes(thing interface{}) {
	// IfStmt with init + else
	if x := State; x > 0 {
		Cap = x
	} else {
		_ = Cap
	}

	// ForStmt with init/cond/post (post is an IncDecStmt on a local —
	// non-Ident target hits the IncDecStmt walkRead branch).
	for i := 0; i < Cap; i++ {
		_ = i
	}

	// RangeStmt with assignment form (k is a package-level write target).
	var k int
	for k = range Items {
		State = k
	}

	// RangeStmt with short-var-decl (k local — no write to package var).
	for k := range Items {
		_ = k
	}

	// SwitchStmt + CaseClause
	switch State {
	case 0:
		Cap = 0
	case 1:
		_ = Cap
	}

	// TypeSwitchStmt
	switch t := thing.(type) {
	case int:
		_ = t
	default:
		_ = State
	}

	// SelectStmt + CommClause (with a default — exercises the empty
	// Comm branch that walkRead would otherwise miss).
	ch := make(chan int)
	select {
	case v := <-ch:
		_ = v
	default:
		_ = State
	}

	// LabeledStmt wrapping a for — extractor must descend through
	// the label to reach the loop body's writes.
Outer:
	for {
		Cap++
		break Outer
	}
}
`

// AllShapes exercises the walker across many control-flow branches at
// once. The assertions are intentionally narrow — we pin the load-
// bearing edges (Cap WRITES from the if-body, State WRITES from the
// switch's range-loop body, Cap WRITES from the labeled for) without
// over-specifying anything that would make the test brittle to AST
// changes. This is primarily a coverage hop: the regression value is
// in any future change to extractGoReads's switch ladder breaking
// extraction silently.
func TestIndex_WritesEdges_ControlFlowShapesEmitEdges(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "pkg/file.go", fixtureControlFlow)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	allID := db.MakeSymbolID("pkg/file.go", "pkg.AllShapes", "Function")
	capID := db.MakeSymbolID("pkg/file.go", "pkg.Cap", "Variable")
	stateID := db.MakeSymbolID("pkg/file.go", "pkg.State", "Variable")

	outbound, err := store.EdgesFrom(allID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom AllShapes: %v", err)
	}
	if !hasEdge(outbound, capID, "WRITES") {
		t.Errorf("WRITES → Cap missing — at least one of the if/switch/labeled-for write paths must emit:\n  outbound: %v", outbound)
	}
	if !hasEdge(outbound, capID, "READS") {
		t.Errorf("READS → Cap missing — `else { _ = Cap }` and `for ... < Cap` must emit:\n  outbound: %v", outbound)
	}
	if !hasEdge(outbound, stateID, "WRITES") {
		t.Errorf("WRITES → State missing — `State = k` inside the range loop body must emit:\n  outbound: %v", outbound)
	}
}

// Pure read functions still get the READS edge (regression gate
// against the WRITES extension breaking the original READS path).
func TestIndex_WritesEdges_ReadOnlyKeepsReadsBehaviour(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "pkg/file.go", fixtureWritesPatterns)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	roID := db.MakeSymbolID("pkg/file.go", "pkg.ReadOnly", "Function")
	limitID := db.MakeSymbolID("pkg/file.go", "pkg.Limit", "Variable")

	outbound, err := store.EdgesFrom(roID, nil)
	if err != nil {
		t.Fatalf("EdgesFrom ReadOnly: %v", err)
	}
	if !hasEdge(outbound, limitID, "READS") {
		t.Errorf("ReadOnly must still emit READS → Limit (WRITES split must not regress READS):\n  outbound: %v", outbound)
	}
	for _, e := range outbound {
		if e.ToID == limitID && e.Kind == "WRITES" {
			t.Errorf("ReadOnly emitted spurious WRITES → Limit (no assignment in body): %v", e)
		}
	}
}
