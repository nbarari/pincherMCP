package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kwad77/pincher/internal/db"
)

// runBenchCLI implements `pincher bench` — the user-facing falsifiable
// savings measurement (#1263 §1).
//
// Runs a representative suite (search / context / trace) against the
// user's own indexed corpus, computes the equivalent "raw Read/Grep"
// baseline cost in tokens, and reports the actual savings ratio per
// tool. Distinct from `make bench` / `make corpus-bench` (CI-only
// perf gates) — this is the artifact a user can run on THEIR project
// to answer "is pincher actually saving me tokens vs not using it?"
//
// Baseline model (intentionally simple in v0.68):
//   - search: total file bytes of every result file (what `grep -l` +
//     N×`cat` would cost an agent)
//   - context: full file bytes of the symbol's file (what `cat` would
//     cost an agent that needed to read the surrounding function)
//   - trace depth=2: sum of unique file bytes across every touched
//     symbol (what an agent N×`Read` would cost while walking callers)
//
// Actual cost = bytes returned in the JSON-serialized response (the
// same shape the MCP framework would send back). Token estimate uses
// the bytes/4 heuristic — consistent with how pincher's own session
// stats compute tokens_used / tokens_saved on every call.
//
// Acceptance: §2 (cross-tool eval harness, comparator implementations,
// canonical workflow corpus) rolls forward to v0.69+. This first cut
// is the runs-on-your-project measurement.
func runBenchCLI(args []string) {
	log.SetOutput(io.Discard)

	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	projectID := fs.String("project", "", "Project ID to benchmark (default: largest by symbol count)")
	n := fs.Int("n", 20, "Number of sample symbols to time per tool (default 20)")
	depth := fs.Int("depth", 2, "trace depth (default 2)")
	asJSON := fs.Bool("json", false, "Emit structured JSON instead of human-readable text")
	seed := fs.Int64("seed", 0, "Random seed for sampling (default: nondeterministic). Set for reproducible benchmark runs.")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher bench [--project ID] [--n N] [--depth D] [--json] [--data-dir DIR] [--seed S]")
		fmt.Fprintln(os.Stderr, "  Runs search / context / trace against the user's indexed corpus and")
		fmt.Fprintln(os.Stderr, "  reports per-tool latency + token-savings vs a full-file Read/Grep baseline.")
		fmt.Fprintln(os.Stderr, "  Use --json for CI pipelines or `pincher bench --project ... | jq ...`.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	dir := *dataDir
	if dir == "" {
		var err error
		dir, err = db.DataDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pincher: failed to determine data directory: %v\n", err)
			os.Exit(1)
		}
	}

	store, err := db.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher: failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	pid := *projectID
	if pid == "" {
		largest, err := benchLargestProjectID(store)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pincher bench: pick largest project: %v\n", err)
			os.Exit(1)
		}
		pid = largest
	}

	rng := rand.New(rand.NewPCG(uint64(*seed), uint64(*seed)+1))
	if *seed == 0 {
		// Nondeterministic by default — every run sees a fresh random
		// sample so a `pincher bench` smoke-loop in CI surfaces variance,
		// not a single sticky symbol's perf profile. Pass --seed for
		// reproducibility (e.g. acceptance comments, regression triage).
		rng = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()/2)))
	}

	sample, err := benchSampleSymbolsWithEdges(store, pid, *n, rng)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher bench: sample symbols: %v\n", err)
		os.Exit(1)
	}
	if len(sample) == 0 {
		fmt.Fprintf(os.Stderr, "pincher bench: no symbols with edges in project %q (was the project indexed?)\n", pid)
		os.Exit(1)
	}

	report := runBenchSuite(store, pid, sample, *depth)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}
	fmt.Print(formatBenchText(report))
}

// BenchReport is the structured form `pincher bench --json` emits.
type BenchReport struct {
	ProjectID string      `json:"project_id"`
	Samples   int         `json:"samples"`
	TraceDepth int        `json:"trace_depth"`
	StartedAt time.Time   `json:"started_at"`
	Tools     []ToolBench `json:"tools"`
}

// ToolBench is the per-tool aggregate of a bench run.
type ToolBench struct {
	Name              string  `json:"name"`
	Calls             int     `json:"calls"`
	P50LatencyMs      float64 `json:"p50_latency_ms"`
	P95LatencyMs      float64 `json:"p95_latency_ms"`
	MeanLatencyMs     float64 `json:"mean_latency_ms"`
	MeanTokensActual  int64   `json:"mean_tokens_actual"`
	MeanTokensBaseline int64  `json:"mean_tokens_baseline"`
	SavingsPct        float64 `json:"savings_pct"`
}

// runBenchSuite runs each tool shape over the sample symbol set and
// returns the aggregate report. Errors per call are tolerated — they
// contribute zero tokens (which is the correct accounting: a failed
// call returns nothing, so it "saved" nothing). The aggregate still
// surfaces a sane latency curve.
func runBenchSuite(store *db.Store, pid string, sample []*db.Symbol, depth int) *BenchReport {
	report := &BenchReport{
		ProjectID:  pid,
		Samples:    len(sample),
		TraceDepth: depth,
		StartedAt:  time.Now(),
	}

	var searchM, contextM, traceM []measurement

	for _, sym := range sample {
		// ───────── search: query by symbol name, score top 20 matches.
		t0 := time.Now()
		results, _ := store.SearchSymbols(pid, sym.Name, "", "", 20)
		lat := float64(time.Since(t0)) / float64(time.Millisecond)
		actual := jsonTokenCount(results)
		baseline := uniqueFileBaselineFromSearch(results)
		searchM = append(searchM, measurement{lat, actual, baseline})

		// ───────── context: GetSymbol + simulated symbol-source read.
		t0 = time.Now()
		got, _ := store.GetSymbolScoped(pid, sym.ID)
		lat = float64(time.Since(t0)) / float64(time.Millisecond)
		var ctxActual int64
		if got != nil {
			ctxActual = jsonTokenCount(got) + int64((got.EndByte-got.StartByte)/4)
		}
		ctxBaseline := fileBytesAsTokens(sym.FilePath)
		contextM = append(contextM, measurement{lat, ctxActual, ctxBaseline})

		// ───────── trace: depth=N outbound BFS.
		t0 = time.Now()
		trace, _ := store.TraceViaCTEScoped(pid, sym.ID, "outbound", nil, depth)
		lat = float64(time.Since(t0)) / float64(time.Millisecond)
		traceActual := jsonTokenCount(trace)
		traceBaseline := uniqueFileBaselineFromTrace(store, pid, trace)
		traceM = append(traceM, measurement{lat, traceActual, traceBaseline})
	}

	report.Tools = []ToolBench{
		aggregate("search", searchM),
		aggregate("context", contextM),
		aggregate("trace", traceM),
	}
	return report
}

// aggregate folds N per-call measurements into a single ToolBench row.
func aggregate(name string, ms []measurement) ToolBench {
	if len(ms) == 0 {
		return ToolBench{Name: name}
	}
	lats := make([]float64, len(ms))
	var sumLat float64
	var sumActual, sumBaseline int64
	for i, m := range ms {
		lats[i] = m.latencyMs
		sumLat += m.latencyMs
		sumActual += m.tokensActual
		sumBaseline += m.tokensBaseline
	}
	sort.Float64s(lats)
	p50 := lats[len(lats)/2]
	p95 := lats[(len(lats)*95)/100]
	meanActual := sumActual / int64(len(ms))
	meanBaseline := sumBaseline / int64(len(ms))
	savings := 0.0
	if meanBaseline > 0 {
		savings = (1.0 - float64(meanActual)/float64(meanBaseline)) * 100.0
	}
	return ToolBench{
		Name:               name,
		Calls:              len(ms),
		P50LatencyMs:       p50,
		P95LatencyMs:       p95,
		MeanLatencyMs:      sumLat / float64(len(ms)),
		MeanTokensActual:   meanActual,
		MeanTokensBaseline: meanBaseline,
		SavingsPct:         savings,
	}
}

// measurement is the per-call tuple aggregate folds into a ToolBench.
// Kept package-private so the public BenchReport shape stays the only
// JSON-exposed surface.
type measurement struct {
	latencyMs      float64
	tokensActual   int64
	tokensBaseline int64
}

// jsonTokenCount serializes v to JSON and returns the byte/4 token
// approximation. Mirrors how pincher's session stats compute
// tokens_used on every MCP response (#tokens-used-envelope capability).
func jsonTokenCount(v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(b) / 4)
}

// fileBytesAsTokens reads file size from disk (no contents fetch) and
// returns bytes/4. Used by the baseline calculation — what reading
// the whole file would cost an agent. Returns 0 on stat failure
// (file moved / deleted / unreadable) — the baseline conservatively
// undercounts in that case rather than crashing the bench.
func fileBytesAsTokens(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size() / 4
}

// uniqueFileBaselineFromSearch dedupes result files and sums their
// on-disk sizes. Matches the "Grep gives me K matches, I Read each
// matching file once" baseline an agent without pincher would pay.
func uniqueFileBaselineFromSearch(results []db.SearchResult) int64 {
	seen := map[string]bool{}
	var total int64
	for _, r := range results {
		if seen[r.Symbol.FilePath] {
			continue
		}
		seen[r.Symbol.FilePath] = true
		total += fileBytesAsTokens(r.Symbol.FilePath)
	}
	return total
}

// uniqueFileBaselineFromTrace dedupes file paths across every touched
// symbol in a trace and sums on-disk sizes. Matches the "N×Read each
// file the trace touches" baseline. Symbol IDs encode file path so we
// don't need a per-row GetSymbol — we extract the file segment from
// the ID directly (saves N round-trips on a depth-2 trace that fans
// to dozens of nodes).
func uniqueFileBaselineFromTrace(store *db.Store, pid string, trace []db.TraceResult) int64 {
	seen := map[string]bool{}
	var total int64
	for _, t := range trace {
		// Symbol ID format: "{file_path}::{qualified_name}#{kind}".
		// Splitting on "::" gives file path as the first segment.
		fp := extractFilePathFromSymbolID(t.SymbolID)
		if fp == "" || seen[fp] {
			continue
		}
		seen[fp] = true
		// Symbol IDs use repo-relative paths; canonicalize to absolute
		// via the project path for the on-disk stat. Falls back to the
		// raw ID segment if the project lookup fails (best-effort).
		abs := filepath.Join(pidPath(store, pid), fp)
		total += fileBytesAsTokens(abs)
	}
	return total
}

// extractFilePathFromSymbolID splits an ID like "internal/db/db.go::db.Open#Function"
// and returns the file path segment. Returns "" if the ID doesn't
// match the expected shape (defensive — older schemas may use
// different formats and we want to skip rather than crash).
func extractFilePathFromSymbolID(id string) string {
	for i := 0; i+1 < len(id); i++ {
		if id[i] == ':' && id[i+1] == ':' {
			return id[:i]
		}
	}
	return ""
}

// pidPath looks up the project's filesystem path. Cached at the
// run-level by returning the same Project.Path for every call within
// a bench run — projects are stable mid-bench.
var pidPathCache = map[string]string{}

func pidPath(store *db.Store, pid string) string {
	if p, ok := pidPathCache[pid]; ok {
		return p
	}
	projects, err := store.ListProjects()
	if err != nil {
		return ""
	}
	for _, p := range projects {
		if p.ID == pid {
			pidPathCache[pid] = p.Path
			return p.Path
		}
	}
	return ""
}

// benchLargestProjectID picks the project with the most symbols.
// Same heuristic as tracelatencybench — the largest project surfaces
// the most realistic latency curve.
func benchLargestProjectID(store *db.Store) (string, error) {
	projects, err := store.ListProjects()
	if err != nil {
		return "", err
	}
	if len(projects) == 0 {
		return "", fmt.Errorf("no indexed projects (run `pincher index <path>` first)")
	}
	best := projects[0]
	for _, p := range projects[1:] {
		if p.SymCount > best.SymCount {
			best = p
		}
	}
	return best.ID, nil
}

// benchSampleSymbolsWithEdges returns up to n random Function/Method
// symbols with at least one edge. Edge-bearing symbols are required
// because the trace measurement is meaningless on orphans (depth=2
// from an orphan returns 0 rows in O(1us), flooring the curve).
func benchSampleSymbolsWithEdges(store *db.Store, projectID string, n int, rng *rand.Rand) ([]*db.Symbol, error) {
	rows, err := store.RO().Query(`
		SELECT s.id
		  FROM symbols s
		 WHERE s.project_id = ?
		   AND s.kind IN ('Function', 'Method')
		   AND EXISTS (
			   SELECT 1 FROM edges e
				WHERE e.project_id = s.project_id
				  AND (e.from_id = s.id OR e.to_id = s.id)
				LIMIT 1
		   )
		 LIMIT 5000`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var allIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		allIDs = append(allIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Fisher-Yates shuffle truncated to n.
	for i := len(allIDs) - 1; i > 0; i-- {
		j := rng.IntN(i + 1)
		allIDs[i], allIDs[j] = allIDs[j], allIDs[i]
	}
	if len(allIDs) > n {
		allIDs = allIDs[:n]
	}
	out := make([]*db.Symbol, 0, len(allIDs))
	for _, id := range allIDs {
		sym, err := store.GetSymbolScoped(projectID, id)
		if err != nil || sym == nil {
			continue
		}
		out = append(out, sym)
	}
	return out, nil
}

// formatBenchText renders the BenchReport as a human-readable table.
// Mirrors `pincher stats` text aesthetic (boxed output) so the two
// commands feel like siblings rather than diverged dialects.
func formatBenchText(r *BenchReport) string {
	var b []byte
	b = append(b, fmt.Sprintf("pincher bench — project=%q  samples=%d  trace_depth=%d\n", r.ProjectID, r.Samples, r.TraceDepth)...)
	b = append(b, fmt.Sprintf("  %-9s  %-10s  %-10s  %-12s  %-14s  %-14s  %-10s\n",
		"tool", "calls", "p50_ms", "p95_ms", "actual_tokens", "baseline_tokens", "savings")...)
	b = append(b, "  ─────────  ──────────  ──────────  ────────────  ──────────────  ──────────────  ──────────\n"...)
	for _, t := range r.Tools {
		b = append(b, fmt.Sprintf("  %-9s  %-10d  %-10.2f  %-12.2f  %-14d  %-14d  %-9.1f%%\n",
			t.Name, t.Calls, t.P50LatencyMs, t.P95LatencyMs, t.MeanTokensActual, t.MeanTokensBaseline, t.SavingsPct)...)
	}
	b = append(b, "\n  Baseline: full-file Read for each touched file. Actual: JSON-serialized response bytes/4.\n"...)
	b = append(b, "  Run `pincher bench --json` for CI-friendly structured output.\n"...)
	return string(b)
}
