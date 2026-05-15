package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// `changes scope="base:<nonexistent-branch>"` previously returned a
// bare errResult — agent saw "git diff failed: ..." with no
// remediation, no list of valid scopes, no signal that the typo'd
// branch was the issue. Now wrapped in errResultRich so the response
// carries the four supported scopes (unstaged/staged/all/base:<branch>)
// as next_steps.

func TestHandleChanges_BadBaseBranch_RichErrorWithScopes(t *testing.T) {
	t.Parallel()
	srv, store, root := newTestServer(t)
	srv.sessionRoot = root
	srv.sessionID = "pchrich"
	store.UpsertProject(db.Project{
		ID: "pchrich", Path: root, Name: "pchrich", IndexedAt: time.Now(),
	})
	writeGoFile(t, root, "main.go", "package main\nfunc main(){}\n")
	// Make the test temp dir a git repo so runGitDiff reaches the
	// base-branch-not-found path instead of "not a git repo".
	if _, err := runCmd(t, root, "git", "init", "-q", "-b", "master"); err != nil {
		t.Skip("git not available")
	}
	runCmd(t, root, "git", "config", "user.email", "t@e")
	runCmd(t, root, "git", "config", "user.name", "t")
	runCmd(t, root, "git", "add", ".")
	runCmd(t, root, "git", "commit", "-q", "-m", "init")

	res, err := srv.handleChanges(context.Background(), makeReq(map[string]any{
		"scope":   "base:totally-not-a-branch",
		"project": "pchrich",
	}))
	if err != nil {
		t.Fatalf("handleChanges: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError on missing base branch; got %s", textOf(t, res))
	}
	body := decode(t, res)
	errStr, _ := body["error"].(string)
	if !strings.Contains(errStr, "git diff failed") {
		t.Errorf("expected 'git diff failed' in error; got %q", errStr)
	}
	meta, _ := body["_meta"].(map[string]any)
	steps, _ := meta["next_steps"].([]any)
	if len(steps) < 4 {
		t.Fatalf("expected 4 scope examples in next_steps; got %d (%v)", len(steps), steps)
	}
	wantScopes := map[string]bool{
		`{"scope":"unstaged"}`:           false,
		`{"scope":"staged"}`:             false,
		`{"scope":"all"}`:                false,
		`{"scope":"base:master"}`:        false,
	}
	for _, s := range steps {
		step, _ := s.(map[string]any)
		args, _ := step["args"].(string)
		if _, want := wantScopes[args]; want {
			wantScopes[args] = true
		}
	}
	for arg, found := range wantScopes {
		if !found {
			t.Errorf("expected next_step with args=%s; got steps=%v", arg, steps)
		}
	}
}
