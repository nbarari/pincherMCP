package ast

import (
	"testing"
)

// #1693 (#1389 cross-language sweep): C `classRE`
// (`^\s*(?:class|struct|union)\s+NAME`) matches the column-0
// continuation line of a multi-line prototype — `int connect(int s,
// \nstruct sockaddr *addr,\n...)` — emitting a phantom `sockaddr`
// Class. System headers hit this 5-6× per file → qualified_name_
// collision. dropCTypeUseClasses drops Class symbols that are
// forward-decls or struct-typed uses, keeping only real definitions.
//
// Four-case shape (#1152): positive (type-uses + forward-decls
// dropped), negative (real definitions kept), control (the matching
// definition of a forward-declared struct survives), cross-check
// (the cClassIsTypeUse discriminator on each shape).

func TestExtractC_StructTypeUseClasses_Dropped_1693(t *testing.T) {
	t.Parallel()
	// A multi-line prototype + a forward decl + a struct-typed field
	// — none of these define a struct, all begin column-0 with
	// `struct NAME`.
	src := []byte(`struct sockaddr;

int connect(
struct sockaddr *addr,
int len);

struct timeval my_global;
`)
	result := Extract(src, "C", "src/net.c")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	for _, s := range result.Symbols {
		if s.Kind == "Class" {
			t.Errorf("phantom Class %q from a forward-decl / type-use line — should be dropped (#1693)", s.Name)
		}
	}
}

// Negative: real struct/union/class definitions MUST survive — the
// filter must not be so broad it eats definitions (that's symbol loss).
func TestExtractC_StructDefinitionsKept_1693(t *testing.T) {
	t.Parallel()
	src := []byte(`struct Point {
	int x;
	int y;
};

union Value {
	int i;
	float f;
};

struct Point
{
	int z;
};
`)
	result := Extract(src, "C", "src/types.c")
	got := map[string]bool{}
	for _, s := range result.Symbols {
		if s.Kind == "Class" {
			got[s.Name] = true
		}
	}
	for _, want := range []string{"Point", "Value"} {
		if !got[want] {
			t.Errorf("real definition %q dropped — dropCTypeUseClasses is too broad (symbol loss)", want)
		}
	}
}

// Control: a struct forward-declared AND defined in the same file —
// the forward decl drops, the definition survives, so the final
// symbol set has exactly one `Node` Class (no collision).
func TestExtractC_ForwardDeclThenDefinition_1693(t *testing.T) {
	t.Parallel()
	src := []byte(`struct Node;

struct Node {
	struct Node *next;
	int value;
};
`)
	result := Extract(src, "C", "src/list.c")
	var nodeCount int
	for _, s := range result.Symbols {
		if s.Kind == "Class" && s.Name == "Node" {
			nodeCount++
		}
	}
	if nodeCount != 1 {
		t.Errorf("expected exactly 1 Node Class (forward-decl dropped, definition kept); got %d", nodeCount)
	}
}

// Cross-check: the cClassIsTypeUse discriminator, char by char.
func TestCClassIsTypeUse_Discriminator_1693(t *testing.T) {
	t.Parallel()
	cases := []struct {
		src      string
		typeUse  bool // want
		why      string
	}{
		{"struct Foo {", false, "brace → definition"},
		{"struct Foo\n{", false, "brace on next line → definition"},
		{"class Foo : public Base {", false, "C++ base clause → definition"},
		{"struct alignas(16) Foo {", false, "alignas paren → ambiguous, keep"},
		{"struct Foo;", true, "semicolon → forward decl"},
		{"struct sockaddr *addr,", true, "pointer + comma → prototype param"},
		{"struct timeval my_global;", true, "value field/var → type-use"},
		{"struct Foo &ref);", true, "C++ ref param → type-use"},
		{"struct Foo arr[10];", true, "array → type-use"},
	}
	for _, c := range cases {
		got := cClassIsTypeUse([]byte(c.src), 0)
		if got != c.typeUse {
			t.Errorf("cClassIsTypeUse(%q) = %v, want %v (%s)", c.src, got, c.typeUse, c.why)
		}
	}
}
