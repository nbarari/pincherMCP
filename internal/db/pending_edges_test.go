package db

import (
	"testing"
)

// #457: unit tests for the persisted-deferred-edges Store surface.
// Integration through Indexer.Index() is covered in internal/index;
// these tests exercise the Store methods directly so the db package's
// coverage reflects the new code.

func TestReplacePendingEdgesForFile_InsertThenReplace(t *testing.T) {
	s := newTestStore(t)
	pid := "proj1"
	_ = s.UpsertProject(testProject(pid))

	// First write — INSERT path.
	initial := []PendingEdge{
		{ProjectID: pid, FromFile: "caller.go", Kind: "CALLS", FromQN: "pkg.Bar", ToName: "Foo", Confidence: 0.7},
		{ProjectID: pid, FromFile: "caller.go", Kind: "CALLS", FromQN: "pkg.Bar", ToName: "Baz", Confidence: 0.7},
	}
	if err := s.ReplacePendingEdgesForFile(pid, "caller.go", initial); err != nil {
		t.Fatalf("first ReplacePendingEdgesForFile: %v", err)
	}

	got, err := s.LoadPendingEdges(pid, "CALLS")
	if err != nil {
		t.Fatalf("LoadPendingEdges: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("after first insert, len(rows) = %d, want 2", len(got))
	}

	// Re-write with a different candidate set — must DELETE old, INSERT new.
	replacement := []PendingEdge{
		{ProjectID: pid, FromFile: "caller.go", Kind: "CALLS", FromQN: "pkg.Bar", ToName: "Qux", Confidence: 0.7},
	}
	if err := s.ReplacePendingEdgesForFile(pid, "caller.go", replacement); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = s.LoadPendingEdges(pid, "CALLS")
	if len(got) != 1 {
		t.Fatalf("after replace, len(rows) = %d, want 1 (DELETE should have cleared the old two)", len(got))
	}
	if got[0].ToName != "Qux" {
		t.Errorf("after replace, ToName = %q, want Qux", got[0].ToName)
	}
}

func TestReplacePendingEdgesForFile_EmptyDeletesOnly(t *testing.T) {
	s := newTestStore(t)
	pid := "proj1"
	_ = s.UpsertProject(testProject(pid))
	_ = s.ReplacePendingEdgesForFile(pid, "caller.go", []PendingEdge{
		{ProjectID: pid, FromFile: "caller.go", Kind: "CALLS", FromQN: "pkg.Bar", ToName: "Foo", Confidence: 0.7},
	})

	// Replace with empty slice — should clear rows for caller.go.
	if err := s.ReplacePendingEdgesForFile(pid, "caller.go", nil); err != nil {
		t.Fatalf("replace empty: %v", err)
	}
	got, _ := s.LoadPendingEdges(pid, "CALLS")
	if len(got) != 0 {
		t.Errorf("after empty replace, len(rows) = %d, want 0", len(got))
	}
}

func TestReplacePendingEdgesForFile_DoesNotTouchOtherFiles(t *testing.T) {
	// A re-extracted file's DELETE must be scoped to that file —
	// other files' rows must persist (this is the entire point of
	// per-file scoping under #457).
	s := newTestStore(t)
	pid := "proj1"
	_ = s.UpsertProject(testProject(pid))
	_ = s.ReplacePendingEdgesForFile(pid, "caller.go", []PendingEdge{
		{ProjectID: pid, FromFile: "caller.go", Kind: "CALLS", FromQN: "pkg.Bar", ToName: "Foo", Confidence: 0.7},
	})
	_ = s.ReplacePendingEdgesForFile(pid, "other.go", []PendingEdge{
		{ProjectID: pid, FromFile: "other.go", Kind: "CALLS", FromQN: "pkg.Quux", ToName: "Foo", Confidence: 0.7},
	})

	// Now replace caller.go's set; other.go's row must survive.
	_ = s.ReplacePendingEdgesForFile(pid, "caller.go", []PendingEdge{
		{ProjectID: pid, FromFile: "caller.go", Kind: "CALLS", FromQN: "pkg.Bar", ToName: "NewTarget", Confidence: 0.7},
	})
	got, _ := s.LoadPendingEdges(pid, "CALLS")
	if len(got) != 2 {
		t.Fatalf("after caller.go replace, len(rows) = %d, want 2 (caller + other)", len(got))
	}
	sawOther := false
	for _, r := range got {
		if r.FromFile == "other.go" && r.FromQN == "pkg.Quux" {
			sawOther = true
		}
	}
	if !sawOther {
		t.Errorf("other.go's row was incorrectly deleted by caller.go replace; got=%v", got)
	}
}

func TestDeletePendingEdgesForFile(t *testing.T) {
	s := newTestStore(t)
	pid := "proj1"
	_ = s.UpsertProject(testProject(pid))
	_ = s.ReplacePendingEdgesForFile(pid, "caller.go", []PendingEdge{
		{ProjectID: pid, FromFile: "caller.go", Kind: "CALLS", FromQN: "pkg.Bar", ToName: "Foo", Confidence: 0.7},
		{ProjectID: pid, FromFile: "caller.go", Kind: "IMPORTS", FromQN: "pkg", ToName: "other", Confidence: 1.0},
	})

	if err := s.DeletePendingEdgesForFile(pid, "caller.go"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ := s.LoadPendingEdges(pid, "CALLS")
	if len(got) != 0 {
		t.Errorf("after Delete, CALLS rows = %d, want 0", len(got))
	}
	got, _ = s.LoadPendingEdges(pid, "IMPORTS")
	if len(got) != 0 {
		t.Errorf("after Delete, IMPORTS rows = %d, want 0", len(got))
	}
}

func TestLoadPendingEdges_FiltersByKind(t *testing.T) {
	s := newTestStore(t)
	pid := "proj1"
	_ = s.UpsertProject(testProject(pid))
	_ = s.ReplacePendingEdgesForFile(pid, "caller.go", []PendingEdge{
		{ProjectID: pid, FromFile: "caller.go", Kind: "CALLS", FromQN: "pkg.Bar", ToName: "Foo", Confidence: 0.7},
		{ProjectID: pid, FromFile: "caller.go", Kind: "READS", FromQN: "pkg.Bar", ToName: "Cache", Confidence: 0.5},
	})

	calls, _ := s.LoadPendingEdges(pid, "CALLS")
	if len(calls) != 1 || calls[0].Kind != "CALLS" {
		t.Errorf("LoadPendingEdges('CALLS') = %v, want one CALLS row", calls)
	}
	reads, _ := s.LoadPendingEdges(pid, "READS")
	if len(reads) != 1 || reads[0].Kind != "READS" {
		t.Errorf("LoadPendingEdges('READS') = %v, want one READS row", reads)
	}
}

func TestLoadPendingEdges_EmptyProject(t *testing.T) {
	s := newTestStore(t)
	got, err := s.LoadPendingEdges("never-existed", "CALLS")
	if err != nil {
		t.Fatalf("LoadPendingEdges: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty-project LoadPendingEdges returned %d rows, want 0", len(got))
	}
}

func TestPendingEdges_ProjectScopedLoad(t *testing.T) {
	// Two projects with the same file_path; LoadPendingEdges must
	// scope by project_id to avoid cross-project bleed.
	s := newTestStore(t)
	_ = s.UpsertProject(testProject("proj-a"))
	_ = s.UpsertProject(testProject("proj-b"))
	_ = s.ReplacePendingEdgesForFile("proj-a", "caller.go", []PendingEdge{
		{ProjectID: "proj-a", FromFile: "caller.go", Kind: "CALLS", FromQN: "a.Bar", ToName: "Foo", Confidence: 0.7},
	})
	_ = s.ReplacePendingEdgesForFile("proj-b", "caller.go", []PendingEdge{
		{ProjectID: "proj-b", FromFile: "caller.go", Kind: "CALLS", FromQN: "b.Bar", ToName: "Foo", Confidence: 0.7},
	})

	a, _ := s.LoadPendingEdges("proj-a", "CALLS")
	if len(a) != 1 || a[0].FromQN != "a.Bar" {
		t.Errorf("proj-a load = %v, want one a.Bar row", a)
	}
	b, _ := s.LoadPendingEdges("proj-b", "CALLS")
	if len(b) != 1 || b[0].FromQN != "b.Bar" {
		t.Errorf("proj-b load = %v, want one b.Bar row", b)
	}

	// Delete in proj-a must not affect proj-b.
	_ = s.DeletePendingEdgesForFile("proj-a", "caller.go")
	a, _ = s.LoadPendingEdges("proj-a", "CALLS")
	if len(a) != 0 {
		t.Errorf("proj-a after Delete = %v, want empty", a)
	}
	b, _ = s.LoadPendingEdges("proj-b", "CALLS")
	if len(b) != 1 {
		t.Errorf("proj-b after proj-a Delete = %v, want untouched (1 row)", b)
	}
}

func TestReplacePendingEdgesForFile_DuplicatesDeduped(t *testing.T) {
	// UNIQUE constraint should silently drop duplicates within a single
	// input batch — caller doesn't have to pre-dedup.
	s := newTestStore(t)
	pid := "proj1"
	_ = s.UpsertProject(testProject(pid))
	dup := PendingEdge{ProjectID: pid, FromFile: "caller.go", Kind: "CALLS", FromQN: "pkg.Bar", ToName: "Foo", Confidence: 0.7}
	if err := s.ReplacePendingEdgesForFile(pid, "caller.go", []PendingEdge{dup, dup, dup}); err != nil {
		t.Fatalf("Replace with duplicates: %v", err)
	}
	got, _ := s.LoadPendingEdges(pid, "CALLS")
	if len(got) != 1 {
		t.Errorf("after dup-insert, len(rows) = %d, want 1 (UNIQUE dedup)", len(got))
	}
}
