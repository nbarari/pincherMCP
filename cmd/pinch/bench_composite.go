package main

// #1398 — pincher bench composite-score rubric.
//
// The composite-score collapses a multi-axis bench result (latency,
// tokens, round-trips, correctness) into one comparable number per
// scenario so reviewers can say "composite X beats atomic chain Y by
// N points" without eyeballing three columns at once. Designed for
// the Phase 4 composite-tool work (#1391) — every composite ships a
// bench scenario, every scenario emits both raw metrics AND the
// composite so weight-tuning is auditable.
//
// Pattern ported from kwad77/stoa's scripts/autoresearch-score.py:
// weighted-sum where some metrics are raw counts (more = better,
// e.g. correctness) and others are improvement-over-baseline gains
// (lower = better, capped at zero). The "gain" shape means a
// regression on any axis contributes nothing to the score (rather
// than pushing it negative) — keeps a single bad axis from
// arithmetic-cancelling a real win on the headline axis.
//
// Defaults reflect:
//   - 10× on accuracy   — correctness floor; a broken composite
//                         can't compensate via latency wins.
//   - 5×  on round_trips_saved — round-trip collapse IS the composite
//                                headline (5 atomic → 1 composite).
//   - 0.05 / 0.02 on latency p50 / p95 — real but secondary; tail
//                                         latency weighted less than
//                                         median.
//   - 0.001 on tokens_saved — large absolute numbers; per-token
//                              weight stays small to avoid dwarfing
//                              correctness/round-trip terms.

// CompositeWeights is the per-axis weighting used by ComposeScore.
// Defaults track DefaultCompositeWeights; callers may override per
// scenario where one axis matters more (e.g., interactive vs batch).
type CompositeWeights struct {
	Accuracy        float64 `json:"accuracy"`
	TokensSaved     float64 `json:"tokens_saved"`
	LatencyP50Saved float64 `json:"latency_p50_saved_ms"`
	LatencyP95Saved float64 `json:"latency_p95_saved_ms"`
	RoundTripsSaved float64 `json:"round_trips_saved"`
}

// DefaultCompositeWeights matches the rubric in #1398. Document
// changes to these constants in the same PR — bench baselines and
// composite scores both depend on them.
var DefaultCompositeWeights = CompositeWeights{
	Accuracy:        10.0,
	TokensSaved:     0.001,
	LatencyP50Saved: 0.05,
	LatencyP95Saved: 0.02,
	RoundTripsSaved: 5.0,
}

// CompositeInputs is the measured + baseline pair the composite
// scores. Baseline is the comparison shape (typically atomic-tool
// chain); measured is the candidate (typically composite). Accuracy
// is in [0, 1] from a per-scenario reference set — manually labeled
// because pincher cannot grade its own answers.
//
// All "saved" axes follow the (baseline - measured) gain pattern,
// capped at zero: a regression contributes nothing rather than
// pulling the composite negative. The accuracy axis is absolute
// (multiplied directly by its weight) because a correctness floor
// is a different shape from an improvement-over-baseline.
type CompositeInputs struct {
	Accuracy           float64 `json:"accuracy"`             // [0, 1]
	MeasuredTokens     int64   `json:"measured_tokens"`
	BaselineTokens     int64   `json:"baseline_tokens"`
	MeasuredP50Ms      float64 `json:"measured_p50_ms"`
	BaselineP50Ms      float64 `json:"baseline_p50_ms"`
	MeasuredP95Ms      float64 `json:"measured_p95_ms"`
	BaselineP95Ms      float64 `json:"baseline_p95_ms"`
	MeasuredRoundTrips int     `json:"measured_round_trips"`
	BaselineRoundTrips int     `json:"baseline_round_trips"`
}

// ComposeScore returns the weighted composite. Caller supplies the
// inputs + weights; both halves are explicit so the rubric is
// auditable from the call site without consulting package globals.
//
// Empty / zero baselines produce zero gain on that axis (not a
// negative number, not a panic). A bench run with no baseline is
// reported as raw measurements only — the composite degenerates to
// `Accuracy * w.Accuracy`.
func ComposeScore(in CompositeInputs, w CompositeWeights) float64 {
	gain := func(base, cur float64) float64 {
		if base <= 0 {
			return 0
		}
		if d := base - cur; d > 0 {
			return d
		}
		return 0
	}
	return in.Accuracy*w.Accuracy +
		gain(float64(in.BaselineTokens), float64(in.MeasuredTokens))*w.TokensSaved +
		gain(in.BaselineP50Ms, in.MeasuredP50Ms)*w.LatencyP50Saved +
		gain(in.BaselineP95Ms, in.MeasuredP95Ms)*w.LatencyP95Saved +
		gain(float64(in.BaselineRoundTrips), float64(in.MeasuredRoundTrips))*w.RoundTripsSaved
}

// CompositeBreakdown is the per-axis contribution to the final
// composite score. Returned alongside the score so reviewers can see
// which axis drove the number — if a composite "wins" entirely on
// round_trips_saved with zero gain elsewhere, that's a meaningfully
// different signal from a balanced win.
//
// Each field is the post-weight value: `RoundTripsSaved` is
// `gain(baseline, measured) * w.RoundTripsSaved`, not the raw gain.
// The sum across all fields equals the composite returned by
// ComposeScore for the same inputs.
type CompositeBreakdown struct {
	Total           float64 `json:"total"`
	Accuracy        float64 `json:"accuracy"`
	TokensSaved     float64 `json:"tokens_saved"`
	LatencyP50Saved float64 `json:"latency_p50_saved"`
	LatencyP95Saved float64 `json:"latency_p95_saved"`
	RoundTripsSaved float64 `json:"round_trips_saved"`
}

// ComposeBreakdown returns the same score as ComposeScore plus the
// per-axis contribution. Use this when emitting bench results so the
// rubric stays auditable per run, not just per release.
func ComposeBreakdown(in CompositeInputs, w CompositeWeights) CompositeBreakdown {
	gain := func(base, cur float64) float64 {
		if base <= 0 {
			return 0
		}
		if d := base - cur; d > 0 {
			return d
		}
		return 0
	}
	b := CompositeBreakdown{
		Accuracy:        in.Accuracy * w.Accuracy,
		TokensSaved:     gain(float64(in.BaselineTokens), float64(in.MeasuredTokens)) * w.TokensSaved,
		LatencyP50Saved: gain(in.BaselineP50Ms, in.MeasuredP50Ms) * w.LatencyP50Saved,
		LatencyP95Saved: gain(in.BaselineP95Ms, in.MeasuredP95Ms) * w.LatencyP95Saved,
		RoundTripsSaved: gain(float64(in.BaselineRoundTrips), float64(in.MeasuredRoundTrips)) * w.RoundTripsSaved,
	}
	b.Total = b.Accuracy + b.TokensSaved + b.LatencyP50Saved + b.LatencyP95Saved + b.RoundTripsSaved
	return b
}
