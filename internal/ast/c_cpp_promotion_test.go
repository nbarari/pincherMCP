package ast

import "testing"

// #1463 v0.73 — C/C++ extractor promotion from 0.70 → 0.85
// (stable regex tier). Tests cover the high-value additions:
// C++ class, C/C++ struct, C/C++ enum, C++11+ scoped enum class,
// and inheritance lists. Existing C function + macro behaviour
// regression-tested via TestExtractC (unchanged).

func TestCPP_ClassWithInheritance(t *testing.T) {
	src := []byte(`#include <iostream>

class Shape {
    public:
        virtual double area() = 0;
};

class Circle : public Shape {
    private:
        double radius;
    public:
        Circle(double r) : radius(r) {}
        double area() override { return 3.14 * radius * radius; }
};

class Rectangle : public Shape, public Drawable {
};`)
	result := Extract(src, "C++", "geo.cpp")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"Shape", "Circle", "Rectangle"} {
		s, ok := byName[n]
		if !ok {
			t.Errorf("expected class %s; got %v", n, cppKeysOf(byName))
		} else if s.Kind != "Class" {
			t.Errorf("%s.Kind = %q; want Class", n, s.Kind)
		}
	}
}

func TestCPP_Struct(t *testing.T) {
	src := []byte(`struct Point {
    int x;
    int y;
};

struct Color : public Drawable {
    unsigned char r, g, b;
};`)
	result := Extract(src, "C++", "geo.cpp")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"Point", "Color"} {
		s, ok := byName[n]
		if !ok {
			t.Errorf("expected struct %s; got %v", n, cppKeysOf(byName))
		} else if s.Kind != "Class" {
			t.Errorf("%s.Kind = %q; want Class (struct modeled as Class)", n, s.Kind)
		}
	}
}

func TestC_StructIsExtracted(t *testing.T) {
	// Plain C struct (no `typedef`) — common in kernel / embedded
	// codebases. Pre-#1463 these were invisible.
	src := []byte(`#include <stdio.h>

struct file_operations {
    int (*open)(void);
    int (*close)(void);
};

struct list_head {
    struct list_head *next;
    struct list_head *prev;
};`)
	result := Extract(src, "C", "fs.c")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"file_operations", "list_head"} {
		if _, ok := byName[n]; !ok {
			t.Errorf("expected struct %s; got %v", n, cppKeysOf(byName))
		}
	}
}

func TestC_EnumIsExtractedAsEnum(t *testing.T) {
	src := []byte(`enum Color {
    RED,
    GREEN,
    BLUE
};

enum LogLevel { LOG_DEBUG = 0, LOG_INFO, LOG_WARN, LOG_ERROR };`)
	result := Extract(src, "C", "log.c")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"Color", "LogLevel"} {
		s, ok := byName[n]
		if !ok {
			t.Errorf("expected enum %s; got %v", n, cppKeysOf(byName))
			continue
		}
		if s.Kind != "Enum" {
			t.Errorf("%s.Kind = %q; want Enum", n, s.Kind)
		}
	}
}

func TestCPP_ScopedEnumClass(t *testing.T) {
	// C++11+ `enum class Name` — strongly-typed enum.
	src := []byte(`enum class Direction {
    North,
    South,
    East,
    West
};

enum struct Priority {
    Low,
    High
};`)
	result := Extract(src, "C++", "enums.cpp")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, n := range []string{"Direction", "Priority"} {
		s, ok := byName[n]
		if !ok {
			t.Errorf("expected scoped enum %s; got %v", n, cppKeysOf(byName))
			continue
		}
		if s.Kind != "Enum" {
			t.Errorf("%s.Kind = %q; want Enum (C++ scoped enum)", n, s.Kind)
		}
	}
}

func TestC_Union(t *testing.T) {
	src := []byte(`union Variant {
    int i;
    float f;
    char *s;
};`)
	result := Extract(src, "C", "variant.c")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, s := range result.Symbols {
		if s.Name == "Variant" && s.Kind == "Class" {
			return
		}
	}
	t.Error("expected union Variant as Class")
}

func TestC_ExistingFunctionBehaviour_Regression(t *testing.T) {
	// Pre-#1463 baseline still works.
	src := []byte(`#include <stdio.h>

int add(int a, int b) {
    return a + b;
}

static void helper(void) {
    printf("hello\n");
}

int main(int argc, char *argv[]) {
    return 0;
}`)
	result := Extract(src, "C", "main.c")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := map[string]ExtractedSymbol{}
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, fn := range []string{"add", "helper", "main"} {
		if _, ok := byName[fn]; !ok {
			t.Errorf("expected function %s; got %v", fn, cppKeysOf(byName))
		}
	}
}

func TestC_ExtractorConfidenceIs085(t *testing.T) {
	if c := RegisteredConfidence("C"); c != 0.85 {
		t.Errorf("C registry confidence = %v; want 0.85 (#1463 promotion)", c)
	}
}

func TestCPP_ExtractorConfidenceIs085(t *testing.T) {
	if c := RegisteredConfidence("C++"); c != 0.85 {
		t.Errorf("C++ registry confidence = %v; want 0.85 (#1463 promotion)", c)
	}
}

func cppKeysOf(m map[string]ExtractedSymbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
