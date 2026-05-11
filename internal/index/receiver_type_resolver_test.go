package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #423 piece 3: receiver-type-aware resolveCalls. Pre-fix, polymorphic
// method names (Close, Read, Write...) were dropped by
// isPolymorphicInterfaceMethodName because the existing receiver-
// method fallback couldn't tell which type's Close was meant.
// With the new path, receiver_type + struct_fields binds the call
// to the precise method.

const piece3FieldChainSrc = `package svc

type Cache struct{}
func (c *Cache) Close() {}

type Connection struct{}
func (c *Connection) Close() {}

type Service struct{
	cache *Cache
	conn  *Connection
}

func (s *Service) Shutdown() {
	s.cache.Close()
	s.conn.Close()
}
`

// TestResolveCalls_RecvFieldMethod_BindsToCorrectType is the headline
// dead_code FP fix: s.cache.Close() must bind to (*Cache).Close, NOT
// to (*Connection).Close, even though "Close" is on the polymorphic
// blocklist. Symmetrically for s.conn.Close().
func TestResolveCalls_RecvFieldMethod_BindsToCorrectType(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/svc.go", piece3FieldChainSrc)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	pid := db.ProjectIDFromPath(dir)

	// Locate both Close methods by parent.
	closes, err := store.GetSymbolsByName(pid, "Close", 5)
	if err != nil {
		t.Fatalf("GetSymbolsByName Close: %v", err)
	}
	var cacheClose, connClose string
	for _, s := range closes {
		if s.Kind != "Method" {
			continue
		}
		switch s.Parent {
		case "svc.*Cache":
			cacheClose = s.ID
		case "svc.*Connection":
			connClose = s.ID
		}
	}
	if cacheClose == "" || connClose == "" {
		t.Fatalf("expected both Close methods extracted; cache=%q conn=%q", cacheClose, connClose)
	}

	// Inbound trace on each Close — must include Shutdown exactly once.
	for name, id := range map[string]string{"Cache.Close": cacheClose, "Connection.Close": connClose} {
		results, err := store.TraceViaCTEScoped(pid, id, "inbound", []string{"CALLS"}, 3)
		if err != nil {
			t.Fatalf("TraceViaCTEScoped %s: %v", name, err)
		}
		shutdownCallers := 0
		for _, r := range results {
			sym, err := store.GetSymbol(r.SymbolID)
			if err != nil || sym == nil {
				continue
			}
			if sym.Name == "Shutdown" {
				shutdownCallers++
			}
		}
		if shutdownCallers != 1 {
			t.Errorf("%s: got %d Shutdown callers, want 1 (#423 piece 3)", name, shutdownCallers)
		}
	}
}

const piece3DirectMethodSrc = `package svc

type Worker struct{}

func (w *Worker) String() string { return "" }

func (w *Worker) Run() {
	_ = w.String()
}

type Other struct{}
func (o *Other) String() string { return "" }
`

// TestResolveCalls_RecvMethod_BindsToReceiverType: a 2-segment ToName
// like "w.String" inside (*Worker).Run resolves precisely to
// (*Worker).String even though String is on the polymorphic blocklist
// and another struct (*Other) also has String.
func TestResolveCalls_RecvMethod_BindsToReceiverType(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/worker.go", piece3DirectMethodSrc)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	pid := db.ProjectIDFromPath(dir)
	syms, _ := store.GetSymbolsByName(pid, "String", 5)
	var workerString, otherString string
	for _, s := range syms {
		if s.Kind != "Method" {
			continue
		}
		switch s.Parent {
		case "svc.*Worker":
			workerString = s.ID
		case "svc.*Other":
			otherString = s.ID
		}
	}
	if workerString == "" || otherString == "" {
		t.Fatalf("expected both String methods extracted; worker=%q other=%q", workerString, otherString)
	}

	// (*Worker).String must have Run as caller.
	results, _ := store.TraceViaCTEScoped(pid, workerString, "inbound", []string{"CALLS"}, 3)
	runCount := 0
	for _, r := range results {
		sym, _ := store.GetSymbol(r.SymbolID)
		if sym != nil && sym.Name == "Run" && sym.Parent == "svc.*Worker" {
			runCount++
		}
	}
	if runCount != 1 {
		t.Errorf("(*Worker).String inbound: got %d Run callers, want 1", runCount)
	}

	// (*Other).String must NOT have Run as caller — that would be the
	// pre-fix false bind from name-only resolution.
	results, _ = store.TraceViaCTEScoped(pid, otherString, "inbound", []string{"CALLS"}, 3)
	for _, r := range results {
		sym, _ := store.GetSymbol(r.SymbolID)
		if sym != nil && sym.Name == "Run" {
			t.Errorf("(*Other).String unexpectedly has Run as caller — false bind across types")
		}
	}
}

// TestResolveCalls_QualifiedFieldType_DoesNotBindWithoutImportGraph
// pins the deliberate scope cut: if the field type is qualified
// (e.g. *exec.Cmd), the resolver SKIPS receiver-type binding rather
// than guessing at packages. The polymorphic-method drop still
// applies, so the call goes unresolved (preferred over a false bind).
func TestResolveCalls_QualifiedFieldType_DoesNotBindWithoutImportGraph(t *testing.T) {
	src := `package svc

type S struct{
	w foreign.Writer
}

func (s *S) Run() {
	s.w.Write(nil)
}
`
	idx, _ := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/s.go", src)
	// Just make sure the indexer doesn't blow up — the resolver path
	// returns "" for qualified types, the polymorphic fallback drops
	// the call, total resolved CALLS = 0 from this file. No crash is
	// the assertion.
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}
}

// TestResolveCalls_DeepFieldChain_DoesNotCrash exercises the
// 4+-segment fall-through. ToName like `o.middle.inner.Process` is
// outside the 2/3-segment scope of v0.19; the resolver must not
// crash and must fall through to existing fallbacks cleanly.
func TestResolveCalls_DeepFieldChain_DoesNotCrash(t *testing.T) {
	src := `package svc

type Inner struct{}
func (i *Inner) Process() {}

type Middle struct{ inner *Inner }
type Outer struct{ middle *Middle }

func (o *Outer) Do() {
	o.middle.inner.Process()
}
`
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/o.go", src)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}
	pid := db.ProjectIDFromPath(dir)
	syms, _ := store.GetSymbolsByName(pid, "Process", 5)
	if len(syms) == 0 {
		t.Fatalf("expected Process method extracted")
	}
}

// TestResolveCalls_NoReceiverTypeOnTopLevelFunc covers the
// empty-ReceiverType early return. The extractor only stamps
// receiver_type inside method bodies; calls from top-level functions
// arrive with empty receiver_type and must fall through cleanly to
// the existing fallbacks.
func TestResolveCalls_NoReceiverTypeOnTopLevelFunc(t *testing.T) {
	src := `package svc

type T struct{}
func (t *T) M() {}

func TopLevel() {
	var t T
	t.M()
}
`
	idx, _ := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/top.go", src)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}
}

// TestResolveCalls_FieldNotInStructFields_FallsThrough exercises the
// case-3 path where the struct exists but the named field doesn't —
// e.g. a typo at the call site, or a field that was removed. The
// receiver-type lookup misses; existing fallbacks decide whether to
// bind.
func TestResolveCalls_FieldNotInStructFields_FallsThrough(t *testing.T) {
	src := `package svc

type Helper struct{}
func (h *Helper) Run() {}

type App struct{ helper *Helper }

func (a *App) Trigger() {
	// 'missing' is not a real field — the resolver should skip
	// receiver-type binding and fall through.
	_ = a.missing
}
`
	idx, _ := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/app.go", src)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}
}
