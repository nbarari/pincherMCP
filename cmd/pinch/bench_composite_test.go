package main

import (
	"math"
	"testing"
)

// #1398 — composite-score rubric tests.
//
// The audit shape is positive / negative / control / cross-check
// (matches the existing test convention noted in CLAUDE.md). For a
// pure-math function: positive = improvement scores positive,
// negative = regression contributes zero (not negative), control =
// no baseline degenerates safely, cross-check = breakdown sum equals
// ComposeScore total.

func TestComposeScore_BalancedImprovement(t *testing.T) {
	// Positive shape: composite wins on every axis. All weighted
	// gains contribute; total is the sum.
	in := CompositeInputs{
		Accuracy:           1.0,
		MeasuredTokens:     1000,
		BaselineTokens:     5000,
		MeasuredP50Ms:      100,
		BaselineP50Ms:      500,
		MeasuredP95Ms:      200,
		BaselineP95Ms:      1000,
		MeasuredRoundTrips: 1,
		BaselineRoundTrips: 6,
	}
	got := ComposeScore(in, DefaultCompositeWeights)
	// Hand-computed:
	//   accuracy:   1.0 * 10        = 10.0
	//   tokens:    (5000-1000)*0.001 =  4.0
	//   p50:       (500-100)*0.05    = 20.0
	//   p95:       (1000-200)*0.02   = 16.0
	//   trips:     (6-1)*5.0         = 25.0
	//   total                        = 75.0
	want := 75.0
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("ComposeScore: got %v, want %v", got, want)
	}
}

func TestComposeScore_RegressionContributesZero(t *testing.T) {
	// Negative shape: measured is WORSE than baseline on tokens,
	// latency, and round_trips. The gain() helper caps each at zero.
	// Composite degenerates to the accuracy term only.
	in := CompositeInputs{
		Accuracy:           1.0,
		MeasuredTokens:     5000, // regression
		BaselineTokens:     1000,
		MeasuredP50Ms:      500, // regression
		BaselineP50Ms:      100,
		MeasuredP95Ms:      1000, // regression
		BaselineP95Ms:      200,
		MeasuredRoundTrips: 6, // regression
		BaselineRoundTrips: 1,
	}
	got := ComposeScore(in, DefaultCompositeWeights)
	want := 10.0 // accuracy only
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("regression should cap gains at 0; got %v want %v", got, want)
	}
}

func TestComposeScore_NoBaselineDegenerates(t *testing.T) {
	// Control shape: no baseline (zero on every axis) means no gain
	// computable. Score is the accuracy term in isolation.
	in := CompositeInputs{
		Accuracy:       0.5,
		MeasuredTokens: 2000,
		MeasuredP50Ms:  150,
		MeasuredP95Ms:  300,
	}
	got := ComposeScore(in, DefaultCompositeWeights)
	want := 5.0 // 0.5 * 10
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("no-baseline composite: got %v want %v", got, want)
	}
}

func TestComposeBreakdown_SumEqualsTotal(t *testing.T) {
	// Cross-check shape: breakdown must be auditable — the per-axis
	// contributions must sum exactly to the total ComposeScore
	// returns. Tests both the integer-baseline and float-baseline
	// halves of the formula in one pass.
	in := CompositeInputs{
		Accuracy:           0.8,
		MeasuredTokens:     2500,
		BaselineTokens:     7500,
		MeasuredP50Ms:      75,
		BaselineP50Ms:      400,
		MeasuredP95Ms:      150,
		BaselineP95Ms:      900,
		MeasuredRoundTrips: 2,
		BaselineRoundTrips: 7,
	}
	score := ComposeScore(in, DefaultCompositeWeights)
	b := ComposeBreakdown(in, DefaultCompositeWeights)

	sum := b.Accuracy + b.TokensSaved + b.LatencyP50Saved + b.LatencyP95Saved + b.RoundTripsSaved
	if math.Abs(b.Total-sum) > 1e-9 {
		t.Errorf("breakdown.Total (%v) != sum-of-fields (%v)", b.Total, sum)
	}
	if math.Abs(b.Total-score) > 1e-9 {
		t.Errorf("breakdown.Total (%v) != ComposeScore (%v) for same inputs", b.Total, score)
	}
}

func TestComposeScore_CustomWeights(t *testing.T) {
	// Positive shape (control variant): same inputs as Balanced
	// above, but weights are zeroed except for round_trips. Verifies
	// the rubric is genuinely caller-tunable, not silently using
	// package defaults.
	in := CompositeInputs{
		Accuracy:           1.0,
		MeasuredTokens:     1000,
		BaselineTokens:     5000,
		MeasuredP50Ms:      100,
		BaselineP50Ms:      500,
		MeasuredP95Ms:      200,
		BaselineP95Ms:      1000,
		MeasuredRoundTrips: 1,
		BaselineRoundTrips: 6,
	}
	w := CompositeWeights{RoundTripsSaved: 5.0} // every other weight zeroed
	got := ComposeScore(in, w)
	want := 25.0 // (6-1) * 5.0 only
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("custom-weights composite: got %v want %v", got, want)
	}
}

func TestComposeScore_DefaultWeightsRanking(t *testing.T) {
	// Cross-check shape: the default-weights ordering is itself a
	// design claim. Accuracy gets the largest single-axis weight
	// (10), round_trips next (5), then latency (0.05 / 0.02), then
	// per-token (0.001). Pin the ordering so weight tuning is
	// visible in PRs.
	w := DefaultCompositeWeights
	if w.Accuracy <= w.RoundTripsSaved {
		t.Errorf("accuracy weight (%v) must exceed round_trips (%v) — correctness is the largest single axis",
			w.Accuracy, w.RoundTripsSaved)
	}
	if w.RoundTripsSaved <= w.LatencyP50Saved {
		t.Errorf("round_trips weight (%v) must exceed latency-p50 (%v) — composite-collapse is the headline",
			w.RoundTripsSaved, w.LatencyP50Saved)
	}
	if w.LatencyP50Saved <= w.LatencyP95Saved {
		t.Errorf("p50 weight (%v) must exceed p95 (%v) — median latency dominates tail in interactive UIs",
			w.LatencyP50Saved, w.LatencyP95Saved)
	}
	if w.LatencyP95Saved <= w.TokensSaved {
		t.Errorf("p95 weight (%v) must exceed per-token (%v) — token-counts are large absolute numbers, weight stays small",
			w.LatencyP95Saved, w.TokensSaved)
	}
}
