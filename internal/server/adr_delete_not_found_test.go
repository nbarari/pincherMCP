package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #1019: handleADR action=delete used to confidently return
// `deleted: true` regardless of whether any row matched. A typo'd
// key or wrong-project-scope call looked successful when in fact
// nothing was deleted. Same silent-confidently-wrong shape as the
// other failure-as-pedagogy gaps closed this cycle (#984/#987/#1008).
// Now: rich envelope with the list-keys recovery step on a no-op.

func TestHandleADR_DeleteNonexistentKey_ReturnsRichEnvelope(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "adr-p1"
	store.UpsertProject(db.Project{ID: "adr-p1", Path: "/tmp/adr-p1", Name: "adr-p1"})

	res, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "delete",
		"key":    "NEVER_EXISTED",
	}))
	if err != nil {
		t.Fatalf("handleADR: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on no-op delete; got %s", textOf(t, res))
	}

	body := decode(t, res)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "NEVER_EXISTED") || !strings.Contains(msg, "not found") {
		t.Errorf("expected message naming the missing key; got %q", msg)
	}

	meta, _ := body["_meta"].(map[string]any)
	steps, _ := meta["next_steps"].([]any)
	foundList := false
	for _, s := range steps {
		step, _ := s.(map[string]any)
		if tool, _ := step["tool"].(string); tool == "adr" {
			args, _ := step["args"].(string)
			if strings.Contains(args, `"action":"list"`) {
				foundList = true
				break
			}
		}
	}
	if !foundList {
		t.Errorf("expected adr action=list next_step to recover from typo'd key; got steps=%v", steps)
	}
}

// Regression guard: deleting a key that DOES exist still returns the
// success envelope (deleted=true) — the enforcement only changes the
// no-op path.
func TestHandleADR_DeleteExistingKey_StillSucceeds(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	srv.sessionID = "adr-p2"
	store.UpsertProject(db.Project{ID: "adr-p2", Path: "/tmp/adr-p2", Name: "adr-p2"})

	setRes, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "set",
		"key":    "REAL_KEY",
		"value":  "real value",
	}))
	if err != nil || setRes.IsError {
		t.Fatalf("set failed: %v / %s", err, textOf(t, setRes))
	}

	res, err := srv.handleADR(context.Background(), makeReq(map[string]any{
		"action": "delete",
		"key":    "REAL_KEY",
	}))
	if err != nil {
		t.Fatalf("handleADR delete: %v", err)
	}
	if res.IsError {
		t.Fatalf("delete of existing key must succeed; got %s", textOf(t, res))
	}
	body := decode(t, res)
	if del, _ := body["deleted"].(bool); !del {
		t.Errorf("deleted must be true for existing key; got %v", body["deleted"])
	}
}
