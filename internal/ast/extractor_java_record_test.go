package ast

import "testing"

// Java records (Java 14+) are type declarations. The Java classRE
// matched only `class`, so `record Point(int x, int y)` fell through
// to funcRE — which reads `record` as a return-type token and emits a
// phantom Method named Point. A record then surfaced under kind=Method
// (wrong for search/trace/dead_code) and never under kind=Class.
// classRE now also matches `record`.
//
// Found dogfooding the extractor across the languages the #1389
// cross-language sweep did not cover (it swept C/C++/C#/Go).

func TestExtractJava_RecordIsClassNotMethod(t *testing.T) {
	t.Parallel()
	src := []byte(`package com.x;

public record Point(int x, int y) {}

record Internal(String id) {}
`)
	r := Extract(src, "Java", "Point.java")
	if r == nil {
		t.Fatal("nil result")
	}
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
		if (s.Name == "Point" || s.Name == "Internal") && s.Kind == "Method" {
			t.Errorf("Java record %q extracted as Method — a record is a type declaration, not a method", s.Name)
		}
	}
	for _, name := range []string{"Point", "Internal"} {
		if got[name] != "Class" {
			t.Errorf("record %q kind = %q; want Class", name, got[name])
		}
	}
}

// Negative: a plain `class` must still extract as Class — the
// (?:class|record) alternation must not disturb the common case, and
// funcRE must still capture ordinary methods.
func TestExtractJava_PlainClassUnaffected(t *testing.T) {
	t.Parallel()
	src := []byte(`public class Widget extends Base {
    public void render() {}
}
`)
	r := Extract(src, "Java", "Widget.java")
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
	}
	if got["Widget"] != "Class" {
		t.Errorf("class Widget kind = %q; want Class", got["Widget"])
	}
	if got["render"] != "Method" {
		t.Errorf("method render kind = %q; want Method (funcRE must still work)", got["render"])
	}
}

// Control: a record with an `implements` clause, plus a real method
// on the following line — the record is a Class, the method stays a
// Method (the classRE claim suppresses funcRE only on the record's
// own line, not the method's).
func TestExtractJava_RecordWithImplements(t *testing.T) {
	t.Parallel()
	src := []byte(`public record Money(long cents) implements Comparable<Money> {
    public int compareTo(Money o) { return 0; }
}
`)
	r := Extract(src, "Java", "Money.java")
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
	}
	if got["Money"] != "Class" {
		t.Errorf("record Money kind = %q; want Class", got["Money"])
	}
	if got["compareTo"] != "Method" {
		t.Errorf("compareTo kind = %q; want Method", got["compareTo"])
	}
}
