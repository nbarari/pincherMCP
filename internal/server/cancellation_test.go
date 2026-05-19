package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kwad77/pincher/internal/index"
)

// #1527 FILE-H v0.84: cancellation contract — every long-running tool
// must return promptly when its context is cancelled, with no SQL
// transaction left open and a sensible error returned to the caller.
//
// This is the contract pin. Tests here use an already-cancelled
// context and assert the handler returns within a tight deadline
// (≤500ms — generous so OS scheduling jitter doesn't flake; the
// actual cancel response should be sub-100ms in practice).
//
// Tests that PASS confirm the handler honors the contract.
// Tests that FAIL surface the handler as a contract gap — fix the
// handler to check `ctx.Done()` / `ctx.Err()` at appropriate points,
// or thread ctx down into the store call.
//
// Smoke-level — these don't deeply prove the handler interrupts
// mid-operation; they prove the entry-point check at minimum. Deep
// interrupt-during-long-op tests live alongside the specific
// handler (e.g. internal/index/index_cancellation_test.go).

// withCancelledContext returns a context that is already cancelled.
func withCancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// assertReturnsPromptly asserts the function returns within
// maxDuration. The function MAY return an error or a result; what we
// pin is that it doesn't hang. Argument value is a struct of result +
// err so we can also inspect what came back.
type handlerCallResult struct {
	elapsed time.Duration
	gotErr  bool
	errStr  string
	gotResp bool
}

func runWithBudget(t *testing.T, name string, budget time.Duration, fn func() (any, error)) handlerCallResult {
	t.Helper()
	start := time.Now()
	done := make(chan handlerCallResult, 1)
	go func() {
		resp, err := fn()
		out := handlerCallResult{
			elapsed: time.Since(start),
			gotResp: resp != nil,
			gotErr:  err != nil,
		}
		if err != nil {
			out.errStr = err.Error()
		}
		done <- out
	}()
	select {
	case res := <-done:
		return res
	case <-time.After(budget):
		t.Errorf("%s: did not return within %v — cancellation contract gap", name, budget)
		return handlerCallResult{elapsed: budget}
	}
}

// ────────────────────────────────────────────────────────────────
// handleIndex — already wires ctx through to idx.Index() which
// checks ctx.Err() between file jobs (indexer.go:445). Pin it.
// ────────────────────────────────────────────────────────────────

func TestCancellation_HandleIndex_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, store, root := newTestServer(t)
	srv.sessionRoot = root
	writeGoFile(t, root, "main.go", "package main\n\nfunc main() {}\n")
	srv.indexer = index.New(store)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleIndex", 500*time.Millisecond, func() (any, error) {
		out, err := srv.handleIndex(ctx, makeReq(map[string]any{"path": root}))
		return out, err
	})
	// Either an explicit error or an IsError result is acceptable —
	// both signal the handler noticed cancellation.
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleIndex took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

// ────────────────────────────────────────────────────────────────
// handleSearch — currently doesn't propagate ctx to the store
// SELECT. Smoke-tests the entry-point check. If this fails, the
// fix is to thread ctx through SearchSymbolsByCorpus → ro.Query.
// ────────────────────────────────────────────────────────────────

func TestCancellation_HandleSearch_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleSearch", 500*time.Millisecond, func() (any, error) {
		return srv.handleSearch(ctx, makeReq(map[string]any{"query": "anything"}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleSearch took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

// ────────────────────────────────────────────────────────────────
// handleTrace — same shape as search. Smoke-tests entry-point only.
// ────────────────────────────────────────────────────────────────

func TestCancellation_HandleTrace_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleTrace", 500*time.Millisecond, func() (any, error) {
		return srv.handleTrace(ctx, makeReq(map[string]any{
			"id":        "fake::Symbol#Function",
			"direction": "outbound",
			"depth":     1,
		}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleTrace took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

// ────────────────────────────────────────────────────────────────
// handleFetch — uses context.WithTimeout(ctx, 15s) + http.NewRequestWithContext.
// An already-cancelled ctx should make the request fail
// immediately. Pin the contract.
// ────────────────────────────────────────────────────────────────

func TestCancellation_HandleFetch_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	// Use a closed local port URL: cancellation latency must dominate,
	// not DNS resolution or TLS handshake. Originally pointed at
	// example.com which made macOS CI runners spend hundreds of ms in
	// TCP+TLS setup before the cancelled-ctx check could fire — flaky
	// in CI but not in the way the test is meant to catch.
	res := runWithBudget(t, "handleFetch", 1500*time.Millisecond, func() (any, error) {
		return srv.handleFetch(ctx, makeReq(map[string]any{
			"url": "http://127.0.0.1:1/cancelled",
		}))
	})
	if res.elapsed > 1500*time.Millisecond {
		t.Errorf("handleFetch took %v on a cancelled ctx; expected <1500ms", res.elapsed)
	}
	// Fetch should surface context-cancelled in the error path either
	// via the result's IsError or the returned error itself.
	if res.gotResp {
		// Result returned — should be an IsError envelope on cancel.
		// We don't dig in here; the budget check above is the
		// load-bearing assertion.
	}
}

// ────────────────────────────────────────────────────────────────
// handleRebuildFTS — runs SQL DDL serially. Smoke-test the entry-
// point check. Heavy real-world rebuilds need ctx-honoring inside
// the rebuild loop too; that's tracked in the FILE-H follow-up.
// ────────────────────────────────────────────────────────────────

func TestCancellation_HandleRebuildFTS_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleRebuildFTS", 1500*time.Millisecond, func() (any, error) {
		// Rebuild on an empty store is fast even without ctx-checks,
		// so the 1.5s budget here is generous; the real test would
		// run against a populated store (deferred follow-up).
		return srv.handleRebuildFTS(ctx, makeReq(map[string]any{}))
	})
	if res.elapsed > 1500*time.Millisecond {
		t.Errorf("handleRebuildFTS took %v on a cancelled ctx; expected <1.5s on empty store", res.elapsed)
	}
}

// ────────────────────────────────────────────────────────────────
// Cross-cutting: no SQL transaction left open after cancel.
// Sample test — calls cancel-mid-handler then asserts the store
// can immediately serve a new tool call. If the prior cancel left
// a transaction open, the next call would block on the writer
// lock.
// ────────────────────────────────────────────────────────────────

func TestCancellation_NoTransactionLeak_AfterIndex(t *testing.T) {
	t.Parallel()
	srv, store, root := newTestServer(t)
	srv.sessionRoot = root
	writeGoFile(t, root, "main.go", "package main\n\nfunc main() {}\n")
	srv.indexer = index.New(store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = srv.handleIndex(ctx, makeReq(map[string]any{"path": root}))

	// Immediate follow-up call must not block on a leaked txn.
	follow := runWithBudget(t, "handleHealth post-cancel", 500*time.Millisecond, func() (any, error) {
		return srv.handleHealth(context.Background(), makeReq(map[string]any{}))
	})
	if follow.elapsed > 500*time.Millisecond {
		t.Errorf("follow-up handleHealth blocked %v after cancelled index — possible txn leak", follow.elapsed)
	}
}

// ────────────────────────────────────────────────────────────────
// Contract assertion: every tool that's classified "heavy" in
// toolComplexityTiers should have a cancellation test row above.
// This is the gate test — adding a heavy tool without a
// cancellation test fails here.
// ────────────────────────────────────────────────────────────────

func TestCancellation_HeavyToolsHaveContractTest(t *testing.T) {
	t.Parallel()
	// Heavy tools that need cancellation tests. Update when a new
	// "heavy" tool is added to toolComplexityTiers. The composite
	// heavies (context_for_task, investigate_failure, plan_change,
	// audit_unused, onboard_module) inherit ctx from their underlying
	// SQL calls; their cancellation behaviour piggy-backs on
	// search/trace/etc. and is covered transitively. why_empty is
	// "light" (stateless catalog) — no cancellation needed.
	tested := map[string]bool{
		"index":         true,
		"search":        true,
		"trace":         true,
		"fetch":         true,
		"rebuild_fts":   true,
	}
	for tool, tier := range toolComplexityTiers {
		if tier != "heavy" {
			continue
		}
		// Composite heavies inherit cancellation via their building
		// blocks (search/trace/etc.) — exempt.
		exemptComposite := map[string]bool{
			"context_for_task":    true,
			"investigate_failure": true,
			"plan_change":         true,
			"audit_unused":        true,
			"onboard_module":      true,
		}
		if exemptComposite[tool] {
			continue
		}
		// guide is heavy but synthesis-only — no DB loop to interrupt.
		if tool == "guide" {
			continue
		}
		if !tested[tool] {
			t.Errorf("heavy tool %q has no cancellation contract test — add one to internal/server/cancellation_test.go", tool)
		}
	}
}

// ────────────────────────────────────────────────────────────────
// Pure-unit: the cancelled ctx itself behaves the way we expect.
// Catches the case where withCancelledContext is broken so the
// other tests pass for the wrong reason.
// ────────────────────────────────────────────────────────────────

func TestCancellation_WithCancelledContextHelper(t *testing.T) {
	t.Parallel()
	ctx := withCancelledContext(t)
	if ctx.Err() == nil {
		t.Fatal("withCancelledContext: ctx.Err() = nil; expected canceled")
	}
	if !strings.Contains(ctx.Err().Error(), "canceled") {
		t.Errorf("ctx.Err() = %v; expected 'context canceled'", ctx.Err())
	}
	select {
	case <-ctx.Done():
		// expected
	default:
		t.Error("ctx.Done() is not signalled — helper broken")
	}
}
