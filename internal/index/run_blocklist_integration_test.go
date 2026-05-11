package index

import (
	"context"
	"testing"

	"github.com/kwad77/pincher/internal/db"
)

// #567: pre-fix `cmd.Run()` on `*exec.Cmd` false-bound to any
// in-project Method named Run via the receiver-method fallback. The
// trace inbound on `(*Supervisor).Run` returned 6 callers, only 1 of
// which was real (`runSupervisedCLI` calling `sup.Run(ctx)`). The
// other 5 were `runGit` / `runGitDiff` etc. calling `cmd.Run()` /
// `verify.Run()` on `*exec.Cmd`.
//
// The fix adds Run to isPolymorphicInterfaceMethodName so the
// trailing-component fallback skips it. Same blocklist symmetric
// across the call-pass and the binding-pass.
const runFalseBindSrc = `package svc

import "os/exec"

type Worker struct{}

// (*Worker).Run is the in-project Method that pre-fix would
// incorrectly absorb every cmd.Run() call site.
func (w *Worker) Run() {
	w.helper()
}

func (w *Worker) helper() {}

// runGitCommand calls cmd.Run() on *exec.Cmd. Pre-fix this would
// false-bind to (*Worker).Run via the trailing-component fallback.
func runGitCommand(args []string) error {
	cmd := exec.Command("git", args...)
	return cmd.Run()
}

// runHTTPServer is the same shape with a different short name.
func runHTTPServer() error {
	cmd := exec.Command("server", "--addr", ":8080")
	return cmd.Run()
}
`

func TestResolveCalls_RunOnExecCmd_NotFalseBound(t *testing.T) {
	idx, store := newTestIndexer(t)
	dir := t.TempDir()
	writeFile(t, dir, "svc/svc.go", runFalseBindSrc)

	if _, err := idx.Index(context.Background(), dir, false); err != nil {
		t.Fatalf("Index: %v", err)
	}

	pid := db.ProjectIDFromPath(dir)

	// Find (*Worker).Run.
	syms, err := store.GetSymbolsByName(pid, "Run", 5)
	if err != nil {
		t.Fatalf("GetSymbolsByName: %v", err)
	}
	var workerRunID string
	for _, s := range syms {
		if s.Kind == "Method" && s.Parent == "svc.*Worker" {
			workerRunID = s.ID
			break
		}
	}
	if workerRunID == "" {
		t.Fatalf("(*Worker).Run not extracted; got %d Run symbols", len(syms))
	}

	results, err := store.TraceViaCTEScoped(pid, workerRunID, "inbound", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTEScoped: %v", err)
	}
	for _, r := range results {
		sym, _ := store.GetSymbol(r.SymbolID)
		if sym == nil {
			continue
		}
		// runGitCommand and runHTTPServer call cmd.Run() on *exec.Cmd
		// — they must NOT appear as inbound callers of (*Worker).Run.
		// The Run blocklist is what defangs the trailing-component
		// fallback; without it (#567 pre-fix) these would surface.
		if sym.Name == "runGitCommand" || sym.Name == "runHTTPServer" {
			t.Errorf("%s false-bound as caller of (*Worker).Run — Run polymorphic blocklist regression (#567)", sym.Name)
		}
	}
}
