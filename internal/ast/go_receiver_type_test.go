package ast

import "testing"

// Tests for #423 extraction-layer changes: struct field capture
// (ExtractedSymbol.Fields) and per-edge receiver-type stamping
// (ExtractedEdge.ReceiverType). The resolver wiring lands in a later
// piece — these tests gate only what extractor.go produces.

// TestExtractGoStructFields_BasicFields covers named fields with
// simple and qualified types. The resolver consumes Fields as a
// map of field-name → type-expression-as-Go-syntax, so the type
// strings must round-trip the way the source author wrote them.
func TestExtractGoStructFields_BasicFields(t *testing.T) {
	src := `package p
type Supervisor struct {
	stdin  io.Writer
	cmd    *exec.Cmd
	count  int
}`
	result := Extract([]byte(src), "Go", "p/p.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	var sup *ExtractedSymbol
	for i := range result.Symbols {
		if result.Symbols[i].Name == "Supervisor" {
			sup = &result.Symbols[i]
			break
		}
	}
	if sup == nil {
		t.Fatal("Supervisor symbol not extracted")
	}
	if sup.Kind != "Class" {
		t.Errorf("Supervisor kind = %q, want Class", sup.Kind)
	}
	want := map[string]string{
		"stdin": "io.Writer",
		"cmd":   "*exec.Cmd",
		"count": "int",
	}
	for k, v := range want {
		if got := sup.Fields[k]; got != v {
			t.Errorf("Fields[%q] = %q, want %q", k, got, v)
		}
	}
	if len(sup.Fields) != len(want) {
		t.Errorf("Fields has %d entries, want %d (%v)", len(sup.Fields), len(want), sup.Fields)
	}
}

// TestExtractGoStructFields_EmbeddedField verifies Go's promoted-
// field naming rule: `sync.Mutex` is keyed as "Mutex", `*log.Logger`
// as "Logger". The resolver needs this so calls like `s.Mutex.Lock()`
// can find the field.
func TestExtractGoStructFields_EmbeddedField(t *testing.T) {
	src := `package p
type S struct {
	sync.Mutex
	*log.Logger
	Plain
}`
	result := Extract([]byte(src), "Go", "p/p.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	var s *ExtractedSymbol
	for i := range result.Symbols {
		if result.Symbols[i].Name == "S" {
			s = &result.Symbols[i]
			break
		}
	}
	if s == nil {
		t.Fatal("S symbol not extracted")
	}
	cases := map[string]string{
		"Mutex":  "sync.Mutex",
		"Logger": "*log.Logger",
		"Plain":  "Plain",
	}
	for k, v := range cases {
		if got := s.Fields[k]; got != v {
			t.Errorf("Fields[%q] = %q, want %q", k, got, v)
		}
	}
}

// TestExtractGoStructFields_TaggedFieldsAndMultiName checks that
// struct tags don't alter capture and that grouped declarations
// (`a, b int`) emit one entry per name.
func TestExtractGoStructFields_TaggedFieldsAndMultiName(t *testing.T) {
	src := "package p\n" +
		"type S struct {\n" +
		"\tName string `json:\"name\"`\n" +
		"\tA, B int\n" +
		"\t_ string\n" +
		"}\n"
	result := Extract([]byte(src), "Go", "p/p.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	var s *ExtractedSymbol
	for i := range result.Symbols {
		if result.Symbols[i].Name == "S" {
			s = &result.Symbols[i]
			break
		}
	}
	if s == nil {
		t.Fatal("S symbol not extracted")
	}
	if s.Fields["Name"] != "string" {
		t.Errorf("Fields[Name] = %q, want string", s.Fields["Name"])
	}
	if s.Fields["A"] != "int" || s.Fields["B"] != "int" {
		t.Errorf("grouped fields not split: A=%q B=%q", s.Fields["A"], s.Fields["B"])
	}
	if _, ok := s.Fields["_"]; ok {
		t.Errorf("blank-identifier field should be skipped, got %q", s.Fields["_"])
	}
}

// TestExtractGoStructFields_EmptyStruct ensures empty/struct-less
// type symbols don't carry a stray non-nil empty map (keeps JSON
// output clean and downstream nil-checks honest).
func TestExtractGoStructFields_EmptyStruct(t *testing.T) {
	src := `package p
type Empty struct{}
type Iface interface { M() }
type Alias = int`
	result := Extract([]byte(src), "Go", "p/p.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	for _, s := range result.Symbols {
		if s.Fields != nil && len(s.Fields) > 0 {
			t.Errorf("symbol %q (kind %s) has unexpected Fields: %v", s.Name, s.Kind, s.Fields)
		}
	}
}

// TestExtractGoCalls_StampsReceiverTypeInsideMethod verifies that
// CALLS edges emitted from inside a method body carry the method's
// receiver-type expression. The resolver uses this to disambiguate
// `recv.field.method` calls.
func TestExtractGoCalls_StampsReceiverTypeInsideMethod(t *testing.T) {
	src := `package p
type Supervisor struct{ stdin io.Writer }
func (s *Supervisor) Spawn() {
	s.stdin.Write(nil)
	helper()
}
func helper() {}`
	result := Extract([]byte(src), "Go", "p/p.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	var sawWrite, sawHelper bool
	for _, e := range result.Edges {
		if e.Kind != "CALLS" || e.FromQN != "p.*Supervisor.Spawn" {
			continue
		}
		if e.ReceiverType != "*Supervisor" {
			t.Errorf("edge %q→%q ReceiverType = %q, want *Supervisor", e.FromQN, e.ToName, e.ReceiverType)
		}
		switch e.ToName {
		case "s.stdin.Write":
			sawWrite = true
		case "helper":
			sawHelper = true
		}
	}
	if !sawWrite {
		t.Error("expected CALLS edge with ToName=s.stdin.Write")
	}
	if !sawHelper {
		t.Error("expected CALLS edge with ToName=helper")
	}
}

// TestExtractGoCalls_NoReceiverTypeForFunctions confirms that plain
// (non-method) functions emit edges with empty ReceiverType. The
// resolver short-circuits on empty so this keeps the existing
// behaviour for free functions intact.
func TestExtractGoCalls_NoReceiverTypeForFunctions(t *testing.T) {
	src := `package p
func Top() {
	helper()
	other()
}
func helper() {}
func other() {}`
	result := Extract([]byte(src), "Go", "p/p.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	var calls int
	for _, e := range result.Edges {
		if e.Kind != "CALLS" || e.FromQN != "p.Top" {
			continue
		}
		calls++
		if e.ReceiverType != "" {
			t.Errorf("edge %q→%q ReceiverType = %q, want empty", e.FromQN, e.ToName, e.ReceiverType)
		}
	}
	if calls != 2 {
		t.Errorf("got %d CALLS from p.Top, want 2", calls)
	}
}

// TestExtractGoCalls_ValueReceiverNotPointer covers the value-
// receiver case so the resolver gets the bare type name (no leading
// `*`) when source code wrote a value receiver.
func TestExtractGoCalls_ValueReceiverNotPointer(t *testing.T) {
	src := `package p
type T struct{}
func (t T) M() { helper() }
func helper() {}`
	result := Extract([]byte(src), "Go", "p/p.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	var found bool
	for _, e := range result.Edges {
		if e.Kind == "CALLS" && e.FromQN == "p.T.M" && e.ToName == "helper" {
			found = true
			if e.ReceiverType != "T" {
				t.Errorf("ReceiverType = %q, want T", e.ReceiverType)
			}
		}
	}
	if !found {
		t.Error("expected CALLS edge p.M → helper")
	}
}
