package server

import (
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// #1046: resolveProjectID's name-match fallback was case-sensitive.
// On case-insensitive filesystems (Windows NTFS, macOS APFS) the
// canonical-path fallback (#997) already accepts mixed-case PATHS,
// but the *name* fallback didn't — `Pincher-repo` passed by an
// agent failed to resolve against the stored `pincher-repo` name.
// Now resolves case-insensitively, with exact-case preferred when
// both an exact-case and a fold-only match exist (collisions on
// the casefold are rare but possible after a rename).

func TestResolveProjectID_NameCaseInsensitive(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	tmp := t.TempDir()
	store.UpsertProject(db.Project{
		ID: "p-lc", Path: tmp, Name: "pincher-repo", IndexedAt: time.Now(),
	})

	got, err := srv.resolveProjectID("Pincher-repo")
	if err != nil {
		t.Fatalf("resolveProjectID(Pincher-repo): %v", err)
	}
	if got != "p-lc" {
		t.Errorf("expected p-lc; got %q", got)
	}

	// Exact-case still works.
	got, err = srv.resolveProjectID("pincher-repo")
	if err != nil {
		t.Fatalf("resolveProjectID(pincher-repo): %v", err)
	}
	if got != "p-lc" {
		t.Errorf("expected p-lc; got %q", got)
	}

	// All-uppercase variant.
	got, err = srv.resolveProjectID("PINCHER-REPO")
	if err != nil {
		t.Fatalf("resolveProjectID(PINCHER-REPO): %v", err)
	}
	if got != "p-lc" {
		t.Errorf("expected p-lc; got %q", got)
	}
}

// When both an exact-case and a different-case project exist with
// matching casefold (rare but possible), prefer the exact-case one.
func TestResolveProjectID_ExactCasePreferredOverCasefold(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	tmpA := t.TempDir()
	tmpB := t.TempDir()
	store.UpsertProject(db.Project{
		ID: "p-mixed", Path: tmpA, Name: "MyProject", IndexedAt: time.Now(),
	})
	store.UpsertProject(db.Project{
		ID: "p-lower", Path: tmpB, Name: "myproject", IndexedAt: time.Now(),
	})

	got, err := srv.resolveProjectID("myproject")
	if err != nil {
		t.Fatalf("resolveProjectID(myproject): %v", err)
	}
	if got != "p-lower" {
		t.Errorf("expected exact-case p-lower; got %q", got)
	}

	got, err = srv.resolveProjectID("MyProject")
	if err != nil {
		t.Fatalf("resolveProjectID(MyProject): %v", err)
	}
	if got != "p-mixed" {
		t.Errorf("expected exact-case p-mixed; got %q", got)
	}
}

// Genuinely unknown project still returns the not-found error.
func TestResolveProjectID_UnknownNameStillFails(t *testing.T) {
	t.Parallel()
	srv, store, _ := newTestServer(t)
	store.UpsertProject(db.Project{
		ID: "p-known", Path: t.TempDir(), Name: "real-project", IndexedAt: time.Now(),
	})

	_, err := srv.resolveProjectID("totally-different-name")
	if err == nil {
		t.Fatal("expected not-found error for unknown name")
	}
}
