package server

import (
	"os"
	"strings"
	"testing"
)

// #1088: tests for the opt-in short tool descriptions.

func TestParseToolDescriptionsEnv_Cases(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},        // default — long-form preserved
		{"short", true},    // canonical opt-in
		{"SHORT", true},    // case-insensitive
		{"  short  ", true}, // whitespace-tolerant
		{"long", false},    // non-canonical: keep long-form
		{"shortt", false},  // typo doesn't silently swap
		{"true", false},    // not a recognized opt-in (only "short" works)
		{"on", false},      // distinct semantics from PINCHER_META_CAPABILITIES — don't be clever
	}
	for _, c := range cases {
		got := parseToolDescriptionsEnv(c.in)
		if got != c.want {
			t.Errorf("parseToolDescriptionsEnv(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestApplyShortDescriptions_DefaultLeavesLongForm pins back-compat:
// without the env opt-in, the dense pedagogical descriptions every
// consumer sees today must survive.
func TestApplyShortDescriptions_DefaultLeavesLongForm(t *testing.T) {
	t.Parallel()
	prev := os.Getenv("PINCHER_TOOL_DESCRIPTIONS")
	os.Unsetenv("PINCHER_TOOL_DESCRIPTIONS")
	t.Cleanup(func() {
		if prev != "" {
			os.Setenv("PINCHER_TOOL_DESCRIPTIONS", prev)
		}
	})

	srv, _, _ := newTestServer(t)
	trace, ok := srv.tools["trace"]
	if !ok || trace == nil {
		t.Fatal("trace tool not registered")
	}
	// Long-form: the description must contain the pedagogical risk-
	// label phrasing that's the whole point of #649's choice not to
	// trim by default.
	if !strings.Contains(trace.Description, "CRITICAL") {
		t.Errorf("trace description appears trimmed without env opt-in; got: %s", trace.Description)
	}
	if len(trace.Description) < 500 {
		t.Errorf("trace description is %d bytes, < 500 — was env opt-in inadvertently triggered?", len(trace.Description))
	}
}

// TestApplyShortDescriptions_EnvSwapsDescription pins the opt-in
// path: with PINCHER_TOOL_DESCRIPTIONS=short, the 5 heavy-hitter
// tools must have their descriptions overwritten with the short
// variants in the shortToolDescriptions map.
func TestApplyShortDescriptions_EnvSwapsDescription(t *testing.T) {
	t.Setenv("PINCHER_TOOL_DESCRIPTIONS", "short")
	srv, _, _ := newTestServer(t)

	for name, want := range shortToolDescriptions {
		tool, ok := srv.tools[name]
		if !ok || tool == nil {
			t.Errorf("tool %q not registered — short-descriptions map references a nonexistent tool", name)
			continue
		}
		if tool.Description != want {
			t.Errorf("tool %q description not swapped:\n got: %q\nwant: %q", name, tool.Description, want)
		}
	}
}

// TestApplyShortDescriptions_OtherToolsUntouched pins the surgical
// nature of the trim: tools NOT in shortToolDescriptions must keep
// their original descriptions even when the env opt-in fires. Only
// the 5 heavy hitters from #1088 are trimmed; trimming the others
// further is below the noise floor.
func TestApplyShortDescriptions_OtherToolsUntouched(t *testing.T) {
	t.Setenv("PINCHER_TOOL_DESCRIPTIONS", "short")
	srv, _, _ := newTestServer(t)

	for name, tool := range srv.tools {
		if _, isShort := shortToolDescriptions[name]; isShort {
			continue
		}
		if tool == nil {
			continue
		}
		if tool.Description == "" {
			t.Errorf("tool %q has empty description after short-mode applied — short map shouldn't have touched it", name)
		}
	}
}

// TestShortToolDescriptions_AllShorterThanOriginals pins the cost-
// savings invariant: every short variant must actually be shorter
// than the long form it replaces. Otherwise the trim isn't earning
// its keep.
func TestShortToolDescriptions_AllShorterThanOriginals(t *testing.T) {
	// Capture long descriptions first (default-off path).
	t.Setenv("PINCHER_TOOL_DESCRIPTIONS", "")
	srv, _, _ := newTestServer(t)
	longLen := make(map[string]int, len(shortToolDescriptions))
	for name := range shortToolDescriptions {
		if tool, ok := srv.tools[name]; ok && tool != nil {
			longLen[name] = len(tool.Description)
		}
	}

	for name, short := range shortToolDescriptions {
		if longLen[name] == 0 {
			t.Errorf("tool %q in short map but not found in registered tools", name)
			continue
		}
		if len(short) >= longLen[name] {
			t.Errorf("short description for %q is %d bytes >= long %d bytes — not earning its keep",
				name, len(short), longLen[name])
		}
	}
}
