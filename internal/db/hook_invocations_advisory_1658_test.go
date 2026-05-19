package db

import (
	"testing"
	"time"
)

// #1658 v0.86: redirect_advisory decisions (#1654 / #1656) must count
// in every hook-stats aggregate alongside legacy redirect rows. The
// pre-fix SQL hardcoded `decision='redirect'`, so once legacy rows
// aged out of the 7-day window the conversion-rate would go to 0
// even though the hook was firing on every Read.
//
// These tests pin the union across all four aggregates:
//   - HookConversionRate7d  (totals)
//   - HookCountsByTool7d    (per-tool)
//   - HookOverrideRate7d    (override percentage)
//   - ResolveHookInvocationsForSession (per-session resolution)
//
// Each test seeds a mix of legacy "redirect" + new "redirect_advisory"
// + "pass_through" rows and asserts that the union shows up.

func TestHookConversionRate7d_UnionsAdvisory_1658(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UnixNano()
	rows := []HookInvocation{
		// 2 legacy redirects, 3 advisory, 1 pass_through. Total "suggestions" = 5.
		{TS: now, SessionID: "s", ToolName: "Read", Decision: "redirect", SuggestedTool: "context"},
		{TS: now + 1, SessionID: "s", ToolName: "Read", Decision: "redirect", SuggestedTool: "context"},
		{TS: now + 2, SessionID: "s", ToolName: "Read", Decision: "redirect_advisory", SuggestedTool: "context"},
		{TS: now + 3, SessionID: "s", ToolName: "Read", Decision: "redirect_advisory", SuggestedTool: "context"},
		{TS: now + 4, SessionID: "s", ToolName: "Grep", Decision: "redirect_advisory", SuggestedTool: "search"},
		{TS: now + 5, SessionID: "s", ToolName: "Read", Decision: "pass_through"},
	}
	for i, r := range rows {
		if err := store.LogHookInvocation(r); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
	}
	_, redirects, _, err := store.HookConversionRate7d()
	if err != nil {
		t.Fatalf("conv rate: %v", err)
	}
	if redirects != 5 {
		t.Errorf("HookConversionRate7d redirects = %d, want 5 (2 legacy + 3 advisory)", redirects)
	}
}

func TestHookCountsByTool7d_UnionsAdvisory_1658(t *testing.T) {
	store := newTestStore(t)
	now := time.Now().UnixNano()
	rows := []HookInvocation{
		{TS: now, SessionID: "s", ToolName: "Read", Decision: "redirect", SuggestedTool: "context"},
		{TS: now + 1, SessionID: "s", ToolName: "Read", Decision: "redirect_advisory", SuggestedTool: "context"},
		{TS: now + 2, SessionID: "s", ToolName: "Read", Decision: "redirect_advisory", SuggestedTool: "context"},
		{TS: now + 3, SessionID: "s", ToolName: "Grep", Decision: "redirect_advisory", SuggestedTool: "search"},
		{TS: now + 4, SessionID: "s", ToolName: "Grep", Decision: "redirect", SuggestedTool: "search"},
	}
	for i, r := range rows {
		if err := store.LogHookInvocation(r); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
	}
	out, err := store.HookCountsByTool7d()
	if err != nil {
		t.Fatalf("by tool: %v", err)
	}
	if got := out["Read"]["redirects"]; got != 3 {
		t.Errorf("Read redirects = %d, want 3 (1 legacy + 2 advisory)", got)
	}
	if got := out["Grep"]["redirects"]; got != 2 {
		t.Errorf("Grep redirects = %d, want 2 (1 legacy + 1 advisory)", got)
	}
}

func TestHookOverrideRate7d_CountsAdvisory_1658(t *testing.T) {
	// HookOverrideRate7d filters by decision IN ('redirect','redirect_advisory').
	// An advisory row that the agent overrode (took_recommendation=0) must
	// count toward the override rate.
	store := newTestStore(t)
	now := time.Now().UnixNano()
	if err := store.LogHookInvocation(HookInvocation{
		TS: now, SessionID: "s", ToolName: "Read",
		Decision: "redirect_advisory", SuggestedTool: "context",
	}); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := store.LogHookInvocation(HookInvocation{
		TS: now + 1, SessionID: "s", ToolName: "Read",
		Decision: "redirect_advisory", SuggestedTool: "context",
	}); err != nil {
		t.Fatalf("log: %v", err)
	}
	// Resolve: only one matching tool call after the redirects. Both
	// redirects suggested context; the single context call resolves
	// each in the per-session loop. Per the existing #629 semantics,
	// the count is "redirects with took_recommendation set", not
	// strict 1:1 pairing.
	if _, err := store.ResolveHookInvocationsForSession("s", []HookSessionCall{
		{TS: now + 10, ToolName: "context"},
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Signature: (pct, overrides, resolved, err)
	_, _, resolved, err := store.HookOverrideRate7d()
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if resolved < 1 {
		t.Errorf("HookOverrideRate7d resolved = %d on advisory rows, want >= 1 (advisory must be counted)", resolved)
	}
}

func TestResolveHookInvocationsForSession_PicksUpAdvisory_1658(t *testing.T) {
	// The session resolver finds unresolved rows by decision IN
	// ('redirect','redirect_advisory'). Without the union, advisory
	// rows would never get took_recommendation set and override
	// metrics would silently zero out.
	store := newTestStore(t)
	now := time.Now().UnixNano()
	if err := store.LogHookInvocation(HookInvocation{
		TS: now, SessionID: "advisory_session", ToolName: "Read",
		Decision: "redirect_advisory", SuggestedTool: "context",
	}); err != nil {
		t.Fatalf("log: %v", err)
	}
	updated, err := store.ResolveHookInvocationsForSession("advisory_session", []HookSessionCall{
		{TS: now + 5, ToolName: "context"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if updated != 1 {
		t.Fatalf("ResolveHookInvocationsForSession updated = %d, want 1 (advisory row must be picked up)", updated)
	}
}
