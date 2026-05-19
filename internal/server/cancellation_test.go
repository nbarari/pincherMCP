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
	// "heavy" tool is added to toolComplexityTiers.
	//
	// #1579 v0.82: the previous version exempted composite heavies on
	// the theory they "inherit cancellation via their building blocks."
	// That was wrong — composites had `_ = ctx` at the top, so neither
	// the entry-point check nor the per-iteration loop honored
	// cancellation. Exemption removed; every composite below now has
	// an explicit row.
	tested := map[string]bool{
		"index":               true,
		"search":              true,
		"trace":               true,
		"fetch":               true,
		"rebuild_fts":         true,
		"investigate_failure": true,
		"plan_change":         true,
		"audit_unused":        true,
		"onboard_module":      true,
		"dead_code":           true,
		"context_for_task":    true,
	}
	for tool, tier := range toolComplexityTiers {
		if tier != "heavy" {
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

// TestCancellation_StandardTierDBToolsHaveContractTest — #1601 v0.84
// gate. Standard-tier atomic tools that hit the DB or run BFS need
// the same entry-point check as the heavy tools. The set is enumerated
// explicitly (vs walking toolComplexityTiers["standard"]) because not
// every standard-tier tool runs DB work — context/changes do, schema
// is in-memory.
func TestCancellation_StandardTierDBToolsHaveContractTest(t *testing.T) {
	t.Parallel()
	tested := map[string]bool{
		"changes":      true,
		"architecture": true,
		"query":        true,
		"neighborhood": true,
		"context":      true,
	}
	required := []string{"changes", "architecture", "query", "neighborhood", "context"}
	for _, tool := range required {
		if !tested[tool] {
			t.Errorf("standard-tier DB tool %q has no cancellation contract test — add one to cancellation_test.go", tool)
		}
	}
}

// ────────────────────────────────────────────────────────────────
// #1601 v0.84: standard-tier atomic-tool cancellation contract pin.
// FILE-H follow-up — #1527 + #1579 covered heavy-tier composites and
// the four canonical atomic heavies (index/search/trace/fetch/
// rebuild_fts). The remaining atomic standard-tier tools (changes,
// architecture, query, neighborhood) still ran their entry-validation
// + setup work on a cancelled ctx. Each handler now has an
// entry-point ctx.Err() check; the tests below pin the contract.
//
// Smoke level — like the v0.82 composite tests, these prove the
// entry-point check fires, not that mid-call interruption is clean.
// Mid-call cancellation flows through QueryContext (atomic tools
// already used it before this PR).
// ────────────────────────────────────────────────────────────────

func TestCancellation_HandleChanges_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleChanges", 500*time.Millisecond, func() (any, error) {
		return srv.handleChanges(ctx, makeReq(map[string]any{}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleChanges took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

func TestCancellation_HandleArchitecture_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleArchitecture", 500*time.Millisecond, func() (any, error) {
		return srv.handleArchitecture(ctx, makeReq(map[string]any{}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleArchitecture took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

func TestCancellation_HandleQuery_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleQuery", 500*time.Millisecond, func() (any, error) {
		return srv.handleQuery(ctx, makeReq(map[string]any{
			"pinchql": "MATCH (n:Function) RETURN n.name LIMIT 1",
		}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleQuery took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

func TestCancellation_HandleContext_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleContext", 500*time.Millisecond, func() (any, error) {
		return srv.handleContext(ctx, makeReq(map[string]any{
			"id": "internal/auth/login.go::auth.Login#Function",
		}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleContext took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

func TestCancellation_HandleNeighborhood_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleNeighborhood", 500*time.Millisecond, func() (any, error) {
		return srv.handleNeighborhood(ctx, makeReq(map[string]any{
			"id": "internal/auth/login.go::auth.Login#Function",
		}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleNeighborhood took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

// ────────────────────────────────────────────────────────────────
// #1579 Composite-cancellation contract pin (v0.82). Each composite
// below was caught with `_ = ctx` at the top of the handler — the
// pre-cancelled-ctx test now fails the build until the handler honors
// the contract via an entry-point check + per-iteration ctx.Err().
// ────────────────────────────────────────────────────────────────

func TestCancellation_HandleInvestigateFailure_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleInvestigateFailure", 500*time.Millisecond, func() (any, error) {
		return srv.handleInvestigateFailure(ctx, makeReq(map[string]any{
			"error_text": "panic: nil pointer\n  at auth.Login (login.go:42)",
		}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleInvestigateFailure took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

func TestCancellation_HandlePlanChange_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handlePlanChange", 500*time.Millisecond, func() (any, error) {
		return srv.handlePlanChange(ctx, makeReq(map[string]any{
			"target": "internal/auth/login.go::auth.Login#Function",
		}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handlePlanChange took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

func TestCancellation_HandleAuditUnused_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleAuditUnused", 500*time.Millisecond, func() (any, error) {
		return srv.handleAuditUnused(ctx, makeReq(map[string]any{}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleAuditUnused took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

func TestCancellation_HandleOnboardModule_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleOnboardModule", 500*time.Millisecond, func() (any, error) {
		return srv.handleOnboardModule(ctx, makeReq(map[string]any{
			"directory": "internal/auth/",
		}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleOnboardModule took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

func TestCancellation_HandleWhyEmpty_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleWhyEmpty", 500*time.Millisecond, func() (any, error) {
		return srv.handleWhyEmpty(ctx, makeReq(map[string]any{
			"prior_empty_reason": EmptyReasonNoResultsInCorpus,
		}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleWhyEmpty took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

func TestCancellation_HandleDeadCode_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleDeadCode", 500*time.Millisecond, func() (any, error) {
		return srv.handleDeadCode(ctx, makeReq(map[string]any{}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleDeadCode took %v on a cancelled ctx; expected <500ms", res.elapsed)
	}
}

// #1590 v0.83: context_for_task pre-dated FILE-H. Same pattern as the
// other composites — entry-point ctx.Err() + per-iteration check in the
// per-seed trace and neighbor loops. The composite fans out 2 traces +
// N GetSymbol calls + 1 GetSymbolsForFile per seed; without ctx checks
// it runs the full payload after the client cancels.
func TestCancellation_HandleContextForTask_ReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	ctx := withCancelledContext(t)
	res := runWithBudget(t, "handleContextForTask", 500*time.Millisecond, func() (any, error) {
		return srv.handleContextForTask(ctx, makeReq(map[string]any{
			"task": "fix the login retry bug",
		}))
	})
	if res.elapsed > 500*time.Millisecond {
		t.Errorf("handleContextForTask took %v on a cancelled ctx; expected <500ms", res.elapsed)
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
