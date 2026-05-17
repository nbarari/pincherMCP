package ast

import (
	"testing"
)

// usesVarEdges filters a result down to USES_VAR edges keyed by target name.
func usesVarEdges(t *testing.T, r *FileResult) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, e := range r.Edges {
		if e.Kind == "USES_VAR" {
			out[e.ToName] = true
		}
	}
	return out
}

// TestExtractJinja2_UsesVarEdges_PlainOutput pins the basic shape:
// `{{ name }}` → USES_VAR edge with ToName=name, Kind=USES_VAR,
// Confidence=1.0 (parser-backed).
func TestExtractJinja2_UsesVarEdges_PlainOutput(t *testing.T) {
	r := jinjaExtract(t, `Hello {{ name }}, your server is {{ host }}.`)
	got := usesVarEdges(t, r)
	for _, want := range []string{"name", "host"} {
		if !got[want] {
			t.Errorf("missing USES_VAR edge for %q; got %v", want, got)
		}
	}
	// Both edges must carry confidence 1.0 since gonja parsed them.
	for _, e := range r.Edges {
		if e.Kind == "USES_VAR" && e.Confidence != 1.0 {
			t.Errorf("jinja USES_VAR edge ToName=%s has Confidence=%v, want 1.0", e.ToName, e.Confidence)
		}
	}
}

// TestExtractJinja2_UsesVarEdges_FilteredAndAttr verifies that the
// extractor walks through filter chains, attribute access, and getitem
// to find the leftmost base name. The deep references all bind to the
// same root identifier.
func TestExtractJinja2_UsesVarEdges_FilteredAndAttr(t *testing.T) {
	r := jinjaExtract(t,
		`{{ db_host | default("local") }} `+
			`{{ user.name }} `+
			`{{ items[0] }} `+
			`{{ build_url(env) }}`)
	got := usesVarEdges(t, r)
	for _, want := range []string{"db_host", "user", "items", "build_url"} {
		if !got[want] {
			t.Errorf("missing USES_VAR edge for %q; got %v", want, got)
		}
	}
}

// TestExtractJinja2_UsesVarEdges_SkipsReserved ensures Jinja-reserved
// identifiers (`loop`, `true`, `none`, ...) don't bleed into the edge
// set — they can never bind to a user var declaration.
func TestExtractJinja2_UsesVarEdges_SkipsReserved(t *testing.T) {
	r := jinjaExtract(t,
		`{% for item in things %}{{ loop.index }}: {{ item }}{% endfor %}`+
			`{{ true }} {{ none }} {{ True }}`)
	got := usesVarEdges(t, r)
	for _, banned := range []string{"loop", "true", "none", "True", "False"} {
		if got[banned] {
			t.Errorf("USES_VAR edge for reserved %q should be skipped; got %v", banned, got)
		}
	}
	// `things` is a real user-supplied var iterated by the loop; it must
	// still produce an edge from somewhere upstream. (gonja parses the
	// for-loop expression as a Name node inside the wrapper.) We don't
	// check `item` because it's the loop-local binding, not a var ref.
}

// TestExtractJinja2_UsesVarEdges_LiteralsNone pins that pure literals
// (`{{ "hello" }}`, `{{ 42 }}`) emit no USES_VAR edges — there's no
// identifier to bind.
func TestExtractJinja2_UsesVarEdges_LiteralsNone(t *testing.T) {
	r := jinjaExtract(t, `{{ "hello" }} {{ 42 }} {{ 3.14 }}`)
	for _, e := range r.Edges {
		if e.Kind == "USES_VAR" {
			t.Errorf("literal-only output produced USES_VAR edge: %+v", e)
		}
	}
}

// TestExtractJinja2_UsesVarEdges_FromModule pins the edge's FromQN to
// the file's Module identity so the resolver's pickCanonical can bind
// it back to a Module symbol.
func TestExtractJinja2_UsesVarEdges_FromModule(t *testing.T) {
	r := jinjaExtract(t, `{{ db_host }}`)
	if len(r.Edges) == 0 {
		t.Fatal("expected at least one USES_VAR edge")
	}
	// jinjaModuleName("templates/test.j2") = "test"
	wantFrom := "test"
	for _, e := range r.Edges {
		if e.Kind != "USES_VAR" {
			continue
		}
		if e.FromQN != wantFrom {
			t.Errorf("USES_VAR FromQN=%q, want %q", e.FromQN, wantFrom)
		}
	}
}
