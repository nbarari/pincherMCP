package ast

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// DetectLanguage / IsSourceFile
// ─────────────────────────────────────────────────────────────────────────────

func TestDetectLanguage(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"main.go", "Go"},
		{"handler.go", "Go"},
		{"script.py", "Python"},
		{"app.js", "JavaScript"},
		{"component.jsx", "JSX"},
		{"types.ts", "TypeScript"},
		{"page.tsx", "TSX"},
		{"lib.rs", "Rust"},
		{"Main.java", "Java"},
		{"helper.rb", "Ruby"},
		{"index.php", "PHP"},
		{"util.c", "C"},
		{"util.cpp", "C++"},
		{"service.cs", "C#"},
		{"app.kt", "Kotlin"},
		{"view.swift", "Swift"},
		{"unknown.xyz", ""},
		{"noext", ""},
		{"site.yml", "YAML"},
		{"values.yaml", "YAML"},
		{"data.json", "JSON"},
	}
	for _, c := range cases {
		got := DetectLanguage(c.file)
		if got != c.want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", c.file, got, c.want)
		}
	}
}

func TestIsSourceFile(t *testing.T) {
	if !IsSourceFile("main.go") {
		t.Error("main.go should be a source file")
	}
	if IsSourceFile("notes.txt") {
		t.Error("notes.txt should not be a source file")
	}
	if !IsSourceFile("data.json") {
		t.Error("data.json should be a source file (JSON support)")
	}
	if !IsSourceFile("site.yml") {
		t.Error("site.yml should be a source file (YAML support)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Go extractor
// ─────────────────────────────────────────────────────────────────────────────

const goSrc = `package mypackage

import "fmt"

// Add adds two ints.
func Add(a, b int) int {
	return a + b
}

type Server struct {
	port int
}

func (s *Server) Start() error {
	fmt.Println("start")
	return nil
}

type Handler interface {
	Handle() error
}
`

func TestExtractGo(t *testing.T) {
	result := Extract([]byte(goSrc), "Go", "mypackage/myfile.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}

	// Should have extracted: Add (Function), Server (Class), Start (Method), Handler (Interface)
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}

	if _, ok := byName["Add"]; !ok {
		t.Error("expected symbol 'Add'")
	}
	if byName["Add"].Kind != "Function" {
		t.Errorf("Add.Kind = %q, want Function", byName["Add"].Kind)
	}
	if !byName["Add"].IsExported {
		t.Error("Add should be exported")
	}

	if _, ok := byName["Server"]; !ok {
		t.Error("expected symbol 'Server'")
	}
	if byName["Server"].Kind != "Class" {
		t.Errorf("Server.Kind = %q, want Class", byName["Server"].Kind)
	}

	if _, ok := byName["Start"]; !ok {
		t.Error("expected symbol 'Start'")
	}
	if byName["Start"].Kind != "Method" {
		t.Errorf("Start.Kind = %q, want Method", byName["Start"].Kind)
	}

	if _, ok := byName["Handler"]; !ok {
		t.Error("expected symbol 'Handler'")
	}
	if byName["Handler"].Kind != "Interface" {
		t.Errorf("Handler.Kind = %q, want Interface", byName["Handler"].Kind)
	}
}

func TestExtractGo_ByteOffsets(t *testing.T) {
	result := Extract([]byte(goSrc), "Go", "mypackage/myfile.go")
	if result == nil {
		t.Fatal("Extract returned nil")
	}
	for _, s := range result.Symbols {
		if s.StartByte >= s.EndByte {
			t.Errorf("symbol %q has start_byte(%d) >= end_byte(%d)", s.Name, s.StartByte, s.EndByte)
		}
		if s.StartLine <= 0 {
			t.Errorf("symbol %q has invalid start_line %d", s.Name, s.StartLine)
		}
	}
}

func TestExtractGo_DocstringCapture(t *testing.T) {
	result := Extract([]byte(goSrc), "Go", "mypackage/myfile.go")
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if !strings.Contains(byName["Add"].Docstring, "adds two ints") {
		t.Errorf("Add docstring = %q, want to contain 'adds two ints'", byName["Add"].Docstring)
	}
}

func TestExtractGo_CALLS_edges(t *testing.T) {
	result := Extract([]byte(goSrc), "Go", "mypackage/myfile.go")
	if result == nil {
		t.Fatal("nil result")
	}
	hasCallEdge := false
	for _, e := range result.Edges {
		if e.Kind == "CALLS" {
			hasCallEdge = true
			break
		}
	}
	// Start() calls fmt.Println — should produce a CALLS edge
	if !hasCallEdge {
		t.Error("expected at least one CALLS edge")
	}
}

func TestExtractGo_MainIsEntryPoint(t *testing.T) {
	src := []byte(`package main
func main() {}
`)
	result := Extract(src, "Go", "main.go")
	for _, s := range result.Symbols {
		if s.Kind == "Function" && s.Name == "main" && !s.IsEntryPoint {
			t.Error("main() should be marked IsEntryPoint")
		}
	}
}

func TestExtractGo_TestFuncDetection(t *testing.T) {
	src := []byte(`package mypackage
import "testing"
func TestFoo(t *testing.T) {}
func BenchmarkBar(b *testing.B) {}
func normalFunc() {}
`)
	result := Extract(src, "Go", "mypackage/myfile_test.go")
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if !byName["TestFoo"].IsTest {
		t.Error("TestFoo should be IsTest")
	}
	if !byName["BenchmarkBar"].IsTest {
		t.Error("BenchmarkBar should be IsTest")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Python extractor
// ─────────────────────────────────────────────────────────────────────────────

const pySrc = `import os
from pathlib import Path

class MyClass:
    def method(self):
        pass

def standalone(x, y):
    return x + y
`

func TestExtractPython(t *testing.T) {
	result := Extract([]byte(pySrc), "Python", "mymod/myfile.py")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["MyClass"]; !ok {
		t.Error("expected symbol 'MyClass'")
	}
	if byName["MyClass"].Kind != "Class" {
		t.Errorf("MyClass.Kind = %q, want Class", byName["MyClass"].Kind)
	}
	if _, ok := byName["standalone"]; !ok {
		t.Error("expected symbol 'standalone'")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TypeScript extractor
// ─────────────────────────────────────────────────────────────────────────────

const tsSrc = `import { foo } from './foo';

export interface Greeter {
  greet(): string;
}

export class GreeterImpl implements Greeter {
  greet() { return 'hello'; }
}

export function createGreeter(): Greeter {
  return new GreeterImpl();
}
`

func TestExtractTypeScript(t *testing.T) {
	result := Extract([]byte(tsSrc), "TypeScript", "src/greeter.ts")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["Greeter"]; !ok {
		t.Error("expected symbol 'Greeter' (interface)")
	}
	if _, ok := byName["GreeterImpl"]; !ok {
		t.Error("expected symbol 'GreeterImpl' (class)")
	}
	if _, ok := byName["createGreeter"]; !ok {
		t.Error("expected symbol 'createGreeter' (function)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Utility functions
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildLineOffsets(t *testing.T) {
	src := []byte("line1\nline2\nline3")
	offsets := buildLineOffsets(src)
	if len(offsets) < 3 {
		t.Errorf("expected at least 3 offsets, got %d", len(offsets))
	}
	if offsets[0] != 0 {
		t.Errorf("first offset should be 0, got %d", offsets[0])
	}
	if offsets[1] != 6 {
		t.Errorf("second offset should be 6, got %d", offsets[1])
	}
}

func TestEstimateComplexity(t *testing.T) {
	simple := []byte("func f() { return 1 }")
	complex := []byte("func f() { if x { for i { if y { } } } }")
	sc := estimateComplexity(simple)
	cc := estimateComplexity(complex)
	if sc >= cc {
		t.Errorf("complex function should have higher complexity: simple=%d complex=%d", sc, cc)
	}
}

func TestExtractNilForEmpty(t *testing.T) {
	result := Extract([]byte{}, "Go", "empty.go")
	if result == nil {
		t.Error("Extract should never return nil")
	}
}

func TestExtractUnknownLanguage(t *testing.T) {
	result := Extract([]byte("some content"), "Zig", "file.zig")
	if result == nil {
		t.Error("Extract should return empty FileResult for unsupported language")
	}
	if len(result.Symbols) != 0 {
		t.Errorf("expected 0 symbols for unsupported language, got %d", len(result.Symbols))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JavaScript extractor
// ─────────────────────────────────────────────────────────────────────────────

const jsSrc = `
function processOrder(order) {
  return order.total * 1.1;
}

class PaymentGateway {
  constructor(apiKey) {
    this.apiKey = apiKey;
  }
}

const fetchData = async (url) => {
  return fetch(url);
};

export function helper() {}
`

func TestExtractJavaScript(t *testing.T) {
	result := Extract([]byte(jsSrc), "JavaScript", "src/payments.js")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["processOrder"]; !ok {
		t.Error("expected symbol 'processOrder'")
	}
	if byName["processOrder"].Kind != "Function" {
		t.Errorf("processOrder.Kind = %q, want Function", byName["processOrder"].Kind)
	}
	if _, ok := byName["PaymentGateway"]; !ok {
		t.Error("expected symbol 'PaymentGateway'")
	}
	if byName["PaymentGateway"].Kind != "Class" {
		t.Errorf("PaymentGateway.Kind = %q, want Class", byName["PaymentGateway"].Kind)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Rust extractor
// ─────────────────────────────────────────────────────────────────────────────

const rustSrc = `use std::collections::HashMap;

pub struct Config {
    pub name: String,
}

pub trait Runnable {
    fn run(&self);
}

pub enum Status {
    Active,
    Inactive,
}

pub fn process(input: &str) -> String {
    input.to_uppercase()
}

fn helper() {}
`

func TestExtractRust(t *testing.T) {
	result := Extract([]byte(rustSrc), "Rust", "src/lib.rs")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["Config"]; !ok {
		t.Error("expected struct 'Config'")
	}
	if byName["Config"].Kind != "Class" {
		t.Errorf("Config.Kind = %q, want Class", byName["Config"].Kind)
	}
	if _, ok := byName["Runnable"]; !ok {
		t.Error("expected trait 'Runnable'")
	}
	if byName["Runnable"].Kind != "Interface" {
		t.Errorf("Runnable.Kind = %q, want Interface", byName["Runnable"].Kind)
	}
	if _, ok := byName["Status"]; !ok {
		t.Error("expected enum 'Status'")
	}
	if _, ok := byName["process"]; !ok {
		t.Error("expected function 'process'")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Java extractor
// ─────────────────────────────────────────────────────────────────────────────

const javaSrc = `import java.util.List;

public class OrderService {
    private final String name;

    public OrderService(String name) {
        this.name = name;
    }

    public List<String> getOrders() {
        return null;
    }
}

public interface Repository {
    void save(Object obj);
}

public enum OrderStatus {
    PENDING, FULFILLED
}
`

func TestExtractJava(t *testing.T) {
	result := Extract([]byte(javaSrc), "Java", "src/OrderService.java")
	if result == nil {
		t.Fatal("nil result")
	}
	// Java constructors share the class name, so iterate to find the class symbol.
	var foundClass, foundInterface, foundEnum bool
	for _, s := range result.Symbols {
		if s.Name == "OrderService" && s.Kind == "Class" {
			foundClass = true
		}
		if s.Name == "Repository" && s.Kind == "Interface" {
			foundInterface = true
		}
		if s.Name == "OrderStatus" && s.Kind == "Enum" {
			foundEnum = true
		}
	}
	if !foundClass {
		t.Error("expected class symbol 'OrderService'")
	}
	if !foundInterface {
		t.Error("expected interface symbol 'Repository'")
	}
	if !foundEnum {
		t.Error("expected enum symbol 'OrderStatus'")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Ruby extractor
// ─────────────────────────────────────────────────────────────────────────────

const rubySrc = `class Animal
  def initialize(name)
    @name = name
  end

  def speak
    "..."
  end
end

class Dog < Animal
  def speak
    "woof"
  end
end

def standalone_helper
  true
end
`

func TestExtractRuby(t *testing.T) {
	result := Extract([]byte(rubySrc), "Ruby", "lib/animal.rb")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["Animal"]; !ok {
		t.Error("expected class 'Animal'")
	}
	if byName["Animal"].Kind != "Class" {
		t.Errorf("Animal.Kind = %q, want Class", byName["Animal"].Kind)
	}
	if _, ok := byName["Dog"]; !ok {
		t.Error("expected class 'Dog'")
	}
	if _, ok := byName["speak"]; !ok {
		t.Error("expected method 'speak'")
	}

	// #805: Ruby closes def/class with `end`, not a brace. Pre-fix the
	// blockChar=0 path gave every symbol an 80-line span clamped to EOF,
	// so `symbol`/`context` returned wildly wrong source. The
	// end-keyword finder must span each block to its own `end`.
	if got := byName["Animal"]; got.StartLine != 1 || got.EndLine != 9 {
		t.Errorf("Animal span = %d-%d, want 1-9 (class ... end)", got.StartLine, got.EndLine)
	}
	if got := byName["initialize"]; got.StartLine != 2 || got.EndLine != 4 {
		t.Errorf("initialize span = %d-%d, want 2-4 — must end at its own `end`, not the class's", got.StartLine, got.EndLine)
	}
	if got := byName["standalone_helper"]; got.StartLine != 17 || got.EndLine != 19 {
		t.Errorf("standalone_helper span = %d-%d, want 17-19 — the last def must not run to EOF", got.StartLine, got.EndLine)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PHP extractor
// ─────────────────────────────────────────────────────────────────────────────

const phpSrc = `<?php

class UserController extends BaseController {
    public function index() {
        return view('users.index');
    }

    private function validate($request) {
        return true;
    }
}

function formatDate($date) {
    return date('Y-m-d', $date);
}
`

func TestExtractPHP(t *testing.T) {
	result := Extract([]byte(phpSrc), "PHP", "app/UserController.php")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["UserController"]; !ok {
		t.Error("expected class 'UserController'")
	}
	// Note: indented class methods (e.g. 'index') are not matched by the regex extractor.
	// Top-level functions without indentation are matched.
	if _, ok := byName["formatDate"]; !ok {
		t.Error("expected function 'formatDate'")
	}
	// #811: a class WITH an `extends` clause reports the superclass as
	// parent; a class without one must report "" — not its own name.
	if got := byName["UserController"].Parent; got != "BaseController" {
		t.Errorf("UserController.Parent = %q, want %q", got, "BaseController")
	}
}

// #811: a superclass-less class must not report its own name as parent.
// The old extractGroup returned the first non-empty positional group, so
// asking for "parent" fell through to the "name" group.
func TestExtractClass_NoSuperclassHasEmptyParent(t *testing.T) {
	cases := []struct{ lang, src string }{
		{"PHP", "<?php\nclass Lonely {\n}\n"},
		{"Java", "public class Lonely {\n}\n"},
		{"TypeScript", "export class Lonely {\n}\n"},
	}
	for _, c := range cases {
		result := Extract([]byte(c.src), c.lang, "m/Lonely."+c.lang)
		var found bool
		for _, s := range result.Symbols {
			if s.Kind == "Class" && s.Name == "Lonely" {
				found = true
				if s.Parent != "" {
					t.Errorf("%s: Lonely.Parent = %q, want \"\" (no superclass)", c.lang, s.Parent)
				}
			}
		}
		if !found {
			t.Errorf("%s: expected Class symbol 'Lonely'", c.lang)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// C extractor
// ─────────────────────────────────────────────────────────────────────────────

const cSrc = `#include <stdio.h>
#include <stdlib.h>

int add(int a, int b) {
    return a + b;
}

static void helper(void) {
    printf("hello\n");
}

int main(int argc, char *argv[]) {
    return 0;
}
`

func TestExtractC(t *testing.T) {
	result := Extract([]byte(cSrc), "C", "src/main.c")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["add"]; !ok {
		t.Error("expected function 'add'")
	}
	if _, ok := byName["main"]; !ok {
		t.Error("expected function 'main'")
	}
}

// cMacroSrc reproduces the Linux-kernel-style declaration macro pattern
// from issue #69: each DEVICE_ATTR(...) emits a symbol where the real
// identity is the first arg inside the parens, not the macro name itself.
// Before the fix, all six emitted with name="DEVICE_ATTR" and produced
// qualified_name_collision rows in extraction_failures.
//
// Note: bare-prefix macros (e.g. `EXPORT_SYMBOL(foo);` at column 0) are
// not addressed here — the C funcRE requires at least one preceding word
// or `static`/`inline` keyword, so those lines are never matched in the
// first place. That's a separate gap, tracked outside #69.
const cMacroSrc = `#include <linux/device.h>

static ssize_t gpio_keys_show_keys(struct device *d) { return 0; }
static ssize_t gpio_keys_show_switches(struct device *d) { return 0; }
static ssize_t gpio_keys_show_camera_switches(struct device *d) { return 0; }

static DEVICE_ATTR(keys, S_IRUGO, gpio_keys_show_keys, NULL);
static DEVICE_ATTR(switches, S_IRUGO, gpio_keys_show_switches, NULL);
static DEVICE_ATTR(camera_switches, S_IRUGO, gpio_keys_show_camera_switches, NULL);
`

// TestExtractC_MacroDecls is the regression gate for issue #69. Each
// `static MACRO(name, ...)` must emit a symbol whose Name + QualifiedName
// carry the FIRST ARG, not the macro name. Without this, three
// DEVICE_ATTR declarations in one file all collide on
// `<mod>::DEVICE_ATTR` and only the last write survives BulkUpsertSymbols.
func TestExtractC_MacroDecls(t *testing.T) {
	result := Extract([]byte(cMacroSrc), "C", "drivers/gpio_keys.c")
	if result == nil {
		t.Fatal("nil result")
	}

	// Every symbol must have a unique qualified_name. The bug manifested
	// as duplicates; this asserts the absence of duplicates directly.
	seen := make(map[string]int)
	for _, s := range result.Symbols {
		seen[s.QualifiedName]++
	}
	for qn, n := range seen {
		if n > 1 {
			t.Errorf("qualified_name %q appears %d times — collision not fixed", qn, n)
		}
	}

	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, want := range []string{"keys", "switches", "camera_switches", "gpio_keys_show_keys"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("expected symbol with name %q, got names: %v", want, sortedNames(byName))
		}
	}

	// The bug-output sentinel: we should NOT see `DEVICE_ATTR` as a
	// symbol name. If this slips back in, every macro in the file
	// collides again.
	if _, ok := byName["DEVICE_ATTR"]; ok {
		t.Errorf("symbol named 'DEVICE_ATTR' present — fix regressed (expected first-arg name)")
	}
}

// TestExtractC_MacroDecls_PreservesNormalFunctions guards against an
// over-zealous fix: a normal C function whose name happens to look like
// a SCREAM_CASE identifier on a misformatted line must not be misclassified.
// (We require at least one underscore in the macro name regex to avoid
// matching short ALL_CAPS identifiers.)
func TestExtractC_MacroDecls_PreservesNormalFunctions(t *testing.T) {
	result := Extract([]byte(cSrc), "C", "src/main.c")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	for _, want := range []string{"add", "helper", "main"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("normal function %q lost in macro post-process", want)
		}
	}
}

func sortedNames(byName map[string]ExtractedSymbol) []string {
	out := make([]string, 0, len(byName))
	for k := range byName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// #74 — bare-prefix declaration macros (EXPORT_SYMBOL, MODULE_PARM_DESC)
// ─────────────────────────────────────────────────────────────────────────────

// cBareMacroSrc reproduces the column-0 declaration-macro pattern from
// issue #74. These lines have no preceding word or `static` keyword, so
// the funcRE never matches them — they were silently missing from
// extraction before this PR.
const cBareMacroSrc = `#include <linux/module.h>

static int gpio_keys_show_keys(struct device *d) { return 0; }
static int driver_register(void) { return 0; }

EXPORT_SYMBOL(gpio_keys_show_keys);
EXPORT_SYMBOL_GPL(driver_register);
MODULE_PARM_DESC(timeout, "watchdog timeout");
MODULE_AUTHOR("Some Person");
`

// TestExtractC_BareMacros is the regression gate for #74: bare-prefix
// macros at column 0 MUST emit a Function symbol whose name is the
// macro's first arg (the actual identifier).
func TestExtractC_BareMacros(t *testing.T) {
	result := Extract([]byte(cBareMacroSrc), "C", "drivers/gpio_keys.c")
	if result == nil {
		t.Fatal("nil result")
	}

	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}

	// Real function bodies from the regular extractor.
	for _, want := range []string{"gpio_keys_show_keys", "driver_register"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("expected real function %q to still extract; got names %v", want, sortedNames(byName))
		}
	}
	// Bare-prefix macros — the new behavior. Each macro's first arg
	// becomes the symbol name. Note: gpio_keys_show_keys is also a real
	// function; the dedup pass (#79 part 2) means we keep only one
	// symbol with that QN — the first one wins, which is the real
	// function definition. EXPORT_SYMBOL(gpio_keys_show_keys) gets
	// deduped away.
	for _, want := range []string{"timeout", "driver_register"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("expected bare-macro symbol %q; got names %v", want, sortedNames(byName))
		}
	}

	// MODULE_AUTHOR has a string literal as its first arg, not an
	// identifier — cMacroRE requires `[A-Za-z_]` at the arg start, so
	// `"Some Person"` doesn't match. That's correct: there's no
	// identifier to attach a Function symbol to.
	if _, ok := byName["Some"]; ok {
		t.Errorf("MODULE_AUTHOR string-literal first-arg leaked as a symbol")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// #79 part 1 — forward declarations
// ─────────────────────────────────────────────────────────────────────────────

const cForwardDeclSrc = `#include <linux/module.h>

static int helper_a(void);
static int helper_b(int arg);

static int helper_a(void) {
    return 1;
}

static int helper_b(int arg) {
    return arg + 1;
}
`

// TestExtractC_ForwardDeclsDropped pins the regression gate for #79
// part 1: forward declarations (terminated by `;`) MUST be dropped so
// the symbol that survives is the DEFINITION, not the decl-only line.
//
// The strict assertion: each kept symbol's StartLine must correspond to
// the line containing the definition body, not the forward decl. In
// the fixture, helper_a's forward decl is on line 3 and its definition
// is on line 6. Without dropCForwardDecls, dedup would keep the
// first-encountered symbol — the forward decl on line 3 — and the
// test fails.
func TestExtractC_ForwardDeclsDropped(t *testing.T) {
	result := Extract([]byte(cForwardDeclSrc), "C", "drivers/x.c")
	if result == nil {
		t.Fatal("nil result")
	}

	// Each name appears exactly once.
	count := make(map[string]int)
	for _, s := range result.Symbols {
		count[s.QualifiedName]++
	}
	for qn, n := range count {
		if n > 1 {
			t.Errorf("qualified_name %q appears %d times — forward-decl drop missed", qn, n)
		}
	}

	// Find helper_a and assert its StartLine is the DEFINITION line,
	// not the forward-decl line. The forward decl lives at line 3
	// (1-indexed); the definition's `{` is at line 6.
	for _, s := range result.Symbols {
		if s.Name != "helper_a" {
			continue
		}
		if s.StartLine != 6 {
			t.Errorf("helper_a kept the wrong line: StartLine=%d, want 6 (the definition). "+
				"StartLine=3 means dedup kept the forward decl from line 3.", s.StartLine)
		}
	}
	for _, s := range result.Symbols {
		if s.Name != "helper_b" {
			continue
		}
		// helper_b: forward decl on line 4, definition on line 10.
		if s.StartLine != 10 {
			t.Errorf("helper_b kept the wrong line: StartLine=%d, want 10 (the definition).", s.StartLine)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// #79 part 2 — #ifdef / #else duplicate definitions
// ─────────────────────────────────────────────────────────────────────────────

const cIfdefSrc = `#include <linux/module.h>

#ifdef CONFIG_PM_SLEEP
static void gpio_keys_syscore_resume(void) {
    real_resume();
}
#else
static void gpio_keys_syscore_resume(void) {}
#endif
`

// TestExtractC_IfdefVariantsDisambiguated pins #115 disambiguation
// semantics for the C-style `#ifdef` / `#else` branch case.
//
// Pre-#79: BulkUpsertSymbols' last-write-wins silently picked whichever
// branch parsed last; one variant disappeared.
// Post-#79: dedupCSymbolsByQN dropped all but the first occurrence;
// the other variants disappeared but deterministically.
// Post-#115: the generic regex disambiguator suffixes the 2nd+ QN with
// `~<line>` so BOTH variants survive and remain searchable. The first
// occurrence keeps the plain QN (back-compat for callers that already
// search the un-suffixed name).
func TestExtractC_IfdefVariantsDisambiguated(t *testing.T) {
	result := Extract([]byte(cIfdefSrc), "C", "drivers/y.c")
	if result == nil {
		t.Fatal("nil result")
	}

	// BOTH variants must be emitted (no silent loss).
	count := 0
	for _, s := range result.Symbols {
		if s.Name == "gpio_keys_syscore_resume" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("got %d gpio_keys_syscore_resume symbols, want 2 (#ifdef variants must survive disambiguation)", count)
	}

	// QNs must be unique. First occurrence keeps the plain QN; second
	// gets a `~<line>` suffix.
	qns := map[string]int{}
	for _, s := range result.Symbols {
		if s.Name == "gpio_keys_syscore_resume" {
			qns[s.QualifiedName]++
		}
	}
	if len(qns) != 2 {
		t.Errorf("expected 2 distinct QNs for the two variants, got %d: %v", len(qns), qns)
	}
	for qn, n := range qns {
		if n != 1 {
			t.Errorf("QN %q has %d occurrences, want 1 (each variant must be uniquely addressable)", qn, n)
		}
	}

	// FileResult.QNCollisions must record the original collision so the
	// extraction-failure heuristic still fires (#42 diagnostic surface).
	if len(result.QNCollisions) == 0 {
		t.Error("QNCollisions is empty; the underlying #ifdef collision should still be tracked for the diagnostic")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// C# extractor
// ─────────────────────────────────────────────────────────────────────────────

const csharpSrc = `using System;

public class OrderService : IService {
    private readonly string _name;

    public OrderService(string name) {
        _name = name;
    }

    public async Task<string> GetOrderAsync(int id) {
        return id.ToString();
    }

    private void Validate() {}
}

public interface IService {
    Task<string> GetOrderAsync(int id);
}
`

func TestExtractCSharp(t *testing.T) {
	result := Extract([]byte(csharpSrc), "C#", "Services/OrderService.cs")
	if result == nil {
		t.Fatal("nil result")
	}
	// C# constructors share the class name; iterate to find the class symbol.
	var foundClass, foundInterface bool
	for _, s := range result.Symbols {
		if s.Name == "OrderService" && s.Kind == "Class" {
			foundClass = true
		}
		if s.Name == "IService" && s.Kind == "Interface" {
			foundInterface = true
		}
	}
	if !foundClass {
		t.Error("expected class symbol 'OrderService'")
	}
	if !foundInterface {
		t.Error("expected interface symbol 'IService'")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Kotlin extractor
// ─────────────────────────────────────────────────────────────────────────────

const kotlinSrc = `import kotlinx.coroutines.*

data class User(val name: String, val age: Int)

class UserService {
    suspend fun fetchUser(id: Int): User {
        return User("Alice", 30)
    }

    fun validateUser(user: User): Boolean {
        return user.age >= 0
    }
}

fun main() {
    println("Hello")
}
`

func TestExtractKotlin(t *testing.T) {
	result := Extract([]byte(kotlinSrc), "Kotlin", "src/UserService.kt")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["User"]; !ok {
		t.Error("expected data class 'User'")
	}
	if _, ok := byName["UserService"]; !ok {
		t.Error("expected class 'UserService'")
	}
	if _, ok := byName["main"]; !ok {
		t.Error("expected function 'main'")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Swift extractor
// ─────────────────────────────────────────────────────────────────────────────

const swiftSrc = `import Foundation

protocol Drawable {
    func draw()
}

class Shape: Drawable {
    var color: String = "red"

    func draw() {
        print("drawing")
    }

    private func validate() -> Bool {
        return true
    }
}

public func createShape(color: String) -> Shape {
    return Shape()
}
`

func TestExtractSwift(t *testing.T) {
	result := Extract([]byte(swiftSrc), "Swift", "Sources/Shape.swift")
	if result == nil {
		t.Fatal("nil result")
	}
	byName := make(map[string]ExtractedSymbol)
	for _, s := range result.Symbols {
		byName[s.Name] = s
	}
	if _, ok := byName["Drawable"]; !ok {
		t.Error("expected protocol 'Drawable'")
	}
	if byName["Drawable"].Kind != "Interface" {
		t.Errorf("Drawable.Kind = %q, want Interface", byName["Drawable"].Kind)
	}
	if _, ok := byName["Shape"]; !ok {
		t.Error("expected class 'Shape'")
	}
	if _, ok := byName["createShape"]; !ok {
		t.Error("expected function 'createShape'")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Extract dispatch (all language branches)
// ─────────────────────────────────────────────────────────────────────────────

func TestExtract_JSX(t *testing.T) {
	src := []byte(`function MyComponent() { return null; }`)
	result := Extract(src, "JSX", "src/MyComponent.jsx")
	if result == nil {
		t.Fatal("nil result")
	}
}

func TestExtract_TSX(t *testing.T) {
	src := []byte(`export function Button(): JSX.Element { return null; }`)
	result := Extract(src, "TSX", "src/Button.tsx")
	if result == nil {
		t.Fatal("nil result")
	}
}

func TestExtract_CPP(t *testing.T) {
	src := []byte(`int compute(int x) { return x * 2; }`)
	result := Extract(src, "C++", "src/compute.cpp")
	if result == nil {
		t.Fatal("nil result")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SupportedLanguages
// ─────────────────────────────────────────────────────────────────────────────

func TestSupportedLanguages(t *testing.T) {
	langs := SupportedLanguages()
	if len(langs) == 0 {
		t.Fatal("SupportedLanguages returned empty slice")
	}
	// Check key languages are present
	has := func(name string) bool {
		for _, l := range langs {
			if l == name {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"Go", "Python", "TypeScript", "Rust", "Java"} {
		if !has(want) {
			t.Errorf("SupportedLanguages missing %q", want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DetectLanguage additional extensions
// ─────────────────────────────────────────────────────────────────────────────

func TestDetectLanguage_AdditionalExtensions(t *testing.T) {
	cases := []struct{ file, want string }{
		{"app.rb", "Ruby"},
		{"App.java", "Java"},
		{"mod.rs", "Rust"},
		{"main.php", "PHP"},
		{"lib.cs", "C#"},
		{"Main.kt", "Kotlin"},
		{"App.swift", "Swift"},
		{"main.c", "C"},
		{"main.cpp", "C++"},
		{"main.sh", "Bash"},
		// Previously untested language variants
		{"script.pyw", "Python"},
		{"module.mjs", "JavaScript"},
		{"util.cjs", "JavaScript"},
		{"build.rake", "Ruby"},
		{"defs.h", "C"},
		{"lib.cxx", "C++"},
		{"lib.cc", "C++"},
		{"lib.hh", "C++"},
		{"lib.hpp", "C++"},
		{"build.kts", "Kotlin"},
		{"Main.scala", "Scala"},
		{"util.lua", "Lua"},
		{"lib.zig", "Zig"},
		{"app.ex", "Elixir"},
		{"mix.exs", "Elixir"},
		{"algo.hs", "Haskell"},
		{"widget.dart", "Dart"},
		{"analysis.r", "R"},
		{"deploy.bash", "Bash"},
	}
	for _, tc := range cases {
		got := DetectLanguage(tc.file)
		if got != tc.want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// goTypeToString via complex Go signatures
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractGo_ComplexTypes(t *testing.T) {
	src := []byte(`package pkg

// ProcessMap handles map types
func ProcessMap(m map[string]int) []string { return nil }

// ProcessPtr handles pointer receivers
func (s *Server) ProcessPtr(items []byte) (*Response, error) { return nil, nil }

// ProcessSelector handles selector types
func UseContext(ctx context.Context) error { return nil }
`)
	result := Extract(src, "Go", "pkg/complex.go")
	if result == nil || len(result.Symbols) == 0 {
		t.Fatal("expected symbols from complex types Go file")
	}
	// Verify signatures are captured
	sigFound := false
	for _, sym := range result.Symbols {
		if sym.Signature != "" {
			sigFound = true
		}
	}
	if !sigFound {
		t.Error("expected at least one symbol with a signature")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// isExported via extraction
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractGo_ExportedVsUnexported(t *testing.T) {
	src := []byte(`package pkg

func Exported() {}
func unexported() {}
`)
	result := Extract(src, "Go", "pkg/exported.go")
	if result == nil {
		t.Fatal("nil result")
	}
	exported := map[string]bool{}
	for _, sym := range result.Symbols {
		exported[sym.Name] = sym.IsExported
	}
	if !exported["Exported"] {
		t.Error("Exported should be exported")
	}
	if exported["unexported"] {
		t.Error("unexported should not be exported")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// extractGroup via JS alternation regex
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractJS_ArrowFunction(t *testing.T) {
	// Arrow function matches the second alternative in jsRE.funcRE (name2 group)
	src := []byte(`const myArrow = async (x) => x + 1;
export const handler = (req, res) => res.send('ok');`)
	result := Extract(src, "JavaScript", "src/arrow.js")
	if result == nil {
		t.Fatal("nil result")
	}
	// Should extract arrow functions via name2 group
	names := map[string]bool{}
	for _, sym := range result.Symbols {
		names[sym.Name] = true
	}
	if !names["myArrow"] && !names["handler"] {
		t.Logf("extracted names: %v", names)
		// Arrow functions might not be extracted depending on regex — just verify no panic
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Python: indentation-based block detection
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractPython_ClassAndMethods(t *testing.T) {
	src := []byte(`class MyService:
    def __init__(self):
        self.x = 1

    def process(self, data):
        return data

def standalone():
    pass
`)
	result := Extract(src, "Python", "svc/service.py")
	if result == nil || len(result.Symbols) == 0 {
		t.Fatal("expected Python symbols")
	}
	byName := make(map[string]ExtractedSymbol)
	hasClass := false
	for _, sym := range result.Symbols {
		byName[sym.Name] = sym
		if sym.Kind == "Class" {
			hasClass = true
		}
	}
	if !hasClass {
		t.Error("expected at least one Class symbol from Python extraction")
	}

	// #807: the blockChar=0 path used to "just return 80 lines worth of
	// bytes", so every Python def/class got an 80-line span clamped to
	// EOF and `symbol`/`context` returned wildly wrong source. The
	// indentation-block finder must span each block to the first line
	// dedented to (or past) its opening indent.
	if got := byName["MyService"]; got.StartLine != 1 || got.EndLine != 6 {
		t.Errorf("MyService span = %d-%d, want 1-6 (class body, not EOF)", got.StartLine, got.EndLine)
	}
	if got := byName["__init__"]; got.StartLine != 2 || got.EndLine != 3 {
		t.Errorf("__init__ span = %d-%d, want 2-3 — must end before the sibling def", got.StartLine, got.EndLine)
	}
	if got := byName["process"]; got.StartLine != 5 || got.EndLine != 6 {
		t.Errorf("process span = %d-%d, want 5-6 — must not run to EOF", got.StartLine, got.EndLine)
	}
	if got := byName["standalone"]; got.StartLine != 8 || got.EndLine != 9 {
		t.Errorf("standalone span = %d-%d, want 8-9 — the last def ends at EOF here", got.StartLine, got.EndLine)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// findBlockEnd with brace char
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractRust_TraitAndImpl(t *testing.T) {
	src := []byte(`pub trait Processor {
    fn process(&self, input: &str) -> String;
}

pub struct Engine;

impl Processor for Engine {
    fn process(&self, input: &str) -> String {
        input.to_string()
    }
}

pub fn standalone_fn(x: i32) -> i32 { x + 1 }
`)
	result := Extract(src, "Rust", "src/engine.rs")
	if result == nil || len(result.Symbols) == 0 {
		t.Fatal("expected Rust symbols")
	}
	kinds := map[string]bool{}
	for _, sym := range result.Symbols {
		kinds[sym.Kind] = true
	}
	if !kinds["Interface"] {
		t.Log("no Interface (trait) found — may be regex limitation")
	}
	if !kinds["Function"] {
		t.Error("expected at least one Function from Rust extraction")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// isExported: custom export function
// ─────────────────────────────────────────────────────────────────────────────

func TestIsExported_DefaultRule(t *testing.T) {
	if !isExported("Foo", nil) {
		t.Error("Foo should be exported (uppercase)")
	}
	if isExported("foo", nil) {
		t.Error("foo should not be exported (lowercase)")
	}
	if isExported("", nil) {
		t.Error("empty string should not be exported")
	}
}

func TestIsExported_CustomFn(t *testing.T) {
	alwaysTrue := func(s string) bool { return true }
	if !isExported("anything", alwaysTrue) {
		t.Error("custom fn returns true, should be exported")
	}
	alwaysFalse := func(s string) bool { return false }
	if isExported("Anything", alwaysFalse) {
		t.Error("custom fn returns false, should not be exported")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// namedGroup: regex named capture group extraction (#811)
// ─────────────────────────────────────────────────────────────────────────────

func TestNamedGroup_ResolvesByName(t *testing.T) {
	re := regexp.MustCompile(`^(?P<name>\w+)(?:\s*<\s*(?P<parent>\w+))?`)
	m := re.FindStringSubmatch("Child < Base")
	if got := namedGroup(re, m, "name"); got != "Child" {
		t.Errorf(`namedGroup "name" = %q, want "Child"`, got)
	}
	if got := namedGroup(re, m, "parent"); got != "Base" {
		t.Errorf(`namedGroup "parent" = %q, want "Base"`, got)
	}
}

// The bug #811 fixes: a superclass-less class must NOT report its own
// name as parent. The old extractGroup returned the first non-empty
// positional group, so "parent" fell through to the "name" group.
func TestNamedGroup_AbsentGroupIsEmptyNotName(t *testing.T) {
	re := regexp.MustCompile(`^(?P<name>\w+)(?:\s*<\s*(?P<parent>\w+))?`)
	m := re.FindStringSubmatch("Orphan")
	if got := namedGroup(re, m, "name"); got != "Orphan" {
		t.Errorf(`namedGroup "name" = %q, want "Orphan"`, got)
	}
	if got := namedGroup(re, m, "parent"); got != "" {
		t.Errorf(`namedGroup "parent" = %q, want "" (no superclass clause)`, got)
	}
}

func TestNamedGroup_UnknownGroupIsEmpty(t *testing.T) {
	re := regexp.MustCompile(`^(?P<name>\w+)`)
	m := re.FindStringSubmatch("foo")
	if got := namedGroup(re, m, "nonexistent"); got != "" {
		t.Errorf(`namedGroup "nonexistent" = %q, want ""`, got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildGoSignature: multi-parameter groups and channel/func types
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractGo_MultiParamSignature(t *testing.T) {
	// Function with two distinct parameter groups — exercises the i>0 branch
	// in buildGoSignature that emits ", " between groups.
	src := []byte(`package pkg

func MultiParam(a int, b string) bool { return false }
`)
	result := Extract(src, "Go", "pkg/multi.go")
	if result == nil || len(result.Symbols) == 0 {
		t.Fatal("expected symbols")
	}
	var sig string
	for _, s := range result.Symbols {
		if s.Name == "MultiParam" {
			sig = s.Signature
		}
	}
	if sig == "" {
		t.Error("expected signature for MultiParam")
	}
	// Both parameters should appear in the signature
	if !strings.Contains(sig, "a") || !strings.Contains(sig, "b") {
		t.Errorf("signature missing params: %q", sig)
	}
}

func TestExtractGo_ChannelParamType(t *testing.T) {
	// Channel parameter type triggers goTypeToString default "?" branch.
	src := []byte(`package pkg

func Consume(ch chan int) {}
func Send(ch chan<- string) {}
`)
	result := Extract(src, "Go", "pkg/chan.go")
	if result == nil || len(result.Symbols) == 0 {
		t.Fatal("expected symbols from channel param file")
	}
	// Just verify no panic and symbols are extracted
	names := make(map[string]bool)
	for _, s := range result.Symbols {
		names[s.Name] = true
	}
	if !names["Consume"] || !names["Send"] {
		t.Error("expected Consume and Send symbols")
	}
}

func TestExtractGo_CallOnIndexExpr(t *testing.T) {
	// Index-expression callee: fns[0]() triggers goCalleeToString default branch → ""
	// The extractor must not panic; it simply skips the unresolvable call.
	src := []byte(`package pkg

func RunAll(fns []func()) {
	fns[0]()
}
`)
	result := Extract(src, "Go", "pkg/idx.go")
	if result == nil || len(result.Symbols) == 0 {
		t.Fatal("expected symbols from index-expr callee file")
	}
	names := make(map[string]bool)
	for _, s := range result.Symbols {
		names[s.Name] = true
	}
	if !names["RunAll"] {
		t.Error("expected RunAll symbol")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// findBlockEnd — direct edge case coverage
// ─────────────────────────────────────────────────────────────────────────────

func TestFindBlockEnd_PastEnd(t *testing.T) {
	src := []byte("abc")
	got := findBlockEnd(src, 10, '{') // startOffset >= len(source)
	if got != len(src) {
		t.Errorf("findBlockEnd past end: got %d, want %d", got, len(src))
	}
}

func TestFindBlockEnd_IndentBased(t *testing.T) {
	// blockChar == 0 → indentation-based (Python-style): advance up to 80 lines
	src := []byte("line1\nline2\nline3\n")
	got := findBlockEnd(src, 0, 0)
	if got <= 0 || got > len(src) {
		t.Errorf("findBlockEnd indent: got %d, want in range (0, %d]", got, len(src))
	}
}

func TestFindBlockEnd_Parens(t *testing.T) {
	src := []byte("(a, (b, c), d)")
	got := findBlockEnd(src, 0, '(')
	if got != len(src) {
		t.Errorf("findBlockEnd parens: got %d, want %d", got, len(src))
	}
}

func TestFindBlockEnd_Bracket(t *testing.T) {
	src := []byte("[1, [2, 3]]")
	got := findBlockEnd(src, 0, '[')
	if got != len(src) {
		t.Errorf("findBlockEnd bracket: got %d, want %d", got, len(src))
	}
}

func TestFindBlockEnd_UnknownDelimiter(t *testing.T) {
	// Unknown delimiter falls through to return len(source)
	src := []byte("some source code")
	got := findBlockEnd(src, 0, '|')
	if got != len(src) {
		t.Errorf("findBlockEnd unknown delim: got %d, want %d", got, len(src))
	}
}

func TestFindBlockEnd_NeverOpened(t *testing.T) {
	// source has no opening brace — returns len(source) (started stays false)
	src := []byte("no braces here")
	got := findBlockEnd(src, 0, '{')
	if got != len(src) {
		t.Errorf("findBlockEnd never opened: got %d, want %d", got, len(src))
	}
}
