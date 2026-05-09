package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBench dumps a synthetic baseline file into a tempdir for testing.
func writeBench(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

const fakeBaseline = `goos: linux
goarch: amd64
pkg: github.com/pincherMCP/pincher/internal/index
cpu: Intel(R) Xeon(R) CPU
BenchmarkA-4   100   1000 ns/op   200 B/op   10 allocs/op
BenchmarkB-4   200   2000 ns/op   400 B/op   20 allocs/op
PASS
`

// TestParseFile_StripsGomaxprocs proves the -N suffix matching is
// independent of the runner's GOMAXPROCS. A baseline captured on a 4-core
// runner must still align with an actual captured on an 8-core runner.
func TestParseFile_StripsGomaxprocs(t *testing.T) {
	dir := t.TempDir()
	p := writeBench(t, dir, "b.txt", fakeBaseline)

	got, err := parseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["BenchmarkA"]; !ok {
		t.Errorf("expected BenchmarkA (sans -N), got keys: %v", keys(got))
	}
	if _, ok := got["BenchmarkB"]; !ok {
		t.Errorf("expected BenchmarkB (sans -N), got keys: %v", keys(got))
	}
}

// TestPercentDelta covers the math gate for regression detection.
func TestPercentDelta(t *testing.T) {
	cases := []struct {
		baseline, actual, want float64
	}{
		{100, 120, 0.20},
		{100, 100, 0.0},
		{100, 80, -0.20},
		{0, 50, 0}, // div-by-zero guard
	}
	for _, c := range cases {
		got := percentDelta(c.baseline, c.actual)
		if got != c.want {
			t.Errorf("percentDelta(%v, %v) = %v, want %v",
				c.baseline, c.actual, got, c.want)
		}
	}
}

// TestRegression_NsOver20Percent — the negative-of-fix gate from the issue:
// "A change that adds 50ms to BenchmarkHandleSymbol MUST fail CI."
// Encoded here as: a 50% ns regression must produce a non-zero exit
// signal from the comparison logic. We test the math gate directly so
// CI behaviour is provable without invoking os.Exit.
func TestRegression_NsOver20Percent(t *testing.T) {
	got := percentDelta(1000, 1500) // +50%
	if got <= nsRegressionThreshold {
		t.Errorf("delta %v should exceed nsRegressionThreshold %v "+
			"— a 50%% regression must fail CI", got, nsRegressionThreshold)
	}
}

// TestRegression_AllocsOver30Percent — the alloc-count gate.
func TestRegression_AllocsOver30Percent(t *testing.T) {
	got := percentDelta(10, 14) // +40%
	if got <= allocsRegressionThreshold {
		t.Errorf("delta %v should exceed allocsRegressionThreshold %v "+
			"— a 40%% alloc regression must fail CI", got, allocsRegressionThreshold)
	}
}

// TestNoiseFloor_15PercentDoesNotFail — the suppression-direction gate.
// 15% wall-clock noise on a fast op MUST NOT trip the regression flag.
// This pins the noise-floor design choice from the issue: "We accept
// noise floor of ~20% on wall-clock to absorb runner variation."
func TestNoiseFloor_15PercentDoesNotFail(t *testing.T) {
	got := percentDelta(1000, 1150) // +15%
	if got > nsRegressionThreshold {
		t.Errorf("delta %v should NOT exceed nsRegressionThreshold %v "+
			"— 15%% must be within noise floor", got, nsRegressionThreshold)
	}
}

// TestCompare_NoRegression — happy path: identical sets produce zero
// regressions, zero missing entries, and a populated table.
func TestCompare_NoRegression(t *testing.T) {
	set := map[string]benchResult{
		"BenchmarkA": {NsPerOp: 1000, AllocsPerOp: 10},
		"BenchmarkB": {NsPerOp: 2000, AllocsPerOp: 20},
	}
	var buf bytes.Buffer
	regressions, missingActual, missingBaseline := compare(set, set, &buf)
	if regressions != 0 || missingActual != 0 || missingBaseline != 0 {
		t.Errorf("identical sets: regressions=%d, missingActual=%d, missingBaseline=%d, want all 0",
			regressions, missingActual, missingBaseline)
	}
	if !strings.Contains(buf.String(), "BenchmarkA") {
		t.Errorf("expected output to include BenchmarkA row, got:\n%s", buf.String())
	}
}

// TestCompare_NsRegressionFlagged — the wall-clock gate fires when ns/op
// jumps beyond the 20% threshold.
func TestCompare_NsRegressionFlagged(t *testing.T) {
	baseline := map[string]benchResult{
		"BenchmarkSlow": {NsPerOp: 1000, AllocsPerOp: 10},
	}
	actual := map[string]benchResult{
		"BenchmarkSlow": {NsPerOp: 1500, AllocsPerOp: 10}, // +50%
	}
	var buf bytes.Buffer
	regressions, _, _ := compare(baseline, actual, &buf)
	if regressions != 1 {
		t.Errorf("expected 1 ns regression, got %d", regressions)
	}
	if !strings.Contains(buf.String(), "[NS-REGRESSION]") {
		t.Errorf("expected NS-REGRESSION flag in output, got:\n%s", buf.String())
	}
}

// TestCompare_AllocsRegressionFlagged — the alloc-count gate fires
// independently of ns/op.
func TestCompare_AllocsRegressionFlagged(t *testing.T) {
	baseline := map[string]benchResult{
		"BenchmarkAlloc": {NsPerOp: 1000, AllocsPerOp: 10},
	}
	actual := map[string]benchResult{
		"BenchmarkAlloc": {NsPerOp: 1000, AllocsPerOp: 14}, // +40%
	}
	var buf bytes.Buffer
	regressions, _, _ := compare(baseline, actual, &buf)
	if regressions != 1 {
		t.Errorf("expected 1 allocs regression, got %d", regressions)
	}
	if !strings.Contains(buf.String(), "[ALLOCS-REGRESSION]") {
		t.Errorf("expected ALLOCS-REGRESSION flag in output, got:\n%s", buf.String())
	}
}

// TestCompare_MissingInActual — a benchmark that disappeared between
// baseline and actual must be surfaced.
func TestCompare_MissingInActual(t *testing.T) {
	baseline := map[string]benchResult{
		"BenchmarkGone": {NsPerOp: 1000, AllocsPerOp: 10},
	}
	actual := map[string]benchResult{}
	var buf bytes.Buffer
	_, missingActual, _ := compare(baseline, actual, &buf)
	if missingActual != 1 {
		t.Errorf("expected 1 missing-in-actual, got %d", missingActual)
	}
	if !strings.Contains(buf.String(), "MISSING IN ACTUAL") {
		t.Errorf("expected MISSING IN ACTUAL line, got:\n%s", buf.String())
	}
}

// TestCompare_NewWithoutBaseline — a benchmark in actual that has no
// baseline forces an explicit baseline-update step.
func TestCompare_NewWithoutBaseline(t *testing.T) {
	baseline := map[string]benchResult{}
	actual := map[string]benchResult{
		"BenchmarkNew": {NsPerOp: 1000, AllocsPerOp: 10},
	}
	var buf bytes.Buffer
	_, _, missingBaseline := compare(baseline, actual, &buf)
	if missingBaseline != 1 {
		t.Errorf("expected 1 new-without-baseline, got %d", missingBaseline)
	}
	if !strings.Contains(buf.String(), "NEW IN ACTUAL") {
		t.Errorf("expected NEW IN ACTUAL line, got:\n%s", buf.String())
	}
}

func keys(m map[string]benchResult) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
