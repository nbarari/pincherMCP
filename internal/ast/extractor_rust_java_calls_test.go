package ast

import (
	"strings"
	"testing"
)

// #1159 v0.62: per-file CALLS pass for Rust + Java, parallel to TS
// #1158 and C #858. Pre-fix, neither language emitted CALLS edges —
// every Rust/Java project's trace/dead_code/neighborhood graph was
// empty, caught by the #858 edge-graph-empty warning.
//
// Tests follow the table-from-the-start shape (#1152): positive
// (CALLS emitted), negative (control-flow keywords filtered),
// control (empty body emits zero CALLS), and cross-check (confidence
// pinned at 0.6 regex-tier).

const rustWithCallsSrc = `pub fn bootstrap() {
	load_config();
	let c = parse_config();
	render(c);
}

pub fn load_config() -> Config {
	read_file()
}

pub fn parse_config() -> Config {
	read_file()
}
`

func TestExtractRust_PerFileCalls_EmitsEdgesFromBody(t *testing.T) {
	r := Extract([]byte(rustWithCallsSrc), "Rust", "src/boot.rs")
	if r == nil {
		t.Fatal("nil result")
	}
	wantTargets := map[string]bool{
		"load_config":  false,
		"parse_config": false,
		"render":       false,
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" || !strings.HasSuffix(e.FromQN, "::bootstrap") {
			continue
		}
		if _, expected := wantTargets[e.ToName]; expected {
			wantTargets[e.ToName] = true
		}
	}
	for target, found := range wantTargets {
		if !found {
			t.Errorf("Rust: expected CALLS edge bootstrap → %q; missing", target)
		}
	}
}

// Control: empty body emits zero CALLS.
const rustNoCallsSrc = `pub fn constant() -> i32 {
	42
}
`

func TestExtractRust_PerFileCalls_EmptyBodyEmitsNoCalls(t *testing.T) {
	r := Extract([]byte(rustNoCallsSrc), "Rust", "src/const.rs")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, e := range r.Edges {
		if e.Kind == "CALLS" && strings.HasSuffix(e.FromQN, "::constant") {
			t.Errorf("Rust call-free function emitted CALLS edge to %q", e.ToName)
		}
	}
}

// Cross-check: regex-tier confidence.
func TestExtractRust_PerFileCalls_ConfidenceIsRegexTier(t *testing.T) {
	r := Extract([]byte(rustWithCallsSrc), "Rust", "src/boot.rs")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" {
			continue
		}
		if e.Confidence != 0.6 {
			t.Errorf("Rust CALLS from %s confidence = %v; want 0.6", e.FromQN, e.Confidence)
		}
	}
}

// Java equivalents.
const javaWithCallsSrc = `public class Bootstrap {
	public static void run() {
		loadConfig();
		Config c = parseConfig();
		render(c);
	}

	public static Config loadConfig() {
		return readFile();
	}

	public static Config parseConfig() {
		return readFile();
	}
}
`

func TestExtractJava_PerFileCalls_EmitsEdgesFromMethod(t *testing.T) {
	r := Extract([]byte(javaWithCallsSrc), "Java", "src/Bootstrap.java")
	if r == nil {
		t.Fatal("nil result")
	}
	wantTargets := map[string]bool{
		"loadConfig":  false,
		"parseConfig": false,
		"render":      false,
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" || !strings.HasSuffix(e.FromQN, ".run") {
			continue
		}
		if _, expected := wantTargets[e.ToName]; expected {
			wantTargets[e.ToName] = true
		}
	}
	for target, found := range wantTargets {
		if !found {
			t.Errorf("Java: expected CALLS edge run → %q; missing", target)
		}
	}
}

const javaNoCallsSrc = `public class Constant {
	public int value() {
		return 42;
	}
}
`

func TestExtractJava_PerFileCalls_EmptyBodyEmitsNoCalls(t *testing.T) {
	r := Extract([]byte(javaNoCallsSrc), "Java", "src/Constant.java")
	if r == nil {
		t.Fatal("nil result")
	}
	for _, e := range r.Edges {
		if e.Kind == "CALLS" && strings.HasSuffix(e.FromQN, ".value") {
			t.Errorf("Java call-free method emitted CALLS edge to %q", e.ToName)
		}
	}
}

// Negative: control-flow keywords filtered for both Rust + Java via
// the shared regexCallKeywords blocklist.
const javaControlFlowSrc = `public class Guard {
	public boolean check(int x) {
		if (x == 0) {
			return true;
		}
		for (int i = 0; i < x; i++) {
			while (i > 100) { break; }
		}
		return false;
	}
}
`

func TestExtractJava_PerFileCalls_FiltersControlFlow(t *testing.T) {
	r := Extract([]byte(javaControlFlowSrc), "Java", "src/Guard.java")
	if r == nil {
		t.Fatal("nil result")
	}
	banned := map[string]struct{}{
		"if": {}, "for": {}, "while": {}, "switch": {},
		"return": {}, "throw": {}, "case": {}, "new": {},
	}
	for _, e := range r.Edges {
		if e.Kind != "CALLS" {
			continue
		}
		if _, b := banned[e.ToName]; b {
			t.Errorf("Java: control-flow keyword %q emitted as CALLS target", e.ToName)
		}
	}
}
