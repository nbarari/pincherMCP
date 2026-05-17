package ast

import (
	"testing"
)

// #1324 v0.71: kernel-style C pairs `static int foo(...) { ... }` with
// `EXPORT_SYMBOL(foo);` further down in the file. Pre-fix, both
// emitted a Function symbol with QN `<path>::foo` — the regular
// extraction at the definition AND the bare-macro pass in
// extractCBareMacros. extractCBareMacros's start-byte dedup didn't
// fire (the EXPORT_SYMBOL line sits at a different offset), so the
// disambiguator added `~<line>` suffixes and the qualified_name_
// collision diagnostic fired on every kernel driver file.
//
// Fix: extractCBareMacros also dedupes by Function name. When the
// macro arg matches the name of an already-extracted Function, skip
// the macro emission — the real definition is the symbol of record.
//
// Tests follow the four-case shape: positive (collision goes away),
// negative (macro-only export without a definition is still emitted),
// control (non-Function symbols with the same name don't suppress),
// cross-check (a macro arg that's a substring of a real function name
// doesn't collide and emits normally).

const cKernelStyleExportSymbolSrc = `#include <linux/module.h>

static int gpio_keys_probe(struct platform_device *pdev) {
	return 0;
}

static int gpio_keys_remove(struct platform_device *pdev) {
	return 0;
}

EXPORT_SYMBOL(gpio_keys_probe);
EXPORT_SYMBOL(gpio_keys_remove);
`

func TestExtractC_ExportSymbolDoesNotCollideWithDefinition_1324(t *testing.T) {
	result := Extract([]byte(cKernelStyleExportSymbolSrc), "C", "drivers/input/gpio_keys.c")
	if result == nil {
		t.Fatal("nil result")
	}

	// Positive: no qualified-name collision. Each name appears at
	// most once across all Function symbols.
	qnCount := map[string]int{}
	for _, s := range result.Symbols {
		if s.Kind != "Function" {
			continue
		}
		qnCount[s.QualifiedName]++
	}
	for qn, n := range qnCount {
		if n > 1 {
			t.Errorf("qualified_name %q appears %d times — kernel-style EXPORT_SYMBOL should dedupe against the real definition", qn, n)
		}
	}

	// Both names must still be present (we kept the definitions, not
	// the macro stubs).
	for _, want := range []string{"gpio_keys_probe", "gpio_keys_remove"} {
		found := false
		for _, s := range result.Symbols {
			if s.Kind == "Function" && s.Name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected Function symbol %q to survive (real definition kept); not found", want)
		}
	}
}

// Negative control: a bare-macro export with NO matching in-file
// definition (the symbol is defined in another TU and only the
// macro annotation lives here) must still emit. The dedup only
// fires when the name was already extracted as a Function.
func TestExtractC_ExportSymbolWithoutDefinitionStillEmits_1324(t *testing.T) {
	src := `EXPORT_SYMBOL(external_helper);
`
	result := Extract([]byte(src), "C", "drivers/input/gpio_keys_glue.c")
	if result == nil {
		t.Fatal("nil result")
	}

	found := false
	for _, s := range result.Symbols {
		if s.Kind == "Function" && s.Name == "external_helper" {
			found = true
			break
		}
	}
	if !found {
		t.Error("EXPORT_SYMBOL with no in-file definition must still emit the macro-arg symbol; got none")
	}
}

// Cross-check: a function NAME that's a substring of the macro arg
// must NOT suppress. `probe` and `gpio_keys_probe` are distinct.
func TestExtractC_NameSubstringDoesNotSuppressMacro_1324(t *testing.T) {
	src := `static int probe(void) {
	return 0;
}

EXPORT_SYMBOL(gpio_keys_probe);
`
	result := Extract([]byte(src), "C", "drivers/input/gpio_keys.c")
	if result == nil {
		t.Fatal("nil result")
	}

	gotProbe := false
	gotGpioKeysProbe := false
	for _, s := range result.Symbols {
		if s.Kind != "Function" {
			continue
		}
		if s.Name == "probe" {
			gotProbe = true
		}
		if s.Name == "gpio_keys_probe" {
			gotGpioKeysProbe = true
		}
	}
	if !gotProbe {
		t.Error("real probe() definition must be kept")
	}
	if !gotGpioKeysProbe {
		t.Error("EXPORT_SYMBOL(gpio_keys_probe) must emit — distinct name, not a substring suppression target")
	}
}
