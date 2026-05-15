package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// When ONLY the legacy `cypher` parameter is passed (no `pinchql`),
// the handler honored it silently — agents using the alias had no
// signal the migration window was closing. Now it fires a
// deprecation warning in `_meta.warnings`, matching the corpus="all"
// soft-redirect pattern (#935).

func TestQuery_CypherAliasOnly_FiresDeprecationWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "qcyp"
	srv.sessionRoot = "/tmp/qcyp"
	store.UpsertProject(db.Project{ID: "qcyp", Path: "/tmp/qcyp", Name: "qcyp", IndexedAt: time.Now()})

	res, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"cypher": "MATCH (n:Function) RETURN n.name LIMIT 1",
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got error %s", textOf(t, res))
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warns, _ := meta["warnings"].([]any)
	foundDeprecation := false
	for _, w := range warns {
		if s, _ := w.(string); strings.Contains(s, "deprecated") && strings.Contains(s, "cypher") {
			foundDeprecation = true
			break
		}
	}
	if !foundDeprecation {
		t.Errorf("expected deprecation warning naming the cypher alias; got warnings=%v", warns)
	}
}

// Idle baseline: `pinchql` only must NOT produce a deprecation
// warning. Pin the non-regression so the warning doesn't start
// firing on every modern call.
func TestQuery_PinchqlOnly_NoDeprecationWarning(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "qpqn"
	srv.sessionRoot = "/tmp/qpqn"
	store.UpsertProject(db.Project{ID: "qpqn", Path: "/tmp/qpqn", Name: "qpqn", IndexedAt: time.Now()})

	res, err := srv.handleQuery(context.Background(), makeReq(map[string]any{
		"pinchql": "MATCH (n:Function) RETURN n.name LIMIT 1",
	}))
	if err != nil {
		t.Fatalf("handleQuery: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	warns, _ := meta["warnings"].([]any)
	for _, w := range warns {
		if s, _ := w.(string); strings.Contains(s, "deprecated") {
			t.Errorf("pinchql-only call must not emit deprecation warning; got %q", s)
		}
	}
}
