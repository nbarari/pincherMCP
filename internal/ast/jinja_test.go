package ast

import (
	"sort"
	"strings"
	"testing"
)

// jinjaExtract is a focused helper.
func jinjaExtract(t *testing.T, src string) *FileResult {
	t.Helper()
	r := Extract([]byte(src), "Jinja2", "templates/test.j2")
	if r == nil {
		t.Fatal("nil result")
	}
	return r
}

// TestExtractJinja2_MacroAndBlock pins the two structural-symbol kinds:
// {% macro %} → Function, {% block %} → Block. Both emit qualified
// names rooted at the file's module name.
func TestExtractJinja2_MacroAndBlock(t *testing.T) {
	r := jinjaExtract(t, `{% block header %}
  <h1>{{ title }}</h1>
{% endblock %}

{% macro render_link(href, text) %}
  <a href="{{ href }}">{{ text }}</a>
{% endmacro %}
`)

	byName := map[string]ExtractedSymbol{}
	for _, s := range r.Symbols {
		byName[s.Name] = s
	}

	if got, ok := byName["header"]; !ok {
		t.Errorf("missing 'header' block symbol")
	} else {
		if got.Kind != "Block" {
			t.Errorf("header.Kind = %q, want Block", got.Kind)
		}
		if got.QualifiedName != "test.header" {
			t.Errorf("header.QN = %q, want test.header", got.QualifiedName)
		}
	}

	if got, ok := byName["render_link"]; !ok {
		t.Errorf("missing 'render_link' macro symbol")
	} else {
		if got.Kind != "Function" {
			t.Errorf("render_link.Kind = %q, want Function", got.Kind)
		}
		if got.QualifiedName != "test.render_link" {
			t.Errorf("render_link.QN = %q, want test.render_link", got.QualifiedName)
		}
		if !strings.Contains(got.Signature, "render_link") {
			t.Errorf("signature missing macro name: %q", got.Signature)
		}
	}
}

// TestExtractJinja2_SetEmitsSetting pins {% set var = expr %} → Setting.
func TestExtractJinja2_SetEmitsSetting(t *testing.T) {
	r := jinjaExtract(t, `{% set timeout = 30 %}
{% set retries = 3 %}
{% set _internal = "hidden" %}
`)

	byName := map[string]ExtractedSymbol{}
	for _, s := range r.Symbols {
		byName[s.Name] = s
	}

	for _, want := range []string{"timeout", "retries", "_internal"} {
		got, ok := byName[want]
		if !ok {
			t.Errorf("missing 'set' symbol %q", want)
			continue
		}
		if got.Kind != "Setting" {
			t.Errorf("%s.Kind = %q, want Setting", want, got.Kind)
		}
	}

	// _internal is the Jinja convention for "private"; IsExported MUST
	// be false. Mirrors the Bash extractor's underscore-prefix rule.
	if byName["_internal"].IsExported {
		t.Errorf("_internal should not be exported")
	}
	if !byName["timeout"].IsExported {
		t.Errorf("timeout should be exported")
	}
}

// TestExtractJinja2_ImportEdges pins {% extends/include/import %} →
// IMPORTS edges. The edge target is the literal filename string.
func TestExtractJinja2_ImportEdges(t *testing.T) {
	r := jinjaExtract(t, `{% extends "base.j2" %}
{% include "header.j2" %}
{% import "macros.j2" as m %}
{% from "filters.j2" import slugify %}
`)

	targets := make([]string, 0, len(r.Edges))
	for _, e := range r.Edges {
		if e.Kind != "IMPORTS" {
			t.Errorf("edge to %q has kind %q, want IMPORTS", e.ToName, e.Kind)
		}
		if e.Confidence != 1.0 {
			t.Errorf("edge to %q has confidence %v, want 1.0", e.ToName, e.Confidence)
		}
		targets = append(targets, e.ToName)
	}
	sort.Strings(targets)

	want := []string{"base.j2", "filters.j2", "header.j2", "macros.j2"}
	if len(targets) != len(want) {
		t.Fatalf("got %d edges, want %d (targets=%v)", len(targets), len(want), targets)
	}
	for i, w := range want {
		if targets[i] != w {
			t.Errorf("edge[%d] = %q, want %q", i, targets[i], w)
		}
	}
}

// TestExtractJinja2_AnsibleFilters confirms that templates using
// Ansible-specific filters (`default`, `to_json`, `b64encode`, etc.)
// parse without erroring. gonja's parser doesn't validate filter
// names — the unknown-filter check is commented out — so we don't
// need to register filter stubs.
func TestExtractJinja2_AnsibleFilters(t *testing.T) {
	r := jinjaExtract(t, `# {{ ansible_hostname | default("unknown") }}
{% set token = secret_value | b64encode %}
{% set config = my_dict | to_json %}
{% set parsed = data | from_yaml %}
`)
	// Should produce 3 Setting symbols without errors.
	count := 0
	for _, s := range r.Symbols {
		if s.Kind == "Setting" {
			count++
		}
	}
	if count != 3 {
		t.Errorf("got %d Setting symbols, want 3 (Ansible filters caused parse errors?)", count)
	}
}

// TestExtractJinja2_EmptyDocument — no symbols, no crash.
func TestExtractJinja2_EmptyDocument(t *testing.T) {
	r := jinjaExtract(t, "")
	if len(r.Symbols) != 0 || len(r.Edges) != 0 {
		t.Errorf("empty doc produced %d symbols / %d edges, want 0/0",
			len(r.Symbols), len(r.Edges))
	}
}

// TestExtractJinja2_ProseOnly — a template with only data + variable
// interpolation (no statements) emits zero symbols. This is the
// common case for many Ansible config templates.
func TestExtractJinja2_ProseOnly(t *testing.T) {
	r := jinjaExtract(t, "Hello {{ name }}, welcome to {{ host }}.\n")
	if len(r.Symbols) != 0 {
		t.Errorf("prose-only template produced %d symbols, want 0", len(r.Symbols))
	}
}

// TestExtractJinja2_MalformedDoesNotPanic — gonja shouldn't panic on
// any input, but the recover() boundary in Extract is the safety net.
// Mirrors the HCL/Markdown panic-boundary pattern.
func TestExtractJinja2_MalformedDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Extract panicked on malformed input: %v", r)
		}
	}()
	for _, src := range []string{
		"{% block",                     // truncated mid-tag
		"{% block name %}{% endblock", // missing closing
		"{{ unclosed",
		"{% set %}",                   // missing target
		string([]byte{0xff, 0xfe, 0}), // invalid UTF-8
	} {
		_ = Extract([]byte(src), "Jinja2", "templates/x.j2")
	}
}

// TestExtractJinja2_RegisteredExtensions guards the extension list.
func TestExtractJinja2_RegisteredExtensions(t *testing.T) {
	for _, ext := range []string{".j2", ".jinja", ".jinja2"} {
		got := DetectLanguage("test" + ext)
		if got != "Jinja2" {
			t.Errorf("DetectLanguage(test%s) = %q, want Jinja2", ext, got)
		}
	}
}

// TestExtractJinja2_ConfidenceIs1 confirms parser-backed routing
// produces confidence 1.0.
func TestExtractJinja2_ConfidenceIs1(t *testing.T) {
	r := jinjaExtract(t, "{% set x = 1 %}\n")
	if len(r.Symbols) != 1 {
		t.Fatalf("got %d, want 1", len(r.Symbols))
	}
	if r.Symbols[0].ExtractionConfidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", r.Symbols[0].ExtractionConfidence)
	}
}
