package ast

import (
	"strings"
	"testing"
)

// AST-level unit tests for the Go reads/writes extractor's walk
// function. Integration tests in internal/index exercise the full
// resolveReads pipeline; these target extractGoReads directly via
// Extract so coverage lands in internal/ast.
//
// Each subtest is a tiny fixture that hits one or two AST shapes the
// switch ladder in extractGoReads cares about (#247 #3 + WRITES
// follow-up). Without coverage on this layer, regressing one branch
// (say, dropping the LabeledStmt case) would silently break extraction
// for that shape — the integration tests would still pass for the
// other shapes that route through different branches.

func TestExtractGo_ReadsWrites_AssignStmt(t *testing.T) {
	src := []byte(`package p

var Cap int

func F() {
	Cap = 1
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "Cap", "WRITES")
}

func TestExtractGo_ReadsWrites_CompoundReadAndWrite(t *testing.T) {
	src := []byte(`package p

var Counter int

func Inc() {
	Counter = Counter + 1
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.Inc", "Counter", "WRITES")
	requireEdge(t, edges, "p.Inc", "Counter", "READS")
}

func TestExtractGo_ReadsWrites_IncDec(t *testing.T) {
	src := []byte(`package p

var Counter int

func Tick() {
	Counter++
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.Tick", "Counter", "WRITES")
}

func TestExtractGo_ReadsWrites_ShortVarDeclSuppressesWrite(t *testing.T) {
	src := []byte(`package p

var Counter int

func Local() {
	Counter := 99
	_ = Counter
}
`)
	edges := extractGoEdges(t, src)
	for _, e := range edges {
		if e.FromQN == "p.Local" && e.ToName == "Counter" && e.Kind == "WRITES" {
			t.Errorf("short-var-decl Counter := 99 must NOT emit WRITES → Counter; got %v", e)
		}
	}
}

func TestExtractGo_ReadsWrites_IfElseBranches(t *testing.T) {
	src := []byte(`package p

var State int
var Cap int

func F() {
	if x := State; x > 0 {
		Cap = x
	} else {
		_ = Cap
	}
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "Cap", "WRITES")
	requireEdge(t, edges, "p.F", "Cap", "READS")
	requireEdge(t, edges, "p.F", "State", "READS")
}

func TestExtractGo_ReadsWrites_ForStmt(t *testing.T) {
	src := []byte(`package p

var Cap int

func F() {
	for i := 0; i < Cap; i++ {
		_ = i
	}
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "Cap", "READS")
}

func TestExtractGo_ReadsWrites_RangeStmtAssignForm(t *testing.T) {
	src := []byte(`package p

var Items []int
var K int

func F() {
	for K = range Items {
		_ = K
	}
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "K", "WRITES")
	requireEdge(t, edges, "p.F", "Items", "READS")
}

func TestExtractGo_ReadsWrites_RangeStmtShortDeclForm(t *testing.T) {
	src := []byte(`package p

var Items []int

func F() {
	for k := range Items {
		_ = k
	}
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "Items", "READS")
	for _, e := range edges {
		if e.FromQN == "p.F" && e.ToName == "k" && e.Kind == "WRITES" {
			t.Errorf("`for k := range` must NOT emit WRITES on local k; got %v", e)
		}
	}
}

func TestExtractGo_ReadsWrites_SwitchStmt(t *testing.T) {
	src := []byte(`package p

var State int
var Cap int

func F() {
	switch State {
	case 0:
		Cap = 0
	case 1:
		_ = Cap
	}
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "State", "READS")
	requireEdge(t, edges, "p.F", "Cap", "WRITES")
	requireEdge(t, edges, "p.F", "Cap", "READS")
}

func TestExtractGo_ReadsWrites_TypeSwitchStmt(t *testing.T) {
	src := []byte(`package p

var State int

func F(thing interface{}) {
	switch t := thing.(type) {
	case int:
		_ = t
	default:
		_ = State
	}
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "State", "READS")
}

func TestExtractGo_ReadsWrites_SelectStmt(t *testing.T) {
	src := []byte(`package p

var State int

func F() {
	ch := make(chan int)
	select {
	case v := <-ch:
		_ = v
	default:
		_ = State
	}
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "State", "READS")
}

func TestExtractGo_ReadsWrites_LabeledStmt(t *testing.T) {
	src := []byte(`package p

var Cap int

func F() {
Outer:
	for {
		Cap++
		break Outer
	}
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "Cap", "WRITES")
}

func TestExtractGo_ReadsWrites_ReturnExprIsRead(t *testing.T) {
	src := []byte(`package p

var Limit int

func F() int {
	return Limit
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "Limit", "READS")
	for _, e := range edges {
		if e.FromQN == "p.F" && e.ToName == "Limit" && e.Kind == "WRITES" {
			t.Errorf("return Limit must NOT emit WRITES; got %v", e)
		}
	}
}

// #1062: a READS inside `go func(){ ... }()` was invisible to the
// extractor because the walker stopped at the FuncLit boundary. Caught
// dogfooding: sessionFlushFast (server.go:64) was used inside
// StartSessionFlusher's launched goroutine and showed as dead code.
func TestExtractGo_ReadsWrites_InsideGoFuncLit(t *testing.T) {
	src := []byte(`package p

var Cadence int

func StartTicker() {
	go func() {
		_ = Cadence
	}()
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.StartTicker", "Cadence", "READS")
}

// #1062: defer func(){ ... }() — same FuncLit-as-CallExpr-Fun shape,
// pre-fix never produced a READS for State.
func TestExtractGo_ReadsWrites_InsideDeferFuncLit(t *testing.T) {
	src := []byte(`package p

var State int

func WithCleanup() {
	defer func() {
		_ = State
	}()
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.WithCleanup", "State", "READS")
}

// #1062: function literal passed as a callback arg — was partially
// covered (default-case ast.Inspect inside walkRead reaches Idents),
// but the WRITES branch in walk() never ran for assignments inside,
// so `Counter = 1` inside the callback dropped to a READS instead.
func TestExtractGo_ReadsWrites_InsideCallbackFuncLitWrite(t *testing.T) {
	src := []byte(`package p

var Counter int

func register(cb func()) { cb() }

func F() {
	register(func() {
		Counter = 1
	})
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "Counter", "WRITES")
}

// #1062: assignment of a closure to a local, then call — the body
// references Limit, which must surface as a READS on the outer function.
func TestExtractGo_ReadsWrites_InsideAssignedClosureBody(t *testing.T) {
	src := []byte(`package p

var Limit int

func F() {
	fn := func() int { return Limit }
	_ = fn
}
`)
	edges := extractGoEdges(t, src)
	requireEdge(t, edges, "p.F", "Limit", "READS")
}

func extractGoEdges(t *testing.T, src []byte) []ExtractedEdge {
	t.Helper()
	r := Extract(src, "Go", "p/file.go")
	if r == nil {
		t.Fatal("Extract returned nil")
	}
	return r.Edges
}

func requireEdge(t *testing.T, edges []ExtractedEdge, from, to, kind string) {
	t.Helper()
	for _, e := range edges {
		if e.FromQN == from && e.ToName == to && strings.EqualFold(e.Kind, kind) {
			return
		}
	}
	t.Errorf("expected %s edge %s → %s; got %v", kind, from, to, edges)
}
