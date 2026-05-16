package server

import (
	"strings"
	"testing"
)

// v0.65 description-honesty audit (continuation): the fetch tool's
// description told agents to use `search kind:Document` — but that
// colon syntax is FTS5-operator-style and is NOT supported on the
// `kind` argument, which is a plain JSON string parameter
// (`{"query":"...","kind":"Document"}`). Agents following the
// description literally would pass `query="kind:Document"` as the
// FTS5 search string and get bizarre BM25 results — silent failure,
// no error path.
//
// Table-from-the-start (#1152):
//   - Positive: description names the correct argument form
//     (`kind="Document"` or `kind=Document`).
//   - Negative: stale FTS5-operator-style `kind:Document` framing
//     pinned against regression.
//   - Cross-check: the schema's `kind` parameter description lists
//     `Document` as a valid value (so the description's
//     recommendation matches the schema's vocabulary).

func TestFetchDescription_NamesCorrectKindSyntax(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tool := srv.tools["fetch"]
	if tool == nil {
		t.Fatal("fetch tool not registered")
	}
	desc := tool.Description

	// Positive: description names the arg-style syntax.
	// Either `kind="Document"` or `kind=\"Document\"` (escaped) is
	// acceptable since the schema parses both — pin on a
	// substring that excludes the broken colon form.
	if !strings.Contains(desc, `kind=`) {
		t.Errorf("fetch description missing arg-style `kind=` recommendation\nGOT:\n%s", desc)
	}
	if !strings.Contains(desc, "Document") {
		t.Errorf("fetch description doesn't name Document kind\nGOT:\n%s", desc)
	}
}

// Negative: pin the FTS5-operator-style "kind:Document" syntax
// won't slip back. This was the stale form that misled agents
// into passing it as a query string.
func TestFetchDescription_DoesNotRecommendFTSOperatorSyntax(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tool := srv.tools["fetch"]
	if tool == nil {
		t.Fatal("fetch tool not registered")
	}
	desc := tool.Description
	// The specific stale fragment was "search kind:Document"
	// (colon syntax, no quotes). FTS5 supports `field:term` for
	// columns but pincher's search treats `kind` as a JSON arg,
	// not an FTS5 column.
	if strings.Contains(desc, "search kind:Document") {
		t.Errorf("fetch description still recommends FTS5-operator-style `kind:Document` — that syntax is not supported on the search tool's `kind` argument\nGOT:\n%s", desc)
	}
}

// Cross-check: the description's `Document` recommendation
// matches the search tool's `kind` schema vocabulary. Pre-fix a
// description could recommend a kind value that the schema's
// own description doesn't list, which is a different drift class
// — pin it here in lockstep.
func TestFetchDescription_DocumentIsInSearchKindVocabulary(t *testing.T) {
	searchSchema := findSearchToolSchema(t)
	props, _ := searchSchema["properties"].(map[string]any)
	kindField, _ := props["kind"].(map[string]any)
	kindDesc, _ := kindField["description"].(string)
	if !strings.Contains(kindDesc, "Document") {
		t.Errorf("fetch description points at Document kind but search.kind schema doesn't list it\nSEARCH.KIND DESCRIPTION:\n%s", kindDesc)
	}
}
