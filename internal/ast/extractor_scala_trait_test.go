package ast

import "testing"

// Scala `trait` was listed in BOTH classRE and interfaceRE, so a
// `trait Store` line matched both — emitting a Store#Class AND a
// Store#Interface, a conflicting-kind duplicate of one declaration.
// `trait` now belongs to interfaceRE only; interfaceRE gained the
// modifier prefix so `sealed trait` still resolves (it would
// otherwise match neither regex and vanish).
//
// Found dogfooding the extractor across the languages the #1389
// cross-language sweep did not cover.

func TestExtractScala_TraitIsInterfaceOnce(t *testing.T) {
	t.Parallel()
	src := []byte(`package com.x

trait Store {
  def get(k: String): Option[String]
}

sealed trait Result
`)
	r := Extract(src, "Scala", "Store.scala")
	if r == nil {
		t.Fatal("nil result")
	}
	kindsByName := map[string][]string{}
	for _, s := range r.Symbols {
		kindsByName[s.Name] = append(kindsByName[s.Name], s.Kind)
	}
	for _, name := range []string{"Store", "Result"} {
		got := kindsByName[name]
		if len(got) != 1 {
			t.Errorf("trait %q emitted %d symbols (%v); want exactly 1 — "+
				"trait must not match both classRE and interfaceRE", name, len(got), got)
			continue
		}
		if got[0] != "Interface" {
			t.Errorf("trait %q kind = %q; want Interface", name, got[0])
		}
	}
}

// Negative/control: class / object / case class must still extract
// as Class — removing `trait` from classRE must not disturb them.
func TestExtractScala_ClassObjectUnaffected(t *testing.T) {
	t.Parallel()
	src := []byte(`class OrderService(items: List[Int])

object UserRepo

case class User(id: String, name: String)
`)
	r := Extract(src, "Scala", "svc.scala")
	got := map[string]string{}
	for _, s := range r.Symbols {
		got[s.Name] = s.Kind
	}
	for _, name := range []string{"OrderService", "UserRepo", "User"} {
		if got[name] != "Class" {
			t.Errorf("%q kind = %q; want Class", name, got[name])
		}
	}
}

// Cross-check: a trait's method still scopes under the trait (the
// scope tracker is set by interfaceRE, #819) — it must come out as a
// Method parented to the trait, not a top-level Function.
func TestExtractScala_TraitMethodScoped(t *testing.T) {
	t.Parallel()
	src := []byte(`trait Repo {
  def load(id: String): String
}
`)
	r := Extract(src, "Scala", "repo.scala")
	var load ExtractedSymbol
	for _, s := range r.Symbols {
		if s.Name == "load" {
			load = s
		}
	}
	if load.Name == "" {
		t.Fatal("trait method `load` not extracted")
	}
	if load.Kind != "Method" {
		t.Errorf("trait method load kind = %q; want Method (scoped under the trait)", load.Kind)
	}
}
