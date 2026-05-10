// Package server implements the pincherMCP MCP server with all 16 tools.
//
// Every tool response includes a "_meta" envelope:
//
//	{
//	  "result": { ... },
//	  "_meta": {
//	    "tokens_used":  450,
//	    "tokens_saved": 12300,
//	    "latency_ms":   3,
//	    "cost_avoided": "$0.0012"
//	  }
//	}
//
// This lets agents track context consumption and remaining budget.
package server

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zeebo/xxh3"
	"github.com/kwad77/pincher/internal/ast"
	"github.com/kwad77/pincher/internal/cypher"
	"github.com/kwad77/pincher/internal/db"
	"github.com/kwad77/pincher/internal/index"
)

// sessionFlushInterval controls how often in-memory session stats are
// persisted to SQLite when this process has no HTTP-dashboard peer.
// 10s keeps the steady-state write rate low.
const sessionFlushInterval = 10 * time.Second

// sessionFlushFast is the accelerated flush cadence used when this
// process detects an HTTP-dashboard peer (another pincher process
// advertising an http_url row in the sessions table). Dropping to 1s
// closes the two-process staleness window the dashboard would otherwise
// surface (#204). The fast cadence only kicks in when calls > 0, so
// idle stdio processes don't churn writes.
const sessionFlushFast = 1 * time.Second

// httpPeerStaleAfter is the max age of an HTTP peer row before we
// stop trusting it as evidence of a live dashboard. 30s is generously
// above the slow flush cadence so the peer signal won't oscillate.
const httpPeerStaleAfter = 30 * time.Second

// Server is the pincherMCP MCP server.
type Server struct {
	mcp      *mcp.Server
	store    *db.Store
	indexer  *index.Indexer
	handlers map[string]mcp.ToolHandler
	// tools holds the same set as handlers but keyed for inspection. Used
	// by the tool-contract golden-file test (#127) so any rename / removal
	// of a tool surfaces as a deliberate, reviewable diff.
	tools   map[string]*mcp.Tool
	version string
	httpKey  string // optional bearer token; empty = no auth required

	// httpAllowOpen is the explicit opt-in to bind HTTP on a non-loopback
	// interface without --http-key. Default false → refuse the bind (the
	// "default-deny remote HTTP" rule, v0.5.0 milestone, #199). Set true
	// only when the operator has out-of-band authentication (e.g. a
	// reverse proxy doing its own auth, a firewall restricting the
	// network, a trusted Docker network).
	httpAllowOpen bool

	// basePath is the externally-visible URL prefix when pincher is served
	// behind a reverse proxy (e.g. "/pincher"). Always normalized: empty,
	// or starts with "/" and has no trailing "/". Affects request routing
	// (incoming prefix is stripped) and link generation (OpenAPI spec,
	// dashboard fetches). See SetBasePath.
	basePath string

	// trustProxy controls whether X-Forwarded-Prefix / X-Forwarded-Proto /
	// X-Forwarded-Host headers are honored. Off by default so a direct
	// (non-proxied) caller can't spoof headers to manipulate generated URLs.
	trustProxy bool

	// Actual bound HTTP address — populated by ListenAndServeHTTP after
	// net.Listen succeeds, so ":0" auto-pick can report the real port.
	mu       sync.Mutex
	httpAddr string

	// fetchAllowLoopback opens the SSRF gate for loopback addresses
	// (127.0.0.0/8, ::1). Default false — production deployments cannot
	// fetch from localhost. Tests using httptest.Server set this to true
	// since httptest binds to 127.0.0.1. Never expose this as a CLI flag
	// without a serious threat-model review; loopback access from the
	// fetch tool is a classic SSRF pivot.
	fetchAllowLoopback bool

	// slowQueryThresholdMS — tool calls whose latency exceeds this value
	// (in milliseconds) are persisted to the slow_queries table (#42 part 2).
	// 0 disables the capture entirely (zero overhead for users who don't
	// opt in). Set via SetSlowQueryThreshold or --slow-query-ms flag.
	slowQueryThresholdMS int64

	// HTTP rate limiting — sliding window per remote IP.
	rateMu      sync.Mutex
	rateWindows map[string][]time.Time // IP → request timestamps in current window
	rateLimit   int                    // max requests per window; 0 = unlimited
	rateWindow  time.Duration          // window size (default 1 minute)

	sessionOnce    sync.Once
	sessionRoot    string
	sessionProject string // derived from sessionRoot
	sessionID      string // db.ProjectIDFromPath(sessionRoot)

	// persistentSessionID is a stable identifier for this process invocation,
	// used as the primary key in the sessions table for persistent ROI tracking.
	persistentSessionID string
	sessionStartedAt    time.Time

	// Session-level savings accumulators (atomic for goroutine safety).
	statsCalls       int64
	statsTokensUsed  int64
	statsTokensSaved int64
	statsLatencyMS   int64

	// Per-language call counts (#240). Sync.Map keyed by language name
	// (e.g. "Go", "Markdown") with *int64 values. Incremented per tool
	// call when the response carries a recognisable language signal;
	// flushed to the calls_by_language column on the sessions table
	// every 10s. Sync.Map over a plain map+mutex because the access
	// pattern is overwhelmingly increments + occasional snapshot, no
	// deletes, and we don't want a hot lock under high call volume.
	statsCallsByLanguage sync.Map

	// Query-failure / retry-rate counters (#241). Incremented from
	// jsonResultWithMeta only when the tool is "query-shaped"
	// (search/query/trace/neighborhood) — admin tools like list,
	// schema, architecture either always succeed or don't have a
	// meaningful result count.
	statsQueriesTotal            int64
	statsQueriesZeroResult       int64
	statsQueriesRetriedSucceeded int64
	statsTokensBurned            int64

	// lastZeroQuery holds the most-recent zero-result tool call's
	// (tool, normalized-query) pair within this session. When the very
	// next call uses the same tool with the same primary query string
	// and returns ≥1 result, we credit it as a successful retry —
	// "agent learned and recovered." Cleared on any non-zero result so
	// that an unrelated successful call between two zero-result calls
	// doesn't fool the counter.
	lastZero struct {
		mu        sync.Mutex
		tool      string
		queryHash string
	}

	// mcpConnected is set to 1 when an MCP client fires onInit.
	// Sessions are only flushed to DB when an MCP client is connected — this
	// prevents the HTTP-only dashboard process from recording its own tool
	// calls (POST /v1/architecture etc.) as fake MCP sessions in the DB.
	mcpConnected int32

	// binaryPath + binaryStartMTime support the stale-binary detector
	// (#278). Captured once at New(). On every health call we re-stat
	// the binary path; if its mtime moved forward, a newer binary
	// landed on disk while this MCP server kept running with the
	// old in-memory copy. The agent gets a one-line prompt to
	// reconnect via /mcp.
	binaryPath       string
	binaryStartMTime time.Time

	// autoRestartOnce guards the #352 self-restart-on-drift exit path
	// so concurrent tool calls don't race to os.Exit. Only fires when
	// PINCHER_AUTO_RESTART_ON_DRIFT=1 is set AND the on-disk binary
	// has been replaced since startup.
	autoRestartOnce sync.Once
	// exitFn is plumbed in for testability — production sets it to
	// os.Exit; tests substitute a recording stub. Defaults to os.Exit
	// in New().
	exitFn func(int)
}

// New creates and registers all 14 MCP tools.
func New(store *db.Store, indexer *index.Indexer, version string) *Server {
	now := time.Now()
	s := &Server{
		store:               store,
		indexer:             indexer,
		handlers:            make(map[string]mcp.ToolHandler),
		tools:               make(map[string]*mcp.Tool),
		version:             version,
		persistentSessionID: fmt.Sprintf("sess-%d", now.UnixNano()),
		sessionStartedAt:    now,
		exitFn:              os.Exit, // #352: substituted by tests
	}
	// Capture the running binary's path + initial mtime so the
	// health stale-binary check (#278) can compare against the
	// current on-disk mtime later. Failures here just leave the
	// fields zero; health reports binary_stale=false in that case.
	if exe, err := os.Executable(); err == nil {
		s.binaryPath = exe
		if info, statErr := os.Stat(exe); statErr == nil {
			s.binaryStartMTime = info.ModTime()
		}
	}
	s.mcp = mcp.NewServer(
		&mcp.Implementation{Name: "pincher", Version: version},
		&mcp.ServerOptions{
			InitializedHandler:      s.onInit,
			RootsListChangedHandler: s.onRoots,
		},
	)
	s.registerTools()
	return s
}

// StartSessionFlusher launches a background goroutine that persists
// session stats to SQLite. The cadence adapts (#204): when an HTTP
// dashboard peer process is detected (another pincher advertising an
// http_url row in the sessions table within httpPeerStaleAfter), the
// ticker drops to sessionFlushFast (1 s) so dashboard updates lag the
// stdio process by ≤1 s instead of ≤10 s. Otherwise the ticker holds
// at sessionFlushInterval (10 s).
//
// Detection happens after every flush, so the cadence transitions
// land at most one slow-tick (10 s) after the peer appears or
// disappears. That's a one-time settling cost, not steady-state lag.
func (s *Server) StartSessionFlusher(ctx context.Context) {
	go func() {
		current := sessionFlushInterval
		t := time.NewTicker(current)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.flushSession() // final flush on shutdown
				return
			case <-t.C:
				s.flushSession()
				wanted := sessionFlushInterval
				if s.hasHTTPPeer() {
					wanted = sessionFlushFast
				}
				if wanted != current {
					t.Reset(wanted)
					current = wanted
				}
			}
		}
	}()
}

// hasHTTPPeer reports whether another pincher process has flushed an
// http_url-bearing sessions row within httpPeerStaleAfter. Used by the
// adaptive session flusher (#204) to drop to sub-second cadence when a
// dashboard is active. Failures and empty results return false so a
// query glitch never strands the cadence at fast (which would amplify
// write load on long-running daily DBs).
func (s *Server) hasHTTPPeer() bool {
	myPID := os.Getpid()
	cutoff := time.Now().Add(-httpPeerStaleAfter).Unix()
	var found int
	err := s.store.RO().QueryRow(
		`SELECT 1 FROM sessions
		 WHERE http_url != '' AND http_pid > 0 AND http_pid != ?
		   AND last_seen >= ?
		 LIMIT 1`,
		myPID, cutoff,
	).Scan(&found)
	return err == nil && found == 1
}

// flushSession persists current in-memory session stats to the sessions table.
//
// Flushes when EITHER an MCP client has connected OR the HTTP listener is
// bound — the HTTP-bound case lets `pincher web` discover the URL even
// when no MCP client has registered yet (e.g. dashboard-first launches).
// Pure stats-less HTTP-only processes still skip the write because calls=0
// short-circuits below the connection gate.
func (s *Server) flushSession() {
	httpURL := s.HTTPAddr()
	if atomic.LoadInt32(&s.mcpConnected) == 0 && httpURL == "" {
		return // no MCP client AND no HTTP listener — nothing useful to record
	}
	calls := atomic.LoadInt64(&s.statsCalls)
	if calls == 0 && httpURL == "" {
		return // nothing to record yet
	}
	tokensUsed := atomic.LoadInt64(&s.statsTokensUsed)
	tokensSaved := atomic.LoadInt64(&s.statsTokensSaved)
	costAvoided := float64(tokensSaved) / 1_000_000.0 * baseCostPer1M
	httpPID := 0
	if httpURL != "" {
		httpURL = "http://" + displayAddr(httpURL) + s.basePath
		httpPID = os.Getpid()
	}
	qm := db.QueryMetrics{
		QueriesTotal:            atomic.LoadInt64(&s.statsQueriesTotal),
		QueriesZeroResult:       atomic.LoadInt64(&s.statsQueriesZeroResult),
		QueriesRetriedSucceeded: atomic.LoadInt64(&s.statsQueriesRetriedSucceeded),
		TokensBurnedOnFailures:  atomic.LoadInt64(&s.statsTokensBurned),
	}
	if err := s.store.RecordSessionWithMetrics(s.persistentSessionID, s.sessionStartedAt, calls, tokensUsed, tokensSaved, costAvoided, httpURL, httpPID, s.snapshotCallsByLanguage(), qm); err != nil {
		slog.Warn("pincher.session.flush.err", "err", err)
	}
}

// snapshotCallsByLanguage serializes the in-memory per-language call
// counter map to a stable-sorted JSON object. Sorted keys keep the
// flushed string deterministic across goroutine ordering, which makes
// `pincher stats` output reproducible and keeps the snapshot tests
// (#33) from flapping. Returns "" when no per-language calls have
// been recorded — the column then stays NULL rather than `{}`,
// distinguishing pre-v15 rows from "v15 row, no language data yet".
func (s *Server) snapshotCallsByLanguage() string {
	type kv struct {
		Lang  string
		Count int64
	}
	var pairs []kv
	s.statsCallsByLanguage.Range(func(k, v any) bool {
		lang, _ := k.(string)
		ptr, _ := v.(*int64)
		if ptr == nil {
			return true
		}
		count := atomic.LoadInt64(ptr)
		if count > 0 {
			pairs = append(pairs, kv{lang, count})
		}
		return true
	})
	if len(pairs) == 0 {
		return ""
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Lang < pairs[j].Lang })
	out := make(map[string]int64, len(pairs))
	for _, p := range pairs {
		out[p.Lang] = p.Count
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// recordCallLanguage increments the per-tool-call language counter by
// 1. Called from jsonResultWithMeta when the response carries a
// recognisable language signal. Empty lang is a no-op so callers don't
// have to gate the call themselves.
func (s *Server) recordCallLanguage(lang string) {
	if lang == "" {
		return
	}
	v, _ := s.statsCallsByLanguage.LoadOrStore(lang, new(int64))
	ptr, _ := v.(*int64)
	if ptr != nil {
		atomic.AddInt64(ptr, 1)
	}
}

// isDeveloperScratchPath reports whether file_path's basename matches
// the developer-scratch naming convention (#275): `scratch_*.go`,
// `.scratch_*.go`, `tmp_*.go`, `_scratch.go`. Used by handleArchitecture
// to keep the entry_points list focused on the project's actual
// entrypoints rather than dev-machine pollution.
//
// Filter applies at the project root only — directory components are
// ignored so a `testdata/corpus/foo/scratch.go` test fixture still
// surfaces. The check is intentionally narrow: anything else
// (`tmp.go`, `playground.go`, `notes.go`) stays visible because the
// false-negative cost is low — at worst the entry_points list shows
// one extra row.
func isDeveloperScratchPath(filePath string) bool {
	base := filePath
	if i := strings.LastIndexAny(filePath, `/\`); i >= 0 {
		base = filePath[i+1:]
	}
	if base == filePath {
		// File is at project root — apply the scratch filter.
	} else {
		// File is nested — keep it visible.
		return false
	}
	switch {
	case strings.HasPrefix(base, "scratch_"),
		strings.HasPrefix(base, ".scratch_"),
		strings.HasPrefix(base, "tmp_"),
		base == "_scratch.go",
		base == "scratch.go",
		base == ".scratch.go":
		return true
	}
	return false
}

// sortTraceCandidates ranks symbols for trace's name-resolution
// heuristic (#319). Callers expect `trace name="main"` to land on
// the binary's actual entry function, not a scratch file's
// `package main` declaration. Order:
//   1. Non-scratch + non-test files first.
//   2. Callable kinds first (Function, Method, Class, Interface,
//      Type) — these can actually have CALLS edges so the trace
//      will yield hops. Module / Setting / Section / Document
//      can't, so they're least preferred.
//   3. Stable on the existing order from GetSymbolsByName when
//      tied — that order is alphabetic by file_path.
func sortTraceCandidates(syms []db.Symbol) {
	kindRank := func(k string) int {
		switch k {
		case "Function":
			return 0
		case "Method":
			return 1
		case "Class", "Interface", "Type", "Enum", "Trait":
			return 2
		case "Variable":
			return 3
		default:
			// Module, Setting, Section, Document, Block, Resource,
			// Output, Local, Provider — none carry CALLS edges.
			return 99
		}
	}
	pathRank := func(p string) int {
		// scratch and test files rank below production. Two buckets:
		// scratch (worst, dev pollution) and test (still legitimate
		// but secondary).
		if isDeveloperScratchPath(p) {
			return 2
		}
		if isTestFile(p) {
			return 1
		}
		return 0
	}
	sort.SliceStable(syms, func(i, j int) bool {
		pi, pj := pathRank(syms[i].FilePath), pathRank(syms[j].FilePath)
		if pi != pj {
			return pi < pj
		}
		return kindRank(syms[i].Kind) < kindRank(syms[j].Kind)
	})
}

// isTestFile reports whether file_path looks like a test file across
// the languages pincher indexes (#305). Used by handleArchitecture
// to keep hotspots focused on production code; pass `include_tests=true`
// to opt back into the legacy mixed list.
//
// Recognised conventions:
//   - Go:        `_test.go`
//   - JS/TS:     `*.test.{js,ts,tsx,jsx,mjs,cjs}`, `*.spec.{js,ts,tsx,jsx}`
//   - Python:    `test_*.py`, `*_test.py`, `tests/` directory
//   - Ruby:      `*_spec.rb`, `*_test.rb`, `spec/` / `test/` directories
//   - Rust:      `tests/` directory (cargo convention; in-file `#[test]` is unfilterable here)
//   - Java/Kotlin/Scala: `*Test.{java,kt,scala}`, `*Spec.{java,kt,scala}`
//   - Generic:   anything in a `__tests__/` (Jest) or `test/` directory
//
// The check works on both `/`- and `\`-separated paths so Windows-
// indexed projects don't slip through.
func isTestFile(filePath string) bool {
	low := strings.ToLower(filePath)
	// Normalise so the directory checks work regardless of OS path style.
	low = strings.ReplaceAll(low, `\`, `/`)
	base := low
	if i := strings.LastIndex(low, "/"); i >= 0 {
		base = low[i+1:]
	}

	// Directory-based test conventions.
	for _, dir := range []string{"/__tests__/", "/tests/", "/test/", "/spec/"} {
		if strings.Contains(low, dir) {
			return true
		}
	}
	// Top-level (no directory prefix) — also catch `tests/...` etc.
	for _, prefix := range []string{"__tests__/", "tests/", "test/", "spec/"} {
		if strings.HasPrefix(low, prefix) {
			return true
		}
	}

	// Filename suffixes.
	suffixes := []string{
		"_test.go",
		"_test.py", "_spec.rb", "_test.rb",
		".test.js", ".test.ts", ".test.tsx", ".test.jsx",
		".test.mjs", ".test.cjs",
		".spec.js", ".spec.ts", ".spec.tsx", ".spec.jsx",
		"test.java", "test.kt", "test.scala",
		"spec.java", "spec.kt", "spec.scala",
	}
	for _, sfx := range suffixes {
		if strings.HasSuffix(base, sfx) {
			return true
		}
	}
	// Python `test_*.py` prefix.
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}
	return false
}

// languageRE matches the first occurrence of `"language":"X"` in a
// marshalled response payload. JSON guarantees a single quoting style,
// so this is safe to scan against the rendered payload (#240). Picks
// the FIRST language seen as the call's "primary language" — adequate
// for bypass detection where any signal beats none.
var languageRE = regexp.MustCompile(`"language"\s*:\s*"([^"]+)"`)

// queryShapedTools is the set of MCP tools whose responses carry a
// meaningful `count` field that should feed retry-rate diagnostics
// (#241). Admin/orientation tools (architecture, list, schema, health,
// stats, guide, adr, fetch, index, symbol, symbols, context, changes)
// are excluded — either they always succeed, don't have a count, or
// their "zero result" shape isn't a friction signal worth tracking.
var queryShapedTools = map[string]bool{
	"search":       true,
	"query":        true,
	"trace":        true,
	"neighborhood": true,
}

// primaryQueryArg returns the argument that uniquely identifies a
// query-shaped tool call, used as the retry-detection key (#241). The
// retry signal is "agent re-issued the same logical query with a
// loosened threshold and got results"; the discriminator is the query
// text itself, not the tuning knobs (min_confidence, limit, kind
// filter) that the retry usually changes.
func primaryQueryArg(tool string, args map[string]any) string {
	switch tool {
	case "search":
		if q, ok := args["query"].(string); ok {
			return q
		}
	case "query":
		if q, ok := args["pinchql"].(string); ok {
			return q
		}
		if q, ok := args["cypher"].(string); ok {
			return q
		}
	case "trace":
		if q, ok := args["name"].(string); ok {
			return q
		}
		if q, ok := args["id"].(string); ok {
			return q
		}
	case "neighborhood":
		if q, ok := args["id"].(string); ok {
			return q
		}
	}
	return ""
}

// recordQueryMetrics updates the v17 query-failure counters (#241).
// Called from jsonResultWithMeta after the response is marshalled so
// tokensUsed is known. Tools outside queryShapedTools no-op. The
// retry-detection rule: if THIS call returns ≥1 results AND the
// previous query-shaped call within the same session was a zero
// result on (sameTool, sameQuery), credit the recovery; otherwise
// just track the zero-result counter and burned-tokens accumulator.
func (s *Server) recordQueryMetrics(tool string, args map[string]any, data map[string]any, tokensUsed int) {
	if !queryShapedTools[tool] {
		return
	}
	var count int
	switch v := data["count"].(type) {
	case int:
		count = v
	case int64:
		count = int(v)
	case float64:
		count = int(v)
	}
	q := primaryQueryArg(tool, args)

	atomic.AddInt64(&s.statsQueriesTotal, 1)

	s.lastZero.mu.Lock()
	prevTool := s.lastZero.tool
	prevHash := s.lastZero.queryHash
	if count == 0 {
		s.lastZero.tool = tool
		s.lastZero.queryHash = q
	} else {
		s.lastZero.tool = ""
		s.lastZero.queryHash = ""
	}
	s.lastZero.mu.Unlock()

	if count == 0 {
		atomic.AddInt64(&s.statsQueriesZeroResult, 1)
		atomic.AddInt64(&s.statsTokensBurned, int64(tokensUsed))
		return
	}
	if prevTool == tool && prevHash == q && q != "" {
		atomic.AddInt64(&s.statsQueriesRetriedSucceeded, 1)
	}
}

// MCPServer returns the underlying *mcp.Server.
func (s *Server) MCPServer() *mcp.Server { return s.mcp }

func (s *Server) onInit(ctx context.Context, req *mcp.InitializedRequest) {
	atomic.StoreInt32(&s.mcpConnected, 1)
	s.sessionOnce.Do(func() {
		s.detectRoot(ctx, req.Session)
	})
}

func (s *Server) onRoots(ctx context.Context, req *mcp.RootsListChangedRequest) {
	s.sessionOnce.Do(func() {
		s.detectRoot(ctx, req.Session)
	})
}

func (s *Server) detectRoot(ctx context.Context, session *mcp.ServerSession) {
	if session != nil {
		if result, err := session.ListRoots(ctx, nil); err == nil && len(result.Roots) > 0 {
			if path, ok := parseFileURI(result.Roots[0].URI); ok {
				s.setRoot(path)
				return
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		s.setRoot(cwd)
	}
}

func (s *Server) setRoot(path string) {
	s.sessionRoot = path
	s.sessionProject = db.ProjectNameFromPath(path)
	s.sessionID = db.ProjectIDFromPath(path)
}

// gzipResponseWriter wraps an http.ResponseWriter, routing writes through a gzip.Writer.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

// ServeHTTP makes Server implement http.Handler.
//
// Route: POST /v1/{tool}  — call any registered tool with a JSON body.
// Route: GET  /v1/health  — liveness probe (returns {"ok":true}).
//
// This enables any HTTP client (OpenAI, Gemini, Cursor, CI/CD pipelines)
// to use pincherMCP without the MCP stdio protocol.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept-Encoding")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Bearer token auth — enforced when --http-key is set.
	//
	// Constant-time comparison via SHA-256 + subtle.ConstantTimeCompare:
	// hashing both inputs to a fixed 32-byte digest first means the
	// comparison time is independent of both the content AND the length
	// of the supplied token. A direct `tok != s.httpKey` (or even a
	// length-aware ConstantTimeCompare on the raw strings) leaks the
	// configured key's length to a network attacker who can measure
	// response timing — once the length is known, character-by-character
	// derivation follows. Hash-and-compare closes both side channels.
	//
	// SECURITY: never replace this with `==` on the raw tokens. The
	// regression test in TestAuth_ConstantTime_LengthInvariant asserts
	// behaviour on same-length and different-length mismatches; the
	// regression test in TestAuth_MalformedHeader_VariousShapes asserts
	// every malformed-header rejection takes the SAME path (no body
	// shape difference, no fast-fail before the constant-time compare).
	//
	// Authorization-header malformed-shape parity (#55):
	//
	// CutPrefix returns (tok, true) when the header starts with "Bearer "
	// and (auth, false) otherwise. We always go through the SHA-256 +
	// constant-time-compare path regardless of whether CutPrefix matched.
	// On mismatch ("Bearer wrongkey"), the compare returns 1 vs the
	// configured key. On malformed shapes ("Basic ...", empty,
	// "bearer key" lowercase), `tok` is the full header value, the
	// hash of which is a 32-byte non-match against the configured
	// key's hash. ConstantTimeCompare runs to completion in both
	// cases — same number of operations, same response body.
	//
	// Why this matters even after PR #44: pre-fix, a request with
	// "Authorization: Basic abc" would produce tok="Authorization:
	// Basic abc" (TrimPrefix is a no-op without exact match), then
	// hash and compare. A request with no Authorization header at all
	// would produce tok="", then hash and compare. Both rejected, but
	// the latter takes one less SHA-256 byte to ingest. Edge-case
	// timing distinguishability. Post-fix: identical work in both.
	if s.httpKey != "" {
		auth := r.Header.Get("Authorization")
		tok, hasBearer := strings.CutPrefix(auth, "Bearer ")
		got := sha256.Sum256([]byte(tok))
		want := sha256.Sum256([]byte(s.httpKey))
		matches := subtle.ConstantTimeCompare(got[:], want[:]) == 1
		if !hasBearer || !matches {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized — set Authorization: Bearer <key>"})
			return
		}
	}

	// Rate limiting — per remote IP sliding window. Honors X-Forwarded-For
	// when --trust-proxy is on so the rate-key reflects the real client
	// behind a reverse proxy (issue #40).
	ip := s.clientIP(r)
	if !s.allowRequest(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("rate limit exceeded — max %d requests per %s", s.rateLimit, s.rateWindow)})
		return
	}

	// Transparently compress responses when the client supports it.
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		w = &gzipResponseWriter{ResponseWriter: w, gz: gz}
	}

	// Reverse-proxy basepath: when configured (or advertised via
	// X-Forwarded-Prefix with trustProxy on), strip the prefix from the
	// request path so /pincher/v1/health and /v1/health both route to the
	// same handler. This lets the proxy preserve OR strip the prefix.
	if prefix := s.effectivePrefix(r); prefix != "" {
		if stripped := strings.TrimPrefix(r.URL.Path, prefix); stripped != r.URL.Path {
			r.URL.Path = stripped
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
		}
	}

	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	if path == "health" {
		// auth_required surfaces whether --http-key is set. The dashboard
		// reads it to decide whether to show a "no auth in place" notice
		// (#203). Server-side enforcement is unchanged — this is purely
		// metadata so clients can render appropriate UX.
		json.NewEncoder(w).Encode(map[string]any{
			"ok":            true,
			"version":       s.version,
			"auth_required": s.httpKey != "",
		})
		return
	}
	if path == "openapi.json" {
		json.NewEncoder(w).Encode(s.openAPISpec(r))
		return
	}
	if path == "dashboard" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Tightened CSP (#56): no 'unsafe-inline' anywhere. The
		// dashboard's CSS and JS now load from /v1/dashboard.css and
		// /v1/dashboard.js so they hit script-src 'self' / style-src 'self'.
		// XSS that injects an inline <script> into the rendered HTML is
		// now BLOCKED BY THE BROWSER even if it bypasses our esc()
		// escape pipeline — defense-in-depth beyond the source-side
		// escaping work in #46.
		s.writeDashboardSecurityHeaders(w, "default-src 'self'; "+
			"script-src 'self'; "+
			"style-src 'self'; "+
			"img-src 'self' data:; "+
			"connect-src 'self'; "+
			"object-src 'none'; "+
			"base-uri 'self'; "+
			"form-action 'self'; "+
			"frame-ancestors 'none'")
		w.Write([]byte(renderDashboard(s.effectivePrefix(r))))
		return
	}
	if path == "dashboard.js" && r.Method == http.MethodGet {
		// Same security headers as the HTML response. Cache for 10 minutes
		// to amortize repeat fetches by the same browser tab. The basepath
		// substitution in the JS body means cache key is per-prefix; since
		// most deployments have a stable prefix, this works as expected.
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=600")
		s.writeDashboardSecurityHeaders(w, "default-src 'none'; frame-ancestors 'none'")
		w.Write([]byte(renderDashboardJS(s.effectivePrefix(r))))
		return
	}
	if path == "dashboard.css" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=600")
		s.writeDashboardSecurityHeaders(w, "default-src 'none'; frame-ancestors 'none'")
		w.Write([]byte(renderDashboardCSS()))
		return
	}
	// GET /v1/stats — dashboard-safe stats reader. Reads from DB only; never
	// touches in-memory atomic counters so it doesn't pollute the MCP session.
	if path == "stats" && r.Method == http.MethodGet {
		// Latest session (MCP process, flushed every 10s)
		var sess map[string]any
		if rows, err := s.store.GetSessions(1); err == nil && len(rows) > 0 {
			r0 := rows[0]
			sess = map[string]any{
				"calls":              r0.Calls,
				"tokens_used":        r0.TokensUsed,
				"tokens_saved":       r0.TokensSaved,
				"total_cost_avoided": fmt.Sprintf("$%.4f", r0.CostAvoided),
				"started_at":         r0.StartedAt.Format(time.RFC3339),
				"last_seen":          r0.LastSeen.Format(time.RFC3339),
			}
		}
		// All-time cumulative
		var allTime map[string]any
		if atCalls, atUsed, atSaved, atCost, err := s.store.GetAllTimeSavings(); err == nil {
			allTime = map[string]any{
				"calls":              atCalls,
				"tokens_used":        atUsed,
				"tokens_saved":       atSaved,
				"total_cost_avoided": fmt.Sprintf("$%.4f", atCost),
			}
		}
		// Session-scoped project ID, if a root has been detected. The
		// dashboard uses this to default the ADR project picker so users
		// don't re-select it every page load.
		resp := map[string]any{"session": sess, "all_time": allTime}
		if s.sessionID != "" {
			resp["session_project"] = s.sessionID
		}
		json.NewEncoder(w).Encode(resp)
		return
	}
	// GET /v1/sessions — per-session savings history for sparkline chart.
	if path == "sessions" && r.Method == http.MethodGet {
		sessions, err := s.store.GetSessions(90) // last 90 sessions
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		// #334: zero-len init so HTTP /v1/sessions returns "sessions":[] (not null) when no sessions exist.
		rows := []map[string]any{}
		for _, sess := range sessions {
			rows = append(rows, map[string]any{
				"session_id":   sess.SessionID,
				"started_at":   sess.StartedAt.Format(time.RFC3339),
				"last_seen":    sess.LastSeen.Format(time.RFC3339),
				"calls":        sess.Calls,
				"tokens_used":  sess.TokensUsed,
				"tokens_saved": sess.TokensSaved,
				"cost_avoided": fmt.Sprintf("$%.4f", sess.CostAvoided),
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"sessions": rows})
		return
	}
	// POST /v1/index-progress — live file progress for a running index job.
	if path == "index-progress" && r.Method == http.MethodPost {
		var body struct {
			Project string `json:"project"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		projectID := body.Project
		if projectID == "" {
			projectID = s.sessionID
		}
		done, total, active := s.indexer.GetProgress(projectID)
		json.NewEncoder(w).Encode(map[string]any{
			"project":     projectID,
			"files_done":  done,
			"files_total": total,
			"active":      active,
		})
		return
	}
	// GET /v1/projects — list all indexed projects.
	if path == "projects" && r.Method == http.MethodGet {
		projects, err := s.store.ListProjects()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"projects": projects})
		return
	}
	// DELETE /v1/projects/empty — bulk-delete every project with zero
	// symbols and zero edges. These accumulate from SessionStart hooks
	// firing in non-code directories and clutter the dashboard.
	if path == "projects/empty" && r.Method == http.MethodDelete {
		n, err := s.store.DeleteEmptyProjects()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"deleted": n})
		return
	}
	// DELETE /v1/projects — remove a project and all its data.
	if path == "projects" && r.Method == http.MethodDelete {
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": "body must be {\"id\":\"<project-id>\"}"})
			return
		}
		if err := s.store.DeleteProject(body.ID); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"deleted": body.ID})
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed — use POST /v1/{tool}"}`, http.StatusMethodNotAllowed)
		return
	}

	handler, ok := s.handlers[path]
	if !ok {
		// Build the available-tools list from the live registry so a new
		// tool added in registerTools() shows up here automatically — keeps
		// this error from drifting (it claimed 14 tools after `fetch` was
		// added to make 15).
		available := make([]string, 0, len(s.handlers))
		for name := range s.handlers {
			available = append(available, name)
		}
		sort.Strings(available)
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"error": fmt.Sprintf("unknown tool %q — available: %s", path, strings.Join(available, ", ")),
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MB limit
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": "failed to read request body"})
		return
	}
	if len(body) == 0 {
		body = []byte("{}")
	}

	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      path,
			Arguments: json.RawMessage(body),
		},
	}

	result, err := handler(r.Context(), req)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}

	if result.IsError {
		w.WriteHeader(http.StatusBadRequest)
	}

	// Extract the text content from the MCP result and re-emit as JSON.
	if len(result.Content) > 0 {
		if tc, ok := result.Content[0].(*mcp.TextContent); ok {
			w.Write([]byte(tc.Text))
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"error": "empty result"})
}

// openAPISpec returns a minimal OpenAPI 3.1 document describing every HTTP tool endpoint.
// Served at GET /v1/openapi.json so any client (Postman, Cursor, copilots) can auto-import.
//
// When deployed behind a reverse proxy, the path keys are prefixed with the
// effective basepath and a "servers" block is added so imported clients build
// the right base URL.
func (s *Server) openAPISpec(r *http.Request) map[string]any {
	prefix := s.effectivePrefix(r)
	tools := []string{"index", "symbol", "symbols", "context", "search", "query", "trace", "changes", "architecture", "schema", "list", "adr", "health", "stats", "fetch"}
	paths := map[string]any{}
	for _, t := range tools {
		paths[prefix+"/v1/"+t] = map[string]any{
			"post": map[string]any{
				"operationId": t,
				"summary":     "Call the " + t + " tool",
				"requestBody": map[string]any{
					"required": true,
					"content":  map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "Tool result", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}},
				},
			},
		}
	}
	paths[prefix+"/v1/health"] = map[string]any{
		"get": map[string]any{"operationId": "health", "summary": "Liveness probe", "responses": map[string]any{"200": map[string]any{"description": "ok"}}},
	}
	spec := map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": "pincherMCP HTTP API", "version": s.version},
		"paths":   paths,
	}
	if prefix != "" || (s.trustProxy && r.Header.Get("X-Forwarded-Host") != "") {
		proto := "http"
		host := r.Host
		if s.trustProxy {
			if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
				proto = p
			}
			if h := r.Header.Get("X-Forwarded-Host"); h != "" {
				host = h
			}
		}
		spec["servers"] = []any{map[string]any{"url": fmt.Sprintf("%s://%s%s", proto, host, prefix)}}
	}
	return spec
}

// ListenAndServeHTTP starts an HTTP server on addr (e.g. ":8080").
// It blocks until ctx is cancelled or the listener fails.
// SetHTTPKey configures a required bearer token for all HTTP requests.
// If key is empty, authentication is disabled (suitable for localhost-only deployments).
func (s *Server) SetHTTPKey(key string) { s.httpKey = key }

// SetHTTPAllowOpen is the explicit opt-in to bind HTTP on a non-loopback
// interface WITHOUT --http-key. By default ListenAndServeHTTP refuses
// such a configuration (#199 default-deny remote HTTP). Set this to true
// only when out-of-band authentication is in place — typically a reverse
// proxy that does its own auth, or a trusted Docker network.
func (s *Server) SetHTTPAllowOpen(allow bool) { s.httpAllowOpen = allow }

// SetBasePath sets the externally-visible URL prefix (e.g. "/pincher") for
// reverse-proxy deployments. Input is normalized: leading "/" is added if
// missing, trailing "/" is stripped, and "" or "/" both clear the prefix.
//
// When set, ServeHTTP strips this prefix from incoming requests before
// routing — so both /pincher/v1/health and /v1/health route to the health
// handler. The OpenAPI spec and embedded dashboard also pick up the prefix
// so generated links and fetches go through the proxy correctly.
func (s *Server) SetBasePath(p string) { s.basePath = normalizeBasePath(p) }

// SetTrustProxy enables honoring X-Forwarded-Prefix / X-Forwarded-Proto /
// X-Forwarded-Host headers for prefix detection and OpenAPI server URL
// generation. Disabled by default — only turn on when behind a trusted
// proxy that strips and re-adds these headers itself.
func (s *Server) SetTrustProxy(t bool) { s.trustProxy = t }

// SetSlowQueryThreshold configures the latency above which tool calls are
// persisted to the slow_queries table (#42 part 2). 0 disables capture.
// Typical: SetSlowQueryThreshold(50) to log calls over 50ms.
func (s *Server) SetSlowQueryThreshold(ms int64) { s.slowQueryThresholdMS = ms }

// maybeRecordSlowQuery is called from {json,text}ResultWithMeta after the
// per-call latency is computed. No-op when slowQueryThresholdMS is 0.
//
// Argument values are passed through redactSensitiveArgs before persisting,
// so secret-shaped values (api_key, bearer, password, token, etc.) are
// replaced with "[redacted]" — never stores raw credentials passed via
// tool calls. The redaction is keyed on the field NAME, so a value that
// happens to look like a token but is keyed under (say) `query` stays
// visible because the user might want to see what slow query they ran.
func (s *Server) maybeRecordSlowQuery(tool string, args map[string]any, latencyMS int64) {
	if s.slowQueryThresholdMS <= 0 {
		return
	}
	if latencyMS < s.slowQueryThresholdMS {
		return
	}
	projectID, _ := args["project"].(string)
	redacted := redactSensitiveArgs(args)
	argsJSON, _ := json.Marshal(redacted)
	if err := s.store.RecordSlowQuery(tool, projectID, latencyMS, string(argsJSON)); err != nil {
		slog.Warn("pincher.slow_query_record.err", "err", err, "tool", tool)
	}
}

// sensitiveArgKeys is the case-insensitive set of argument names whose
// values should be redacted before persisting to slow_queries. Match is
// substring on the lowercased key name; a key like `my_api_key` triggers
// redaction because it contains `api_key`.
//
// We err on the side of over-redaction. Losing a debug detail is cheap;
// persisting a credential is expensive.
var sensitiveArgKeys = []string{
	"api_key", "apikey", "api-key",
	"bearer", "token",
	"password", "passwd", "secret",
	"authorization", "auth",
}

// redactSensitiveArgs walks an args map and replaces any value whose key
// matches a sensitive pattern with the literal string "[redacted]".
// Returns a NEW map; the input is not mutated.
//
// Recursion: nested maps and []any of maps are walked. Other value types
// (strings, numbers, bools) are passed through unchanged.
func redactSensitiveArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if isSensitiveKey(k) {
			out[k] = "[redacted]"
			continue
		}
		switch val := v.(type) {
		case map[string]any:
			out[k] = redactSensitiveArgs(val)
		case []any:
			out[k] = redactSensitiveSlice(val)
		default:
			out[k] = v
		}
	}
	return out
}

func redactSensitiveSlice(in []any) []any {
	out := make([]any, len(in))
	for i, v := range in {
		switch val := v.(type) {
		case map[string]any:
			out[i] = redactSensitiveArgs(val)
		case []any:
			out[i] = redactSensitiveSlice(val)
		default:
			out[i] = v
		}
	}
	return out
}

func isSensitiveKey(k string) bool {
	lc := strings.ToLower(k)
	for _, pat := range sensitiveArgKeys {
		if strings.Contains(lc, pat) {
			return true
		}
	}
	return false
}

// BasePath returns the configured basepath, or "" if none.
func (s *Server) BasePath() string { return s.basePath }

// normalizeBasePath canonicalizes user-supplied prefixes:
//
//	""        → ""
//	"/"       → ""
//	"pincher" → "/pincher"
//	"/api/"   → "/api"
func normalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

// writeDashboardSecurityHeaders writes the CSP plus the three legacy
// hardening headers shared by all dashboard responses (HTML, JS, CSS).
// CSP is per-resource — the HTML needs the full resource policy; the
// asset endpoints just need clickjacking protection and a strict default.
func (s *Server) writeDashboardSecurityHeaders(w http.ResponseWriter, csp string) {
	w.Header().Set("Content-Security-Policy", csp)
	// X-Content-Type-Options stops MIME-sniffing-based attacks where a
	// crafted response body is interpreted as a different type.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// X-Frame-Options is the legacy header for the same protection
	// frame-ancestors gives in the CSP — kept for older browsers.
	w.Header().Set("X-Frame-Options", "DENY")
	// Referrer-Policy keeps the dashboard's URL from leaking to any
	// third-party origin via outbound links.
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// effectivePrefix returns the externally-visible URL prefix for this request.
// Precedence: X-Forwarded-Prefix (only when trustProxy is on) → s.basePath → "".
func (s *Server) effectivePrefix(r *http.Request) string {
	if s.trustProxy {
		if p := r.Header.Get("X-Forwarded-Prefix"); p != "" {
			return normalizeBasePath(p)
		}
	}
	return s.basePath
}

// clientIP returns the rate-limit / logging key for r. Behaviour:
//
//   - When trustProxy is on and X-Forwarded-For is present: use the leftmost
//     non-empty entry in the comma-separated list. RFC 7239 / X-Forwarded-For
//     convention: each proxy appends its own peer to the right, so the
//     leftmost entry is the original client and the rightmost is the proxy
//     immediately upstream of pincher.
//   - Otherwise: extract the host portion of r.RemoteAddr via net.SplitHostPort,
//     which correctly handles bracketed IPv6 forms like "[::1]:8080" that
//     the previous strings.Cut(":") implementation mangled into "[".
//   - On any parse failure: fall back to r.RemoteAddr unchanged. Better to
//     have a key (even an imperfect one) than no rate limiting at all.
//
// Spoof gate: when trustProxy is off (the default), X-Forwarded-For is
// IGNORED — direct callers MUST NOT influence the rate-limit key by setting
// the header themselves. This mirrors the trust gate used by effectivePrefix
// for X-Forwarded-Prefix.
//
// XFF parsing details (#41 item 6):
//   - Port stripping: some proxies emit `1.2.3.4:8080` or `[::1]:8080`.
//     Without stripping, ephemeral source ports would each get their own
//     rate-limit bucket — bypassing per-IP throttling. We use the same
//     net.SplitHostPort fallback as the RemoteAddr branch.
//   - Empty leftmost (`, 1.2.3.4`): falls through to RemoteAddr. Better
//     to have a stable key than a bracket character.
//   - Header injection: Go's net/http already rejects values containing
//     CR/LF at parse time (validHeaderValue). The TestClientIP_*
//     header-injection sanity test below pins that assumption.
//   - Multiple XFF headers: RFC allows the same header to appear more
//     than once. r.Header.Get returns ONLY the first instance — which
//     is the legitimate proxy chain. A trailing second XFF header
//     injected by an attacker downstream of pincher's trusted proxy
//     is ignored by design.
func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Comma-separated list — take the leftmost entry (original client).
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				// Strip a trailing :port if present so ephemeral ports
				// don't fragment the rate-limit key. SplitHostPort
				// handles bracketed IPv6 ([::1]:8080) correctly; on
				// failure (no port) we use the value as-is.
				if host, _, err := net.SplitHostPort(ip); err == nil && host != "" {
					return host
				}
				return ip
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// SetRateLimit caps HTTP requests to limit per window duration per remote IP.
// limit=0 disables rate limiting. Typical: SetRateLimit(60, time.Minute).
func (s *Server) SetRateLimit(limit int, window time.Duration) {
	s.rateLimit = limit
	s.rateWindow = window
	s.rateWindows = make(map[string][]time.Time)
}

// allowRequest returns true if the remote IP is within its rate limit window.
func (s *Server) allowRequest(ip string) bool {
	if s.rateLimit <= 0 {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-s.rateWindow)
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	// Prune expired timestamps.
	ts := s.rateWindows[ip]
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	ts = ts[i:]
	if len(ts) >= s.rateLimit {
		s.rateWindows[ip] = ts
		return false
	}
	s.rateWindows[ip] = append(ts, now)
	return true
}

func (s *Server) ListenAndServeHTTP(ctx context.Context, addr string) error {
	// Default-deny gate (#199): refuse to bind a non-loopback interface
	// without --http-key unless the operator has explicitly opted in via
	// SetHTTPAllowOpen. Catches the "user accidentally publishes an open
	// API on their LAN" footgun. The pre-bind check means we never even
	// briefly advertise the port for an unsafe configuration.
	//
	// We honor `--http-allow-open` (s.httpAllowOpen) for legitimate cases
	// — reverse-proxy fronting, trusted Docker network, etc. The earlier
	// #149 warning still fires in that path so the operator sees the
	// state in logs.
	if s.httpKey == "" && !s.httpAllowOpen && !isLoopbackBind(addr) {
		return fmt.Errorf(
			"refusing to bind HTTP on %s without --http-key (set --http-key/$PINCHER_HTTP_KEY for Bearer auth, "+
				"or pass --http 127.0.0.1:<port> for loopback-only, "+
				"or pass --http-allow-open if a reverse proxy / trusted network handles auth)",
			addr)
	}

	// Retry port binding for up to 10 seconds. When the MCP client reconnects
	// (e.g. /mcp in Claude Code) the previous process may briefly hold the port
	// while it shuts down. Retrying here makes the dashboard resilient.
	var ln net.Listener
	var bindErr error
	for attempt := 0; attempt < 20; attempt++ {
		ln, bindErr = net.Listen("tcp", addr)
		if bindErr == nil {
			break
		}
		slog.Warn("pincher.http.bind.retry", "addr", addr, "attempt", attempt+1, "err", bindErr)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	if bindErr != nil {
		return fmt.Errorf("bind %s after retries: %w", addr, bindErr)
	}

	// Capture the actual bound address so ":0" (OS-picked port) surfaces the
	// real port in logs, saves it on the Server for HTTPAddr(), and emits a
	// friendly stderr line so humans see the URL without hunting in slog.
	actualAddr := ln.Addr().String()
	s.mu.Lock()
	s.httpAddr = actualAddr
	s.mu.Unlock()

	// Eager-flush so a `pincher web` invocation issued shortly after this
	// process starts can already see the URL in the sessions table — without
	// this, it'd have to wait up to sessionFlushInterval (10 s) for the
	// background ticker. Doing it on a goroutine avoids blocking the HTTP
	// startup path on a SQLite write.
	go s.flushSession()

	srv := &http.Server{Addr: actualAddr, Handler: s}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			slog.Warn("pincher.http.shutdown.err", "err", err)
		}
	}()
	slog.Info("pincher.http.listen", "addr", actualAddr)
	fmt.Fprintf(os.Stderr, "pincherMCP: HTTP listening on http://%s%s\n", displayAddr(actualAddr), s.basePath)

	// Trust gate: warn loudly if the HTTP API is exposed without a bearer
	// token AND the bind isn't loopback-only. The combination is a real
	// risk — a non-loopback bind without auth means anyone who can reach
	// the host can hit /v1/* with no credentials. We don't refuse the
	// configuration (some users legitimately front pincher with a reverse
	// proxy that does its own auth), but the log + stderr line make the
	// state observable so operators can fix it before traffic arrives.
	if s.httpKey == "" && !isLoopbackBind(actualAddr) {
		slog.Warn("pincher.http.no_auth_open_bind",
			"addr", actualAddr,
			"hint", "set --http-key <token> (or PINCHER_HTTP_KEY env) to require Bearer auth, or bind to 127.0.0.1: for loopback-only access")
		fmt.Fprintf(os.Stderr,
			"\n  WARNING: HTTP server is bound on %s without --http-key — anyone who can reach this host can call pincher tools.\n"+
				"  To require auth: pass --http-key <token> (or set PINCHER_HTTP_KEY).\n"+
				"  To restrict to local: bind 127.0.0.1:<port> instead of :<port>.\n\n",
			actualAddr)
	}

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// HTTPAddr returns the HTTP server's bound address, or "" if HTTP is not
// running. For ":0" binds this reflects the port the OS actually chose.
func (s *Server) HTTPAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.httpAddr
}

// displayAddr turns a net.Listener address ("[::]:8080", "0.0.0.0:8080",
// ":8080") into something you can click or paste into curl.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

// isLoopbackBind reports whether `addr` (host:port form) binds only to
// loopback. Used by the trust gate so we don't warn-spam users who
// deliberately bind 127.0.0.1: or [::1]: for local-only access.
//
// Treats unspecified hosts ("", "::", "0.0.0.0") as NON-loopback because
// they accept traffic from any reachable interface.
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Hostname like "localhost" — resolve isn't worth doing here;
		// treat as loopback only if it's literally "localhost".
		return strings.EqualFold(host, "localhost")
	}
	return ip.IsLoopback()
}

func parseFileURI(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p), true
}

// resolveProjectID returns the project ID for the given name/ID, falling back to session project.
// mustProject resolves the "project" arg and returns (projectID, nil) or ("", errResult).
// Handlers call: if pid, e := s.mustProject(args); e != nil { return e, nil }
func (s *Server) mustProject(args map[string]any) (string, *mcp.CallToolResult) {
	pid, err := s.resolveProjectID(str(args, "project"))
	if err != nil {
		return "", errResult(err.Error())
	}
	return pid, nil
}

func (s *Server) resolveProjectID(projectArg string) (string, error) {
	if projectArg == "" {
		if s.sessionID == "" {
			return "", fmt.Errorf("no project specified and no session project detected")
		}
		// Auto-index the session project on first use if it isn't in the DB yet.
		// This makes every tool work out-of-the-box without an explicit `index` call.
		if s.sessionRoot != "" {
			if p, _ := s.store.GetProject(s.sessionID); p == nil {
				slog.Info("pincher.auto_index.start", "path", s.sessionRoot)
				if _, err := s.indexer.Index(context.Background(), s.sessionRoot, false); err != nil {
					slog.Warn("pincher.auto_index.err", "err", err)
					return s.sessionID, fmt.Errorf("project not yet indexed — auto-index failed (%v). Run the `index` tool manually to retry", err)
				}
				slog.Info("pincher.auto_index.done", "path", s.sessionRoot)
			}
		}
		return s.sessionID, nil
	}
	// Accept either a name or ID
	p, err := s.store.GetProject(projectArg)
	if err != nil {
		return "", err
	}
	if p != nil {
		return p.ID, nil
	}
	// Try matching by name
	all, err := s.store.ListProjects()
	if err != nil {
		return "", err
	}
	for _, proj := range all {
		if proj.Name == projectArg {
			return proj.ID, nil
		}
	}
	return "", fmt.Errorf("project %q not found — use `list` to see available projects", projectArg)
}

// resolveProjectRoot returns the filesystem root for a project.
func (s *Server) resolveProjectRoot(projectID string) (string, error) {
	p, err := s.store.GetProject(projectID)
	if err != nil || p == nil {
		if s.sessionRoot != "" {
			return s.sessionRoot, nil
		}
		return "", fmt.Errorf("project not found")
	}
	return p.Path, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool registration
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) addTool(tool *mcp.Tool, handler mcp.ToolHandler) {
	s.mcp.AddTool(tool, handler)
	s.handlers[tool.Name] = handler
	s.tools[tool.Name] = tool
}

func (s *Server) registerTools() {
	// 1. index
	s.addTool(&mcp.Tool{
		Name:        "index",
		Description: "**Call once per project before using any other tool.** Indexes a repository: extracts symbols with byte offsets, builds the knowledge graph, populates FTS5 search — all in one pass. Incremental by default (content-hash checks skip unchanged files; the watcher keeps it fresh during a session). Pass `force=true` to re-parse every file (rare; only after schema/extractor changes).",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"path":{"type":"string","description":"Absolute path to the repository root. Defaults to session project root."},
				"force":{"type":"boolean","description":"If true, re-parse all files even if unchanged."}
			}
		}`),
	}, s.handleIndex)

	// 2. symbol
	s.addTool(&mcp.Tool{
		Name:        "symbol",
		Description: "**Use after `search`** to read one symbol's source by stable ID. O(1) byte-offset seeking — never re-parses the file. ID format: `{file_path}::{qualified_name}#{kind}`. **Prefer `context`** when you also need the symbol's dependencies, or **`symbols`** for batching multiple lookups (one round trip instead of N). Pass `fields` (comma-separated) to project specific keys and skip the source disk read when not needed.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"Stable symbol ID. Format: '{file_path}::{qualified_name}#{kind}'"},
				"project":{"type":"string","description":"Project name or ID. Defaults to session project."},
				"fields":{"type":"string","description":"Comma-separated allow-list of response keys (e.g. 'id,signature'). Omit for all fields. Skipping 'source' avoids the byte-offset disk read."}
			}
		}`),
	}, s.handleSymbol)

	// 3. symbols (batch)
	s.addTool(&mcp.Tool{
		Name:        "symbols",
		Description: "**Use instead of repeated `symbol` calls** when you have several IDs. Batch fetches up to 100 symbols in a single SQL round trip + per-symbol byte-offset reads. Returns `[{id, source, signature, file_path, start_line}, ...]` in the same order as the input `ids`. Missing IDs surface as `{id, error: \"not found\"}` rather than failing the whole batch.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["ids"],"properties":{
				"ids":{"type":"array","items":{"type":"string"},"description":"Array of stable symbol IDs."},
				"project":{"type":"string"}
			}
		}`),
	}, s.handleSymbols)

	// 4. context
	s.addTool(&mcp.Tool{
		Name:        "context",
		Description: "**Use before editing a function** to read it together with everything it directly imports/calls — one shot, ~90% token reduction vs reading files. Returns `{symbol: {source, ...}, imports: [{source, ...}]}`. Prefer this over `symbol` whenever you need to understand how a function works in context, not just see its source.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"Symbol ID to fetch with its imports."},
				"project":{"type":"string"}
			}
		}`),
	}, s.handleContext)

	// 5. search
	s.addTool(&mcp.Tool{
		Name:        "search",
		Description: "**Use before `Grep`/`Read`** when looking for code by name or content. Always start here when you don't know the exact symbol ID. Returns signature + a 5-line snippet for each result — often enough to answer without a follow-up call. Uses FTS5 BM25 ranking. Examples: 'processOrder' for a function, 'auth*' for prefix, '\"token validation\"' for a phrase. Filter by `kind=Function` / `language=Go` / `corpus=config|docs` to narrow. Use `context` on the result ID only if you need full source + dependencies.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["query"],"properties":{
				"query":{"type":"string","description":"FTS5 search query. Supports: prefix (auth*), phrase (\"login flow\"), AND/OR."},
				"project":{"type":"string"},
				"kind":{"type":"string","description":"Filter by symbol kind: Function|Method|Class|Interface|Enum|Type|Variable|Module|Setting|Section|Document|Resource|DataSource|Output|Local|Provider|Block"},
				"language":{"type":"string","description":"Filter by language: Go|Python|TypeScript|HCL|YAML|Markdown|etc"},
				"corpus":{"type":"string","enum":["","code","config","docs"],"description":"FTS5 corpus to search. Default (omitted or '') is 'code' — source-code identifiers (Function/Method/Class/etc). 'config' restricts to YAML/JSON/HCL/TOML Settings/Resources/Outputs; 'docs' to Markdown sections + fetched Documents. Use a specific corpus to avoid BM25 dilution from unrelated symbol kinds. (The legacy 'all' value was removed in v0.5; older callers passing 'all' are soft-redirected to 'code' with a deprecation log line.)"},
				"limit":{"type":"integer","description":"Max results (default 20)"},
				"fields":{"type":"string","description":"Comma-separated fields to include in each result, e.g. 'id,name,file_path'. Omit for all fields. Use to reduce token usage when you only need IDs or signatures."},
				"min_confidence":{"type":"number","description":"Minimum extraction_confidence (0.0-1.0). Default is query-aware (#247 #5): exact-identifier queries (single token, no wildcards/spaces/quotes) default to 0.0; phrase / wildcard / multi-word queries default to 0.71. Rationale: 0.71 filters bottom-floor doc-section symbols that can BM25-match wide queries; an exact identifier query can't legitimately match doc-section noise so the floor isn't needed. Set explicitly to override either default. Inclusive: a symbol scored at or above the threshold IS returned."}
			}
		}`),
	}, s.handleSearch)

	// 6. query
	s.addTool(&mcp.Tool{
		Name:        "query",
		Description: "**Use when you need structural relationships, not text matches** — pinchQL graph queries over the symbol graph. pinchQL is a pragmatic Cypher-shaped subset: `MATCH`, `WHERE`, `RETURN`, `LIMIT`, single-hop joins (`-[:CALLS]->`), and bounded variable-length BFS (`-[:CALLS*1..3]->`). Examples: callers `MATCH (a)-[:CALLS]->(b) WHERE b.name=\"Open\" RETURN a.name`; classes in a file `MATCH (n:Class) WHERE n.file_path CONTAINS \"server\" RETURN n.name`; multi-hop `MATCH (a)-[:CALLS*1..3]->(b) WHERE a.name=\"main\" RETURN b.name`. The legacy `cypher` parameter name is still accepted as a soft alias for one release. Prefer `search` for name/text lookups, `trace` for fixed-shape callgraph BFS — both are cheaper.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"pinchql":{"type":"string","description":"pinchQL query. Example: MATCH (f:Function)-[:CALLS]->(g) WHERE f.name='main' RETURN g.name LIMIT 20. Alias: cypher (deprecated, kept for one release)."},
				"cypher":{"type":"string","description":"Deprecated alias for pinchql; kept for one release. Pass either, not both."},
				"project":{"type":"string"},
				"max_rows":{"type":"integer","description":"Max rows (default 200, max 10000)"},
				"min_confidence":{"type":"number","description":"Minimum extraction_confidence (0.0-1.0). Default 0.0 (no filter). Filters rows whose query selects an extraction_confidence column; rows from queries that don't return confidence are unaffected."}
			}
		}`),
	}, s.handleQuery)

	// 7. trace
	s.addTool(&mcp.Tool{
		Name:        "trace",
		Description: "**Use before changing behaviour** that other code depends on, to find callers (inbound) or what it calls (outbound). Risk labels: CRITICAL=direct callers, HIGH=2 hops, MEDIUM=3 hops. Use `search` first to confirm the exact function name; ambiguous names fall back to the first match (use `changes` if you have an exact symbol ID instead). Default traversal follows CALLS-family edges; pass `kinds=READS,WRITES` to trace data-flow edges instead (or `kinds=CALLS,READS` to mix).",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["name"],"properties":{
				"name":{"type":"string","description":"Function name to trace (short name, e.g. 'ProcessOrder')"},
				"project":{"type":"string"},
				"direction":{"type":"string","enum":["outbound","inbound","both"],"description":"outbound=what it calls, inbound=what calls it. Default: both"},
				"depth":{"type":"integer","description":"BFS depth 1-5 (default 3)"},
				"risk":{"type":"boolean","description":"Add CRITICAL/HIGH/MEDIUM/LOW risk labels (default true)"},
				"min_confidence":{"type":"number","description":"Minimum extraction_confidence (0.0-1.0). Default 0.0 (no filter). Hops whose target symbol scores below the threshold are excluded from the result."},
				"kinds":{"type":"string","description":"Comma-separated list of edge kinds to traverse (e.g. 'CALLS' or 'READS,WRITES'). Default: CALLS-family (CALLS,HTTP_CALLS,ASYNC_CALLS) — covers the typical 'who calls this' use case. Pass READS / WRITES (Go vars only, see #264/#265) to follow data-flow edges. Whitespace and case-insensitive."}
			}
		}`),
	}, s.handleTrace)

	// 8. changes
	s.addTool(&mcp.Tool{
		Name:        "changes",
		Description: "**Use before final response after code edits** to surface the blast radius. Maps `git diff` to affected symbols, BFS-traces impact, returns `changed_symbols` + impacted callers tagged CRITICAL/HIGH/MEDIUM/LOW + summary counts + `tests_to_run` (test functions that exercise the changed symbols, ranked by overlap descending — re-run the top entries before pushing). Scopes: `unstaged` (default) / `staged` / `all` (includes untracked) / a branch name.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string"},
				"scope":{"type":"string","enum":["unstaged","staged","all"],"description":"Which diff to analyse (default: unstaged)"},
				"depth":{"type":"integer","description":"Blast radius BFS depth 1-5 (default 3)"}
			}
		}`),
	}, s.handleChanges)

	// 9. architecture
	s.addTool(&mcp.Tool{
		Name:        "architecture",
		Description: "**Call once at the start of unfamiliar work** to orient. Returns language breakdown, entry points, hotspot functions (most-called = highest change risk), and graph statistics. Hotspots default to production code only (test helpers are filtered); pass include_tests=true to surface them too. Much cheaper than reading files to understand the structure.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string"},
				"include_tests":{"type":"boolean","description":"If true, include hotspots from test files (*_test.go, *.spec.ts, etc.). Default false — test helpers like newTestServer dominate raw call counts but aren't useful for orientation."}
			}
		}`),
	}, s.handleArchitecture)

	// 10. schema
	s.addTool(&mcp.Tool{
		Name:        "schema",
		Description: "**Use before writing a `query`** to see what node/edge kinds exist in this project. Returns node-kind counts (Function, Class, Method, …), edge-kind counts (CALLS, IMPORTS, …), and totals.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string"}
			}
		}`),
	}, s.handleSchema)

	// 11. list
	s.addTool(&mcp.Tool{
		Name:        "list",
		Description: "**Use to confirm which projects are indexed** before scoping a query with `project=`. Returns `[{name, path, files, symbols, edges, indexed_at}, ...]` for active projects. Paginated: defaults to 50 entries per call (limit/offset), with the next page surfaced in `_meta.next_steps` when more remain. Defaults filter out projects whose on-disk path no longer exists or whose last index is older than `active_within_days` (14 by default). Pass `active=false`/`include_dead=true` to widen the filter, `limit=0` for the legacy unbounded dump, `prune_dead=true` to physically remove dead-on-disk projects from the store.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"active":{"type":"boolean","description":"Filter to projects indexed within active_within_days. Default true."},
			"active_within_days":{"type":"integer","description":"Activity window for active=true. Default 14."},
			"include_dead":{"type":"boolean","description":"Include projects whose on-disk path no longer exists. Default false."},
			"prune_dead":{"type":"boolean","description":"Permanently delete projects whose on-disk path no longer exists. Default false. Pruned ids returned in the pruned field. Set include_dead=true instead when you want to *see* dead rows, not delete them."},
			"limit":{"type":"integer","description":"Max rows returned per page. Default 50. Pass 0 for legacy unbounded behaviour."},
			"offset":{"type":"integer","description":"Skip the first N rows (default 0). Use the value from _meta.next_steps to walk pages."}
		}}`),
	}, s.handleList)

	// 12. adr
	s.addTool(&mcp.Tool{
		Name:        "adr",
		Description: "**Use to record decisions/conventions/gotchas** that should survive across sessions. Persistent project knowledge store. Actions: `set` (store), `get` (retrieve), `list` (all entries), `delete`. Examples: `adr set PURPOSE 'payment processing service'`; `adr set STACK 'Go+SQLite+Redis'`; `adr list` to recall everything stored. Call `adr list` early in unfamiliar work — prior agents' notes often save a `search` chain.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["action"],"properties":{
				"action":{"type":"string","enum":["get","set","list","delete"]},
				"project":{"type":"string"},
				"key":{"type":"string","description":"ADR key (e.g. 'PURPOSE', 'STACK', 'PATTERNS')"},
				"value":{"type":"string","description":"ADR value (required for action=set)"}
			}
		}`),
	}, s.handleADR)

	// 13. health
	s.addTool(&mcp.Tool{
		Name:        "health",
		Description: "**Use to verify extraction quality before trusting graph results**, or to detect a stale index. Returns schema version, index staleness, and per-language coverage with parser identity (AST vs Regex) and avg/p10/p50 confidence per (language, kind). A low p10 on a corpus you care about means `search` results in that area need a higher `min_confidence` to be reliable.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string","description":"Project to report on. Defaults to session project."}
			}
		}`),
	}, s.handleHealth)

	// 14. stats
	s.addTool(&mcp.Tool{
		Name:        "stats",
		Description: "**Use to track context-budget savings** for the current session and all-time. Returns tokens used, tokens saved (vs reading whole files), cost avoided, call count, plus per-project index size (files, symbols, edges). Useful as a sanity check that pincher tools are being preferred over `Read`/`Grep` — if `tokens_saved` is 0 after a chunk of work, the agent is probably bypassing the index.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string","description":"Project to include in index size breakdown. Defaults to session project."}
			}
		}`),
	}, s.handleStats)

	// 15. fetch
	s.addTool(&mcp.Tool{
		Name:        "fetch",
		Description: "**Use to pull external reference material into the project knowledge base** — API docs, library READMEs, specs, RFCs. Fetches a URL, extracts its text, stores it as a searchable `Document` symbol. After fetching, use `search kind:Document` to find it, or `symbol` with the returned ID to retrieve the full text. The Document kind lives in the `docs` corpus, so `corpus=docs` searches surface it alongside Markdown sections.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["url"],"properties":{
				"url":{"type":"string","description":"HTTP or HTTPS URL to fetch"},
				"project":{"type":"string","description":"Project to attach the document to. Defaults to session project."},
				"title":{"type":"string","description":"Override the page title used as the document name."}
			}
		}`),
	}, s.handleFetch)

	// 16. guide
	s.addTool(&mcp.Tool{
		Name:        "guide",
		Description: "**Call first when you don't know which tool to use.** Takes a free-form task description (\"fix login retry bug\", \"refactor the auth middleware\", \"understand how indexing works\") and returns 2-3 recommended pincher tool calls with reasoning. A starter tool — eliminates the decision friction of choosing between search/context/trace/changes from scratch.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["task"],"properties":{
				"task":{"type":"string","description":"Free-form description of what you're trying to do (e.g. 'fix the login timeout bug', 'add caching to the API gateway', 'understand how the indexer handles symlinks')."},
				"project":{"type":"string","description":"Project name or ID. Defaults to session project."}
			}
		}`),
	}, s.handleGuide)

	// 17. neighborhood
	s.addTool(&mcp.Tool{
		Name:        "neighborhood",
		Description: "**Use for in-file refactor planning** — given a seed symbol ID, returns every symbol in the same file (signatures + line ranges) ordered by source position. One round-trip vs N `symbol` calls or one whole-file `Read`. Paginated: defaults to 50 neighbors per call (limit/offset), with the next page surfaced in `_meta.next_steps` when the file has more. Default response excludes `source`; pass `include_source=true` to also fetch each neighbor's body.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"Stable symbol ID of the seed. The neighborhood is every symbol that shares its file."},
				"project":{"type":"string","description":"Project name or ID. Defaults to session project."},
				"include_source":{"type":"boolean","description":"If true, also fetch each neighbor's source body via the byte-offset path. Default false (signatures only — much cheaper)."},
				"include_self":{"type":"boolean","description":"If true, include the seed symbol itself in the neighbors list. Default false (caller already has it)."},
				"limit":{"type":"integer","description":"Maximum neighbors to return (default 50). Files with more symbols paginate via _meta.next_steps."},
				"offset":{"type":"integer","description":"Skip the first N neighbors (default 0). Use the value from _meta.next_steps to walk the file."}
			}
		}`),
	}, s.handleNeighborhood)

	// 18. init
	s.addTool(&mcp.Tool{
		Name:        "init",
		Description: "**Seed an editor's pincher usage policy file** without dropping into a separate shell. Same surface as `pincher init` CLI but defaults to dry-run for safety; pass `write=true` to actually mutate files. Targets: claude / cursor / cursor-legacy / windsurf / aider / detect / all. The continue target is rejected (always-global, escapes project scope from an MCP context). Returns per-target {target, path, action, diff_preview, bytes_in, bytes_out}.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"target":{"type":"string","description":"Editor target: claude|cursor|cursor-legacy|windsurf|aider|detect|all. Default: detect."},
				"write":{"type":"boolean","description":"If true, mutate target files. Default false (dry-run)."},
				"project_path":{"type":"string","description":"Project root override. Defaults to the session project root."}
			}
		}`),
	}, s.handleInit)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool handlers
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) handleIndex(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	path := str(args, "path")
	if path == "" {
		path = s.sessionRoot
	}
	if path == "" {
		return errResult("path is required (no session root detected)"), nil
	}
	force := boolArg(args, "force")

	result, err := s.indexer.Index(ctx, path, force)
	if err != nil {
		return errResult(fmt.Sprintf("index error: %v", err)), nil
	}

	data := map[string]any{
		"project":     result.Project,
		"path":        result.Path,
		"files":       result.Files,
		"symbols":     result.Symbols,
		"edges":       result.Edges,
		"skipped":     result.Skipped,
		"blocked":     result.Blocked,
		"deleted":     result.Deleted, // #326: files removed from disk since last run, GC'd this index
		"duration_ms": result.DurationMS,
	}
	// When the index produces zero symbols, surface *why* in _meta so the
	// agent doesn't guess "is it broken?" — the answer is usually obvious
	// from the counts (no source files vs all blocked vs all unchanged).
	// Trustworthy + explainable: the user gets a clear diagnostic instead
	// of a silent zero. Skipped on healthy non-zero runs.
	if result.Symbols == 0 {
		switch {
		case result.Files == 0 && result.Blocked == 0 && result.Skipped == 0:
			data["_meta"] = map[string]any{
				"diagnosis": "no indexable source files found at this path",
				"hint":      "verify the path is a project root (contains code in a recognised language) or check `pincher health` for indexing failures",
			}
		case result.Files == 0 && result.Blocked > 0:
			data["_meta"] = map[string]any{
				"diagnosis": fmt.Sprintf("all %d files were blocked by ast.ShouldSkip (lockfiles, minified bundles, source maps)", result.Blocked),
				"hint":      "expected for vendor-only or build-artifact-only directories; index a parent directory if your sources are nested elsewhere",
			}
		case result.Files == 0 && result.Skipped > 0 && !force:
			data["_meta"] = map[string]any{
				"diagnosis": fmt.Sprintf("incremental index — all %d files unchanged since last run", result.Skipped),
				"hint":      "this is the expected fast path. Pass `force=true` if you suspect index corruption.",
			}
		default:
			data["_meta"] = map[string]any{
				"diagnosis": "files were processed but no symbols extracted",
				"hint":      "language detection may be missing extension support; check `pincher health` per-language coverage",
			}
		}
	}
	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

func (s *Server) handleSymbol(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	id := str(args, "id")
	if id == "" {
		return errResult("id is required"), nil
	}
	projectArg := str(args, "project")
	fieldsArg := str(args, "fields")

	// Resolve the requested project up front so the symbol lookup can be
	// scoped (#2). Without scoping, two indexed projects with a colliding
	// ID like `cmd/main.go::main.main#Function` would shadow each other,
	// and a request authenticated for project A could return project B's
	// row. When `project` is omitted, fall back to the unscoped lookup
	// so no caller breaks — that path remains the documented behaviour
	// for callers that hold an ID but don't track which project owns it.
	var resolvedProjectID string
	if projectArg != "" {
		if pid, err := s.resolveProjectID(projectArg); err == nil {
			resolvedProjectID = pid
		}
	}

	var sym *db.Symbol
	var err error
	if resolvedProjectID != "" {
		sym, err = s.store.GetSymbolScoped(resolvedProjectID, id)
	} else {
		sym, err = s.store.GetSymbol(id)
	}
	if err != nil {
		return errResult(fmt.Sprintf("db error: %v", err)), nil
	}
	if sym == nil {
		// Stale ID? Check symbol_moves for a redirect (handles file renames/moves).
		if newID, ok := s.store.ResolveStaleID(s.sessionID, id); ok {
			if resolvedProjectID != "" {
				sym, err = s.store.GetSymbolScoped(resolvedProjectID, newID)
			} else {
				sym, err = s.store.GetSymbol(newID)
			}
			if err != nil {
				return errResult(fmt.Sprintf("db error resolving stale id: %v", err)), nil
			}
		}
	}
	if sym == nil {
		return errResult(fmt.Sprintf("symbol %q not found", id)), nil
	}

	// projectID drives byte-offset root resolution. When the caller
	// passed an explicit project, prefer it; otherwise use the symbol's
	// own project_id (already verified by the scoped lookup above when
	// applicable).
	projectID := sym.ProjectID
	if resolvedProjectID != "" {
		projectID = resolvedProjectID
	}
	root, err := s.resolveProjectRoot(projectID)
	if err != nil {
		root = s.sessionRoot
	}

	// Build field allow-set for projection (nil = all fields). Mirrors the
	// pattern in handleSearch so callers can ask for {id,signature} only and
	// skip the byte-offset disk read entirely on bulk lookups.
	var fieldSet map[string]bool
	if fieldsArg != "" {
		fieldSet = make(map[string]bool)
		for _, f := range strings.Split(fieldsArg, ",") {
			fieldSet[strings.TrimSpace(f)] = true
		}
	}
	includeSource := fieldSet == nil || fieldSet["source"]

	// O(1) byte-offset retrieval — the pincherMCP core innovation. Skip the
	// disk read when the projection excludes source (Document fallback also
	// pulls from sym.Docstring, which is already in memory).
	source := ""
	if includeSource {
		if root != "" {
			source, _ = index.ReadSymbolSource(root, *sym)
		}
		// Document symbols (fetched URLs) store their content in Docstring —
		// no local file to seek.
		if source == "" && sym.Kind == "Document" {
			source = sym.Docstring
		}
	}

	// Estimate token savings vs. reading the whole file.
	// Baseline: agent would read the entire file to find this symbol.
	fileSizeBytes := avgFileSize // conservative fallback
	if root != "" {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(sym.FilePath))); err == nil {
			fileSizeBytes = int(fi.Size())
		}
	}
	symbolBytes := sym.EndByte - sym.StartByte
	tokensSaved := max(0, fileSizeBytes-symbolBytes) / charsPerToken

	allFields := map[string]any{
		"id":                    sym.ID,
		"name":                  sym.Name,
		"qualified_name":        sym.QualifiedName,
		"kind":                  sym.Kind,
		"language":              sym.Language,
		"file_path":             sym.FilePath,
		"start_line":            sym.StartLine,
		"end_line":              sym.EndLine,
		"start_byte":            sym.StartByte,
		"end_byte":              sym.EndByte,
		"signature":             sym.Signature,
		"return_type":           sym.ReturnType,
		"docstring":             sym.Docstring,
		"complexity":            sym.Complexity,
		"is_exported":           sym.IsExported,
		"extraction_confidence": sym.ExtractionConfidence,
		"source":                source,
	}

	var data map[string]any
	if fieldSet == nil {
		data = allFields
	} else {
		data = make(map[string]any, len(fieldSet))
		for f := range fieldSet {
			data[f] = allFields[f]
		}
	}
	// #317: warn when the file on disk has changed since indexing —
	// byte offsets we just used point at content that no longer
	// matches the indexed symbol. Only emitted when source was
	// actually requested (offset-driven reads are the only path
	// where a stale offset produces visible wrongness).
	if includeSource && root != "" {
		s.attachStalenessWarning(data, projectID, sym, root)
	}
	return s.jsonResultWithMeta(data, start, tool, args, tokensSaved), nil
}

// attachStalenessWarning compares the on-disk file's xxh3 hash to
// the hash captured at index time. On mismatch it adds a
// _meta.warnings line and a re-index next_step so the agent
// knows the source body may not match the symbol (#317).
func (s *Server) attachStalenessWarning(data map[string]any, projectID string, sym *db.Symbol, root string) {
	if sym == nil {
		return
	}
	stored := s.store.GetFileHash(projectID, sym.FilePath)
	if stored == "" {
		// File hash never recorded — likely a Document or pre-#236 row.
		// Nothing to compare against.
		return
	}
	live, ok := fileHashOnDisk(filepath.Join(root, filepath.FromSlash(sym.FilePath)))
	if !ok || live == stored {
		return
	}
	meta, _ := data["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	warnings, _ := meta["warnings"].([]string)
	warnings = append(warnings,
		fmt.Sprintf("file %q modified since last index — source bytes may not match the symbol; re-index to refresh", sym.FilePath))
	meta["warnings"] = warnings
	steps, _ := meta["next_steps"].([]map[string]string)
	steps = append(steps, map[string]string{
		"tool": "index",
		"args": nextStepArgs(map[string]any{"path": root, "force": true}),
		"why":  "file changed since last index — re-index so byte offsets match the current source",
	})
	meta["next_steps"] = steps
	data["_meta"] = meta
}

// fileHashOnDisk returns the xxh3 hex hash of the file at absPath
// using the same format the indexer records (`fmt.Sprintf("%x", xxh3.Hash(content))`).
// Returns (_, false) when the file can't be read; caller treats
// missing as "no warning to emit" rather than a hard failure (#317).
func fileHashOnDisk(absPath string) (string, bool) {
	b, err := os.ReadFile(absPath)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%x", xxh3.Hash(b)), true
}

// maxBatchSymbols caps the number of IDs accepted by the symbols batch tool
// to prevent unbounded DB query loops and excessive memory usage.
const maxBatchSymbols = 100

func (s *Server) handleSymbols(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	ids := strSlice(args, "ids")
	if len(ids) == 0 {
		return errResult("ids array is required"), nil
	}
	if len(ids) > maxBatchSymbols {
		return errResult(fmt.Sprintf("too many ids: max %d per call, got %d", maxBatchSymbols, len(ids))), nil
	}

	projectArg := str(args, "project")
	root := s.sessionRoot
	var resolvedProjectID string
	if projectArg != "" {
		if pid, err := s.resolveProjectID(projectArg); err == nil {
			resolvedProjectID = pid
			if r, err := s.resolveProjectRoot(pid); err == nil {
				root = r
			}
		}
	}

	// One round trip to SQLite for the whole batch. Was N round trips
	// (loop over GetSymbol) — for a 100-ID batch that's the dominant
	// component of handler latency on a cached corpus. Project-scoped
	// (#2) when the caller passed `project`, so a colliding ID can't
	// surface a row from a different repo.
	bySymID, err := s.store.GetSymbolsByIDs(resolvedProjectID, ids)
	if err != nil {
		return errResult(fmt.Sprintf("db error: %v", err)), nil
	}

	results := make([]map[string]any, 0, len(ids))
	// Collect file paths for honest token-savings accounting (#220).
	// Pre-dedup at savedVsFileSizes time, but we still gather them here
	// so the helper sees the actual set of files an agent would otherwise
	// have read. A 30-ID batch hitting 12 unique files credits 12 file
	// sizes, not 30 × per-file-estimate as the prior savedVsFullRead path
	// did.
	filePaths := make([]string, 0, len(ids))
	for _, id := range ids {
		sym, ok := bySymID[id]
		if !ok || sym == nil {
			results = append(results, map[string]any{"id": id, "error": "not found"})
			continue
		}
		source := ""
		if root != "" {
			source, _ = index.ReadSymbolSource(root, *sym)
		}
		// Document symbols (fetched URLs) store their content in Docstring —
		// no local file to seek. Mirrors the fallback in handleSymbol so a
		// batch lookup of mixed source-file + Document symbols returns the
		// same shape as N single-symbol calls.
		if source == "" && sym.Kind == "Document" {
			source = sym.Docstring
		}
		// #336: project the same field set as handleSymbol so a one-ID
		// `symbols` batch returns the same shape as a single `symbol` call.
		// Without parity, callers had to know which tool to use to access
		// fields like qualified_name / extraction_confidence.
		entry := map[string]any{
			"id":                    sym.ID,
			"name":                  sym.Name,
			"qualified_name":        sym.QualifiedName,
			"kind":                  sym.Kind,
			"language":              sym.Language,
			"file_path":             sym.FilePath,
			"start_line":            sym.StartLine,
			"end_line":              sym.EndLine,
			"start_byte":            sym.StartByte,
			"end_byte":              sym.EndByte,
			"signature":             sym.Signature,
			"return_type":           sym.ReturnType,
			"docstring":             sym.Docstring,
			"complexity":            sym.Complexity,
			"is_exported":           sym.IsExported,
			"extraction_confidence": sym.ExtractionConfidence,
			"source":                source,
		}
		// #317 staleness warning, per-entry. Each batch result carries its
		// own _meta when the file's on-disk hash diverges from the indexed
		// one. Mirrors the per-symbol path so a mixed batch (some stale,
		// some fresh) reports accurately at the entry level.
		if root != "" && sym.Kind != "Document" {
			pidForHash := resolvedProjectID
			if pidForHash == "" {
				pidForHash = sym.ProjectID
			}
			s.attachStalenessWarning(entry, pidForHash, sym, root)
		}
		results = append(results, entry)
		// Document symbols have no on-disk file; skip them in the
		// savings baseline so we don't os.Stat a non-existent path.
		if sym.Kind != "Document" && sym.FilePath != "" {
			filePaths = append(filePaths, sym.FilePath)
		}
	}

	responseJSON, _ := json.Marshal(results)
	data := map[string]any{
		"symbols": results,
		"count":   len(results),
	}
	return s.jsonResultWithMeta(data, start, tool, args, savedVsFileSizes(root, filePaths, responseJSON)), nil
}

func (s *Server) handleContext(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	id := str(args, "id")
	if id == "" {
		return errResult("id is required"), nil
	}

	sym, err := s.store.GetSymbol(id)
	if err != nil || sym == nil {
		return errResult(fmt.Sprintf("symbol %q not found", id)), nil
	}

	root, _ := s.resolveProjectRoot(sym.ProjectID)
	source, _ := index.ReadSymbolSource(root, *sym)

	// Find IMPORTS edges from this symbol
	importEdges, _ := s.store.EdgesFrom(sym.ID, []string{"IMPORTS"})
	// #332: zero-len init so JSON shape is stable when the symbol has
	// no imports (same fix as #328/#330).
	imports := []map[string]any{}
	var importPaths []string
	for _, e := range importEdges {
		imp, err := s.store.GetSymbol(e.ToID)
		if err != nil || imp == nil {
			continue
		}
		impSource, _ := index.ReadSymbolSource(root, *imp)
		imports = append(imports, map[string]any{
			"id":        imp.ID,
			"name":      imp.Name,
			"kind":      imp.Kind,
			"file_path": imp.FilePath,
			"source":    impSource,
		})
		importPaths = append(importPaths, imp.FilePath)
	}

	// Savings = would have read the full source file + every import file; gave only symbols.
	// Include the primary symbol's file in the baseline.
	allPaths := append([]string{sym.FilePath}, importPaths...)
	data := map[string]any{
		"symbol":  map[string]any{"id": sym.ID, "name": sym.Name, "kind": sym.Kind, "source": source},
		"imports": imports,
	}
	// Context returns the symbol + its callees (the outbound direction). The
	// natural next move is the inbound direction — find the symbol's callers
	// before changing it. For non-callable kinds (Setting, Section), no
	// suggestion is offered: there's nothing further to chase.
	if next := suggestContextNextSteps(*sym); len(next) > 0 {
		data["_meta"] = map[string]any{"next_steps": next}
	}
	// #317: warn if the seed file changed since indexing — same
	// signal as in handleSymbol. Only the seed; checking every
	// import would multiply the cost without much value.
	if root != "" {
		s.attachStalenessWarning(data, sym.ProjectID, sym, root)
	}
	responseJSON, _ := json.Marshal(data)
	return s.jsonResultWithMeta(data, start, tool, args, savedVsFileSizes(root, allPaths, responseJSON)), nil
}

func suggestContextNextSteps(sym db.Symbol) []map[string]string {
	switch sym.Kind {
	case "Function", "Method", "Class", "Interface", "Type", "Struct", "Trait":
		return []map[string]string{
			{"tool": "trace", "args": fmt.Sprintf(`{"name":"%s","direction":"inbound"}`, sym.Name),
				"why": "find callers — context already showed callees, inbound is the missing half before a behaviour change"},
		}
	default:
		return nil
	}
}

func (s *Server) handleSearch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	// #344: TrimSpace before validating so whitespace-only queries
	// (e.g. " ", "\t", "\n") don't leak through to FTS5 as a low-level
	// SQLite "syntax error near \"\"" — return a friendly input-error
	// instead.
	query := strings.TrimSpace(str(args, "query"))
	if query == "" {
		return errResult("query is required (and must contain non-whitespace characters)"), nil
	}
	projectArg := str(args, "project")
	kind := str(args, "kind")
	language := str(args, "language")
	corpus := str(args, "corpus")
	// "all" is deprecated and being removed (#106). Soft-redirect to "code"
	// — that's what new callers should use, and it preserves the dominant
	// real-world use case (search for an identifier). A hard error would
	// break existing scripts; a soft redirect with a warning surfaces the
	// deprecation without breakage. The schema-level drop is tracked at #106.
	if corpus == "all" {
		slog.Warn("pincher.search.corpus_all_deprecated",
			"action", "redirected to 'code'",
			"recommendation", "call search once per corpus (code/config/docs); see #106")
		corpus = ""
	}
	limit := intArg(args, "limit", 20)
	fieldsArg := str(args, "fields")
	// #34 Phase 4 + #112 calibration baseline: 0.71 filters the
	// bottom-floor symbols (README/CHANGELOG/CONTRIBUTING H1 sections that
	// land at exactly 0.70 on real corpora; ~3.6% of symbols on typical
	// mixed corpora). Without this, doc-quality noise dominated wide
	// keyword searches.
	//
	// #247 query-aware adjustment: when the query looks like an exact
	// identifier (single token, no spaces / wildcards / quotes), the
	// doc-quality floor is irrelevant — there's no documentation symbol
	// named e.g. `registerTools`. Use 0.0 in that case so an exact-name
	// search never silently zero-results on a real symbol. Phrase /
	// wildcard / multi-word queries keep the 0.71 baseline because they
	// can match doc-section titles and need the floor.
	//
	// Callers pass min_confidence explicitly to override either default.
	minConfidence := floatArg(args, "min_confidence", defaultMinConfidenceFor(query))

	// project=* searches all indexed projects — no project filter applied.
	var projectID string
	if projectArg != "*" {
		var err error
		projectID, err = s.resolveProjectID(projectArg)
		if err != nil {
			return errResult(err.Error()), nil
		}
	}

	// Fetch extra rows when filtering so the post-filter result count
	// approaches the requested limit. 4× headroom is enough for typical
	// corpora; a min_confidence=0.7 filter on a Settings-heavy corpus
	// might still under-deliver, but that's the right signal — the agent
	// asked for high-precision and the corpus didn't have it.
	fetchLimit := limit
	if minConfidence > 0 {
		fetchLimit = limit * 4
	}
	// #289: FTS5 treats `.` and `-` as syntactic; bare dotted identifiers
	// like `os.Stat` produce raw "fts5: syntax error" messages that don't
	// help the caller. Auto-quote tokens containing those characters so
	// the natural identifier query just works. Explicit FTS5 syntax
	// (already-quoted phrases, `auth*` prefix, `name:value` column-prefix,
	// boolean operators) is preserved.
	ftsQuery := sanitizeFTS5Query(query)
	results, err := s.store.SearchSymbolsByCorpus(projectID, ftsQuery, kind, language, corpus, fetchLimit)
	if err != nil {
		return errResult(fmt.Sprintf("search error: %v", err)), nil
	}

	// #113 corpus fallthrough: when the user did NOT pass an explicit corpus,
	// the default routes to `code`. Pure-config (Terraform, Ansible) and
	// pure-docs projects have zero symbols in the code corpus, so a default
	// search would always return 0 even when the data exists in `config`
	// or `docs`. Retry with the next-most-specific corpus, surfacing the
	// fallthrough chain in `_meta` so the agent can see what happened.
	//
	// Skipped when the user explicitly set corpus (any value, including
	// "code") — empty results are then a deliberate scope choice.
	fellthroughTo := ""
	if corpus == "" && len(results) == 0 {
		for _, fb := range []string{db.CorpusConfig, db.CorpusDocs} {
			fbResults, fbErr := s.store.SearchSymbolsByCorpus(projectID, ftsQuery, kind, language, fb, fetchLimit)
			if fbErr != nil {
				return errResult(fmt.Sprintf("search error (fallthrough %s): %v", fb, fbErr)), nil
			}
			if len(fbResults) > 0 {
				results = fbResults
				fellthroughTo = fb
				break
			}
		}
	}

	// rawPreConfidenceCount is the result count BEFORE the min_confidence
	// post-filter (#246). Used by the empty-result diagnosis verifier:
	// if the raw count was > 0 but post-filter is 0, min_confidence is
	// the verified cause — no need to re-run any query.
	rawPreConfidenceCount := len(results)

	// Apply the threshold AFTER fetch; FTS5 BM25 ordering is preserved.
	if minConfidence > 0 {
		filtered := results[:0]
		for _, r := range results {
			if r.Symbol.ExtractionConfidence >= minConfidence {
				filtered = append(filtered, r)
			}
		}
		results = filtered
		if len(results) > limit {
			results = results[:limit]
		}
	}

	// Resolve project root once for snippet reads.
	root, _ := s.resolveProjectRoot(projectID)

	// Build field allow-set for projection (nil = all fields).
	var fieldSet map[string]bool
	if fieldsArg != "" {
		fieldSet = make(map[string]bool)
		for _, f := range strings.Split(fieldsArg, ",") {
			fieldSet[strings.TrimSpace(f)] = true
		}
	}

	// snippetLines is the max lines of source included per result.
	// Callers can suppress snippets via fields= projection.
	const snippetLines = 5
	// snippetReadCap bounds the disk read used to compute the 5-line snippet.
	// Without it, a Setting/Section symbol whose byte range spans a whole
	// YAML mapping or Markdown heading block would cause the indexer to slurp
	// tens of KB just to slice 5 lines off the top. 2 KB is plenty for 5
	// lines of even densely-packed source (avg ~200 chars/line).
	const snippetReadCap = 2048

	allFields := map[string]any{}
	// #334: zero-len init so search returns "results":[] (not null) when zero hits.
	rows := []map[string]any{}
	for _, r := range results {
		allFields["id"] = r.Symbol.ID
		allFields["name"] = r.Symbol.Name
		allFields["qualified_name"] = r.Symbol.QualifiedName
		allFields["kind"] = r.Symbol.Kind
		allFields["language"] = r.Symbol.Language
		allFields["file_path"] = r.Symbol.FilePath
		allFields["start_line"] = r.Symbol.StartLine
		allFields["signature"] = r.Symbol.Signature
		allFields["score"] = r.Score
		allFields["extraction_confidence"] = r.Symbol.ExtractionConfidence

		// Add a short snippet so Claude can often skip a follow-up symbol/context call.
		// Suppress for variables/types where the signature IS the content.
		// Skip the disk read entirely when the caller's fields= projection excludes
		// snippet — otherwise we'd read kilobytes per result and discard them.
		includeSnippet := fieldSet == nil || fieldSet["snippet"]
		snippet := ""
		if includeSnippet && root != "" && r.Symbol.Kind != "Variable" && r.Symbol.Kind != "Type" {
			if src, err := index.ReadSymbolSourceCapped(root, r.Symbol, snippetReadCap); err == nil && src != "" {
				lines := strings.SplitN(src, "\n", snippetLines+1)
				if len(lines) > snippetLines {
					lines = lines[:snippetLines]
					lines = append(lines, "…")
				}
				snippet = strings.Join(lines, "\n")
			}
		}
		allFields["snippet"] = snippet

		if fieldSet == nil {
			row := make(map[string]any, len(allFields))
			for k, v := range allFields {
				row[k] = v
			}
			rows = append(rows, row)
		} else {
			row := make(map[string]any, len(fieldSet))
			for f := range fieldSet {
				row[f] = allFields[f]
			}
			rows = append(rows, row)
		}
	}

	// Token savings: each result came from a file the agent would have read in full.
	responseJSON, _ := json.Marshal(rows)
	var filePaths []string
	for _, r := range results {
		filePaths = append(filePaths, r.Symbol.FilePath)
	}
	tokensSaved := savedVsFileSizes(root, filePaths, responseJSON)

	// Histogram of result confidences for the response envelope.
	confs := make([]float64, 0, len(results))
	for _, r := range results {
		confs = append(confs, r.Symbol.ExtractionConfidence)
	}

	meta := map[string]any{
		"confidence_distribution": confidenceDistribution(confs),
	}
	if fellthroughTo != "" {
		meta["fellthrough_to"] = fellthroughTo
	}
	// Suggest the obvious next tool call per top result kind. Reduces
	// decision friction — agents see the next move spelled out instead of
	// having to choose between symbol/context/trace from scratch. The
	// suggestions are the workflow rules from CLAUDE.md, applied to the
	// concrete top-1 result.
	if len(results) > 0 {
		meta["next_steps"] = suggestNextStepsForResults(results)
		// #350: misleading-match detection. When the query is an exact
		// identifier and a kind filter is set, a non-empty result with no
		// exact-name match means BM25 surfaced a partial-token match
		// (e.g. handleIndex with kind=Function returned a test function
		// containing "Handle" + "Search" tokens — the real handleIndex
		// is a Method, excluded by the filter). Run the same relaxation
		// verifyEmptySearchCause uses for the empty case; if the kind-
		// relaxed query has an exact-name match, surface it so the agent
		// isn't fooled by the partial.
		if kind != "" && isExactIdentifierQuery(query) && !resultsContainExactName(results, query) {
			if relaxed, err := s.store.SearchSymbolsByCorpus(projectID, ftsQuery, "", language, corpus, 5); err == nil {
				for _, rr := range relaxed {
					if rr.Symbol.Name == query {
						meta["exact_match_in_other_kind"] = map[string]any{
							"kind":      rr.Symbol.Kind,
							"id":        rr.Symbol.ID,
							"file_path": rr.Symbol.FilePath,
							"hint":      fmt.Sprintf("exact name %q exists with kind=%q; current kind=%q filter is hiding it. Top result is a BM25 partial-token match, not a name match.", query, rr.Symbol.Kind, kind),
						}
						// Prepend a relax-the-kind next_step.
						steps, _ := meta["next_steps"].([]map[string]string)
						steps = append([]map[string]string{{
							"tool": "search",
							"args": nextStepArgs(map[string]any{"query": query, "language": language, "corpus": corpus}),
							"why":  fmt.Sprintf("exact match for %q exists with kind=%q — drop the kind filter to surface it", query, rr.Symbol.Kind),
						}}, steps...)
						meta["next_steps"] = steps
						break
					}
				}
			}
		}
	} else {
		// Zero results, even after corpus fallthrough. Surface a
		// best-guess diagnosis + concrete recovery suggestions so the
		// agent doesn't have to guess from a bare `count: 0`. Mirrors
		// handleIndex's empty-state diagnosis (#147).
		//
		// #246: prefer a *verified* cause when available. The verifier
		// re-runs relaxed queries (drops kind / language / corpus one
		// at a time) and reports which filter, when removed, surfaces
		// results. Only falls back to the static diagnosis when no
		// single relaxation helps — that path covers spelling errors
		// and "wrong project" cases that no in-query tweak fixes.
		relax := func(q, k, lang, corp string) (int, error) {
			r, err := s.store.SearchSymbolsByCorpus(projectID, q, k, lang, corp, 1)
			if err != nil {
				return 0, err
			}
			return len(r), nil
		}
		if cause, steps, ok := verifyEmptySearchCause(query, kind, language, corpus, minConfidence, rawPreConfidenceCount, relax); ok {
			meta["diagnosis"] = cause
			meta["next_steps"] = steps
		} else {
			meta["diagnosis"] = diagnoseEmptySearch(query, kind, language, corpus, minConfidence)
			meta["next_steps"] = suggestEmptySearchNextSteps(query, kind, language, minConfidence)
		}
	}
	data := map[string]any{
		"results": rows,
		"count":   len(rows),
		"query":   query,
		"_meta":   meta,
	}
	return s.jsonResultWithMeta(data, start, tool, args, tokensSaved), nil
}

// emptySearchRelaxer probes how a search would behave with one or more
// filters relaxed. Returns (count, err) for the relaxed query against
// the same project. Injected from handleSearch so the verifier stays
// unit-testable; tests pass a deterministic fake to assert behavior
// without spinning up a real DB.
type emptySearchRelaxer func(query, kind, language, corpus string) (int, error)

// verifyEmptySearchCause returns a *verified* zero-result diagnosis
// when one filter is provably responsible — i.e. dropping that filter
// surfaces results. Falls through to the static `diagnoseEmptySearch`
// (ok=false return) when no single relaxation helps; that path covers
// spelling, wrong project, and genuinely-absent symbols.
//
// Order of probing (matches the existing diagnosis priority but each
// step is *verified* rather than *guessed* — #246):
//  1. min_confidence (no extra query needed; rawCount > 0 == verified)
//  2. kind (re-run without kind)
//  3. language (re-run without language)
//  4. corpus (re-run with default code corpus)
//  5. multiple filters together — try dropping the kind+language pair
//
// Each probe runs against the same project. Probe failures are silent;
// the caller falls back to the static diagnosis when ok=false. Cost:
// at most one FTS5 query per set filter, only on the zero-result path.
func verifyEmptySearchCause(
	query, kind, language, corpus string,
	minConfidence float64,
	rawPreConfidenceCount int,
	relax emptySearchRelaxer,
) (cause string, nextSteps []map[string]string, ok bool) {
	// Step 1 — min_confidence is verifiable without any extra query: if
	// the raw FTS5 result set was non-empty but the confidence post-
	// filter dropped everything, the threshold is provably the cause.
	if minConfidence > 0 && rawPreConfidenceCount > 0 {
		cause = fmt.Sprintf(
			"%d match(es) returned by FTS5 but every result scored below min_confidence ≥ %.2f — drop the threshold to surface them",
			rawPreConfidenceCount, minConfidence,
		)
		nextSteps = []map[string]string{
			{
				"tool": "search",
				"args": nextStepArgs(map[string]any{"query": query, "min_confidence": 0.0}),
				"why":  "verified: relaxing the confidence threshold surfaces the results",
			},
		}
		return cause, nextSteps, true
	}

	// Step 2-4 — single-filter relaxations. Try the most specific filter
	// first (kind narrows hardest), then language, then corpus.
	if kind != "" {
		if n, err := relax(query, "", language, corpus); err == nil && n > 0 {
			return fmt.Sprintf(
					"%d match(es) exist for %q but kind=%q excludes them — drop the kind filter",
					n, query, kind,
				), []map[string]string{{
					"tool": "search",
					"args": nextStepArgs(map[string]any{"query": query}),
					"why":  fmt.Sprintf("verified: dropping kind=%q surfaces %d match(es)", kind, n),
				}}, true
		}
	}
	if language != "" {
		if n, err := relax(query, kind, "", corpus); err == nil && n > 0 {
			return fmt.Sprintf(
					"%d match(es) exist for %q but language=%q excludes them — drop the language filter",
					n, query, language,
				), []map[string]string{{
					"tool": "search",
					"args": nextStepArgs(map[string]any{"query": query}),
					"why":  fmt.Sprintf("verified: dropping language=%q surfaces %d match(es)", language, n),
				}}, true
		}
	}
	if corpus != "" && corpus != "code" {
		if n, err := relax(query, kind, language, "code"); err == nil && n > 0 {
			return fmt.Sprintf(
					"%d match(es) exist for %q in corpus=code but corpus=%q excludes them — switch to the default corpus",
					n, query, corpus,
				), []map[string]string{{
					"tool": "search",
					"args": nextStepArgs(map[string]any{"query": query}),
					"why":  fmt.Sprintf("verified: corpus=code (the default) has %d match(es)", n),
				}}, true
		}
	}

	// Step 5 — pair-drop fallback. Two filters together can mask results
	// neither alone hides; try kind+language together when both are set.
	if kind != "" && language != "" {
		if n, err := relax(query, "", "", corpus); err == nil && n > 0 {
			return fmt.Sprintf(
					"%d match(es) exist for %q but kind=%q AND language=%q together exclude them — drop both filters",
					n, query, kind, language,
				), []map[string]string{{
					"tool": "search",
					"args": nextStepArgs(map[string]any{"query": query}),
					"why":  fmt.Sprintf("verified: dropping kind+language together surfaces %d match(es)", n),
				}}, true
		}
	}

	// No relaxation surfaced results — caller falls back to static
	// diagnosis. This covers spelling errors, wrong project, and
	// symbols that genuinely don't exist in the index.
	return "", nil, false
}

// sanitizeFTS5Query auto-quotes whitespace-separated tokens that
// contain characters FTS5 treats as syntactic (`.`, `-`). Without this,
// natural identifier queries like `os.Stat` or `my-component` raise a
// raw "fts5: syntax error" the caller can't recover from without
// learning FTS5 quoting (#289).
//
// The function is intentionally conservative — it only wraps a token
// when an alphanumeric character sits on both sides of the special
// char (`os.Stat`, `my-component`). That preserves:
//   - Explicit quoted phrases ("login flow") — early return on the
//     first `"` so anything quoted is passed through verbatim.
//   - Wildcards (`auth*`, `os.Stat*` becomes `"os.Stat"*`).
//   - Column-prefix syntax (`name:value`, `kind:Function` — the colon
//     is FTS5-legitimate, only `.` and `-` get wrapped).
//   - Boolean operators (AND, OR, NOT) — those are bare keywords with
//     no special chars, so they don't match the wrap predicate.
//   - Already-correct queries with no special chars (most identifier
//     searches).
func sanitizeFTS5Query(q string) string {
	if q == "" {
		return q
	}
	// If the user explicitly used FTS5 quoting, bail out — anything
	// inside quotes was their choice and we shouldn't second-guess it.
	if strings.Contains(q, `"`) {
		return q
	}
	tokens := strings.Fields(q)
	for i, tok := range tokens {
		tokens[i] = wrapTokenIfNeeded(tok)
	}
	return strings.Join(tokens, " ")
}

// wrapTokenIfNeeded returns tok wrapped in FTS5 phrase quotes if it
// contains a `.` or `-` between alphanumerics. Strips a trailing `*`
// before testing and re-adds it so prefix queries (`os.Stat*`) keep
// working. Returns tok unchanged otherwise.
func wrapTokenIfNeeded(tok string) string {
	suffix := ""
	core := tok
	if strings.HasSuffix(core, "*") {
		core = core[:len(core)-1]
		suffix = "*"
	}
	if !needsQuoting(core) {
		return tok
	}
	return `"` + core + `"` + suffix
}

func needsQuoting(s string) bool {
	if len(s) < 3 {
		return false
	}
	for i := 1; i < len(s)-1; i++ {
		// #289 added `.` and `-`; #356 adds `:` (FTS5 treats it as
		// column-prefix syntax: `colname:term`). When the colon sits
		// between alphanumerics in user input it's almost always a
		// path/port/key separator (e.g. `localhost:8080`, `mod:fn`,
		// YAML key paths), not an FTS5 column lookup.
		if s[i] == '.' || s[i] == '-' || s[i] == ':' {
			if isAlphanum(s[i-1]) && isAlphanum(s[i+1]) {
				return true
			}
		}
	}
	return false
}

func isAlphanum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// defaultMinConfidenceFor picks the right min_confidence default for a
// query that doesn't carry an explicit threshold (#247 #5).
//
// The 0.71 baseline filters bottom-floor noise (README/CHANGELOG H1
// sections at 0.70) on wide keyword searches — necessary because a
// doc-section title can BM25-match an unrelated identifier query. But
// for an exact identifier query like `registerTools`, no documentation
// symbol could share the name; the doc-quality floor is irrelevant
// and silently drops valid results.
//
// Heuristic: if the query is a single identifier-shaped token (one or
// more letters/digits/underscores, no spaces, no wildcards, no
// quotes), default to 0.0 — surface every match. Anything more
// complex (phrase, wildcard, multi-word) keeps 0.71.
//
// This is a default only; explicit min_confidence on the call wins.
func defaultMinConfidenceFor(query string) float64 {
	if isExactIdentifierQuery(query) {
		return 0.0
	}
	return 0.71
}

// resultsContainExactName reports whether any result in results has its
// Symbol.Name exactly equal to name. Used by the #350 misleading-match
// detector to decide whether a kind-relaxation hint is warranted.
func resultsContainExactName(results []db.SearchResult, name string) bool {
	for _, r := range results {
		if r.Symbol.Name == name {
			return true
		}
	}
	return false
}

// isExactIdentifierQuery reports whether the query looks like a single
// programming-language identifier (one or more letters/digits/underscores,
// no spaces / wildcards / quotes). The heuristic is reused by:
//   - defaultMinConfidenceFor (#247 #5) to skip the doc-quality floor
//   - the #350 misleading-match check to detect when a kind filter
//     excluded an exact-name match in another kind
func isExactIdentifierQuery(query string) bool {
	if query == "" {
		return false
	}
	if strings.ContainsAny(query, " \t\"*") {
		return false
	}
	for _, r := range query {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// diagnoseEmptySearch returns a one-line explanation of the most likely
// cause of zero results. Ordered most-specific first: min_confidence
// is checked before filter args, because a numeric threshold is the
// least-obvious filter to remember when debugging.
func diagnoseEmptySearch(query, kind, language, corpus string, minConfidence float64) string {
	switch {
	case minConfidence > 0:
		return fmt.Sprintf("no matches at min_confidence ≥ %.2f — bottom-floor symbols (lockfile keys, README headings) need min_confidence=0.0 to surface", minConfidence)
	case kind != "":
		return fmt.Sprintf("no matches with kind=%q — try without the kind filter to see all matching symbols", kind)
	case language != "":
		return fmt.Sprintf("no matches in language=%q — try without the language filter or check the project's actual language mix via `architecture`", language)
	case corpus != "" && corpus != "code":
		return fmt.Sprintf("no matches in corpus=%q — try corpus=code (the default for source identifiers) or omit corpus", corpus)
	case !strings.ContainsAny(query, "*\""):
		return fmt.Sprintf("no exact-term matches for %q — wildcards (`%s*`) or phrase queries (`\"%s\"`) often surface partial-name hits FTS5 BM25 misses", query, query, query)
	default:
		return "no matches across code/config/docs corpora — check spelling, or `list` to confirm the project is indexed and pick a different `project=`"
	}
}

// suggestEmptySearchNextSteps returns concrete tool calls the agent
// can run as recovery moves. Ordered: drop the most-specific filter
// first so agents converge on the right call quickly. Always includes
// a `list` suggestion as the universal fallback (catches "wrong
// project" mistakes that no in-query tweak fixes).
func suggestEmptySearchNextSteps(query, kind, language string, minConfidence float64) []map[string]string {
	steps := []map[string]string{}
	if minConfidence > 0 {
		steps = append(steps, map[string]string{
			"tool": "search",
			"args": nextStepArgs(map[string]any{"query": query, "min_confidence": 0.0}),
			"why":  "drop the confidence threshold to surface bottom-floor matches",
		})
	}
	if kind != "" {
		steps = append(steps, map[string]string{
			"tool": "search",
			"args": nextStepArgs(map[string]any{"query": query}),
			"why":  "retry without the kind filter to widen the result set",
		})
	}
	if language != "" {
		steps = append(steps, map[string]string{
			"tool": "search",
			"args": nextStepArgs(map[string]any{"query": query}),
			"why":  "retry without the language filter",
		})
	}
	if !strings.ContainsAny(query, "*\"") {
		steps = append(steps, map[string]string{
			"tool": "search",
			"args": nextStepArgs(map[string]any{"query": query + "*"}),
			"why":  "wildcard match catches partial-name hits BM25 ranking can miss",
		})
	}
	steps = append(steps, map[string]string{
		"tool": "list",
		"args": `{}`,
		"why":  "confirm the right project is indexed — wrong project = no matches no matter the query",
	})
	return steps
}

// nextStepArgs builds a JSON-encoded args string with proper escaping
// for embedded quotes/backslashes (#315). Falls back to "{}" if
// json.Marshal fails (it shouldn't for the shapes we pass — string,
// int, float, bool, nil — but the fallback keeps the tool call shape
// valid even on a programming error).
func nextStepArgs(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// suggestNextSteps returns 1-2 follow-up tool suggestions tailored to the
// top search result's kind. Mirrors the workflow advice in CLAUDE.md but
// concretised against the actual ID, so the agent doesn't have to translate
// "use context on a Function result" into "context(id=...)".
//
// `nameAmbiguous` says whether the top result's `Name` is shared with at
// least one other returned row. When true, the trace recommendation uses
// `qualified_name` instead of bare `name` so the agent doesn't follow the
// suggestion into the wrong symbol — `trace` resolves an ambiguous bare
// name to the first match silently. (#291)
func suggestNextSteps(top db.Symbol, nameAmbiguous bool) []map[string]string {
	id := top.ID
	traceName := top.Name
	traceWhy := "find callers if you're about to change behaviour other code depends on"
	if nameAmbiguous && top.QualifiedName != "" {
		traceName = top.QualifiedName
		traceWhy = "find callers; using qualified_name because bare name is shared with other returned results"
	}
	switch top.Kind {
	case "Function", "Method":
		return []map[string]string{
			{"tool": "context", "args": fmt.Sprintf(`{"id":"%s"}`, id),
				"why": "read the function plus everything it directly imports/calls (one shot, ~90% token reduction)"},
			{"tool": "trace", "args": fmt.Sprintf(`{"name":"%s"}`, traceName),
				"why": traceWhy},
		}
	case "Class", "Interface", "Type", "Enum":
		return []map[string]string{
			{"tool": "context", "args": fmt.Sprintf(`{"id":"%s"}`, id),
				"why": "read the type plus its imports"},
		}
	case "Setting", "Variable", "Resource", "Output", "Provider":
		return []map[string]string{
			{"tool": "symbol", "args": fmt.Sprintf(`{"id":"%s"}`, id),
				"why": "fetch the value's full context (signature already shown in this result)"},
		}
	case "Section", "Heading", "Document":
		return []map[string]string{
			{"tool": "symbol", "args": fmt.Sprintf(`{"id":"%s","fields":"source"}`, id),
				"why": "fetch the section body (no need for context — docs don't have callgraphs)"},
		}
	default:
		return []map[string]string{
			{"tool": "symbol", "args": fmt.Sprintf(`{"id":"%s"}`, id),
				"why": "fetch the full source for this kind"},
		}
	}
}

// suggestNextStepsForResults tailors the per-kind suggestions from
// suggestNextSteps to the *shape* of the result set (#247 #2). The
// pre-fix per-kind path always emitted the same template-shaped
// suggestions regardless of how many results came back or how they
// were distributed across files; the _meta payload was 30%+ of the
// response by size with low signal per byte.
//
// Tailoring rules — verified, not guessed:
//
//  1. Many results spread across many files (>10 results across >5
//     files) → prepend `architecture` so the agent orients before
//     drilling into a single hit.
//  2. Single high-confidence result (count==1, conf >= 0.9) → trim
//     redundant secondary suggestions. The agent already has the
//     unique answer; the second step (e.g. trace on a Function with
//     one definition) is noise the agent can call themselves if they
//     decide they need it.
//
// Anything else falls through to the existing per-kind suggestions.
// Conservative by design — the bar for *adding* a suggestion is
// "the result shape strongly indicates the agent will benefit"; the
// bar for *removing* a suggestion is "the kept one is sufficient".
func suggestNextStepsForResults(results []db.SearchResult) []map[string]string {
	if len(results) == 0 {
		return nil
	}
	top := results[0].Symbol

	fileSet := make(map[string]bool, len(results))
	nameCount := 0
	for _, r := range results {
		fileSet[r.Symbol.FilePath] = true
		if r.Symbol.Name == top.Name {
			nameCount++
		}
	}
	fileCount := len(fileSet)
	nameAmbiguous := nameCount > 1

	// Shape 1: many results spread across many files. Orient before drilling.
	if len(results) > 10 && fileCount > 5 {
		base := suggestNextSteps(top, nameAmbiguous)
		out := make([]map[string]string, 0, len(base)+1)
		out = append(out, map[string]string{
			"tool": "architecture",
			"args": "{}",
			"why":  fmt.Sprintf("results span %d files — orient first before drilling into one match", fileCount),
		})
		// Keep the top per-kind suggestion only; trim the secondary so
		// the architecture suggestion doesn't get crowded out.
		if len(base) > 0 {
			out = append(out, base[0])
		}
		return out
	}

	// Shape 2: single high-confidence result. Trim secondary suggestions.
	if len(results) == 1 && top.ExtractionConfidence >= 0.9 {
		base := suggestNextSteps(top, nameAmbiguous)
		if len(base) > 1 {
			return base[:1]
		}
		return base
	}

	return suggestNextSteps(top, nameAmbiguous)
}

func (s *Server) handleQuery(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	// Accept both `pinchql` (current) and `cypher` (legacy alias, kept
	// for one release per #206). New callers should use `pinchql`; the
	// alias is honored silently so existing scripts keep working.
	cql := str(args, "pinchql")
	if cql == "" {
		cql = str(args, "cypher")
	}
	if cql == "" {
		return errResult("pinchql query is required (parameter `pinchql`; legacy alias `cypher` also accepted)"), nil
	}
	maxRows := intArg(args, "max_rows", 200)
	minConfidence := floatArg(args, "min_confidence", 0.0)

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	// Cypher queries are pure SELECTs — route to the reader pool (#51).
	exec := &cypher.Executor{DB: s.store.RO(), MaxRows: maxRows, ProjectID: projectID}
	// Defense-in-depth deadline. The Executor honors context cancellation
	// via QueryContext, but the incoming MCP context may not have one —
	// so a pathological query (huge LIMIT × complex regex) could run
	// indefinitely. 10s is well above the documented 99th-percentile
	// latency (~5ms BFS depth 3) but bounded enough that a runaway
	// query doesn't tie up the server.
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := exec.Execute(queryCtx, cql)
	if err != nil {
		return errResult(fmt.Sprintf("cypher error: %v", err)), nil
	}

	// min_confidence filters rows whose query projects an
	// `extraction_confidence` column. Rows from queries that don't return
	// confidence are unaffected — Cypher might project arbitrary columns.
	rows := result.Rows
	confs := make([]float64, 0, len(rows))
	if minConfidence > 0 {
		filtered := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if c, ok := rowConfidence(row); ok {
				if c >= minConfidence {
					filtered = append(filtered, row)
					confs = append(confs, c)
				}
			} else {
				// No confidence column projected → pass through.
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	} else {
		for _, row := range rows {
			if c, ok := rowConfidence(row); ok {
				confs = append(confs, c)
			}
		}
	}

	// #338: ensure rows is never nil so JSON marshals as [] not null.
	// The cypher Executor's Result.Rows defaults to nil when no MATCH
	// rows; both the filtered and unfiltered branches above can leave it
	// nil. Same fix shape as #328 / #330 / #332 / #334.
	if rows == nil {
		rows = []map[string]any{}
	}

	responseJSON, _ := json.Marshal(rows)
	meta := map[string]any{
		"confidence_distribution": confidenceDistribution(confs),
	}
	// #338: when the result rows expose an `id` column (RETURN n.id, f.id,
	// etc.), suggest a `context` follow-up on the top row. Mirrors the
	// next_steps pattern in search/trace/changes/architecture so an agent
	// doesn't have to re-derive the obvious next call from a query result.
	if id := firstRowID(rows); id != "" {
		meta["next_steps"] = []map[string]string{
			{"tool": "context", "args": fmt.Sprintf(`{"id":"%s"}`, id),
				"why": "read the top result's full source + imports — the typical follow-up after a query MATCH"},
		}
	}
	data := map[string]any{
		"columns": result.Columns,
		"rows":    rows,
		"total":   len(rows),
		"_meta":   meta,
	}
	return s.jsonResultWithMeta(data, start, tool, args, savedVsFullRead(len(rows), responseJSON)), nil
}

// firstRowID returns the id of the first row in rows when any column
// looks like a symbol id (`id`, `n.id`, `f.id`, ...). Used by handleQuery
// to propose a context next_step when the user RETURNed an id column
// (#338). Returns "" when no row carries an id we can use.
func firstRowID(rows []map[string]any) string {
	if len(rows) == 0 {
		return ""
	}
	r := rows[0]
	// Direct `id` projection first.
	if v, ok := r["id"].(string); ok && v != "" {
		return v
	}
	// Aliased forms — Cypher `RETURN n.id` produces a column named `n.id`.
	for k, v := range r {
		if !strings.HasSuffix(k, ".id") {
			continue
		}
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// rowConfidence pulls the `extraction_confidence` column off a Cypher result
// row if present. Cypher queries project arbitrary columns, so a row may not
// carry confidence at all — filter logic falls back to pass-through in that
// case rather than silently dropping the row.
func rowConfidence(row map[string]any) (float64, bool) {
	v, ok := row["extraction_confidence"]
	if !ok {
		return 0, false
	}
	switch f := v.(type) {
	case float64:
		return f, true
	case float32:
		return float64(f), true
	}
	return 0, false
}

func (s *Server) handleTrace(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	name := str(args, "name")
	if name == "" {
		return errResult("name is required"), nil
	}
	direction := str(args, "direction")
	if direction == "" {
		direction = "both"
	}
	depth := intArg(args, "depth", 3)
	addRisk := boolArgDefault(args, "risk", true)
	minConfidence := floatArg(args, "min_confidence", 0.0)

	// kinds: comma-separated list of edge kinds to traverse (e.g.
	// "CALLS" or "READS,WRITES"). Empty/missing = default (CALLS
	// family). Whitespace and case differences are tolerated so a
	// caller passing "reads, writes" matches the same as "READS,WRITES".
	kindsArg := str(args, "kinds")
	var edgeKinds []string
	if kindsArg != "" {
		for _, k := range strings.Split(kindsArg, ",") {
			k = strings.ToUpper(strings.TrimSpace(k))
			if k != "" {
				edgeKinds = append(edgeKinds, k)
			}
		}
	}

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	// Resolve the start symbol explicitly so we can surface ambiguity in
	// _meta — agents that hit a same-named symbol elsewhere (common: many
	// `Run`, `Handler`, `Open` per project) need a hint to refine. Trace's
	// own picks-first heuristic stays the same; this just makes the choice
	// observable.
	starts, err := s.store.GetSymbolsByName(projectID, name, 5)
	if err != nil {
		return errResult(fmt.Sprintf("trace lookup: %v", err)), nil
	}
	if len(starts) == 0 {
		return errResult(fmt.Sprintf("symbol %q not found in project", name)), nil
	}
	// #319: rank candidates so the picked target is the most useful
	// trace seed. Precedence:
	//   1. Non-scratch, non-test files first (scratch_*.go, *_test.go)
	//   2. Callable kinds first (Function, Method) — Modules/Settings
	//      can match a name but they have no CALLS edges, so tracing
	//      them returns 0 hops and looks like a real empty result.
	//   3. Stable order from GetSymbolsByName for everything else.
	sortTraceCandidates(starts)
	hops, err := s.indexer.TraceByID(ctx, projectID, starts[0].ID, direction, depth, addRisk, edgeKinds...)
	if err != nil {
		return errResult(fmt.Sprintf("trace error: %v", err)), nil
	}

	// Filter by min_confidence — drop hops whose target falls below threshold.
	// Always collect confidences for the response distribution (regardless of
	// whether the threshold filter is active).
	confs := make([]float64, 0, len(hops))
	if minConfidence > 0 {
		filtered := hops[:0]
		for _, h := range hops {
			if h.Symbol.ExtractionConfidence >= minConfidence {
				filtered = append(filtered, h)
				confs = append(confs, h.Symbol.ExtractionConfidence)
			}
		}
		hops = filtered
	} else {
		for _, h := range hops {
			confs = append(confs, h.Symbol.ExtractionConfidence)
		}
	}

	// Group by depth
	byDepth := make(map[int][]map[string]any)
	riskCounts := map[string]int{"CRITICAL": 0, "HIGH": 0, "MEDIUM": 0, "LOW": 0}
	for _, h := range hops {
		entry := map[string]any{
			"id":         h.Symbol.ID,
			"name":       h.Symbol.Name,
			"kind":       h.Symbol.Kind,
			"file_path":  h.Symbol.FilePath,
			"start_line": h.Symbol.StartLine,
			"via":        h.Via,
		}
		if addRisk {
			entry["risk"] = h.Risk
			riskCounts[h.Risk]++
		}
		byDepth[h.Depth] = append(byDepth[h.Depth], entry)
	}

	// #332: zero-len init so JSON shape is stable when trace finds no hops.
	hopsList := []map[string]any{}
	for d := 1; d <= depth; d++ {
		if nodes, ok := byDepth[d]; ok {
			hop := map[string]any{"depth": d, "nodes": nodes}
			if addRisk {
				hop["risk"] = index.RiskLabel(d)
			}
			hopsList = append(hopsList, hop)
		}
	}

	responseJSON, _ := json.Marshal(hopsList)
	var tracedPaths []string
	for _, h := range hops {
		tracedPaths = append(tracedPaths, h.Symbol.FilePath)
	}
	traceRoot, _ := s.resolveProjectRoot(projectID)
	meta := map[string]any{
		"confidence_distribution": confidenceDistribution(confs),
	}
	// Surface name-ambiguity so the agent can refine instead of trusting
	// the first-match heuristic silently. Records up to 5 alternative
	// matches (the GetSymbolsByName cap) with enough info to disambiguate.
	if len(starts) > 1 {
		alts := make([]map[string]any, 0, len(starts))
		for _, s := range starts {
			alts = append(alts, map[string]any{
				"id":             s.ID,
				"qualified_name": s.QualifiedName,
				"kind":           s.Kind,
				"file_path":      s.FilePath,
			})
		}
		meta["ambiguous_match"] = map[string]any{
			"resolved_to": starts[0].ID,
			"alternatives": alts,
			"hint":         fmt.Sprintf("name %q matched %d symbols; trace used the first (%s). Pass an exact ID via TraceByID, or call `search` with kind/language filters to narrow.", name, len(starts), starts[0].ID),
		}
	}
	// Suggest the obvious next move after a trace. The agent has the
	// blast radius; the next step is reading the highest-risk hop's
	// source. Pick the first CRITICAL hop if there is one, otherwise
	// the first HIGH, etc. For empty traces (zero callers/callees),
	// suggest reading the start symbol itself.
	if len(hops) > 0 {
		topHop := hops[0]
		for _, h := range hops {
			if h.Risk == "CRITICAL" {
				topHop = h
				break
			}
		}
		meta["next_steps"] = []map[string]string{
			{"tool": "context", "args": fmt.Sprintf(`{"id":"%s"}`, topHop.Symbol.ID),
				"why": fmt.Sprintf("read the %s-risk hop's full source + imports before deciding to edit", topHop.Risk)},
		}
	} else {
		// Empty trace = no inbound/outbound CALLS edges. Likely a leaf
		// (no callers) or an entry point (no callees). Direct the agent
		// to the source itself.
		meta["next_steps"] = []map[string]string{
			{"tool": "context", "args": fmt.Sprintf(`{"id":"%s"}`, starts[0].ID),
				"why": "no call edges found at this depth — read the symbol's own source instead"},
		}
	}
	data := map[string]any{
		"root":      name,
		"direction": direction,
		"hops":      hopsList,
		"total":     len(hops),
		"_meta":     meta,
	}
	if addRisk {
		data["risk_summary"] = riskCounts
	}
	return s.jsonResultWithMeta(data, start, tool, args, savedVsFileSizes(traceRoot, tracedPaths, responseJSON)), nil
}

func (s *Server) handleChanges(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	projectArg := str(args, "project")
	scope := str(args, "scope")
	if scope == "" {
		scope = "unstaged"
	}
	depth := intArg(args, "depth", 3)

	projectID, err := s.resolveProjectID(projectArg)
	if err != nil {
		return errResult(err.Error()), nil
	}
	root, err := s.resolveProjectRoot(projectID)
	if err != nil {
		return errResult(err.Error()), nil
	}

	// Run git diff
	diffOutput, diffErr := runGitDiff(root, scope)
	if diffErr != nil {
		return errResult(fmt.Sprintf("git diff failed: %v", diffErr)), nil
	}

	// Parse changed files from diff
	changedFiles := parseGitDiffFiles(diffOutput)

	// Find symbols in changed files
	var changedSymbols []db.Symbol
	for _, f := range changedFiles {
		syms, err := s.store.GetSymbolsForFile(projectID, f)
		if err != nil {
			continue
		}
		changedSymbols = append(changedSymbols, syms...)
	}

	// BFS trace for blast radius. Use TraceByID so a changed `Run` /
	// `Handler` / `Open` resolves to the *exact* symbol that changed,
	// not whichever same-named symbol the name-based lookup picks first
	// (#5). The previous Trace(name, ...) path computed blast radius
	// from a sibling symbol when one name had multiple definitions.
	//
	// #247 #4: alongside the impacted-symbol collection, track which
	// test symbols reach each changed symbol — separately from the
	// `seen` dedupe so a test reached via multiple changed symbols gets
	// its overlap counted, not collapsed into the first path. Used to
	// produce the tests_to_run array sorted by overlap descending.
	// #330: pre-allocate as zero-len so the JSON field is always [], never
	// null. A nil slice marshals to null, forcing every consumer to
	// null-check; same fix shape as #328 on health.extraction_coverage.
	impacted := []map[string]any{}
	seen := make(map[string]bool)
	testHits := make(map[string]map[string]bool) // test sym ID → set of changed sym IDs that reach it
	testSyms := make(map[string]db.Symbol)       // test sym ID → the symbol (for output projection)
	for _, sym := range changedSymbols {
		hops, err := s.indexer.TraceByID(ctx, projectID, sym.ID, "inbound", depth, true)
		if err != nil {
			continue
		}
		for _, h := range hops {
			if h.Symbol.IsTest {
				if _, ok := testHits[h.Symbol.ID]; !ok {
					testHits[h.Symbol.ID] = make(map[string]bool)
					testSyms[h.Symbol.ID] = h.Symbol
				}
				testHits[h.Symbol.ID][sym.ID] = true
			}
			if seen[h.Symbol.ID] {
				continue
			}
			seen[h.Symbol.ID] = true
			impacted = append(impacted, map[string]any{
				"id":         h.Symbol.ID,
				"name":       h.Symbol.Name,
				"kind":       h.Symbol.Kind,
				"file_path":  h.Symbol.FilePath,
				"risk":       h.Risk,
				"changed_by": sym.Name,
			})
		}
	}

	// Build tests_to_run sorted by overlap descending (then test ID
	// ascending for stable output). Overlap = how many distinct
	// changed symbols this test reaches; higher overlap = more bang
	// per re-run. Deterministic ordering keeps any future snapshot
	// test on this surface stable.
	type testRow struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		FilePath string `json:"file_path"`
		Overlap  int    `json:"overlap"`
	}
	testsToRun := make([]testRow, 0, len(testHits))
	for testID, hits := range testHits {
		sym := testSyms[testID]
		testsToRun = append(testsToRun, testRow{
			ID:       testID,
			Name:     sym.Name,
			FilePath: sym.FilePath,
			Overlap:  len(hits),
		})
	}
	sort.Slice(testsToRun, func(i, j int) bool {
		if testsToRun[i].Overlap != testsToRun[j].Overlap {
			return testsToRun[i].Overlap > testsToRun[j].Overlap
		}
		return testsToRun[i].ID < testsToRun[j].ID
	})

	// Build risk summary
	riskCounts := map[string]int{"CRITICAL": 0, "HIGH": 0, "MEDIUM": 0, "LOW": 0}
	for _, item := range impacted {
		if r, ok := item["risk"].(string); ok {
			riskCounts[r]++
		}
	}

	// #330: zero-len init so the JSON field is [] when no symbols changed.
	changedSymNames := []map[string]any{}
	for _, sym := range changedSymbols {
		changedSymNames = append(changedSymNames, map[string]any{
			"id": sym.ID, "name": sym.Name, "kind": sym.Kind, "file_path": sym.FilePath,
		})
	}

	responseJSON, _ := json.Marshal(impacted)
	totalTracedSyms := len(changedSymbols) + len(impacted)
	data := map[string]any{
		"changed_files":   changedFiles,
		"changed_symbols": changedSymNames,
		"impacted":        impacted,
		"tests_to_run":    testsToRun,
		"summary": map[string]any{
			"changed_files":   len(changedFiles),
			"changed_symbols": len(changedSymbols),
			"total_impacted":  len(impacted),
			"tests_to_run":    len(testsToRun),
			"critical":        riskCounts["CRITICAL"],
			"high":            riskCounts["HIGH"],
			"medium":          riskCounts["MEDIUM"],
			"low":             riskCounts["LOW"],
		},
	}
	// Suggest the next move based on what changes found. CRITICAL impact
	// → trace the affected callers to inspect the chain. Non-zero impact
	// without CRITICAL → read context on the most-impacted symbol.
	// No impact → the change is local; suggest writing tests.
	if nextSteps := suggestChangesNextSteps(impacted, changedSymNames, riskCounts); len(nextSteps) > 0 {
		data["_meta"] = map[string]any{"next_steps": nextSteps}
	}
	return s.jsonResultWithMeta(data, start, tool, args, savedVsFullRead(totalTracedSyms, responseJSON)), nil
}

// suggestChangesNextSteps picks 1-2 follow-up actions for handleChanges
// based on the diff's blast radius. Mirrors the dead-simple decision
// rules an experienced reviewer would apply: high impact = inspect, zero
// impact = the change is contained.
func suggestChangesNextSteps(impacted []map[string]any, changedSyms []map[string]any, riskCounts map[string]int) []map[string]string {
	if len(changedSyms) == 0 {
		return nil // No changed symbols — nothing actionable.
	}
	// Pick the first CRITICAL impact if any; else first HIGH; else first item.
	var topImpact map[string]any
	for _, label := range []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"} {
		for _, im := range impacted {
			if r, _ := im["risk"].(string); r == label {
				topImpact = im
				break
			}
		}
		if topImpact != nil {
			break
		}
	}
	if topImpact != nil {
		impactedID, _ := topImpact["id"].(string)
		impactedName, _ := topImpact["name"].(string)
		risk, _ := topImpact["risk"].(string)
		out := []map[string]string{
			{"tool": "context", "args": fmt.Sprintf(`{"id":"%s"}`, impactedID),
				"why": fmt.Sprintf("read %s — the %s-risk caller most likely to be affected", impactedName, risk)},
		}
		if riskCounts["CRITICAL"] > 0 {
			out = append(out, map[string]string{
				"tool": "trace", "args": fmt.Sprintf(`{"name":"%s"}`, impactedName),
				"why": "follow the call chain further — CRITICAL-risk impact often has cascading callers worth inspecting",
			})
		}
		return out
	}
	// No callers found — the change is contained to the changed symbols.
	// #292: when every changed symbol is a documentation kind (Section,
	// Heading, Document), there's no callgraph to walk and the section
	// title makes a poor FTS5 query (em-dashes, colons, dot-prefixed
	// numbers). Return a one-line note instead of a search call that
	// would either error or BM25-match unrelated code symbols.
	if allDocKinds(changedSyms) {
		return []map[string]string{
			{"tool": "", "note": "documentation-only change — no callers to trace",
				"why": "all changed symbols are Section/Heading/Document kinds; the file is doc, not code"},
		}
	}
	// Code change with no callers found — propose an FTS5 search using
	// the first non-doc symbol's name. Skips Section symbols since their
	// titles aren't useful FTS5 queries.
	first := firstCodeSymbolName(changedSyms)
	if first == "" {
		// Mixed but couldn't find a code-shaped name — return nothing
		// rather than guess.
		return nil
	}
	return []map[string]string{
		{"tool": "search", "args": fmt.Sprintf(`{"query":"%s","kind":"Function","corpus":"code"}`, first),
			"why": "no callers found — change is contained. Consider searching for related tests or writing one for the new behaviour."},
	}
}

// allDocKinds reports whether every entry in syms has a documentation
// kind (Section/Heading/Document). Used to suppress search-shaped
// next_steps for doc-only diffs (#292).
func allDocKinds(syms []map[string]any) bool {
	if len(syms) == 0 {
		return false
	}
	for _, s := range syms {
		k, _ := s["kind"].(string)
		switch k {
		case "Section", "Heading", "Document":
			continue
		default:
			return false
		}
	}
	return true
}

// firstCodeSymbolName returns the name of the first non-doc symbol in
// syms, skipping Section/Heading/Document. Returns "" when no code-
// shaped symbol is present (#292).
func firstCodeSymbolName(syms []map[string]any) string {
	for _, s := range syms {
		k, _ := s["kind"].(string)
		switch k {
		case "Section", "Heading", "Document":
			continue
		}
		if name, _ := s["name"].(string); name != "" {
			return name
		}
	}
	return ""
}

func (s *Server) handleArchitecture(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	p, _ := s.store.GetProject(projectID)

	// Language breakdown
	langs := make(map[string]int)
	if langRows, err := s.store.RO().QueryContext(ctx,
		`SELECT language, COUNT(*) FROM symbols WHERE project_id=? GROUP BY language ORDER BY COUNT(*) DESC LIMIT 20`,
		projectID); err == nil {
		defer langRows.Close()
		for langRows.Next() {
			var lang string
			var cnt int
			if scanErr := langRows.Scan(&lang, &cnt); scanErr == nil {
				langs[lang] = cnt
			}
		}
		_ = langRows.Err()
	}

	// Entry points. #275: skip top-level scratch files (`scratch_*.go`,
	// `.scratch_*.go`, `tmp_*.go`) — they declare `package main` so the
	// indexer flags them is_entry_point=1, but they're developer
	// scratch space, not real entrypoints. Filter at the basename
	// level only so a legitimate testdata/corpus/.../scratch.go fixture
	// can still surface (path components past the basename are ignored).
	// #332: zero-len init so JSON shape is stable for projects without
	// any indexed entry points.
	entryPoints := []map[string]any{}
	if epRows, err := s.store.RO().QueryContext(ctx,
		`SELECT name, file_path, start_line FROM symbols WHERE project_id=? AND is_entry_point=1 LIMIT 40`,
		projectID); err == nil {
		defer epRows.Close()
		for epRows.Next() {
			var name, fp string
			var line int
			if scanErr := epRows.Scan(&name, &fp, &line); scanErr == nil {
				if isDeveloperScratchPath(fp) {
					continue
				}
				entryPoints = append(entryPoints, map[string]any{"name": name, "file_path": fp, "start_line": line})
				if len(entryPoints) >= 20 {
					break
				}
			}
		}
		_ = epRows.Err()
	}

	// Hotspots (most-called). #305: by default exclude test files —
	// test helpers (`newTestServer`, `makeReq`, `decode`) have huge
	// in-degree because every test imports them, but they're not
	// signal for "what's the most important code in this project?"
	// Fetch ~5x more than we want and post-filter so the top-N stays
	// at the intended size after dropping tests.
	includeTests := boolArg(args, "include_tests")
	hotspotFetchLimit := 50
	if includeTests {
		hotspotFetchLimit = 10 // legacy path — no filter, no over-fetch
	}
	rawHotspots, _ := s.store.GetHotspots(projectID, hotspotFetchLimit)
	var hotspots []db.Symbol
	// #332: zero-len init so JSON shape is stable for projects without
	// any callable hotspots (early-stage indexes, doc-only repos).
	hotspotMaps := []map[string]any{}
	for _, h := range rawHotspots {
		if !includeTests && isTestFile(h.FilePath) {
			continue
		}
		hotspots = append(hotspots, h)
		hotspotMaps = append(hotspotMaps, map[string]any{
			"name": h.Name, "kind": h.Kind, "file_path": h.FilePath,
		})
		if len(hotspotMaps) >= 10 {
			break
		}
	}

	// Graph stats
	_, _, kindCounts, edgeKindCounts, _ := s.store.GraphStats(projectID)

	data := map[string]any{
		"project":         p,
		"languages":       langs,
		"entry_points":    entryPoints,
		"hotspots":        hotspotMaps,
		"node_kinds":      kindCounts,
		"edge_kinds":      edgeKindCounts,
	}
	// Suggest the obvious next moves after orientation. The agent has the
	// hotspots + entry points; the next step is reading the top hotspot's
	// source. Mirrors the pattern in handleSearch's _meta.next_steps.
	if len(hotspots) > 0 {
		data["_meta"] = map[string]any{
			// Hotspot is the most-called symbol — by construction it's
			// canonical, so name ambiguity isn't a concern here.
			"next_steps": suggestNextSteps(hotspots[0], false),
		}
	} else if len(entryPoints) > 0 {
		// No hotspots (project has no CALLS edges yet — common for
		// regex-extracted languages where cross-file resolution is
		// limited). Direct the agent to the first entry point instead.
		first, _ := entryPoints[0]["name"].(string)
		data["_meta"] = map[string]any{
			"next_steps": []map[string]string{
				{"tool": "search", "args": fmt.Sprintf(`{"query":"%s","kind":"Function"}`, first),
					"why": "no hotspot graph available; start from the entry point and explore from there"},
			},
		}
	} else {
		// Truly empty — neither hotspots nor entry points. Either the
		// project isn't indexed, or it is indexed but contains zero
		// callable symbols (docs-only, config-only, lockfile-only).
		// Disambiguate via the project's reported sym count.
		symCount := 0
		if p != nil {
			symCount = p.SymCount
		}
		var diagnosis string
		var nextSteps []map[string]string
		switch {
		case p == nil:
			diagnosis = "project not found in the index"
			nextSteps = []map[string]string{
				{"tool": "list", "args": `{}`, "why": "see all indexed projects — the right project name might differ from what you passed"},
				{"tool": "index", "args": `{"path":"/path/to/project"}`, "why": "index the project if it's not yet present"},
			}
		case symCount == 0:
			diagnosis = "project is indexed but contains zero symbols — likely all files were filtered (lockfiles, minified bundles) or none are in supported languages"
			nextSteps = []map[string]string{
				{"tool": "health", "args": fmt.Sprintf(`{"project":"%s"}`, projectID), "why": "per-language extraction coverage shows whether files were detected but skipped vs not detected at all"},
				{"tool": "index", "args": fmt.Sprintf(`{"path":"%s","force":true}`, p.Path), "why": "force a re-index in case the previous run hit a partial-state bug"},
			}
		default:
			diagnosis = fmt.Sprintf("project has %d symbols but no callable hotspots or entry points — likely config/docs-only (Settings, Sections, no Functions)", symCount)
			nextSteps = []map[string]string{
				{"tool": "search", "args": fmt.Sprintf(`{"query":"*","corpus":"config","project":"%s"}`, projectID), "why": "list all Settings to confirm this is a config/docs project"},
			}
		}
		data["_meta"] = map[string]any{
			"diagnosis":  diagnosis,
			"next_steps": nextSteps,
		}
	}
	// Architecture is a metadata-only response — file/symbol/edge counts,
	// language histogram, hotspot symbol names. There is no file-read
	// alternative an agent would have used instead, so the savings
	// baseline is 0 (#219). The prior `savedVsFullRead(symCount, …)`
	// formula attributed `symCount × avgFileSize` per call, which on
	// real corpora over-claimed by 4-6 orders of magnitude and
	// dominated the cumulative session counter.
	//
	// `tokens_used` (the response payload size) is still tracked via
	// jsonResultWithMeta — users see exactly what this call cost; they
	// just no longer see fictional savings.
	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

func (s *Server) handleSchema(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	symCount, edgeCount, kindCounts, edgeKindCounts, err := s.store.GraphStats(projectID)
	if err != nil {
		return errResult(fmt.Sprintf("stats error: %v", err)), nil
	}

	data := map[string]any{
		"symbols":         symCount,
		"edges":           edgeCount,
		"node_kinds":      kindCounts,
		"edge_kinds":      edgeKindCounts,
	}
	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

func (s *Server) handleList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)
	projects, err := s.store.ListProjects()
	if err != nil {
		return errResult(fmt.Sprintf("list error: %v", err)), nil
	}

	// #274 args: active filter (default true) + limit + include_dead.
	// Defaults are tuned for the typical "agent calls list to orient"
	// case: drop dead-on-disk paths, limit to recent activity, cap
	// output token cost. Set include_dead=true or active=false for
	// the legacy unfiltered shape.
	activeOnly := true
	if v, ok := args["active"].(bool); ok {
		activeOnly = v
	}
	includeDead := false
	if v, ok := args["include_dead"].(bool); ok {
		includeDead = v
	}
	// #302: explicit prune flag. When true (and only when true), the
	// dead-on-disk projects we'd otherwise just hide are physically
	// removed from the store. include_dead=true short-circuits the
	// prune (caller asked to *see* them, not delete them).
	pruneDead := false
	if v, ok := args["prune_dead"].(bool); ok {
		pruneDead = v
	}
	// #301: pagination. Pre-fix `limit=0` meant "all rows", which on
	// dev machines with 100+ indexed projects (worktree fan-out from
	// adjacent tools) returned a 10K-token response for what's almost
	// always a yes/no orientation lookup. Default to 50; the caller
	// can ask for more via explicit `limit`.
	limit := 50
	if v, ok := args["limit"].(float64); ok {
		if v > 0 {
			limit = int(v)
		} else {
			// limit=0 (or negative) was the legacy "all rows" sentinel —
			// preserve it so existing scripts still work without hitting
			// the new default cap.
			limit = -1 // sentinel: no cap
		}
	}
	offset := 0
	if v, ok := args["offset"].(float64); ok && v > 0 {
		offset = int(v)
	}
	// Default activity threshold: 14 days. Configurable per-call via
	// `active_within_days` for users who want the broader view.
	activeWithinDays := 14
	if v, ok := args["active_within_days"].(float64); ok && v > 0 {
		activeWithinDays = int(v)
	}
	cutoff := time.Now().Add(-time.Duration(activeWithinDays) * 24 * time.Hour)

	// Filter first, paginate after — `count` reports the post-filter
	// total so the caller can decide whether the next page is worth
	// fetching. #334: zero-len init so list returns "projects":[] (not
	// null) when the store has no projects.
	filtered := []map[string]any{}
	dropped := 0
	var pruned []string // #302: ids of dead-on-disk projects we deleted
	for _, p := range projects {
		// Drop dead-on-disk paths unless the caller explicitly
		// opts back in. Cheap (one os.Stat per project); on a
		// dev machine with 100+ stale worktrees this is the
		// load-bearing token-cost reduction.
		if !includeDead {
			if _, err := os.Stat(p.Path); os.IsNotExist(err) {
				dropped++
				if pruneDead {
					// #302: delete the row so it doesn't keep
					// appearing in subsequent list calls. Failure
					// to delete is non-fatal — we still hide it
					// and let the next call try again.
					if delErr := s.store.DeleteProject(p.ID); delErr == nil {
						pruned = append(pruned, p.ID)
					}
				}
				continue
			}
		}
		if activeOnly && p.IndexedAt.Before(cutoff) {
			dropped++
			continue
		}
		filtered = append(filtered, map[string]any{
			"id":         p.ID,
			"name":       p.Name,
			"path":       p.Path,
			"files":      p.FileCount,
			"symbols":    p.SymCount,
			"edges":      p.EdgeCount,
			"indexed_at": p.IndexedAt.Format(time.RFC3339),
		})
	}
	total := len(filtered)

	// Apply pagination window. `limit=-1` is the legacy "all rows" path
	// from above; keep it skipping the cap so existing scripts stay
	// stable.
	pageStart := offset
	if pageStart > total {
		pageStart = total
	}
	pageEnd := total
	if limit >= 0 {
		pageEnd = pageStart + limit
		if pageEnd > total {
			pageEnd = total
		}
	}
	rows := filtered[pageStart:pageEnd]

	data := map[string]any{
		"projects":     rows,
		"count":        total,
		"filtered_out": dropped,
		"page": map[string]any{
			"limit":    limit,
			"offset":   offset,
			"returned": len(rows),
		},
	}
	// #302: surface what got pruned (only when the caller asked).
	// Empty list when nothing was deletable; non-nil when prune_dead
	// was set so the caller can confirm deletion happened.
	if pruneDead {
		if pruned == nil {
			pruned = []string{} // empty array, not null, when nothing pruned
		}
		data["pruned"] = pruned
	}
	// Surface next page when the response is partial.
	if limit >= 0 && pageEnd < total {
		data["_meta"] = map[string]any{
			"next_steps": []map[string]string{
				{
					"tool": "list",
					"args": fmt.Sprintf(`{"limit":%d,"offset":%d}`, limit, pageEnd),
					"why":  fmt.Sprintf("%d total projects after filters; you've seen %d-%d. Page to see the rest.", total, pageStart+1, pageEnd),
				},
			},
		}
	}
	// Empty-state guidance — first-contact agents (fresh install,
	// no projects yet) need to know the next step is `index`. A bare
	// `count: 0` is silent failure: the index is real and queryable,
	// just empty.
	if total == 0 {
		meta := map[string]any{
			"diagnosis": "no projects indexed yet — pincher's symbol store is empty",
			"next_steps": []map[string]string{
				{"tool": "index", "args": `{"path":"/path/to/your/project"}`,
					"why": "index a repo to populate the symbol store; subsequent `search`/`context`/`trace` calls require at least one indexed project"},
			},
		}
		if dropped > 0 {
			// Filtered-empty rather than truly-empty: tell the agent
			// how to surface the suppressed rows.
			meta["diagnosis"] = fmt.Sprintf("no active projects in the last %d days (%d filtered out as stale or dead-on-disk)", activeWithinDays, dropped)
			meta["next_steps"] = []map[string]string{
				{"tool": "list", "args": `{"active":false}`,
					"why": "include projects whose last index is older than the activity window"},
				{"tool": "list", "args": `{"include_dead":true}`,
					"why": "include projects whose on-disk path no longer exists (stale DB rows)"},
			}
		}
		data["_meta"] = meta
	}
	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

func (s *Server) handleADR(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)
	action := str(args, "action")
	key := str(args, "key")
	value := str(args, "value")

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	var data map[string]any
	switch action {
	case "get":
		if key == "" {
			return errResult("key is required for action=get"), nil
		}
		val, ok, err := s.store.GetADR(projectID, key)
		if err != nil {
			return errResult(err.Error()), nil
		}
		if !ok {
			return errResult(fmt.Sprintf("ADR key %q not found", key)), nil
		}
		data = map[string]any{"key": key, "value": val}

	case "set":
		if key == "" || value == "" {
			return errResult("key and value are required for action=set"), nil
		}
		if err := s.store.SetADR(projectID, key, value); err != nil {
			return errResult(err.Error()), nil
		}
		data = map[string]any{"key": key, "stored": true}

	case "list":
		entries, err := s.store.ListADRs(projectID)
		if err != nil {
			return errResult(err.Error()), nil
		}
		data = map[string]any{"entries": entries}

	case "delete":
		if key == "" {
			return errResult("key is required for action=delete"), nil
		}
		if err := s.store.DeleteADR(projectID, key); err != nil {
			return errResult(err.Error()), nil
		}
		data = map[string]any{"key": key, "deleted": true}

	default:
		return errResult(fmt.Sprintf("unknown action %q", action)), nil
	}

	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

func (s *Server) handleHealth(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)
	projectArg := str(args, "project")

	// Resolve project — optional; health without a project still returns schema version + db path.
	projectID := ""
	if pid, err := s.resolveProjectID(projectArg); err == nil {
		projectID = pid
	}

	report, err := s.store.HealthCheck(projectID)
	if err != nil {
		return errResult(fmt.Sprintf("health check error: %v", err)), nil
	}

	// Override parser identity using the extractor's registered confidence
	// (its self-declared parser quality at registration time) rather than the
	// avg per-symbol confidence we get from the symbols table. Path penalties
	// drag per-symbol scores below 0.99 even for AST extractors, which made
	// HealthCheck mis-label Go as "Regex" on lockfile-heavy corpora. The
	// registered value never moves. Fall back to the heuristic if the language
	// has no registered extractor (shouldn't happen, but defends against
	// renamed extractors after a re-index).
	for i := range report.Coverage {
		if rc := ast.RegisteredConfidence(report.Coverage[i].Language); rc >= 0 {
			if rc >= 0.99 {
				report.Coverage[i].Parser = "AST"
			} else {
				report.Coverage[i].Parser = "Regex"
			}
		}
	}

	data := map[string]any{
		"schema_version": report.SchemaVersion,
		"db_path":        report.DBPath,
	}

	// #278: stale-binary detection. If a newer pincher.exe landed on
	// disk while this MCP server is still running with the old
	// in-memory copy, surface a binary_stale=true flag with a
	// reconnect hint. Best-effort — failures (Windows AV scan
	// holding the file, exe path moved) silently report false.
	binaryReplaced := false
	if s.binaryPath != "" && !s.binaryStartMTime.IsZero() {
		if info, err := os.Stat(s.binaryPath); err == nil && info.ModTime().After(s.binaryStartMTime) {
			data["binary_stale"] = true
			data["binary_stale_message"] = "Newer pincher binary on disk; restart the MCP server (/mcp reconnect) to pick up changes."
			binaryReplaced = true
		}
	}
	if report.Project != nil {
		data["project"] = map[string]any{
			"name":              report.Project.Name,
			"path":              report.Project.Path,
			"files":             report.Project.FileCount,
			"symbols":           report.Project.SymCount,
			"edges":             report.Project.EdgeCount,
			"indexed_at":        report.Project.IndexedAt.Format(time.RFC3339),
			"staleness_human":   report.StalenessHuman,
			"staleness_seconds": report.StalenessSecs,
			"binary_version":    report.Project.BinaryVersion, // #304
		}
		data["extraction_coverage"] = report.Coverage

		// #304: index-vs-binary version drift. The CALLS edges and
		// other resolution-dependent fields are only as good as the
		// binary that produced them. When the running server's
		// version doesn't match the project's stored version, surface
		// a re-index recommendation so trace doesn't silently return
		// 0-hop "no callers" results from pre-fix data. Empty
		// stored version is rendered as "unknown" — the row pre-dates
		// the v18 migration so we can't compare; we recommend
		// re-index unconditionally for those.
		if report.Project.BinaryVersion != s.version {
			data["index_drift"] = true
			detail := fmt.Sprintf(
				"project indexed by binary_version=%q; running server is %q. Some CALLS/resolution edges may reflect older rules — re-index to refresh.",
				report.Project.BinaryVersion, s.version,
			)
			if report.Project.BinaryVersion == "" {
				detail = fmt.Sprintf(
					"project indexed before v18 migration (binary_version unknown); running server is %q. Re-index to refresh resolution-dependent edges.",
					s.version,
				)
			}
			data["index_drift_message"] = detail
		}
	}

	// #276: surface next-step hints from health so an agent has the
	// obvious follow-up call spelled out instead of having to choose.
	// Only emitted when the report carries an actionable signal:
	// stale index, low-confidence language, or no project resolved.
	steps := suggestHealthNextSteps(report)
	// #304: drift step prepended (most actionable) when binary_version
	// disagrees with the running server.
	if drift, _ := data["index_drift"].(bool); drift && report.Project != nil {
		steps = append([]map[string]string{{
			"tool": "index",
			"args": nextStepArgs(map[string]any{"path": report.Project.Path, "force": true}),
			"why":  "binary_version drift — re-index to refresh resolution-dependent edges so trace results stay accurate",
		}}, steps...)
	}
	if len(steps) > 0 {
		data["_meta"] = map[string]any{"next_steps": steps}
	}

	// #352: when PINCHER_AUTO_RESTART_ON_DRIFT=1 is set AND the on-disk
	// binary has been replaced since startup, exit cleanly so Claude
	// Code's MCP transport respawns into the rebuilt binary. The
	// response is still returned to the caller — the exit fires from
	// inside maybeAutoRestart's sync.Once after we've enqueued the
	// reply, so the agent sees the health output (with binary_stale=true)
	// and the next tool call hits a fresh process.
	driftDetected, _ := data["index_drift"].(bool)
	s.maybeAutoRestart(binaryReplaced, driftDetected)

	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

// suggestHealthNextSteps composes the next_steps surface from a
// HealthReport. Order is most-actionable-first: stale index > low
// confidence > orientation. Empty when there's nothing useful to
// suggest (healthy project, fresh index).
func suggestHealthNextSteps(report *db.HealthReport) []map[string]string {
	var steps []map[string]string

	// No project resolved: the most useful next move is `list` so the
	// agent can pick a project to scope to.
	if report.Project == nil {
		steps = append(steps, map[string]string{
			"tool": "list",
			"args": "{}",
			"why":  "no project resolved — list active projects to pick one before querying",
		})
		return steps
	}

	// Stale index: the project's last indexed_at is meaningfully behind
	// real time. Suggest re-index. Threshold is conservative (>1h)
	// because pincher's watcher keeps things fresh during a session;
	// >1h staleness usually means the watcher hasn't been running.
	if report.StalenessSecs > 3600 {
		steps = append(steps, map[string]string{
			"tool": "index",
			"args": fmt.Sprintf(`{"path":%q}`, report.Project.Path),
			"why":  "index is " + report.StalenessHuman + " stale — re-run to pick up file changes",
		})
	}

	// Low-confidence language: any (language, kind) with p10 < 0.7
	// means searching that corpus at the default min_confidence=0.71
	// will under-emit. Suggest the corresponding `search` floor drop.
	for _, c := range report.Coverage {
		for _, k := range c.ByKind {
			if k.P10 < 0.7 {
				steps = append(steps, map[string]string{
					"tool": "search",
					"args": fmt.Sprintf(`{"query":"…","language":%q,"kind":%q,"min_confidence":0.0}`, c.Language, k.Kind),
					"why":  c.Language + " " + k.Kind + " p10=" + fmt.Sprintf("%.2f", k.P10) + " sits below the default 0.71 floor — drop min_confidence to surface those symbols",
				})
				goto coverageDone // one suggestion per call is enough
			}
		}
	}
coverageDone:

	// Always-helpful tail: orientation. If the project is large
	// enough that an agent might not know where to start, point at
	// architecture. Cheap to surface.
	if report.Project.SymCount > 100 {
		steps = append(steps, map[string]string{
			"tool": "architecture",
			"args": "{}",
			"why":  "orient before querying: returns entry points, hotspots, language breakdown",
		})
	}

	return steps
}

func (s *Server) handleStats(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	calls := atomic.LoadInt64(&s.statsCalls)
	tokensUsed := atomic.LoadInt64(&s.statsTokensUsed)
	tokensSaved := atomic.LoadInt64(&s.statsTokensSaved)
	totalLatency := atomic.LoadInt64(&s.statsLatencyMS)
	totalCostAvoided := float64(tokensSaved) / 1_000_000.0 * baseCostPer1M

	avgLatency := int64(0)
	if calls > 0 {
		avgLatency = totalLatency / calls
	}

	// Flush current session to DB so all-time totals are fresh.
	s.flushSession()

	// If this process has no live MCP session (e.g. HTTP-only dashboard server),
	// read the most recent session row from DB so "This Session" shows real data.
	if calls == 0 {
		if rows, err := s.store.GetSessions(1); err == nil && len(rows) > 0 {
			r := rows[0]
			calls = r.Calls
			tokensUsed = r.TokensUsed
			tokensSaved = r.TokensSaved
			totalCostAvoided = r.CostAvoided
		}
	}

	// All-time savings summed across every persisted session.
	atCalls, atUsed, atSaved, atCost, _ := s.store.GetAllTimeSavings()

	// Optional session project — populated when a root has been detected.
	var proj *db.Project
	if s.sessionID != "" {
		if p, _ := s.store.GetProject(s.sessionID); p != nil {
			proj = p
		}
	}

	const w = 44 // inner width of box
	line := func(label, value string) string {
		content := fmt.Sprintf("  %-20s %s", label, value)
		if len(content) < w {
			content += strings.Repeat(" ", w-len(content))
		}
		return "│" + content + "│\n"
	}
	header := func(title string) string {
		pad := w - 2 - len(title)
		left := pad / 2
		right := pad - left
		return "│ " + strings.Repeat(" ", left) + title + strings.Repeat(" ", right) + " │\n"
	}
	sep := "├" + strings.Repeat("─", w) + "┤\n"
	commify := func(n int64) string {
		s := fmt.Sprintf("%d", n)
		for i := len(s) - 3; i > 0; i -= 3 {
			s = s[:i] + "," + s[i:]
		}
		return s
	}

	baseline := tokensUsed + tokensSaved
	ratio := ""
	if tokensUsed > 0 && tokensSaved > 0 {
		ratio = fmt.Sprintf("  %.0fx", float64(baseline)/float64(tokensUsed))
	}

	var b strings.Builder
	b.WriteString("┌" + strings.Repeat("─", w) + "┐\n")
	b.WriteString(header("SESSION"))
	b.WriteString(line("Tool calls:", commify(calls)))
	b.WriteString(line("Without pincher:", "~"+commify(baseline)+" tokens"))
	b.WriteString(line("With pincher:", commify(tokensUsed)+" tokens"))
	b.WriteString(line("Saved:", "~"+commify(tokensSaved)+" tokens"+ratio))
	b.WriteString(line("Cost avoided:", fmt.Sprintf("$%.4f", totalCostAvoided)))
	b.WriteString(line("Avg latency:", fmt.Sprintf("%d ms", avgLatency)))

	// ALL-TIME section — only render when the DB has data (otherwise it's
	// just a row of zeros, noisy for first-use).
	if atCalls > 0 {
		b.WriteString(sep)
		b.WriteString(header("ALL-TIME"))
		b.WriteString(line("Tool calls:", commify(atCalls)))
		b.WriteString(line("Tokens used:", commify(atUsed)))
		b.WriteString(line("Tokens saved:", "~"+commify(atSaved)))
		b.WriteString(line("Cost avoided:", fmt.Sprintf("$%.4f", atCost)))
	}

	// PROJECT section — visible whenever a session project is set.
	if proj != nil {
		b.WriteString(sep)
		b.WriteString(header("PROJECT"))
		b.WriteString(line("Name:", proj.Name))
		b.WriteString(line("Files:", commify(int64(proj.FileCount))))
		b.WriteString(line("Symbols:", commify(int64(proj.SymCount))))
		b.WriteString(line("Edges:", commify(int64(proj.EdgeCount))))
	}

	b.WriteString("└" + strings.Repeat("─", w) + "┘")
	return s.textResultWithMeta(b.String(), start, tool, args, 0), nil
}


// maxFetchBytes caps the HTTP response body read to 512 KB.
const maxFetchBytes = 512 * 1024

// maxDocstringBytes caps the extracted text stored per Document symbol to 32 KB.
const maxDocstringBytes = 32 * 1024

// maxFetchRedirects caps the redirect chain depth in handleFetch. Each hop
// is re-validated through validateFetchURL so a public-looking initial URL
// can't redirect into RFC1918 / loopback / link-local ranges.
const maxFetchRedirects = 5

// validateFetchURL parses rawURL and returns an error if it is unsafe to
// fetch. Two gates: scheme allow-list (http/https only) and SSRF block-list
// against the resolved IPs. DNS resolution happens here — before any TCP
// connection is opened — so a host whose A record points into RFC1918 is
// refused at validation time, not after the connection lands inside a
// private network.
//
// SECURITY: every IP returned by net.LookupIP is checked. A multi-A-record
// host that mixes one public IP and one 127.0.0.1 entry is refused — the
// http stack might otherwise pick the loopback entry on retry.
func (s *Server) validateFetchURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed (only http/https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url has no host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("dns lookup failed for %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("dns lookup for %q returned no addresses", host)
	}
	for _, ip := range ips {
		if blockReason := s.fetchIPBlockReason(ip); blockReason != "" {
			return fmt.Errorf("blocked: %s (%s resolves to %s)", blockReason, host, ip)
		}
	}
	return nil
}

// fetchIPBlockReason returns a non-empty reason string if ip is in one of the
// SSRF block ranges. Empty string means the IP is allowed for fetching.
//
// Block list (per RFC + cloud-metadata practice):
//   - Loopback (127/8 v4, ::1 v6) — unless fetchAllowLoopback is set (tests)
//   - Link-local (169.254/16, fe80::/10) — covers AWS/GCP/Azure metadata
//   - Private networks (10/8, 172.16/12, 192.168/16, fc00::/7)
//   - Multicast and unspecified addresses
func (s *Server) fetchIPBlockReason(ip net.IP) string {
	if ip.IsLoopback() {
		if s.fetchAllowLoopback {
			return ""
		}
		return "loopback address"
	}
	if ip.IsLinkLocalUnicast() {
		return "link-local address (cloud metadata range)"
	}
	if ip.IsPrivate() {
		return "private network address (RFC1918/RFC4193)"
	}
	if ip.IsMulticast() {
		return "multicast address"
	}
	if ip.IsUnspecified() {
		return "unspecified address"
	}
	return ""
}

func (s *Server) handleFetch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	rawURL := str(args, "url")
	if rawURL == "" {
		return errResult("url is required"), nil
	}
	titleOverride := str(args, "title")

	if err := s.validateFetchURL(rawURL); err != nil {
		return errResult(fmt.Sprintf("invalid url %q: %v", rawURL, err)), nil
	}

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	// Fetch with a 15-second deadline scoped to this call's context.
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return errResult(fmt.Sprintf("request build error: %v", err)), nil
	}
	httpReq.Header.Set("User-Agent", "pincherMCP/1.0")
	httpReq.Header.Set("Accept", "text/html,text/plain,*/*")

	// Re-validate every redirect target against the SSRF block-list and
	// cap the chain depth. The default http.Client follows up to 10
	// redirects with no per-hop validation — a malicious site could
	// redirect into RFC1918 or to 169.254.169.254 (cloud metadata)
	// undetected.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxFetchRedirects {
				return fmt.Errorf("too many redirects (limit %d)", maxFetchRedirects)
			}
			if err := s.validateFetchURL(req.URL.String()); err != nil {
				return fmt.Errorf("redirect target blocked: %w", err)
			}
			return nil
		},
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return errResult(fmt.Sprintf("fetch error: %v", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errResult(fmt.Sprintf("server returned HTTP %d for %s", resp.StatusCode, rawURL)), nil
	}

	rawBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return errResult(fmt.Sprintf("read error: %v", err)), nil
	}

	pageTitle, text := extractTextFromHTML(string(rawBytes))
	if titleOverride != "" {
		pageTitle = titleOverride
	}
	if pageTitle == "" {
		pageTitle = rawURL
	}
	if len(text) > maxDocstringBytes {
		text = text[:maxDocstringBytes] + "\n[truncated]"
	}

	symID := db.MakeSymbolID(rawURL, rawURL, "Document")
	sym := db.Symbol{
		ID:                   symID,
		ProjectID:            projectID,
		FilePath:             rawURL,
		Name:                 pageTitle,
		QualifiedName:        rawURL,
		Kind:                 "Document",
		Language:             "text",
		Docstring:            text,
		Signature:            rawURL,
		ExtractionConfidence: 1.0,
	}
	if err := s.store.BulkUpsertSymbols([]db.Symbol{sym}); err != nil {
		return errResult(fmt.Sprintf("store error: %v", err)), nil
	}

	// Token savings: baseline = agent reads the raw response; we return compressed text.
	respJSON, _ := json.Marshal(map[string]any{"text": text})
	tokensSaved := max(0, len(rawBytes)/charsPerToken-db.ApproxTokens(string(respJSON)))

	data := map[string]any{
		"id":        symID,
		"url":       rawURL,
		"title":     pageTitle,
		"text":      text,
		"raw_bytes": len(rawBytes),
		"stored":    true,
	}
	return s.jsonResultWithMeta(data, start, tool, args, tokensSaved), nil
}

// guideShape is the inferred intent of a task description, picked by
// classifyTaskShape. Each shape maps to a default workflow that the
// guide tool returns to the agent. Stable string values so future
// callers can reason about them programmatically.
type guideShape string

const (
	shapeFix        guideShape = "fix"        // bug fix, error, broken behaviour
	shapeAdd        guideShape = "add"        // new feature, new symbol
	shapeRefactor   guideShape = "refactor"   // rename, restructure, move
	shapeUnderstand guideShape = "understand" // orient, explain, explore
	shapeTest       guideShape = "test"       // write or find tests
	shapeReview     guideShape = "review"     // pre-commit review, blast radius
	shapeFind       guideShape = "find"       // find/where is — search-leaning (#284)
	shapeTraceIn    guideShape = "trace_in"   // who calls / what calls — trace inbound (#284)
	shapeTraceOut   guideShape = "trace_out"  // what does X call — trace outbound (#284)
	shapeUnknown    guideShape = "unknown"    // fallback
)

// classifyTaskShape inspects a task description and returns the most
// likely intent. Keyword-based heuristic — no parsing or NLP. Order
// matters: the first matching shape wins because some keywords
// overlap ("fix tests" → tests, not fix).
//
// Pure function; pinned by tests so future keyword tweaks don't drift.
func classifyTaskShape(task string) guideShape {
	t := strings.ToLower(task)
	contains := func(needles ...string) bool {
		for _, n := range needles {
			if strings.Contains(t, n) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("test", "spec ", "coverage"):
		return shapeTest
	case contains("review", "diff", "before commit", "blast radius", "pre-commit", "impact"):
		return shapeReview
	case contains("fix", "bug", "broken", "error", "regression", "crash", "wrong"):
		return shapeFix
	case contains("refactor", "rename", "restructure", "extract", "clean up"):
		// Note: "split", "move" intentionally NOT in this list — both are
		// also nouns ("FTS5 split", "the move detector") and would over-
		// match. Lose those signal words rather than false-positive.
		return shapeRefactor
	case contains("add", "implement", "build", "new feature", "support for", "introduce"):
		return shapeAdd
	// #284: trace-shape questions explicitly mention who/what calls a
	// symbol. Match the verb pattern before the broader "understand"
	// catch-all so "who calls X" doesn't fall through to architecture.
	case contains("who calls", "what calls", "callers of", "called by", "depends on", "depend on"):
		return shapeTraceIn
	// "downstream of X" / "calls from" are unambiguously trace-out.
	// "what does X call" needs disambiguation: we treat it as
	// trace-out only when "call" appears anywhere in the task. Without
	// "call" the task is shapeUnderstand ("what does the indexer do").
	case contains("downstream of", "calls from"):
		return shapeTraceOut
	case contains("what does"):
		if strings.Contains(t, "call") {
			return shapeTraceOut
		}
		return shapeUnderstand
	case contains("understand", "explain", "how does", "what is", "explore", "learn", "orient"):
		return shapeUnderstand
	// #284: search-shape questions explicitly mention finding/locating.
	// Below the trace cases so "who calls X" doesn't grab "find" if it
	// happens to appear in the task ("find out who calls X").
	case contains("find ", "where is", "where are", "locate", "look up", "lookup"):
		return shapeFind
	default:
		// #290: fall back to `find` when the task carries a qualified
		// identifier (`os.Stat`, `pkg/sub`, `Class::method`). A user
		// typing those almost always wants to *find* it, even without
		// a verb keyword. Better than `unknown` which routes to a
		// generic architecture+search recommendation.
		if qualifiedIdentifierHint(task) != "" {
			return shapeFind
		}
		return shapeUnknown
	}
}

// guideRecommendations returns the default 2-3 next-tool suggestions
// for a given task shape. The "args" slot is a best-effort template —
// the guide tool fills in anything it can extract from the task string
// (e.g. the most-likely symbol name) and leaves the rest as a
// placeholder the agent fills in.
func guideRecommendations(shape guideShape, taskHint string) []map[string]string {
	queryArgs := nextStepArgs(map[string]any{"query": taskHint})
	queryFnArgs := nextStepArgs(map[string]any{"query": taskHint, "kind": "Function"})
	traceInArgs := nextStepArgs(map[string]any{"name": taskHint, "direction": "inbound"})
	traceOutArgs := nextStepArgs(map[string]any{"name": taskHint, "direction": "outbound"})
	switch shape {
	case shapeFix:
		return []map[string]string{
			{"tool": "search", "args": queryArgs,
				"why": "find the symbol the bug lives in"},
			{"tool": "context", "args": `{"id":"<from-search>"}`,
				"why": "read the function plus everything it calls — usually enough to spot the bug without opening any file"},
			{"tool": "trace", "args": `{"name":"<symbol-name>"}`,
				"why": "find callers if the bug might affect upstream code"},
		}
	case shapeAdd:
		return []map[string]string{
			{"tool": "architecture", "args": `{}`,
				"why": "orient — see hotspots and entry points before adding new code"},
			{"tool": "search", "args": queryArgs,
				"why": "find similar existing code; copy its shape rather than reinvent"},
		}
	case shapeRefactor:
		return []map[string]string{
			{"tool": "search", "args": queryArgs,
				"why": "locate the symbol you want to refactor"},
			{"tool": "trace", "args": `{"name":"<symbol-name>","direction":"inbound"}`,
				"why": "find callers — refactors that miss callers cause regressions"},
		}
	case shapeUnderstand:
		return []map[string]string{
			{"tool": "architecture", "args": `{}`,
				"why": "high-level orientation: languages, entry points, hotspots"},
			{"tool": "search", "args": queryArgs,
				"why": "find the central symbol the question is about"},
			{"tool": "context", "args": `{"id":"<from-search>"}`,
				"why": "read it together with its imports — minimal token cost"},
		}
	case shapeTest:
		return []map[string]string{
			{"tool": "search", "args": queryFnArgs,
				"why": "find the function under test"},
			{"tool": "context", "args": `{"id":"<from-search>"}`,
				"why": "read it with its dependencies before deciding what to test"},
		}
	case shapeReview:
		return []map[string]string{
			{"tool": "changes", "args": `{}`,
				"why": "see your git diff mapped to symbols + blast radius"},
			{"tool": "context", "args": `{"id":"<from-changes>"}`,
				"why": "read each high-risk impacted caller before declaring done"},
		}
	case shapeFind:
		return []map[string]string{
			{"tool": "search", "args": queryArgs,
				"why": "BM25 search using the discriminating phrase from your task"},
			{"tool": "context", "args": `{"id":"<from-search>"}`,
				"why": "read the top hit with its imports — usually answers \"what is this?\" without opening any file"},
		}
	case shapeTraceIn:
		return []map[string]string{
			{"tool": "search", "args": queryArgs,
				"why": "first confirm the exact symbol name; ambiguous names trace the wrong target"},
			{"tool": "trace", "args": traceInArgs,
				"why": "find every caller (CRITICAL=direct, HIGH=2 hops, MEDIUM=3 hops)"},
		}
	case shapeTraceOut:
		return []map[string]string{
			{"tool": "search", "args": queryArgs,
				"why": "first confirm the exact symbol name"},
			{"tool": "trace", "args": traceOutArgs,
				"why": "find what this symbol calls (downstream dependency map)"},
		}
	default:
		// Unknown shape — orient first, then ask a refined question.
		return []map[string]string{
			{"tool": "architecture", "args": `{}`,
				"why": "high-level orientation always pays before unfamiliar work"},
			{"tool": "search", "args": queryArgs,
				"why": "best-effort search using your task keywords; refine the query after seeing results"},
		}
	}
}

// taskHintFromString extracts the most discriminating phrase from a
// task description (#284). Prefers a run of consecutive non-stopword
// tokens over a single longest word — for "find all functions that
// handle the http listener lifecycle", the right hint is
// "http listener lifecycle", not "functions". The longest *run* wins;
// ties broken by run position (later runs preferred, since user
// intent typically peaks at the end of a sentence).
//
// #290: qualified identifiers (`os.Stat`, `pkg/sub`, `Class::method`)
// short-circuit the run-detection. They're almost always the user's
// intended subject and should be passed to `search` as a single unit
// rather than split into bare words.
//
// Returns "" only when the task is exclusively stop words.
func taskHintFromString(task string) string {
	if hint := qualifiedIdentifierHint(task); hint != "" {
		return hint
	}
	stopWords := map[string]bool{
		// articles + conjunctions + prepositions
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"to": true, "for": true, "in": true, "on": true, "of": true,
		"with": true, "and": true, "or": true, "but": true, "at": true,
		"by": true, "from": true, "as": true, "be": true, "do": true,
		"can": true, "all": true, "any": true, "some": true,
		// task verbs (the shape detector handles these; not signal in the hint)
		"fix":  true, "fixes": true, "fixed": true,
		"add":  true, "adds":  true, "added": true,
		"remove": true, "rename": true, "refactor": true,
		"understand": true, "explain": true, "explore": true,
		"review": true, "test": true, "tests": true, "implement": true,
		"build": true, "builds": true, "built": true, "make": true,
		"use":   true, "using": true, "find":  true, "show":  true,
		"handle": true, "handles": true,
		// generic interrogatives
		"what": true, "how": true, "why": true, "which": true, "where": true,
		"when": true, "who": true, "does": true, "this": true, "that": true,
		"these": true, "those": true, "i": true, "we": true, "it": true,
		"its": true, "they": true, "you": true,
		// generic adverbs / fillers
		"here": true, "there": true, "now": true, "then": true,
		"works": true, "working": true, "work": true,
		// pincher-noise
		"bug": true, "broken": true, "error": true, "errors": true,
		"regression": true, "feature": true, "features": true,
		"support": true, "function": true, "functions": true,
		"method":  true, "methods": true, "code":   true, "files": true,
		// generic project / scope nouns (#290)
		"codebase":  true,
		"repo":      true, "repository": true, "project": true,
		"file":      true, "module":     true, "package": true,
		"directory": true, "folder":     true,
	}
	tokens := strings.FieldsFunc(task, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_')
	})

	// Slice tokens into runs of consecutive non-stopwords. Stop words
	// act as run breaks. A run of length 1 is fine — that just means
	// no multi-token phrase is present.
	var runs [][]string
	var cur []string
	for _, tok := range tokens {
		if stopWords[strings.ToLower(tok)] {
			if len(cur) > 0 {
				runs = append(runs, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, tok)
	}
	if len(cur) > 0 {
		runs = append(runs, cur)
	}

	if len(runs) == 0 {
		return ""
	}

	// Pick the longest run by token count; ties broken by total
	// character length (a 3-word run of long names beats a 3-word run
	// of short ones); further ties broken by position (later wins).
	bestIdx := 0
	bestLen := len(runs[0])
	bestChars := totalLen(runs[0])
	for i := 1; i < len(runs); i++ {
		runLen := len(runs[i])
		runChars := totalLen(runs[i])
		switch {
		case runLen > bestLen:
			bestIdx, bestLen, bestChars = i, runLen, runChars
		case runLen == bestLen && runChars > bestChars:
			bestIdx, bestLen, bestChars = i, runLen, runChars
		case runLen == bestLen && runChars == bestChars:
			bestIdx = i // later wins on tie
		}
	}
	return strings.Join(runs[bestIdx], " ")
}

// qualifiedIdentifierHint returns the highest-signal qualified
// identifier in `task` (a token containing internal `.`, `::`, or `/`
// between alphanumerics) — `os.Stat`, `pkg/sub`, `Class::method`.
// Returns "" when no qualifier is present (caller falls through to
// the run-based hint extractor). #290.
//
// Filename tokens like `indexer.go`, `config.yaml` also fit the
// "internal qualifier" shape but they're almost always *scope*, not
// *subject* — the user mentions them to narrow the search, not as
// the thing they're searching for. The selector deprioritises
// filename-shaped tokens by sorting them after non-filename ones,
// then breaks remaining ties by length (longest wins).
func qualifiedIdentifierHint(task string) string {
	// Tokenize on whitespace + punctuation that can't be part of a
	// qualified identifier. Keep word chars + `.`, `:`, `/`, `-`.
	tokens := strings.FieldsFunc(task, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= 'A' && r <= 'Z':
			return false
		case r >= '0' && r <= '9':
			return false
		case r == '_' || r == '.' || r == ':' || r == '/' || r == '-':
			return false
		}
		return true
	})
	best := ""
	bestIsFilename := true // start in worst-case bucket so any non-filename wins
	for _, tok := range tokens {
		// Trim leading/trailing special chars — `,os.Stat,` shouldn't
		// preserve the commas, and a bare `.foo` or `foo.` isn't an
		// identifier.
		tok = strings.Trim(tok, "._:-/")
		if !hasInternalQualifier(tok) {
			continue
		}
		isFile := looksLikeFilename(tok)
		switch {
		case best == "":
			best, bestIsFilename = tok, isFile
		case bestIsFilename && !isFile:
			// non-filename always beats filename
			best, bestIsFilename = tok, isFile
		case bestIsFilename == isFile && len(tok) > len(best):
			// same bucket: longest wins
			best, bestIsFilename = tok, isFile
		}
	}
	return best
}

// looksLikeFilename returns true if tok ends in a recognised source-
// code or config file extension. Used by qualifiedIdentifierHint to
// deprioritise tokens like `indexer.go` against `os.Stat`.
func looksLikeFilename(tok string) bool {
	exts := []string{
		".go", ".py", ".rs", ".ts", ".tsx", ".js", ".jsx", ".java",
		".rb", ".php", ".cs", ".cpp", ".cc", ".c", ".h", ".hpp",
		".kt", ".swift", ".scala", ".lua", ".dart", ".r", ".zig",
		".ex", ".exs", ".hs", ".elm",
		".json", ".yaml", ".yml", ".toml", ".hcl", ".tf", ".tfvars",
		".md", ".markdown", ".mdx", ".mdc", ".txt", ".rst",
		".html", ".css", ".scss", ".sh", ".bash", ".zsh",
		".sql", ".proto", ".graphql", ".vue", ".svelte",
	}
	low := strings.ToLower(tok)
	for _, ext := range exts {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}

// hasInternalQualifier reports whether s contains a `.`, `:`, or `/`
// run flanked by alphanumerics on both sides. Single (`os.Stat`) and
// double (`Class::method`) variants both qualify; bare `os.` or `:foo`
// do not.
func hasInternalQualifier(s string) bool {
	if len(s) < 3 {
		return false
	}
	isAlnum := func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
	}
	isSep := func(c byte) bool { return c == '.' || c == ':' || c == '/' }
	for i := 0; i < len(s); i++ {
		if !isSep(s[i]) {
			continue
		}
		// Walk to the end of this separator run.
		j := i
		for j < len(s) && isSep(s[j]) {
			j++
		}
		// Need an alphanumeric immediately before the run start and
		// immediately after the run end.
		if i == 0 || j >= len(s) {
			i = j
			continue
		}
		if isAlnum(s[i-1]) && isAlnum(s[j]) {
			return true
		}
		i = j - 1 // -1 so the for-loop increment lands on j
	}
	return false
}

func totalLen(toks []string) int {
	n := 0
	for _, t := range toks {
		n += len(t)
	}
	return n
}

func (s *Server) handleGuide(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)
	task := str(args, "task")
	if task == "" {
		return errResult("task is required (free-form description of what you're trying to do)"), nil
	}
	shape := classifyTaskShape(task)
	hint := taskHintFromString(task)
	if hint == "" {
		// Fall back to the first non-trivial token so search args isn't
		// completely empty. Edge case for very short or all-stop-word tasks.
		hint = task
	}
	recommendations := guideRecommendations(shape, hint)

	data := map[string]any{
		"task":          task,
		"shape":         string(shape),
		"hint":          hint,
		"recommended_next_tools": recommendations,
	}
	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

// extractTextFromHTML strips HTML markup and returns (title, bodyText).
// It removes <script>, <style>, <head>, <nav>, and <footer> blocks wholesale,
// then strips remaining tags with a simple scanner. No external dependencies.
func extractTextFromHTML(raw string) (title, text string) {
	lower := strings.ToLower(raw)

	// Extract <title> content.
	if i := strings.Index(lower, "<title"); i >= 0 {
		if j := strings.Index(lower[i:], ">"); j >= 0 {
			s := i + j + 1
			if k := strings.Index(lower[s:], "</title>"); k >= 0 {
				title = strings.TrimSpace(raw[s : s+k])
			}
		}
	}

	// Remove noisy blocks wholesale before tag stripping.
	for _, tag := range []string{"script", "style", "head", "nav", "footer"} {
		open := "<" + tag
		close := "</" + tag + ">"
		for {
			lo := strings.ToLower(raw)
			si := strings.Index(lo, open)
			if si < 0 {
				break
			}
			ei := strings.Index(lo[si:], close)
			if ei < 0 {
				raw = raw[:si]
				break
			}
			raw = raw[:si] + " " + raw[si+ei+len(close):]
		}
	}

	// Strip remaining tags with a single-pass scanner.
	var b strings.Builder
	b.Grow(len(raw) / 2)
	inTag := false
	for i := 0; i < len(raw); i++ {
		switch {
		case raw[i] == '<':
			inTag = true
			b.WriteByte(' ')
		case raw[i] == '>':
			inTag = false
		case !inTag:
			b.WriteByte(raw[i])
		}
	}

	// Collapse whitespace.
	text = strings.Join(strings.Fields(b.String()), " ")
	return
}

// ─────────────────────────────────────────────────────────────────────────────
// _meta envelope
// ─────────────────────────────────────────────────────────────────────────────

// baseCostPer1M is the approximate cost per 1M tokens for Claude Sonnet (USD).
const baseCostPer1M = 3.0

// avgFileSize is the estimated chars in a typical source file an agent would
// have to read to locate a symbol without pincherMCP. Real files in this repo
// average ~33KB; 20KB is a conservative cross-language estimate.
const avgFileSize = 20_000

// charsPerToken is the approximate number of source-code characters per BPE
// token. Used only for baseline estimates where we don't have the actual text.
const charsPerToken = 4

// savedVsFullRead returns estimated tokens saved: (N symbols × avgFileSize) minus
// the actual payload size. The baseline is "read the whole file per symbol",
// which is what an agent does without a code graph.
func savedVsFullRead(count int, payloadBytes []byte) int {
	baselineTokens := count * avgFileSize / charsPerToken
	return max(0, baselineTokens-db.ApproxTokens(string(payloadBytes)))
}

// savedVsFileSizes returns estimated tokens saved using actual file sizes looked
// up from the filesystem. More accurate than savedVsFullRead for tools that
// know which files are being accessed.
func savedVsFileSizes(root string, filePaths []string, payloadBytes []byte) int {
	total := 0
	seen := make(map[string]bool)
	for _, fp := range filePaths {
		if seen[fp] {
			continue
		}
		seen[fp] = true
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(fp))); err == nil {
			total += int(fi.Size())
		} else {
			total += avgFileSize
		}
	}
	return max(0, total/charsPerToken-db.ApproxTokens(string(payloadBytes)))
}

func (s *Server) jsonResultWithMeta(data map[string]any, start time.Time, tool string, args map[string]any, tokensSaved int) *mcp.CallToolResult {
	latency := time.Since(start).Milliseconds()
	s.maybeRecordSlowQuery(tool, args, latency)

	// Estimate tokens in this response
	b, _ := json.Marshal(data)
	tokensUsed := db.ApproxTokens(string(b))

	// Cost avoided by not sending tokensSaved tokens to the model
	costAvoided := float64(tokensSaved) / 1_000_000.0 * baseCostPer1M

	// Merge into any pre-existing `_meta` rather than overwriting, so handlers
	// can attach handler-specific fields (e.g. `confidence_distribution` from
	// #34 Phase 3) before calling.
	meta, _ := data["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["tokens_used"] = tokensUsed
	meta["tokens_saved"] = tokensSaved
	meta["latency_ms"] = latency
	meta["cost_avoided"] = fmt.Sprintf("$%.4f", costAvoided)
	// Human-readable savings line. Trains agents + users that pincher is
	// cheaper than reading files; a one-liner per response is the most
	// effective place to surface it (per #138/#139/#140 adoption thread).
	// Suppressed when nothing was saved (admin tools like list/health/stats
	// where the comparison "vs reading files" doesn't apply).
	if tokensSaved > 0 {
		meta["savings"] = fmt.Sprintf("saved ~%s tokens vs reading files (used %s, %dms, %s)",
			humanInt(tokensSaved), humanInt(tokensUsed), latency, meta["cost_avoided"])
	}
	data["_meta"] = meta

	// Accumulate session stats. On the very first call, flush immediately so
	// the dashboard sees the new session within milliseconds, not after 10s.
	newCalls := atomic.AddInt64(&s.statsCalls, 1)
	atomic.AddInt64(&s.statsTokensUsed, int64(tokensUsed))
	atomic.AddInt64(&s.statsTokensSaved, int64(tokensSaved))
	atomic.AddInt64(&s.statsLatencyMS, latency)

	// Per-language call attribution (#240). Scans the marshalled
	// payload for the first `"language":"X"` occurrence; records the
	// call against that language. Tools that don't yield a language
	// field (architecture, list, schema, health, stats, guide) are
	// not attributed and stay invisible to bypass detection — that's
	// fine because the use case is "agent did X file-type work but
	// pincher saw 0 X calls" and only the symbol-/search-bearing
	// tools matter for that signal.
	if m := languageRE.FindSubmatch(b); len(m) > 1 {
		s.recordCallLanguage(string(m[1]))
	}

	// Query-failure / retry-rate counters (#241). Only the four
	// query-shaped tools contribute; everything else is a no-op.
	s.recordQueryMetrics(tool, args, data, tokensUsed)

	// First call of a new session: flush immediately so the dashboard sees
	// the session within milliseconds rather than waiting for the 10s ticker.
	if newCalls == 1 {
		go s.flushSession()
	}

	out, _ := json.MarshalIndent(data, "", "  ")
	result := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
	}

	// #364: every tool response is a restart trigger. When
	// PINCHER_AUTO_RESTART_ON_DRIFT=1 is set AND the on-disk binary has
	// been replaced since startup, exit cleanly so the next tool call
	// hits a fresh process. sync.Once gates the exit so concurrent
	// in-flight calls don't race. When the env var is unset (default),
	// this is a single os.Getenv check — sub-µs.
	s.checkAutoRestart()
	return result
}

// textResultWithMeta performs the same session accounting as jsonResultWithMeta
// but returns a pre-formatted text string rather than a JSON object. Used by
// handleStats so the output is human-readable on the command line.
func (s *Server) textResultWithMeta(text string, start time.Time, tool string, args map[string]any, tokensSaved int) *mcp.CallToolResult {
	latency := time.Since(start).Milliseconds()
	s.maybeRecordSlowQuery(tool, args, latency)
	tokensUsed := db.ApproxTokens(text)
	costAvoided := float64(tokensSaved) / 1_000_000.0 * baseCostPer1M

	newCalls := atomic.AddInt64(&s.statsCalls, 1)
	atomic.AddInt64(&s.statsTokensUsed, int64(tokensUsed))
	atomic.AddInt64(&s.statsTokensSaved, int64(tokensSaved))
	atomic.AddInt64(&s.statsLatencyMS, latency)

	if newCalls == 1 {
		go s.flushSession()
	}

	// Append a compact meta line so callers still see accounting info.
	full := text + fmt.Sprintf("\n  tokens used %-6d  latency %d ms  cost avoided $%.4f\n", tokensUsed, latency, costAvoided)
	result := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: full}},
	}
	// #364: same restart hook as jsonResultWithMeta — see comment there.
	s.checkAutoRestart()
	return result
}

// humanInt formats an int with thousands separators ("14200" -> "14,200").
// Cheaper than pulling in golang.org/x/text/language for this one use site.
func humanInt(n int) string {
	if n < 0 {
		return "-" + humanInt(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return humanInt(n/1000) + "," + fmt.Sprintf("%03d", n%1000)
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Argument helpers
// ─────────────────────────────────────────────────────────────────────────────

// beginCall combines the two-line handler preamble into one.
// beginCall returns the call's start time, tool name, and parsed args.
// Tool name is captured here once so jsonResultWithMeta can stamp it
// onto the slow_queries row without each handler re-extracting it.
func beginCall(req *mcp.CallToolRequest) (time.Time, string, map[string]any) {
	return time.Now(), req.Params.Name, parseArgs(req)
}

func parseArgs(req *mcp.CallToolRequest) map[string]any {
	if len(req.Params.Arguments) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &m); err != nil {
		slog.Warn("pincher.parse_args.invalid_json", "err", err)
		return map[string]any{}
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func str(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func strSlice(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return def
}

func boolArg(args map[string]any, key string) bool {
	v, _ := args[key].(bool)
	return v
}

func boolArgDefault(args map[string]any, key string, def bool) bool {
	v, ok := args[key]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

// floatArg extracts a float64 from the JSON-decoded args map. JSON numbers
// always decode to float64 in Go, so this is a thin guard against missing
// keys / wrong-type callers (e.g. an int passed via Go-side test code).
//
// Used for #34 Phase 3's `min_confidence` parameter on search/query/trace.
// Default 0.0 means "no filter" — every symbol passes through.
func floatArg(args map[string]any, key string, def float64) float64 {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch f := v.(type) {
	case float64:
		return f
	case int:
		return float64(f)
	case int64:
		return float64(f)
	}
	return def
}

// ─────────────────────────────────────────────────────────────────────────────
// Git helpers
// ─────────────────────────────────────────────────────────────────────────────

func runGitDiff(root, scope string) (string, error) {
	args := []string{"diff", "--name-only"}
	switch scope {
	case "staged":
		args = append(args, "--cached")
	case "all":
		args = append(args, "HEAD")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	// `git diff` only reports tracked changes. `unstaged` and `all` are
	// the user's "what's not yet committed?" queries; an untracked new
	// source file (a brand-new test file, a fresh handler, a new docs
	// page) is uncommitted by definition and belongs in pre-commit
	// safety analysis (#6). `staged` deliberately doesn't include
	// untracked files — by definition they're not staged yet.
	if scope == "" || scope == "unstaged" || scope == "all" {
		untracked, lsErr := runGitLsUntracked(root)
		if lsErr == nil && untracked != "" {
			return string(out) + untracked, nil
		}
	}
	return string(out), nil
}

// runGitLsUntracked returns one untracked, non-ignored path per line —
// the same format as `git diff --name-only`, so the result can be
// concatenated with a diff output without a separate parser. Errors are
// returned to the caller; runGitDiff treats them as soft (best-effort)
// since pre-commit analysis without untracked files is still useful.
func runGitLsUntracked(root string) (string, error) {
	cmd := exec.Command("git", "ls-files", "--others", "--exclude-standard")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func parseGitDiffFiles(diff string) []string {
	var files []string
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			files = append(files, line)
		}
	}
	return files
}


