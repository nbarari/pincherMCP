package ast

import "testing"

// Tests for #259: JS regex must NOT match `const X = (expr).method()`
// patterns as Function symbols. The arrow-function branch now requires
// `=>` after the parameter list close.

// false-positive cases from the luci-app-travelmate dogfooding session.
// Pre-fix these rendered as kind=Function with confidence 0.975, polluting
// search results for `iface`, `zone`, `vpnMatch`.
func TestExtractJS_NotFunction_ChainedMethodCalls(t *testing.T) {
	src := []byte(`const iface = (document.getElementById('iface').value || 'trm_wwan').toLowerCase();
const zone  = (document.getElementById('zone').value  || 'wan').toLowerCase();
const vpnMatch = (info.data.ext_hooks || '').match(/vpn:\s*(.)/);
const trimmed = (input || '').trim();
const result = (a + b).toString();
const wrapped = (anything).method();
`)
	result := Extract(src, "JavaScript", "src/false_positives.js")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, sym := range result.Symbols {
		if sym.Kind == "Function" {
			t.Errorf("regression: extractJavaScript emitted Function symbol %q from a chained-method-call assignment\n  qualified=%q line=%d-%d",
				sym.Name, sym.QualifiedName, sym.StartLine, sym.EndLine)
		}
	}
}

// happy-path: real arrow functions still extract. Pin every arrow
// signature shape we care about so a future regex tweak that
// over-tightens surfaces in CI as a regression.
func TestExtractJS_RealArrowFunctionsStillExtract(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"empty params", "const f = () => 1;"},
		{"single param", "const f = (x) => x + 1;"},
		{"multi params", "const f = (a, b) => a + b;"},
		{"async", "const f = async (x) => await x;"},
		{"export const", "export const handler = (req, res) => res.send('ok');"},
		{"default value with call", "const f = (a = foo()) => a;"},
		{"destructured object", "const f = ({ a, b }) => a + b;"},
		{"destructured array", "const f = ([a, b]) => a + b;"},
		{"rest", "const f = (...args) => args.length;"},
		{"block body", "const f = (a) => { return a + 1; };"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := Extract([]byte(c.src), "JavaScript", "src/arrow.js")
			if result == nil {
				t.Fatal("nil result")
			}
			found := false
			for _, sym := range result.Symbols {
				if sym.Name == "f" || sym.Name == "handler" {
					if sym.Kind != "Function" {
						t.Errorf("symbol %q kind=%q, want Function", sym.Name, sym.Kind)
					}
					found = true
				}
			}
			if !found {
				t.Errorf("real arrow function regressed: no Function symbol extracted from\n  %s", c.src)
			}
		})
	}
}

// `function NAME` (the non-arrow branch) is unaffected by #259.
// Pin it explicitly so a regression there can be distinguished from
// an arrow regression.
func TestExtractJS_FunctionKeywordStillExtracts(t *testing.T) {
	src := []byte(`function add(a, b) { return a + b; }
async function fetchData(url) { return fetch(url); }
export function exported(x) { return x; }
export async function asyncExported(x) { return x; }
`)
	result := Extract(src, "JavaScript", "src/decl.js")
	if result == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{"add": true, "fetchData": true, "exported": true, "asyncExported": true}
	for _, sym := range result.Symbols {
		if sym.Kind == "Function" {
			delete(want, sym.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing Function symbols: %v", want)
	}
}

// Same matrix for TypeScript — the regex shares the bug and the fix.
func TestExtractTS_NotFunction_ChainedMethodCalls(t *testing.T) {
	src := []byte(`const iface: string = (document.getElementById('iface').value || 'trm_wwan').toLowerCase();
const vpnMatch: RegExpMatchArray | null = (info.data.ext_hooks || '').match(/vpn:\s*(.)/);
`)
	result := Extract(src, "TypeScript", "src/false_positives.ts")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, sym := range result.Symbols {
		if sym.Kind == "Function" {
			t.Errorf("TS regression: emitted Function symbol %q from chained-method-call assignment", sym.Name)
		}
	}
}

// #260: object-literal methods extract as Function symbols.
//
// The LuCI view.extend({...}) idiom and Vue 2 methods: { … } block
// are the highest-volume regex-era miss in JS extraction. Pin the
// shapes we now match so a future regex change can't silently
// regress them.
// #261: top-level const/let/var emit Variable symbols.
//
// ESLint flat configs and constants modules consist entirely of
// top-level value declarations and previously contributed zero
// symbols to the index. Pin the shape we now emit so they're
// discoverable via search.
func TestExtractJS_TopLevelVarsEmitVariable(t *testing.T) {
	src := []byte(`export const jsdoc_relaxed_rules = {
    'jsdoc/check-alignment': 'warn',
};

export const jsdoc_strict_rules = {
    'jsdoc/check-types': 'error',
};

const internal_constant = 42;
let mutable_state = null;
var legacy_global = 'hi';
`)
	result := Extract(src, "JavaScript", "src/eslint.config.mjs")
	if result == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{
		"jsdoc_relaxed_rules": true,
		"jsdoc_strict_rules":  true,
		"internal_constant":   true,
		"mutable_state":       true,
		"legacy_global":       true,
	}
	exported := map[string]bool{}
	for _, sym := range result.Symbols {
		if sym.Kind == "Variable" {
			delete(want, sym.Name)
			if sym.IsExported {
				exported[sym.Name] = true
			}
		}
	}
	if len(want) > 0 {
		t.Errorf("missing Variable symbols: %v", want)
	}
	for _, name := range []string{"jsdoc_relaxed_rules", "jsdoc_strict_rules"} {
		if !exported[name] {
			t.Errorf("expected %q to be is_exported=true", name)
		}
	}
	for _, name := range []string{"internal_constant", "mutable_state", "legacy_global"} {
		if exported[name] {
			t.Errorf("expected %q to be is_exported=false (no `export` keyword on the line)", name)
		}
	}
}

// #261: an arrow-function declaration must NOT also emit a Variable
// symbol — the Function emission already covers it. Otherwise we'd
// double-emit on the most common shape in modern JS.
func TestExtractJS_ArrowFunctionDoesNotDoubleEmitVariable(t *testing.T) {
	src := []byte(`export const handler = (req, res) => res.send('ok');
const noop = () => {};
`)
	result := Extract(src, "JavaScript", "src/handlers.js")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, sym := range result.Symbols {
		if sym.Kind == "Variable" && (sym.Name == "handler" || sym.Name == "noop") {
			t.Errorf("regression: arrow function %q double-emitted as Variable", sym.Name)
		}
	}
}

func TestExtractTS_TopLevelVarsEmitVariable(t *testing.T) {
	src := []byte(`export const FOO: number = 42;
export const BAR: { a: string } = { a: 'x' };
const tuple: [number, string] = [1, 'a'];
`)
	result := Extract(src, "TypeScript", "src/constants.ts")
	if result == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{"FOO": true, "BAR": true, "tuple": true}
	for _, sym := range result.Symbols {
		if sym.Kind == "Variable" {
			delete(want, sym.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing TS Variable symbols: %v", want)
	}
}

func TestExtractJS_ObjectLiteralMethods(t *testing.T) {
	src := []byte(`return view.extend({
    load: function () {
        return Promise.all([fetch('/api')]);
    },
    render: function (result) {
        return E('div', {}, [result]);
    },
    onClick: async function (ev) {
        await this.handle(ev);
    },
    onChange: (ev) => {
        this.update(ev);
    },
});
`)
	result := Extract(src, "JavaScript", "src/view.js")
	if result == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{"load": true, "render": true, "onClick": true, "onChange": true}
	for _, sym := range result.Symbols {
		if sym.Kind == "Function" || sym.Kind == "Method" {
			delete(want, sym.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing object-literal method symbols: %v", want)
	}
}

// Negative: arbitrary `name: value` lines without a function form
// must NOT extract. Especially `case foo:`, `default:`, and plain
// property assignments like `foo: 'bar'`.
func TestExtractJS_NotMethod_PlainProperties(t *testing.T) {
	src := []byte(`const config = {
    name: 'pincher',
    version: 17,
    enabled: true,
    items: [1, 2, 3],
};

switch (kind) {
    case 'a':
        break;
    default:
        break;
}
`)
	result := Extract(src, "JavaScript", "src/config.js")
	if result == nil {
		t.Fatal("nil result")
	}
	for _, sym := range result.Symbols {
		if sym.Kind == "Function" || sym.Kind == "Method" {
			t.Errorf("regression: emitted %s symbol %q from a non-function property", sym.Kind, sym.Name)
		}
	}
}

func TestExtractTS_ObjectLiteralMethods(t *testing.T) {
	src := []byte(`export default {
    load: function (): Promise<void> {
        return fetch('/api');
    },
    render: (data: Result): Element => {
        return E('div', {}, [data]);
    },
    handle: async function (ev: Event) {
        await this.process(ev);
    },
};
`)
	result := Extract(src, "TypeScript", "src/view.ts")
	if result == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{"load": true, "render": true, "handle": true}
	for _, sym := range result.Symbols {
		if sym.Kind == "Function" || sym.Kind == "Method" {
			delete(want, sym.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing TS object-literal methods: %v", want)
	}
}

func TestExtractTS_RealArrowFunctionsStillExtract(t *testing.T) {
	src := []byte(`const f = (a: number, b: string): string => b + a;
const g = async (x: number) => await Promise.resolve(x);
export const handler = (req: Request, res: Response) => res.send('ok');
`)
	result := Extract(src, "TypeScript", "src/arrow.ts")
	if result == nil {
		t.Fatal("nil result")
	}
	want := map[string]bool{"f": true, "g": true, "handler": true}
	for _, sym := range result.Symbols {
		if sym.Kind == "Function" {
			delete(want, sym.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing TS arrow functions: %v", want)
	}
}
