package server

import (
	"encoding/json"
	"sort"
	"sync"
	"sync/atomic"
)

// #1631 v0.85: in-conversation next_steps adherence telemetry.
//
// Pincher's value scales with calls × savings-per-call. The session-
// boundary hook (#627) measures whether an agent prompt redirected to
// a pincher call. Once the agent is IN a session, the dominant lever
// is whether it FOLLOWS pincher's `_meta.next_steps[]` recommendations
// or ignores them and falls back to Read/Grep. Pre-#1631 that signal
// was unmeasured.
//
// The tracker stashes each emitted (tool, canonical-args) pair per
// session in a small ring buffer. When the agent's NEXT query-shaped
// call matches a stashed pair, the match is credited and the entry is
// consumed. `emitted` and `followed` atomic counters let dashboards
// surface `adherence_pct = followed / emitted * 100` — the in-
// conversation outcome metric that pairs with the hook-boundary
// conversion rate.
//
// Scope and trade-offs:
//   - In-memory only. Adherence resets on server restart; persistent
//     surfacing across restarts is filed as a follow-up requiring a
//     schema migration on the `sessions` table.
//   - Per-session ring cap = 20. A multi-tool workflow can run several
//     intervening calls before acting on an earlier suggestion; the
//     cap is generous enough to absorb that without unbounded growth.
//   - Match-and-consume. Each stashed recommendation fires at most
//     once — prevents a single suggestion from credit-counting against
//     many subsequent calls.
//   - Args canonicalization. Pincher emits `next_steps[].args` as a
//     JSON string; the agent's actual call carries args as a map.
//     Both are normalized to JSON-with-sorted-keys so equivalent
//     inputs (different key order, whitespace) match.

// nextStepRecommendation is one stashed entry in the per-session ring.
type nextStepRecommendation struct {
	tool    string
	argsKey string // JSON-with-sorted-keys, computed once at record time
}

// nextStepsAdherenceTracker counts emitted and followed recommendations.
// Zero-value is usable thanks to the lazy-init in RecordEmitted, so
// Server can embed it without touching the constructor.
type nextStepsAdherenceTracker struct {
	mu            sync.Mutex
	perSession    map[string][]nextStepRecommendation
	statsEmitted  int64
	statsFollowed int64
}

// adherenceRingCap caps the per-session backlog of stashed
// recommendations. Tuned for a multi-tool workflow where several
// intervening calls can run before the agent acts on an earlier
// suggestion — generous, but bounded so an over-eager handler can't
// grow the map without bound.
const adherenceRingCap = 20

// RecordEmitted is called by jsonResultWithMeta after the verbose-prune
// (#622) has decided which next_steps actually ship to the agent. Each
// surviving entry's (tool, canonical-args) is pushed into the session
// ring; statsEmitted increments per recorded entry. Older entries are
// FIFO-evicted when the ring overflows.
func (n *nextStepsAdherenceTracker) RecordEmitted(sessionID string, steps []map[string]string) {
	if sessionID == "" || len(steps) == 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.perSession == nil {
		n.perSession = map[string][]nextStepRecommendation{}
	}
	bucket := n.perSession[sessionID]
	for _, st := range steps {
		tool := st["tool"]
		if tool == "" {
			continue
		}
		bucket = append(bucket, nextStepRecommendation{
			tool:    tool,
			argsKey: canonicalArgsJSON(st["args"]),
		})
		atomic.AddInt64(&n.statsEmitted, 1)
	}
	if len(bucket) > adherenceRingCap {
		bucket = bucket[len(bucket)-adherenceRingCap:]
	}
	n.perSession[sessionID] = bucket
}

// CheckAndConsume is called at the top of recordQueryMetrics — the
// post-handler hook every query-shaped tool runs through. If (tool,
// args) matches a stashed recommendation for this session, the entry
// is consumed (FIFO-removed) and statsFollowed increments. Returns
// true when a match was credited so callers can choose to log /
// surface the event downstream.
//
// The match-and-consume invariant prevents one suggestion from
// counting against multiple subsequent calls — if the agent calls
// `trace direction=outbound` twice after pincher suggested it once,
// only the first counts as adherence.
func (n *nextStepsAdherenceTracker) CheckAndConsume(sessionID, tool string, args map[string]any) bool {
	if sessionID == "" || tool == "" {
		return false
	}
	key := canonicalArgsMap(args)
	n.mu.Lock()
	defer n.mu.Unlock()
	bucket, ok := n.perSession[sessionID]
	if !ok {
		return false
	}
	for i, rec := range bucket {
		if rec.tool == tool && rec.argsKey == key {
			n.perSession[sessionID] = append(bucket[:i], bucket[i+1:]...)
			atomic.AddInt64(&n.statsFollowed, 1)
			return true
		}
	}
	return false
}

// Stats returns the running emitted + followed counters. Adherence
// percentage is computed by the caller (typically: followed / emitted
// * 100, with the zero-emitted case rendered as N/A).
func (n *nextStepsAdherenceTracker) Stats() (emitted, followed int64) {
	return atomic.LoadInt64(&n.statsEmitted), atomic.LoadInt64(&n.statsFollowed)
}

// canonicalArgsJSON parses a JSON args string and re-emits with sorted
// keys. Falls back to the raw string when the input doesn't parse as
// an object — preserves the recommendation's recordability rather than
// dropping it on a malformed args field.
func canonicalArgsJSON(s string) string {
	trimmed := s
	if trimmed == "" || trimmed == "{}" {
		return "{}"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return trimmed
	}
	return canonicalArgsMap(m)
}

// canonicalArgsMap serializes a map to JSON with keys sorted
// lexicographically — the canonical form for adherence matching.
func canonicalArgsMap(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf := make([]byte, 0, 64)
	buf = append(buf, '{')
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		kb, _ := json.Marshal(k)
		buf = append(buf, kb...)
		buf = append(buf, ':')
		vb, _ := json.Marshal(m[k])
		buf = append(buf, vb...)
	}
	buf = append(buf, '}')
	return string(buf)
}
