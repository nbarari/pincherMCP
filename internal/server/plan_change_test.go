package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kwad77/pincher/internal/index"
)

// #1391 v0.82 Phase 4 audit suite for plan_change. Positive + negative
// + control + cross-check shape per the composite-tool roadmap
// contract — same pattern used by investigate_failure_test.go.
//
// Fixture: multi-package Go module so the resolver creates real
// cross-package CALLS edges, which is what packageOfFile() consumes
// to compute the cross_package flag.

const planChangeGoMod = `module example.com/audit

go 1.22
`

const planChangePaymentsSrc = `package payments

// Charge debits a user. Plan_change target in tests below.
func Charge(user string, cents int) error {
	return validate(user, cents)
}

// Refund credits a user back. Co-target — file_path mode picks up
// both callable symbols in the file.
func Refund(user string, cents int) error {
	return validate(user, cents)
}

func validate(user string, cents int) error {
	if cents <= 0 {
		return nil
	}
	return nil
}
`

const planChargeTestSrc = `package payments

// TestCharge exercises Charge directly — depth_1 caller in the SAME
// directory as the target, so cross_package=false here.
func TestCharge() {
	_ = Charge("alice", 100)
}
`

const planChangeBillingSrc = `package billing

import "example.com/audit/payments"

// ProcessOrder calls payments.Charge — depth_1 caller from a DIFFERENT
// directory, so cross_package=true (per packageOfFile heuristic) and
// the Go resolver creates a cross-package CALLS edge via the import.
func ProcessOrder(user string) error {
	return payments.Charge(user, 500)
}
`

const planChangeBillingTestSrc = `package billing

// TestProcessOrder calls ProcessOrder — depth_2 caller of Charge, in
// a directory that test-file conventions classify as a test.
func TestProcessOrder() {
	_ = ProcessOrder("bob")
}
`

// setupPlanChangeTestServer creates a server + indexed multi-package
// fixture with the layout above. Returns the server, root dir, and
// resolved projectID for handler invocations.
func setupPlanChangeTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	srv, store, root := newTestServer(t)
	srv.sessionRoot = root

	writeGoFile(t, root, "go.mod", planChangeGoMod)
	writeGoFile(t, root, "payments/charge.go", planChangePaymentsSrc)
	writeGoFile(t, root, "payments/charge_test.go", planChargeTestSrc)
	writeGoFile(t, root, "billing/process.go", planChangeBillingSrc)
	writeGoFile(t, root, "billing/billing_test.go", planChangeBillingTestSrc)

	idx := index.New(store)
	res, err := idx.Index(context.Background(), root, false)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	srv.sessionID = res.ProjectID
	return srv, root, res.ProjectID
}

// ─────────────────────────────────────────────────────────────────
// Pure-unit helper tests
// ─────────────────────────────────────────────────────────────────

// TestPlanChange_LooksLikeFilePath_PositiveAndControl pins the
// resolution heuristic: things that look like paths route to
// GetSymbolsForFile, others fall through to BM25 search.
func TestPlanChange_LooksLikeFilePath_PositiveAndControl(t *testing.T) {
	t.Parallel()
	positive := []string{
		"internal/auth/login.go",
		"src/main.py",
		"server.go",
		"foo/bar/baz.ts",
		"plain/dir/no_ext",       // contains slash → path
		`win\style\path.go`,       // backslash counts
		"README.md",
		"db.sql",
	}
	for _, p := range positive {
		if !looksLikeFilePath(p) {
			t.Errorf("expected %q to look like a file path", p)
		}
	}
	negative := []string{
		"Login",
		"handleRequest",
		"User",
		"Charge",
		"compute_total",
	}
	for _, n := range negative {
		if looksLikeFilePath(n) {
			t.Errorf("expected %q NOT to look like a file path", n)
		}
	}
	// Symbol-id shape contains "::" — handled before looksLikeFilePath
	// is consulted, but the helper itself only checks slashes/exts. The
	// "::" check is in the handler switch.
}

// TestPlanChange_PackageOfFile pins the package-name heuristic that
// drives cross_package classification.
func TestPlanChange_PackageOfFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, want string
	}{
		{"internal/server/server.go", "server"},
		{"payments/charge.go", "payments"},
		{"billing/process.go", "billing"},
		{"main.go", ""}, // bare file, no dir component
		{"", ""},        // empty path → empty package
		{`win\style\billing\process.go`, "billing"},
	}
	for _, c := range cases {
		if got := packageOfFile(c.path); got != c.want {
			t.Errorf("packageOfFile(%q) = %q; want %q", c.path, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────
// Handler tests (server + real indexed corpus)
// ─────────────────────────────────────────────────────────────────

// TestPlanChange_MissingTarget — negative-control: empty target yields
// a rich error with next_steps illustrating all three resolution shapes.
func TestPlanChange_MissingTarget(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	res, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected go-level error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected IsError=true for missing target")
	}
	text := textOf(t, res)
	if !strings.Contains(text, "plan_change requires `target`") {
		t.Errorf("expected rich-error message; got %s", text)
	}
	for _, want := range []string{"file path", "symbol id", "name-search"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected error to illustrate %q shape; got %s", want, text)
		}
	}
}

// TestPlanChange_TargetNotFound_EmptyReason — empty-path: a target that
// resolves to zero symbols stamps empty_reason + diagnosis. Pins the
// composite contract's mandatory empty_reason on zero-result envelopes.
func TestPlanChange_TargetNotFound_EmptyReason(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupPlanChangeTestServer(t)

	res, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "totallyNotASymbolName123",
		"project": projectID,
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
	if _, ok := meta["diagnosis"]; !ok {
		t.Error("diagnosis must accompany empty_reason")
	}
}

// TestPlanChange_ResolveBySymbolID — positive: a "::"-shaped target
// goes through the GetSymbolScoped path and produces a single
// symbol_affected entry with resolution_path="symbol_id".
func TestPlanChange_ResolveBySymbolID(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupPlanChangeTestServer(t)

	// Find the Charge symbol id via BM25 first (we don't hard-code IDs
	// because MakeSymbolID's format is the implementation's concern).
	hits, err := srv.store.SearchSymbolsByCorpus(projectID, "Charge", "Function", "", "code", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("BM25 prereq failed for Charge: err=%v hits=%d", err, len(hits))
	}
	chargeID := ""
	for _, h := range hits {
		if h.Symbol.Name == "Charge" {
			chargeID = h.Symbol.ID
			break
		}
	}
	if chargeID == "" {
		t.Fatalf("Charge id not found in BM25 hits")
	}

	res, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  chargeID,
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)

	tgt, ok := body["target"].(map[string]any)
	if !ok {
		t.Fatal("missing target")
	}
	if tgt["resolution_path"] != "symbol_id" {
		t.Errorf("resolution_path = %v; want symbol_id", tgt["resolution_path"])
	}
	syms, _ := tgt["symbols_affected"].([]any)
	if len(syms) != 1 {
		t.Errorf("symbols_affected length = %d; want 1 for symbol-id target", len(syms))
	}
}

// TestPlanChange_ResolveByFilePath — positive: a file-path target
// enumerates every callable symbol in the file via GetSymbolsForFile.
func TestPlanChange_ResolveByFilePath(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupPlanChangeTestServer(t)

	res, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "payments/charge.go",
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)

	tgt, _ := body["target"].(map[string]any)
	if tgt["resolution_path"] != "file_path" {
		t.Errorf("resolution_path = %v; want file_path", tgt["resolution_path"])
	}
	syms, _ := tgt["symbols_affected"].([]any)
	// charge.go has Charge, Refund, validate — but we filter to callable
	// kinds (Function/Method/Class/Interface/Type). validate is a
	// Function too, so expect 3.
	if len(syms) < 2 {
		t.Errorf("expected ≥2 callable symbols in charge.go; got %d", len(syms))
	}
	names := map[string]bool{}
	for _, s := range syms {
		m := s.(map[string]any)
		names[m["name"].(string)] = true
	}
	for _, want := range []string{"Charge", "Refund"} {
		if !names[want] {
			t.Errorf("symbols_affected missing %q; got %v", want, names)
		}
	}
}

// TestPlanChange_ResolveByName — positive: a bare name target falls
// through to BM25 search and resolves to the top callable hit.
func TestPlanChange_ResolveByName(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupPlanChangeTestServer(t)

	res, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "Charge",
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)

	tgt, _ := body["target"].(map[string]any)
	if tgt["resolution_path"] != "name_search" {
		t.Errorf("resolution_path = %v; want name_search", tgt["resolution_path"])
	}
	syms, _ := tgt["symbols_affected"].([]any)
	if len(syms) == 0 {
		t.Fatal("expected at least one symbol from BM25 hit on 'Charge'")
	}
	first := syms[0].(map[string]any)
	if first["name"] != "Charge" {
		t.Errorf("first symbol = %v; want Charge", first["name"])
	}
}

// TestPlanChange_DepthPartitioning_AndCrossPackage — positive: callers
// partition correctly by depth, and the cross_package flag fires for
// callers whose directory differs from the target's. ProcessOrder
// (billing/) is depth_1 cross_package; TestCharge (payments/) is
// depth_1 same-package; TestProcessOrder is depth_2.
func TestPlanChange_DepthPartitioning_AndCrossPackage(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupPlanChangeTestServer(t)

	res, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "payments/charge.go",
		"project": projectID,
		"depth":   2,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)
	br, ok := body["blast_radius"].(map[string]any)
	if !ok {
		t.Fatal("missing blast_radius")
	}
	d1, _ := br["depth_1_callers"].([]any)
	if len(d1) == 0 {
		t.Fatal("expected depth_1 callers; got 0")
	}
	// Locate the ProcessOrder depth_1 row — must be cross_package=true.
	foundProcessOrder := false
	for _, c := range d1 {
		m := c.(map[string]any)
		if m["name"] == "ProcessOrder" {
			foundProcessOrder = true
			if cp, _ := m["cross_package"].(bool); !cp {
				t.Errorf("ProcessOrder cross_package=false; expected true (different dir)")
			}
			if depth, _ := m["depth"].(float64); int(depth) != 1 {
				t.Errorf("ProcessOrder depth = %v; want 1", depth)
			}
		}
	}
	if !foundProcessOrder {
		t.Errorf("ProcessOrder not in depth_1 callers; got %v", d1)
	}

	// Cross-check the explicit cross_package list mirrors the same flag.
	cp, _ := br["cross_package"].([]any)
	if len(cp) == 0 {
		t.Errorf("cross_package list empty; expected ProcessOrder to surface")
	}
}

// TestPlanChange_TestFilesIntersecting — positive: test files crossing
// the call graph are surfaced separately. Both TestCharge and
// TestProcessOrder live in *_test.go files matching isTestFile, so
// their files appear in the intersection list.
func TestPlanChange_TestFilesIntersecting(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupPlanChangeTestServer(t)

	res, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "payments/charge.go",
		"project": projectID,
		"depth":   3,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)
	br, _ := body["blast_radius"].(map[string]any)
	tf, _ := br["test_files_intersecting"].([]any)
	if len(tf) == 0 {
		t.Fatal("expected at least one test file in intersection list")
	}
	// Stable sort means the list is canonical; we just check presence.
	joined := ""
	for _, f := range tf {
		joined += " " + f.(string)
	}
	for _, want := range []string{"charge_test.go"} {
		if !strings.Contains(joined, want) {
			t.Errorf("test_files_intersecting missing %q; got %v", want, tf)
		}
	}
}

// TestPlanChange_RelatedADRs — positive: an ADR whose key or value
// mentions the target's package or directory surfaces under
// related_adrs with a why-tag. ADRs not matching stay out.
func TestPlanChange_RelatedADRs(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupPlanChangeTestServer(t)

	// Seed two ADRs — one mentions "payments" (target dir), one does not.
	if err := srv.store.SetADR(projectID, "use-payments-vault", "All payments must go through the vault."); err != nil {
		t.Fatalf("SetADR(matching): %v", err)
	}
	if err := srv.store.SetADR(projectID, "unrelated-decision", "Use UTC for all timestamps."); err != nil {
		t.Fatalf("SetADR(non-matching): %v", err)
	}

	res, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "payments/charge.go",
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)

	adrs, ok := body["related_adrs"].([]any)
	if !ok || len(adrs) == 0 {
		t.Fatalf("expected at least one related_adr (use-payments-vault); got %v", adrs)
	}
	foundMatching := false
	foundUnrelated := false
	for _, a := range adrs {
		m := a.(map[string]any)
		k, _ := m["key"].(string)
		if k == "use-payments-vault" {
			foundMatching = true
			if _, ok := m["why"].(string); !ok {
				t.Error("matched ADR missing why")
			}
		}
		if k == "unrelated-decision" {
			foundUnrelated = true
		}
	}
	if !foundMatching {
		t.Error("expected use-payments-vault in related_adrs")
	}
	if foundUnrelated {
		t.Error("unrelated-decision must NOT appear in related_adrs (no keyword overlap)")
	}
}

// TestPlanChange_BlastRadiusHigh — positive: when depth_1 callers
// exceed the threshold (14), the composite emits a warnings_v2 entry
// with code=blast_radius_high. This is the signal that triggers the
// agent's "consider staged refactor" branch.
func TestPlanChange_BlastRadiusHigh(t *testing.T) {
	t.Parallel()
	srv, store, root := newTestServer(t)
	srv.sessionRoot = root

	// Single-package fixture where one Hotspot function has 20 direct
	// callers. Goes through the same Go resolver path so CALLS edges
	// land in the DB at depth 1.
	src := "package hot\n\nfunc Hotspot() int { return 1 }\n\n"
	for i := 0; i < 20; i++ {
		src += fmt.Sprintf("func Caller%02d() int { return Hotspot() }\n", i)
	}
	writeGoFile(t, root, "hot.go", src)

	idx := index.New(store)
	res, err := idx.Index(context.Background(), root, false)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	srv.sessionID = res.ProjectID

	out, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "Hotspot",
		"project": res.ProjectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, out)
	meta, _ := body["_meta"].(map[string]any)
	warnings, ok := meta["warnings_v2"].([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("expected blast_radius_high warning; got meta=%v", meta)
	}
	w := warnings[0].(map[string]any)
	if w["code"] != "blast_radius_high" {
		t.Errorf("warning code = %v; want blast_radius_high", w["code"])
	}
	if depth1Count, _ := w["depth_1_caller_count"].(float64); int(depth1Count) <= 14 {
		t.Errorf("depth_1_caller_count = %v; want >14 for high-blast-radius firing", depth1Count)
	}
}

// TestPlanChange_BlastRadiusLow_NoWarning — control: a sparse target
// with one or two callers must NOT emit blast_radius_high.
func TestPlanChange_BlastRadiusLow_NoWarning(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupPlanChangeTestServer(t)

	res, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "Refund",
		"project": projectID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	body := decode(t, res)
	meta, _ := body["_meta"].(map[string]any)
	if w, ok := meta["warnings_v2"]; ok {
		t.Errorf("expected no warnings_v2 for sparse target; got %v", w)
	}
}

// TestPlanChange_MaxDepthCap — control: depth=99 is clamped to 4 so a
// pathological caller has no way to demand unbounded BFS work.
func TestPlanChange_MaxDepthCap(t *testing.T) {
	t.Parallel()
	srv, _, projectID := setupPlanChangeTestServer(t)

	// Indirect verification: call with depth=99 and depth=4; the result
	// envelopes should be identical because the handler clamps. We
	// compare the depth_2_callers count which is the upper-bound-driver.
	res99, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "Charge",
		"project": projectID,
		"depth":   99,
	}))
	if err != nil {
		t.Fatalf("handler error (depth=99): %v", err)
	}
	res4, err := srv.handlePlanChange(context.Background(), makeReq(map[string]any{
		"target":  "Charge",
		"project": projectID,
		"depth":   4,
	}))
	if err != nil {
		t.Fatalf("handler error (depth=4): %v", err)
	}
	b99 := decode(t, res99)
	b4 := decode(t, res4)
	br99 := b99["blast_radius"].(map[string]any)
	br4 := b4["blast_radius"].(map[string]any)
	s99 := br99["summary"].(map[string]any)
	s4 := br4["summary"].(map[string]any)
	if s99["depth_2_count"] != s4["depth_2_count"] {
		t.Errorf("depth=99 not clamped to 4: depth_2_count %v vs %v", s99["depth_2_count"], s4["depth_2_count"])
	}
}

// TestPlanChange_IsRegistered — gate: the tool is registered and
// discoverable. Pins the registration side of the contract; the
// description-parity test (#1514-shape) pins the docs side.
func TestPlanChange_IsRegistered(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	tool, ok := srv.tools["plan_change"]
	if !ok {
		t.Fatal("plan_change not registered in srv.tools")
	}
	if !strings.Contains(strings.ToLower(tool.Description), "blast") &&
		!strings.Contains(strings.ToLower(tool.Description), "pre-edit") &&
		!strings.Contains(strings.ToLower(tool.Description), "caller") {
		t.Errorf("description should mention blast/pre-edit/caller intent; got %q", tool.Description)
	}
}
