package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// Go `init` functions are invoked by the runtime at package
// initialization — they carry zero inbound CALLS edges by language
// design, never by an explicit call. dead_code (and audit_unused,
// which builds on the same SQL path) flagged every `func init()` as
// a high-confidence safe-to-delete candidate; acting on that breaks
// package initialization — pincher's own `ast/*.go` init()s register
// the language extractors, so "deleting" them silently disables
// Bash/HCL/HTML/Jinja/Markdown extraction. GetDeadCode now carves
// out Go `init`, mirroring the qualified_name_collision `init`
// carve-out (#1696, #1389 cross-language sweep).
//
// Found by dogfooding `audit_unused` against pincher-repo.

func TestDeadCode_GoInitExcluded(t *testing.T) {
	t.Parallel()
	src := `package boot

import "fmt"

var registered bool

func init() {
	registered = true
}

func init() {
	fmt.Println("second init also runs, by language design")
}

func use() { fmt.Println(registered) }
`
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/boot\n\ngo 1.25\n")
	writeFile(t, dir, "boot/boot.go", src)
	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}
	pid := db.ProjectIDFromPath(dir)

	dead, err := store.GetDeadCode(pid, []string{"Function"}, "Go", 1.0, 100)
	if err != nil {
		t.Fatalf("GetDeadCode: %v", err)
	}
	var sawInit, sawUse bool
	for _, s := range dead {
		if s.Name == "init" {
			sawInit = true
		}
		if s.Name == "use" {
			sawUse = true
		}
	}
	if sawInit {
		t.Error("Go `func init()` flagged as dead code — init is runtime-invoked at " +
			"package initialization, has zero callers by language design, and deleting " +
			"it breaks package init (pincher's own ast/*.go init()s register extractors)")
	}
	// Control: a genuinely uncalled non-init function in the same file
	// must still surface, so the carve-out hasn't blunted the signal.
	if !sawUse {
		t.Error("uncalled function `use` did not surface in dead_code — " +
			"the init carve-out is over-broad and suppressed real dead code")
	}
}
