package db

import (
	"testing"
	"time"
)

// #1399. ListFilesWithHashesForProject batches the file/hash readout
// for `pincher verify`. The N-round-trip alternative (loop GetFileHash
// for each path returned by ListFilesForProject) would dominate latency
// on the warp_rc / Codex / sniffer scale projects (4k+ files).

func TestListFilesWithHashesForProject_RoundTrip(t *testing.T) {
	s := newTestStore(t)

	if err := s.UpsertProject(Project{ID: "p1", Path: "/p1", Name: "p1", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	seed := []FileHashEntry{
		{Path: "a.go", Hash: "aaaa"},
		{Path: "b/c.go", Hash: "bbbb"},
		{Path: "z.go", Hash: "zzzz"},
	}
	for _, e := range seed {
		if err := s.SetFileHash("p1", e.Path, e.Hash); err != nil {
			t.Fatalf("SetFileHash: %v", err)
		}
	}

	got, err := s.ListFilesWithHashesForProject("p1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(seed) {
		t.Fatalf("got %d entries; want %d", len(got), len(seed))
	}
	// SQL ORDER BY path → results sort lexicographically. Confirm.
	want := []string{"a.go", "b/c.go", "z.go"}
	for i, e := range got {
		if e.Path != want[i] {
			t.Errorf("entries[%d].Path = %q, want %q", i, e.Path, want[i])
		}
	}
	// Hash round-trip.
	hashByPath := map[string]string{}
	for _, e := range got {
		hashByPath[e.Path] = e.Hash
	}
	if hashByPath["a.go"] != "aaaa" {
		t.Errorf("a.go hash = %q, want aaaa", hashByPath["a.go"])
	}
}

func TestListFilesWithHashesForProject_EmptyProject(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(Project{ID: "empty", Path: "/e", Name: "e", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.ListFilesWithHashesForProject("empty")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries on empty project; want 0", len(got))
	}
	// JSON invariant — empty slice, not nil.
	if got == nil {
		t.Error("ListFilesWithHashesForProject returned nil slice; want empty []")
	}
}

// Cross-project scoping — files in project B don't leak into A's
// result, and vice versa.
func TestListFilesWithHashesForProject_ScopedByProjectID(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(Project{ID: "pa", Path: "/pa", Name: "pa", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if err := s.UpsertProject(Project{ID: "pb", Path: "/pb", Name: "pb", IndexedAt: time.Now()}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	_ = s.SetFileHash("pa", "alpha.go", "aaaa")
	_ = s.SetFileHash("pa", "beta.go", "bbbb")
	_ = s.SetFileHash("pb", "gamma.go", "gggg")

	aRows, _ := s.ListFilesWithHashesForProject("pa")
	bRows, _ := s.ListFilesWithHashesForProject("pb")
	if len(aRows) != 2 {
		t.Errorf("project A files = %d, want 2", len(aRows))
	}
	if len(bRows) != 1 {
		t.Errorf("project B files = %d, want 1", len(bRows))
	}
	for _, e := range bRows {
		if e.Path == "alpha.go" || e.Path == "beta.go" {
			t.Errorf("project A's file %q leaked into project B's result", e.Path)
		}
	}
}
