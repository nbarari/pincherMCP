package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/index"
)

// #1391 Phase 4 audit suite for investigate_failure. Positive +
// negative + control + cross-check shape per the composite-tool
// roadmap contract.

const investigateFailureGoSrc = `package authz

// Login authenticates a user. Test fixture for investigate_failure
// composite — its name + qualified-name must match the synthetic
// stack-frame token passed in the test below.
func Login(user string) error {
	return validateCreds(user)
}

func validateCreds(user string) error {
	return nil
}

// Caller exercises Login so we can verify caller fan-in scoring.
func Caller() error {
	return Login("alice")
}
`

func setupInvestigateTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	srv, store, root := newTestServer(t)
	srv.sessionRoot = root
	writeGoFile(t, root, "authz.go", investigateFailureGoSrc)
	idx := index.New(store)
	res, err := idx.Index(context.Background(), root, false)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	srv.sessionID = res.ProjectID
	return srv, root, res.ProjectID
}

// ─────────────────────────────────────────────────────────────────
// Pure-unit parser tests (no server / no DB)
// ─────────────────────────────────────────────────────────────────

// TestInvestigateFailure_ParseFrames_GoPanic — positive: a Go panic
// trace yields the expected frame names + file paths. Pins the
// parser's behaviour against the canonical Go panic shape.
func TestInvestigateFailure_ParseFrames_GoPanic(t *testing.T) {
	t.Parallel()
	trace := `panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a8123]

goroutine 1 [running]:
main.processOrder(0x0)
	/home/dev/app/order.go:42 +0x55
main.handleRequest(0xc000010240, 0xc000010300)
	/home/dev/app/server.go:117 +0xa2
`
	names, files := parseStackFrames(trace)

	wantNames := map[string]bool{"processOrder": false, "handleRequest": false}
	for _, n := range names {
		if _, ok := wantNames[n]; ok {
			wantNames[n] = true
		}
	}
	for n, found := range wantNames {
		if !found {
			t.Errorf("expected name %q in parsed frames; got %v", n, names)
		}
	}
	for _, n := range names {
		if stopwordFrames[n] {
			t.Errorf("stopword %q leaked into parsed names", n)
		}
	}

	wantFiles := []string{"/home/dev/app/order.go", "/home/dev/app/server.go"}
	for _, want := range wantFiles {
		found := false
		for _, f := range files {
			if f == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected file %q in parsed paths; got %v", want, files)
		}
	}
}

// TestInvestigateFailure_ParseFrames_PythonTraceback — positive:
// Python's `File "..."` shape parses too. Composite is language-agnostic.
func TestInvestigateFailure_ParseFrames_PythonTraceback(t *testing.T) {
	t.Parallel()
	trace := `Traceback (most recent call last):
  File "/app/handler.py", line 42, in process_order
    result = compute_total(order)
  File "/app/billing.py", line 15, in compute_total
    return sum(item.price for item in order)
TypeError: 'NoneType' object is not iterable
`
	names, files := parseStackFrames(trace)

	wantNames := map[string]bool{"process_order": false, "compute_total": false}
	for _, n := range names {
		if _, ok := wantNames[n]; ok {
			wantNames[n] = true
		}
	}
	for n, found := range wantNames {
		if !found {
			t.Errorf("expected name %q in parsed frames; got %v", n, names)
		}
	}

	wantFiles := []string{"/app/handler.py", "/app/billing.py"}
	for _, want := range wantFiles {
		found := false
		for _, f := range files {
			if f == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected file %q; got %v", want, files)
		}
	}
}

// TestInvestigateFailure_ParseFrames_DottedNames — positive: dotted
// names contribute to BOTH the dotted form AND the short form so BM25
// can find the symbol whether it was extracted under its qualified or
// short name.
func TestInvestigateFailure_ParseFrames_DottedNames(t *testing.T) {
	t.Parallel()
	trace := `panic at auth.Login (login.go:42)
called from server.HandleRequest (server.go:88)
`
	names, _ := parseStackFrames(trace)

	want := []string{"auth.Login", "Login", "server.HandleRequest", "HandleRequest"}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected name %q in parsed frames; got %v", w, names)
		}
	}
}

// TestInvestigateFailure_StopwordsFiltered — control: stopword-only
// trace parses to zero names. Pins the stopword set.
func TestInvestigateFailure_StopwordsFiltered(t *testing.T) {
	t.Parallel()
	trace := "panic: runtime Error in goroutine main\nTraceback at line File"
	names, _ := parseStackFrames(trace)
	for _, n := range names {
		if stopwordFrames[n] {
			t.Errorf("stopword %q survived filter", n)
		}
	}
}

// ─────────────────────────────────────────────────────────────────
// Handler tests (server + real indexed corpus)
// ─────────────────────────────────────────────────────────────────

// TestInvestigateFailure_MissingErrorText — negative-control: zero
// arguments returns a rich error.
func TestInvestigateFailure_MissingErrorText(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handleInvestigateFailure(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected go-level error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected IsError=true for missing error_text")
	}
	text := textOf(t, res)
	if !strings.Contains(text, "investigate_failure requires `error_text`") {
		t.Errorf("expected rich-error message; got %s", text)
	}
}

// TestInvestigateFailure_NoFramesParse — empty path: input with no
// identifier-shaped tokens hits the parser's empty branch and stamps
// empty_reason + diagnosis.
func TestInvestigateFailure_NoFramesParse(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupInvestigateTestServer(t)

	res, err := srv.handleInvestigateFailure(context.Background(), makeReq(map[string]any{
		"error_text": "&& || !! ?? **",
		"project":    projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)

	meta, ok := body["_meta"].(map[string]any)
	if !ok {
		t.Fatal("missing _meta")
	}
	if meta["empty_reason"] != EmptyReasonNoResultsInCorpus {
		t.Errorf("empty_reason = %v; want %s", meta["empty_reason"], EmptyReasonNoResultsInCorpus)
	}
	fp, ok := body["frames_parsed"].(map[string]any)
	if !ok {
		t.Fatal("missing frames_parsed")
	}
	if names, _ := fp["names"].([]any); len(names) != 0 {
		t.Errorf("expected zero parsed names; got %v", names)
	}
}

// TestInvestigateFailure_NoMatchingSymbols — positive empty-path:
// frames parse but no symbol resolves. Distinguishable diagnosis
// (mentions "parsed N frame name(s)").
func TestInvestigateFailure_NoMatchingSymbols(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupInvestigateTestServer(t)

	res, err := srv.handleInvestigateFailure(context.Background(), makeReq(map[string]any{
		"error_text": "panic at totallyUnindexedFunctionName (nowhere.go:1)",
		"project":    projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)

	meta, ok := body["_meta"].(map[string]any)
	if !ok {
		t.Fatal("missing _meta")
	}
	diag, _ := meta["diagnosis"].(string)
	if !strings.Contains(diag, "parsed") || !strings.Contains(diag, "frame name") {
		t.Errorf("expected diagnosis to mention parsed frames; got %q", diag)
	}
	suspects, _ := body["implicated_symbols"].([]any)
	if len(suspects) != 0 {
		t.Errorf("expected zero suspects; got %d", len(suspects))
	}
}

// TestInvestigateFailure_RankByFrameMatch — positive happy path:
// trace mentioning an indexed Function name yields a ranked suspect
// with the stack_frame_match evidence flag.
func TestInvestigateFailure_RankByFrameMatch(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupInvestigateTestServer(t)

	res, err := srv.handleInvestigateFailure(context.Background(), makeReq(map[string]any{
		"error_text": "panic at authz.Login (authz.go:6)\n  called from main.processRequest (main.go:42)",
		"project":    projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)

	suspects, ok := body["implicated_symbols"].([]any)
	if !ok || len(suspects) == 0 {
		t.Fatalf("expected at least one suspect; got %v", suspects)
	}
	// At least one suspect should be Login.
	foundLogin := false
	for _, s := range suspects {
		m := s.(map[string]any)
		if name, _ := m["name"].(string); name == "Login" {
			foundLogin = true
			evidence, _ := m["evidence"].([]any)
			hasFrameMatch := false
			for _, e := range evidence {
				if es, _ := e.(string); es == "stack_frame_match" {
					hasFrameMatch = true
				}
			}
			if !hasFrameMatch {
				t.Errorf("Login suspect missing stack_frame_match evidence; got %v", evidence)
			}
		}
	}
	if !foundLogin {
		t.Errorf("expected Login among suspects; got %v", suspects)
	}
}

// TestInvestigateFailure_RankingIsDeterministic — cross-check: same
// input produces the same ranking across two invocations. Determinism
// is load-bearing for tests + reproducible debugging.
func TestInvestigateFailure_RankingIsDeterministic(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupInvestigateTestServer(t)

	args := map[string]any{
		"error_text": "panic at Login (authz.go:6)\n  called from validateCreds (authz.go:11)",
		"project":    projectID,
	}

	res1, _ := srv.handleInvestigateFailure(context.Background(), makeReq(args))
	res2, _ := srv.handleInvestigateFailure(context.Background(), makeReq(args))
	b1 := decode(t, res1)
	b2 := decode(t, res2)

	s1, _ := b1["implicated_symbols"].([]any)
	s2, _ := b2["implicated_symbols"].([]any)
	if len(s1) != len(s2) {
		t.Fatalf("non-deterministic suspect count: %d vs %d", len(s1), len(s2))
	}
	for i := range s1 {
		m1 := s1[i].(map[string]any)
		m2 := s2[i].(map[string]any)
		if m1["symbol_id"] != m2["symbol_id"] {
			t.Errorf("rank[%d] non-deterministic: %v vs %v", i, m1["symbol_id"], m2["symbol_id"])
		}
	}
}

// TestInvestigateFailure_MaxSuspectsCap — control: max_suspects=2
// returns ≤ 2 ranked suspects.
func TestInvestigateFailure_MaxSuspectsCap(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupInvestigateTestServer(t)

	res, err := srv.handleInvestigateFailure(context.Background(), makeReq(map[string]any{
		"error_text":   "panic at Login validateCreds Caller (authz.go:6)",
		"project":      projectID,
		"max_suspects": 2,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)
	suspects, _ := body["implicated_symbols"].([]any)
	if len(suspects) > 2 {
		t.Errorf("max_suspects=2 not honoured; got %d suspects", len(suspects))
	}
}

// TestInvestigateFailure_IsRegistered — gate: the tool is registered
// and discoverable. Pins runtime-registration side of the contract;
// the doc-listing parity test (#1514) pins the docs side.
func TestInvestigateFailure_IsRegistered(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tool, ok := srv.tools["investigate_failure"]
	if !ok {
		t.Fatal("investigate_failure not registered in srv.tools")
	}
	if !strings.Contains(tool.Description, "stack trace") {
		t.Errorf("description should mention stack trace; got %q", tool.Description)
	}
}
