package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// #790: the catastrophic-target guard (filesystem root, $HOME) lived
// only in the `pincher index` CLI path — the MCP `index` tool handler
// ran no guard at all, so an `index` call pointed at / or $HOME would
// happily walk it and bloat the shared DB with cache "symbols". The
// handler now calls index.IsBloatTrap before walking.
func TestHandleIndex_RefusesFilesystemRoot(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	root := "/"
	if runtime.GOOS == "windows" {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		root = filepath.VolumeName(cwd) + `\`
	}

	r, err := srv.handleIndex(context.Background(), makeReq(map[string]any{"path": root}))
	if err != nil {
		t.Fatalf("handleIndex: %v", err)
	}
	if !r.IsError {
		t.Fatalf("indexing the filesystem root must be refused; got success")
	}
	if body := errorBody(r); !strings.Contains(body, "refusing to index") {
		t.Errorf("error should explain the refusal; got %q", body)
	}
}

func TestHandleIndex_RefusesUserHome(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no HOME available")
	}
	srv, _, _ := newTestServer(t)

	r, err := srv.handleIndex(context.Background(), makeReq(map[string]any{"path": home}))
	if err != nil {
		t.Fatalf("handleIndex: %v", err)
	}
	if !r.IsError {
		t.Fatalf("indexing $HOME must be refused; got success")
	}
	if body := errorBody(r); !strings.Contains(body, "refusing to index") {
		t.Errorf("error should explain the refusal; got %q", body)
	}
}

// A normal project directory (no project marker required — MCP index is
// a deliberate action, hookMode=false) must still index fine.
func TestHandleIndex_AllowsOrdinaryDir(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	repoDir := t.TempDir()
	writeGoFile(t, repoDir, "pkg/service.go", simpleGoSrc)

	r, err := srv.handleIndex(context.Background(), makeReq(map[string]any{"path": repoDir}))
	if err != nil {
		t.Fatalf("handleIndex: %v", err)
	}
	if r.IsError {
		t.Fatalf("an ordinary temp dir must index without tripping the bloat trap: %v", decode(t, r))
	}
}
