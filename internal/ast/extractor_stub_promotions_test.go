package ast

import (
	"strings"
	"testing"
)

// #1161 v0.63: Lua / Elixir / Zig promoted from stub-tier (0.0,
// always-empty FileResult) to regex-tier (0.70). Tests follow the
// table-from-the-start shape (#1152): positive (function symbol
// extracted), positive (CALLS edges emitted from body), control
// (Scala/Haskell/Dart/R remain stub — deliberate deferral).

// LUA --------------------------------------------------------------

const luaSrc = `local function bootstrap()
  load_config()
  parse_config()
  render()
end

function module.helper()
  return 42
end

local function private_helper()
  return 1
end
`

func TestExtractLua_PromotedToRegex_ExtractsFunctions(t *testing.T) {
	r := Extract([]byte(luaSrc), "Lua", "src/main.lua")
	if r == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{
		"bootstrap":      false,
		"helper":         false,
		"private_helper": false,
	}
	for _, s := range r.Symbols {
		if s.Kind != "Function" {
			continue
		}
		if _, expected := want[s.Name]; expected {
			want[s.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("Lua function %q not extracted", name)
		}
	}
}

func TestExtractLua_PerFileCalls_EmitsEdges(t *testing.T) {
	r := Extract([]byte(luaSrc), "Lua", "src/main.lua")
	if r == nil {
		t.Fatal("nil result")
	}
	wantTargets := map[string]bool{
		"load_config":  false,
		"parse_config": false,
		"render":       false,
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" || !strings.HasSuffix(e.FromQN, "bootstrap") {
			continue
		}
		if _, expected := wantTargets[e.ToName]; expected {
			wantTargets[e.ToName] = true
		}
	}
	for target, found := range wantTargets {
		if !found {
			t.Errorf("Lua: expected CALLS edge bootstrap → %q; missing", target)
		}
	}
}

// ZIG --------------------------------------------------------------

const zigSrc = `pub fn bootstrap() void {
    load_config();
    const c = parse_config();
    render(c);
}

fn private_helper() i32 {
    return 1;
}

export fn entry_point() void {
    bootstrap();
}
`

func TestExtractZig_PromotedToRegex_ExtractsFunctions(t *testing.T) {
	r := Extract([]byte(zigSrc), "Zig", "src/main.zig")
	if r == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{
		"bootstrap":      false,
		"private_helper": false,
		"entry_point":    false,
	}
	for _, s := range r.Symbols {
		if s.Kind != "Function" {
			continue
		}
		if _, expected := want[s.Name]; expected {
			want[s.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("Zig function %q not extracted", name)
		}
	}
}

func TestExtractZig_PerFileCalls_EmitsEdges(t *testing.T) {
	r := Extract([]byte(zigSrc), "Zig", "src/main.zig")
	if r == nil {
		t.Fatal("nil result")
	}
	wantTargets := map[string]bool{
		"load_config":  false,
		"parse_config": false,
		"render":       false,
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" || !strings.HasSuffix(e.FromQN, "bootstrap") {
			continue
		}
		if _, expected := wantTargets[e.ToName]; expected {
			wantTargets[e.ToName] = true
		}
	}
	for target, found := range wantTargets {
		if !found {
			t.Errorf("Zig: expected CALLS edge bootstrap → %q; missing", target)
		}
	}
}

// ELIXIR -----------------------------------------------------------

const elixirSrc = `defmodule Bootstrap do
  def run do
    load_config()
    parse_config()
    render()
  end

  defp private_helper do
    42
  end

  defmacro guard_macro do
    quote do: nil
  end
end
`

func TestExtractElixir_PromotedToRegex_ExtractsFunctions(t *testing.T) {
	r := Extract([]byte(elixirSrc), "Elixir", "lib/bootstrap.ex")
	if r == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{
		"run":            false,
		"private_helper": false,
		"guard_macro":    false,
	}
	// Inside `defmodule Bootstrap do ... end` the regex extractor's
	// currentClass tracker scopes def/defp/defmacro as Methods.
	// Either kind is acceptable — the assertion is "the symbol
	// surfaces at all"; the kind delineation matches Elixir's
	// module-as-class shape.
	for _, s := range r.Symbols {
		if s.Kind != "Function" && s.Kind != "Method" {
			continue
		}
		if _, expected := want[s.Name]; expected {
			want[s.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("Elixir def %q not extracted", name)
		}
	}
}

// Cross-check: defmodule produces a Class symbol so callers can
// scope queries by module.
func TestExtractElixir_DefmoduleEmitsClass(t *testing.T) {
	r := Extract([]byte(elixirSrc), "Elixir", "lib/bootstrap.ex")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, s := range r.Symbols {
		if s.Kind == "Class" && s.Name == "Bootstrap" {
			return
		}
	}
	t.Error("defmodule Bootstrap not surfaced as a Class symbol")
}

func TestExtractElixir_PerFileCalls_EmitsEdges(t *testing.T) {
	r := Extract([]byte(elixirSrc), "Elixir", "lib/bootstrap.ex")
	if r == nil {
		t.Fatal("nil result")
	}
	wantTargets := map[string]bool{
		"load_config":  false,
		"parse_config": false,
		"render":       false,
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" || !strings.HasSuffix(e.FromQN, "run") {
			continue
		}
		if _, expected := wantTargets[e.ToName]; expected {
			wantTargets[e.ToName] = true
		}
	}
	for target, found := range wantTargets {
		if !found {
			t.Errorf("Elixir: expected CALLS edge run → %q; missing", target)
		}
	}
}

// Control: Haskell remains stub-tier — indentation-sensitive layout
// makes regex-tier representation significantly harder. Pins the
// v0.63 deferral decision so a future regex extractor has to opt in
// to a non-stub registration and this test then fails loudly,
// prompting proper test coverage for the new extractor.
//
// Scala, Dart, R were also stub-tier pre-v0.63 round 2; they're
// covered by their own positive-extraction tests below.
func TestExtractStubTier_HaskellRemainsEmpty(t *testing.T) {
	r := Extract([]byte("module M where\nfoo :: Int\nfoo = 42\n"), "Haskell", "src/M.hs")
	if r == nil {
		return // acceptable
	}
	if len(r.Symbols) > 0 {
		t.Errorf("Haskell should still be stub-tier in v0.63; got %d symbols. If you implemented an extractor, update this test to cover it.",
			len(r.Symbols))
	}
}

// SCALA -----------------------------------------------------------

const scalaSrc = `class Foo {
  def bar(x: Int): Int = x + 1
  def baz(): String = "hello"

  private def helper(): Unit = {
    println("private")
  }
}

object Constants {
  def pi: Double = 3.14
}
`

func TestExtractScala_PromotedToRegex_ExtractsFunctions(t *testing.T) {
	r := Extract([]byte(scalaSrc), "Scala", "src/Foo.scala")
	if r == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{
		"bar":    false,
		"baz":    false,
		"helper": false,
		"pi":     false,
	}
	for _, s := range r.Symbols {
		if s.Kind != "Function" && s.Kind != "Method" {
			continue
		}
		if _, expected := want[s.Name]; expected {
			want[s.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("Scala def %q not extracted", name)
		}
	}
}

// DART ------------------------------------------------------------

const dartSrc = `void main() {
  print("hello");
  greet("world");
}

String greet(String name) {
  return "Hi, " + name;
}

class Cart {
  void add(Item item) {
    items.add(item);
  }
}
`

func TestExtractDart_PromotedToRegex_ExtractsFunctions(t *testing.T) {
	r := Extract([]byte(dartSrc), "Dart", "src/main.dart")
	if r == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{
		"main":  false,
		"greet": false,
		"add":   false,
	}
	for _, s := range r.Symbols {
		if s.Kind != "Function" && s.Kind != "Method" {
			continue
		}
		if _, expected := want[s.Name]; expected {
			want[s.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("Dart symbol %q not extracted", name)
		}
	}
}

// R ---------------------------------------------------------------

const rSrc = `foo <- function(x) {
  x + 1
}

bar = function(y) {
  y * 2
}

helper.fn <- function() {
  42
}
`

func TestExtractR_PromotedToRegex_ExtractsFunctions(t *testing.T) {
	r := Extract([]byte(rSrc), "R", "src/util.r")
	if r == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{
		"foo":       false,
		"bar":       false,
		"helper.fn": false,
	}
	for _, s := range r.Symbols {
		if s.Kind != "Function" {
			continue
		}
		if _, expected := want[s.Name]; expected {
			want[s.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("R function %q not extracted", name)
		}
	}
}
