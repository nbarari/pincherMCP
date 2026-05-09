package server

// Confidence-filtering and distribution helpers (#34 Phase 3).
//
// Wired into search/query/trace handlers. Defaults to no-filter (0.0 threshold)
// so existing callers see no behavior change. The histogram is included in
// every response's `_meta.confidence_distribution` so callers can sanity-check
// at-a-glance: "did the result set come back high-quality, or is it dominated
// by low-confidence YAML keys?"
//
// Buckets pin to fixed edges (0.0/0.5/0.7/0.9/1.0) so the histogram is
// directly comparable across calls — what falls into the 0.7-0.9 bin in one
// search result is the same kind of thing in another.

// confidenceBuckets defines the histogram edges. A confidence c falls in
// bucket i if confidenceBuckets[i] <= c < confidenceBuckets[i+1] (with the
// final bucket inclusive of 1.0). Values are deliberately aligned with the
// signal table: 0.5 is the lockfile-suppressed floor; 0.7 is the planned
// Phase 4 default min_confidence; 0.9 marks the "definitely real config or
// AST-extracted" ceiling.
var confidenceBuckets = []float64{0.0, 0.5, 0.7, 0.9, 1.0}

var confidenceBucketLabels = []string{
	"0.0-0.5",
	"0.5-0.7",
	"0.7-0.9",
	"0.9-1.0",
}

// confidenceDistribution returns a histogram of the input confidences keyed
// by bucket label. Empty input returns an empty (but non-nil) map so JSON
// callers see `{}` rather than `null`.
//
// Sum invariant: sum of all bucket counts == len(confidences). The
// `_meta.confidence_distribution MUST sum to result count` assertion from
// #34's negative-tests section pins this property.
func confidenceDistribution(confidences []float64) map[string]int {
	out := make(map[string]int, len(confidenceBucketLabels))
	for _, l := range confidenceBucketLabels {
		out[l] = 0
	}
	for _, c := range confidences {
		out[bucketLabel(c)]++
	}
	return out
}

// bucketLabel returns the histogram bin label that contains c. The final
// bucket (0.9-1.0) is closed on the right so c=1.0 lands in it rather than
// falling off the table.
func bucketLabel(c float64) string {
	for i := 0; i < len(confidenceBuckets)-1; i++ {
		lo := confidenceBuckets[i]
		hi := confidenceBuckets[i+1]
		// Last bucket is closed on the right.
		if i == len(confidenceBuckets)-2 {
			if c >= lo && c <= hi {
				return confidenceBucketLabels[i]
			}
		}
		if c >= lo && c < hi {
			return confidenceBucketLabels[i]
		}
	}
	// Out-of-range (shouldn't happen — confidence is clamped in Compose())
	// — bucket as 0.0-0.5 conservatively.
	return confidenceBucketLabels[0]
}
