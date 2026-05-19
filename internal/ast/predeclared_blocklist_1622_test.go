package ast

import (
	"testing"
)

// #1622 v0.87: pin isPredeclaredOrBlank's expanded coverage of the
// Go predeclared-identifier set. v0.85 dogfood measured 95% drop
// rate on READS pending edges; expanding the blocklist from the
// historical 5-name set to the full Go spec §6.1 list cuts the
// drops where they generate no useful work.
//
// These tests are the contract: any future regression that
// removes a name here (or quietly shrinks the matcher) surfaces
// immediately. They also document the design choice — a project
// Variable named e.g. `int` would now be skipped, and that's
// considered acceptable per the function's doc comment.

func TestIsPredeclaredOrBlank_BlankAndConstants_1622(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"_", "true", "false", "nil", "iota"} {
		if !isPredeclaredOrBlank(name) {
			t.Errorf("isPredeclaredOrBlank(%q) = false; want true (legacy 5-name set)", name)
		}
	}
}

func TestIsPredeclaredOrBlank_PredeclaredTypes_1622(t *testing.T) {
	t.Parallel()
	types := []string{
		"bool", "byte", "complex64", "complex128", "error",
		"float32", "float64",
		"int", "int8", "int16", "int32", "int64", "rune", "string",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"any", "comparable",
	}
	for _, name := range types {
		if !isPredeclaredOrBlank(name) {
			t.Errorf("isPredeclaredOrBlank(%q) = false; predeclared type must be in blocklist", name)
		}
	}
}

func TestIsPredeclaredOrBlank_BuiltinFunctions_1622(t *testing.T) {
	t.Parallel()
	builtins := []string{
		"append", "cap", "clear", "close", "complex", "copy",
		"delete", "imag", "len", "make", "max", "min", "new",
		"panic", "print", "println", "real", "recover",
	}
	for _, name := range builtins {
		if !isPredeclaredOrBlank(name) {
			t.Errorf("isPredeclaredOrBlank(%q) = false; builtin must be in blocklist", name)
		}
	}
}

// Control: ordinary identifiers MUST NOT be in the blocklist. A
// regression where this returns true for `foo` would cause every
// project read to drop silently.
func TestIsPredeclaredOrBlank_RejectsOrdinaryNames_1622(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"foo", "bar", "Symbol", "Store", "extractor",
		"NewServer", "handleHealth", "myInt", "trueValue",
		// Substring overlaps with blocklist entries — must not match.
		"integer", "stringify", "errors",
	} {
		if isPredeclaredOrBlank(name) {
			t.Errorf("isPredeclaredOrBlank(%q) = true; ordinary identifier must NOT be blocked (would silently drop project reads)", name)
		}
	}
}

// Edge case: empty string must not panic and should return false
// (the caller already filters "" via the emit*-side checks, but
// the blocklist must not crash on it either).
func TestIsPredeclaredOrBlank_EmptyString_1622(t *testing.T) {
	t.Parallel()
	if isPredeclaredOrBlank("") {
		t.Error("isPredeclaredOrBlank(\"\") = true; empty string must be false (defensive)")
	}
}
