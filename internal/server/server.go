// Package server implements the pincherMCP MCP server with all 22 tools.
//
// Every tool response includes a "_meta" envelope:
//
//	{
//	  "result": { ... },
//	  "_meta": {
//	    "tokens_used":  450,
//	    "tokens_saved": 12300,
//	    "latency_ms":   3
//	  }
//	}
//
// This lets agents track context consumption and remaining budget. There is
// no $-cost field: we don't know the user's model or pricing (#476
// SAVINGS_HONESTY), so a hardcoded dollar baseline would be a guess.
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	// outputSchemas maps tool name → JSON Schema describing its 200
	// response body (#581). Populated by addToolWithOutput at
	// registerTools time; consumed by openAPISpec to render real
	// per-endpoint response contracts.
	outputSchemas map[string]json.RawMessage
	version       string

	// toolArgKeys is the per-tool allow-list of declared input args
	// (#499). Computed lazily under toolArgKeysOnce on first
	// unknownArgs call so the cost is paid once per process, not per
	// request.
	toolArgKeys     map[string]map[string]bool
	toolArgKeysOnce sync.Once
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

	// mcpHTTPPath, when non-empty, mounts the MCP streamable-HTTP transport
	// (#651, v0.54) on the existing HTTP server at this path. Empty (the
	// default) disables the transport entirely — pincher continues to serve
	// MCP over stdio only. Always normalized: starts with "/" and has no
	// trailing "/". Honors basePath at request time so a reverse proxy
	// stripping "/pincher" still routes "/pincher/mcp" to the handler.
	// Set via SetMCPHTTPPath; lazily wired in ServeHTTP so the SDK
	// dependency on http.Handler is paid only when the feature is on.
	mcpHTTPPath        string
	mcpHTTPHandlerOnce sync.Once
	mcpHTTPHandler     http.Handler

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

	// #620: dedupe binary_version_warning. Once a given (project, indexed-by
	// version) pair has surfaced its drift warning to this server process,
	// suppress subsequent emissions of the same warning. Each unique pair
	// fires exactly once per server lifetime — fresh process or a version
	// change re-arms it. Key shape: "projectID:indexed-version".
	driftWarningsEmitted sync.Map

	// persistentSessionID is a stable identifier for this process invocation,
	// used as the primary key in the sessions table for persistent ROI tracking.
	persistentSessionID string
	sessionStartedAt    time.Time

	// Session-level savings accumulators (atomic for goroutine safety).
	statsCalls       int64
	statsTokensUsed  int64
	statsTokensSaved int64
	statsLatencyMS   int64

	// capabilities is the runtime-detected feature set this binary
	// supports (#649). Computed once at New() time; immutable thereafter.
	// Routers read this from _meta.capabilities to make integration
	// decisions without parsing version strings. Each capability tag
	// corresponds to a feature with a runtime probe (gate test in
	// capability_test.go enforces). When a feature ships or is removed,
	// its capability tag is added/removed in lockstep.
	capabilities []string

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
	// autoRestartDelay is the grace period between deciding to restart
	// and actually calling exitFn. Production defaults to 100ms in
	// New() so the in-flight tool response — which is *not yet
	// returned* from jsonResultWithMeta when checkAutoRestart fires —
	// has time to bubble back through the SDK and be written to
	// stdout before the process terminates. Without this delay, the
	// synchronous os.Exit inside checkAutoRestart loses the response
	// the client is waiting on (#371). Tests set this to 0 in
	// newTestServer so the existing exit-gate assertions stay
	// deterministic.
	autoRestartDelay time.Duration

	// projectIDCache (#401) avoids the per-MCP-call SQL hit in
	// resolveProjectID. Keys are caller-supplied project args (name
	// or ID); values are *projectIDCacheEntry with the resolved ID
	// and an expiry timestamp. 60s TTL bounds staleness for the
	// rare case where a project is renamed mid-session — the next
	// lookup after expiry refreshes from the store. handleIndex
	// invalidates explicitly when it knows projects changed.
	//
	// sync.Map (not map+RWMutex) because the access pattern is
	// overwhelmingly reads + occasional writes, no deletes during
	// hot loops; the same shape statsCallsByLanguage uses above.
	projectIDCache sync.Map

	// accessedFiles is the per-session set of file paths the agent has
	// already received content for via a pincher tool (#478). Used by
	// savedVsFileSizesSession to drop the baseline from full_file_read
	// to partial_read on repeat access: the second `context`/`symbol`
	// call against the same file does NOT claim a fresh full-file
	// saving, because the file is already in the agent's context window.
	// Keyed by "{projectID}|{relPath}" to keep paths from different
	// projects disjoint. Values are struct{}{}. Process-lifetime; never
	// pruned — sessions die on respawn.
	accessedFiles sync.Map

	// contextDiffCache backs the #655 diff-encoded-context feature
	// (PINCHER_DIFF_CONTEXT=1). It records the last `context` payload
	// served per (project, symbol) this process so a repeat call can
	// short-circuit: file hash unchanged → {unchanged:true}; changed →
	// the symbol's source as a line diff. Keyed "{projectID}|{symbolID}",
	// value *contextDiffEntry. Process-lifetime like accessedFiles — the
	// "session" ends when the process respawns.
	contextDiffCache sync.Map
	// diffContext gates contextDiffCache. Read once from
	// PINCHER_DIFF_CONTEXT at New(); default-off in v0.56 until perf
	// validates, then default-on.
	diffContext bool

	// events is the SSE fan-out bus for GET /v1/events (#654). The
	// indexer publishes index_started / index_complete through it via
	// the hook wired in New(); /v1/events subscribers drain it. Always
	// non-nil after New().
	events *eventBus
}

// contextDiffEntry is one cached `context` fetch (#655): the backing
// file's content hash at fetch time plus the primary symbol's source we
// served. A repeat fetch compares the live hash against fileHash to
// decide unchanged-vs-diff, and diffs against source when changed.
type contextDiffEntry struct {
	fileHash string
	source   string
}

// projectIDCacheEntry is the cached resolution of a project arg.
// Empty IDs are NOT cached (they signal "not found", which we want
// to revalidate next call so a freshly-indexed project becomes
// visible immediately).
type projectIDCacheEntry struct {
	id        string
	expiresAt time.Time
}

const projectIDCacheTTL = 60 * time.Second

// New creates and registers all 22 MCP tools.
func New(store *db.Store, indexer *index.Indexer, version string) *Server {
	now := time.Now()
	s := &Server{
		store:               store,
		indexer:             indexer,
		handlers:            make(map[string]mcp.ToolHandler),
		tools:               make(map[string]*mcp.Tool),
		version:             version,
		persistentSessionID: pickPersistentSessionID(now),
		sessionStartedAt:    now,
		exitFn:              os.Exit, // #352: substituted by tests
		autoRestartDelay:    autoRestartExitDelay,
		diffContext:         os.Getenv("PINCHER_DIFF_CONTEXT") == "1", // #655
		events:              newEventBus(),                           // #654
	}
	// #654: wire the indexer's lifecycle hook to the SSE bus so
	// index_started / index_complete reach /v1/events subscribers. The
	// bus fan-out is non-blocking, so this never stalls indexing.
	if indexer != nil {
		indexer.SetEventHook(func(eventType string, payload map[string]any) {
			pid, _ := payload["project_id"].(string)
			s.events.publish(sseEvent{Type: eventType, ProjectID: pid, Payload: payload})
		})
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
	// #649: compute capability advertisement once at startup. Routers
	// consume this from _meta.capabilities to make integration decisions
	// (do I need to fall back to polling, or can I subscribe via SSE?
	// is the streamable-HTTP transport available, or stdio-only? etc.)
	// without scraping version strings or trial-and-error calls.
	s.capabilities = computeCapabilities(s)
	// #581: stamp the per-tool OpenAPI response schemas after
	// registration. Done as a post-pass so registerTools stays
	// readable (~24 addTool calls without an extra arg each) and the
	// schemas live in their own file. The gate test pins every
	// registered tool to declare one.
	s.outputSchemas = make(map[string]json.RawMessage, len(s.tools))
	for name := range s.tools {
		if raw := outputSchemaJSON(name); raw != nil {
			s.outputSchemas[name] = raw
		}
	}
	// #420: when the supervisor supplied a stable PINCHER_SESSION_ID,
	// reload prior counters from the sessions row so the session-level
	// stats survive a supervised respawn. Flushes are INSERT OR REPLACE
	// on the same key, so seeding atomics and then flushing won't
	// double-count. Best-effort: if the row doesn't exist (first inner
	// for this supervisor) or the read fails, atomics stay at zero.
	if row, err := store.GetSessionByID(s.persistentSessionID); err == nil && row != nil {
		atomic.StoreInt64(&s.statsCalls, row.Calls)
		atomic.StoreInt64(&s.statsTokensUsed, row.TokensUsed)
		atomic.StoreInt64(&s.statsTokensSaved, row.TokensSaved)
		atomic.StoreInt64(&s.statsQueriesTotal, row.QueryMetrics.QueriesTotal)
		atomic.StoreInt64(&s.statsQueriesZeroResult, row.QueryMetrics.QueriesZeroResult)
		atomic.StoreInt64(&s.statsQueriesRetriedSucceeded, row.QueryMetrics.QueriesRetriedSucceeded)
		atomic.StoreInt64(&s.statsTokensBurned, row.QueryMetrics.TokensBurnedOnFailures)
		// Preserve the original session start so uptime/wall-clock
		// math reflects the supervisor's lifetime, not the inner's.
		if !row.StartedAt.IsZero() {
			s.sessionStartedAt = row.StartedAt
		}
		// Restore per-language counters from the flushed JSON snapshot.
		if row.CallsByLanguage != "" {
			var byLang map[string]int64
			if jsonErr := json.Unmarshal([]byte(row.CallsByLanguage), &byLang); jsonErr == nil {
				for lang, count := range byLang {
					v, _ := s.statsCallsByLanguage.LoadOrStore(lang, new(int64))
					if ptr, ok := v.(*int64); ok && ptr != nil {
						atomic.StoreInt64(ptr, count)
					}
				}
			}
		}
	}
	return s
}

// pickPersistentSessionID returns the session ID this process should use
// when flushing to the sessions table. Under supervised mode the
// supervisor passes PINCHER_SESSION_ID via env so that successive inner
// processes share a single sessions row — counters then persist across
// respawn (#420). Bare invocations fall back to a per-process timestamp
// ID, preserving the legacy "sess-<unixnano>" shape.
func pickPersistentSessionID(now time.Time) string {
	if id := strings.TrimSpace(os.Getenv("PINCHER_SESSION_ID")); id != "" {
		return id
	}
	return fmt.Sprintf("sess-%d", now.UnixNano())
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
	// cost_avoided is deprecated (#476 SAVINGS_HONESTY follow-up): we don't
	// know the user's model or pricing, so any $-figure is a guess.
	// Persist 0 to keep the DB column stable; readers no longer display it.
	const costAvoided = 0.0
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

// isTestFixturePath reports whether file_path lives under a directory
// pincher uses for hand-crafted test fixtures (NOT real tests, which
// `isTestFile` already catches). Used by handleArchitecture to keep
// entry_points focused on the project's own entrypoints — a
// `testdata/corpus/go-project/cmd/cli/main.go` shaped fixture
// declares `package main` and trips the indexer's is_entry_point
// flag, but it isn't an entrypoint of *this* project.
//
// Matched directory segments (case-insensitive, both / and \):
//   - testdata/        (Go's stdlib convention; also pincher's pinned-corpus dir)
//   - test-fixtures/, test_fixtures/
//   - __fixtures__/    (Jest convention)
//   - fixtures/        (broad; common in Ruby, Rails, generic test data)
//
// Distinct from `isTestFile`: tests exercise the production code,
// fixtures are inputs *to* tests. A fixture's symbols should not
// surface in orientation views, but they SHOULD remain searchable.
func isTestFixturePath(filePath string) bool {
	low := strings.ToLower(filePath)
	low = strings.ReplaceAll(low, `\`, `/`)
	for _, dir := range []string{"/testdata/", "/test-fixtures/", "/test_fixtures/", "/__fixtures__/", "/fixtures/"} {
		if strings.Contains(low, dir) {
			return true
		}
	}
	for _, prefix := range []string{"testdata/", "test-fixtures/", "test_fixtures/", "__fixtures__/", "fixtures/"} {
		if strings.HasPrefix(low, prefix) {
			return true
		}
	}
	return false
}

// isRuntimeInvokedGoSymbol returns true when the symbol's name marks
// it as a Go function the language runtime invokes via reflection or
// implicit registration — so the static CALLS graph cannot see the
// caller and the symbol is necessarily a false positive in dead_code
// (#492).
//
// The list is conservative and language-gated:
//   - init: called by Go runtime at package load. Cannot be called
//     explicitly. Always reachable when its package is imported.
//   - TestMain: invoked by `go test` discovery; ditto.
//   - main: entry point, but main() always also lives in a `package
//     main` file — Go won't compile without it. is_entry_point should
//     already cover this, but belt-and-suspenders.
//
// Method-set members called via interface dispatch (String, Error,
// MarshalJSON, etc.) deliberately NOT included here — those need a
// separate interface-satisfaction pass (#493) because the same name
// in a non-interface-method context is legitimately checkable.
func isRuntimeInvokedGoSymbol(language, name string) bool {
	if language != "Go" {
		return false
	}
	switch name {
	case "init", "TestMain", "main":
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
		// scratch, fixture, and test files all rank below production
		// code. Order:
		//   - scratch (worst, dev pollution; #275)
		//   - testdata fixtures (#393/#398: e.g. testdata/corpus/...
		//     declares package main and trips name-collision with
		//     real symbols like `Open` / `Run` / `main`)
		//   - test (still legitimate code, just secondary)
		if isDeveloperScratchPath(p) {
			return 3
		}
		if isTestFixturePath(p) {
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

// isHotspotKind reports whether a symbol kind belongs in architecture
// hotspots — the change-risk surface an agent should orient around (#380).
//
// Inbound CALLS edge counts get inflated by Variables (a JS script's
// `var result` accumulator can rack up the most reads in a project),
// Settings (YAML keys referenced everywhere), and Sections (Markdown
// headings linked from a TOC). None of these are code an agent can
// safely refactor against. Hotspots should mean "if you change this,
// what depends on it?" — that's Function/Method/Class/Interface/Type/Module.
//
// Mirrors the kind-filter precedent in `firstCodeSymbolName` (excludes
// Section/Heading/Document) and the test-file precedent in `isTestFile`.
func isHotspotKind(kind string) bool {
	switch kind {
	case "Function", "Method", "Class", "Interface", "Type", "Module":
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
				s.maybeReindexOnDrift()
				return
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		s.setRoot(cwd)
		s.maybeReindexOnDrift()
	}
}

func (s *Server) setRoot(path string) {
	s.sessionRoot = path
	s.sessionProject = db.ProjectNameFromPath(path)
	s.sessionID = db.ProjectIDFromPath(path)
}

// maybeReindexOnDrift kicks off a background force re-index of the
// session project when the binary that indexed it differs from the
// running binary (#719).
//
// Why: the supervisor's auto-restart-on-drift respawns the inner onto
// a swapped binary, but the index on disk was still built by the OLD
// binary's extraction rules. `health` detects and surfaces this, but
// nothing *heals* it — search/query/trace silently serve a stale graph
// (a v0.55 dogfood run saw a 3× symbol gap) until a manual
// `index force=true`. For the AFK autonomous loop that manual step is
// the gap: the swap + respawn is automated, the re-index was not.
//
// Runs once per session (detectRoot is sessionOnce-guarded). Best
// effort and non-blocking: a not-yet-indexed project (resolveProjectID
// auto-indexes that on first use) or a matching version is a no-op,
// and the re-index runs on a background context so it outlives the
// initialize request. The existing `_meta.binary_version_warning`
// already tells the agent results may shift while it converges.
func (s *Server) maybeReindexOnDrift() {
	indexedBy, drifted := s.driftReindexNeeded()
	if !drifted {
		return
	}
	slog.Info("pincher.drift_reindex.start",
		"project", s.sessionProject,
		"indexed_by", indexedBy,
		"running", s.version)
	go func() {
		if _, err := s.indexer.Index(context.Background(), s.sessionRoot, true); err != nil {
			slog.Warn("pincher.drift_reindex.err", "project", s.sessionProject, "err", err)
			return
		}
		slog.Info("pincher.drift_reindex.done", "project", s.sessionProject)
	}()
}

// driftReindexNeeded reports whether the session project's index was
// built by a different binary than the one now running — and returns
// the indexing binary's version for the log line. A not-yet-indexed
// project (resolveProjectID auto-indexes that on first use) or a
// matching version is not drift.
func (s *Server) driftReindexNeeded() (indexedBy string, drifted bool) {
	if s.sessionID == "" || s.sessionRoot == "" {
		return "", false
	}
	p, err := s.store.GetProject(s.sessionID)
	if err != nil || p == nil {
		return "", false
	}
	if p.BinaryVersion == "" || p.BinaryVersion == s.version {
		return p.BinaryVersion, false
	}
	return p.BinaryVersion, true
}

// gzipResponseWriter wraps an http.ResponseWriter, routing writes through a gzip.Writer.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

// headResponseWriter discards body bytes while preserving headers and
// status. Used to satisfy RFC 7231 §4.3.2: a HEAD response MUST carry
// the same headers as the corresponding GET, with no message body.
// #609: routing HEAD as GET + this writer means container liveness
// probes that issue HEAD (the spec-recommended verb) get the same
// Content-Type / ETag / Cache-Control / Allow they'd get from GET.
type headResponseWriter struct {
	http.ResponseWriter
}

func (w *headResponseWriter) Write(b []byte) (int, error) { return len(b), nil }

// httpGetOnlyRoutes lists /v1 paths that exist only as GET (plus HEAD
// per RFC 7231). Used by the dispatcher to return 405 Method Not
// Allowed with `Allow: GET, HEAD` instead of the misleading
// "unknown tool" 404 when a client POSTs to a known GET endpoint
// (#609). Keep in sync with the GET handlers in ServeHTTP.
var httpGetOnlyRoutes = map[string]bool{
	"dashboard":     true,
	"dashboard.js":  true,
	"dashboard.css": true,
	"stats":         true,
	"sessions":      true,
	"hook-stats":    true, // v0.37 hook conversion-rate dashboard panel (#628)
	"openapi.json":  true,
	"health":        true,
	"ready":         true, // #660: k8s readiness probe (200 vs 503)
}

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
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept-Encoding, X-Request-ID")
	w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")

	// #657: correlation ID. Read X-Request-ID (or mint a UUID v7), echo
	// it on the response header, and thread it through ctx so the tool-
	// handler wrapper can stamp _meta.request_id. Resolved before auth
	// and rate limiting so even 401/429 responses carry a traceable ID.
	reqID := sanitizeRequestID(r.Header.Get(requestIDHeader))
	w.Header().Set(requestIDHeader, reqID)
	r = r.WithContext(withRequestIDContext(r.Context(), reqID))

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// #609: route HEAD as GET internally and wrap the response writer
	// to drop the body. Per RFC 7231 §4.3.2, HEAD must return the same
	// headers as GET — wrapping post-routing keeps every header (ETag,
	// Cache-Control, Content-Type, Allow) byte-identical without
	// duplicating handler code.
	if r.Method == http.MethodHead {
		r.Method = http.MethodGet
		w = &headResponseWriter{ResponseWriter: w}
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
	// #588: container orchestrators (Docker, Kubernetes, fly.io,
	// ECS) ping /v1/health and /v1/openapi.json as liveness probes
	// and they can't carry a bearer token without significant config
	// gymnastics (mount the secret, sidecar, etc.). Both paths are
	// documentation-shaped — health surfaces version + auth_required
	// + binary_stale; openapi.json surfaces the dynamic spec built
	// from registered tools. Neither leaks project state. Skip the
	// bearer check for them so liveness probes work alongside
	// --http-key. Every other endpoint still enforces auth.
	pathTrimmed := strings.TrimPrefix(r.URL.Path, "/v1/")
	isPublicProbe := pathTrimmed == "health" || pathTrimmed == "openapi.json" || pathTrimmed == "ready"
	if s.httpKey != "" && !isPublicProbe {
		auth := r.Header.Get("Authorization")
		tok, hasBearer := strings.CutPrefix(auth, "Bearer ")
		got := sha256.Sum256([]byte(tok))
		want := sha256.Sum256([]byte(s.httpKey))
		matches := subtle.ConstantTimeCompare(got[:], want[:]) == 1
		if !hasBearer || !matches {
			writeError(w, http.StatusUnauthorized, "unauthorized",
				"unauthorized — set Authorization: Bearer <key>")
			return
		}
	}

	// Rate limiting — per remote IP sliding window. Honors X-Forwarded-For
	// when --trust-proxy is on so the rate-key reflects the real client
	// behind a reverse proxy (issue #40).
	ip := s.clientIP(r)
	if !s.allowRequest(ip) {
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			fmt.Sprintf("rate limit exceeded — max %d requests per %s", s.rateLimit, s.rateWindow))
		return
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

	// #651/#687: streamable-HTTP MCP transport. When SetMCPHTTPPath is set,
	// requests at that path (or any sub-path) delegate to the MCP SDK's
	// streamable-HTTP handler. Mounted post-basepath-strip so reverse-proxy
	// /pincher/mcp routes correctly. Auth + rate-limiting above already
	// applied — the SDK handler sees only authenticated requests.
	//
	// Routed BEFORE the gzip wrap on purpose: the SDK serves a long-lived
	// text/event-stream and flushes per SSE event, but gzipResponseWriter
	// buffers and doesn't implement http.Flusher — wrapping it strands
	// every event in the gzip buffer and the client's SSE read (and
	// session.Close) hangs forever. The MCP transport frames its own
	// payloads; it neither needs nor tolerates the gateway's gzip layer.
	if s.mcpHTTPPath != "" && (r.URL.Path == s.mcpHTTPPath || strings.HasPrefix(r.URL.Path, s.mcpHTTPPath+"/")) {
		s.streamableHTTPHandler().ServeHTTP(w, r)
		return
	}

	// #654: /v1/events SSE stream. Routed before the gzip wrap for the
	// same reason as the MCP transport above — gzipResponseWriter buffers
	// and isn't an http.Flusher, so wrapping a long-lived event stream
	// strands every frame. Auth + rate-limiting + basepath-strip have
	// already run; the GET-only gate is inline here since the route is
	// handled before the httpGetOnlyRoutes map below.
	if r.URL.Path == "/v1/events" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				"/v1/events requires GET")
			return
		}
		s.handleEvents(w, r)
		return
	}

	// Transparently compress responses when the client supports it.
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		w = &gzipResponseWriter{ResponseWriter: w, gz: gz}
	}

	// #590: root URL → dashboard. A user typing the bare URL in a
	// browser shouldn't see "method not allowed — use POST /v1/{tool}";
	// the dashboard IS the front door. 302 redirect (NOT 301) so we
	// can change the front door later without poisoning bookmarks.
	// Honors basepath so /pincher/ → /pincher/v1/dashboard.
	if r.URL.Path == "/" && r.Method == http.MethodGet {
		http.Redirect(w, r, s.effectivePrefix(r)+"/v1/dashboard", http.StatusFound)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/v1/")

	// #609: GET-only routes must reject non-GET methods up front with
	// `Allow: GET, HEAD`. Without this gate, openapi.json and health
	// (which previously didn't check r.Method at all) would happily
	// answer PUT/DELETE requests with their normal payload — quietly
	// violating REST semantics and confusing API consumers expecting
	// 405. HEAD is already mapped to GET at the top of ServeHTTP, so
	// it falls through and returns the GET payload (writer wrapper
	// drops the body).
	if httpGetOnlyRoutes[path] && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			fmt.Sprintf("endpoint /v1/%s requires GET or HEAD", path))
		return
	}

	if path == "health" {
		// auth_required surfaces whether --http-key is set. The dashboard
		// reads it to decide whether to show a "no auth in place" notice
		// (#203). Server-side enforcement is unchanged — this is purely
		// metadata so clients can render appropriate UX.
		//
		// #536: dashboard_version is the server version that produced the
		// dashboard JS the browser is currently running. The JS bakes its
		// own build version in at render time and polls health to detect
		// "your tab is running stale JS against a newer server" — common
		// after a binary upgrade because the JS Cache-Control max-age=600
		// can keep stale JS in the browser for 10 minutes after upgrade.
		// Same value as `version` in this release; carried as a separate
		// field so we can advance one without the other later (e.g. a
		// dashboard rebuild without a server bump).
		json.NewEncoder(w).Encode(map[string]any{
			"ok":                true,
			"version":           s.version,
			"dashboard_version": s.version,
			"auth_required":     s.httpKey != "",
		})
		return
	}
	if path == "openapi.json" {
		json.NewEncoder(w).Encode(s.openAPISpec(r))
		return
	}
	// #660: GET /v1/ready — k8s-style readiness probe distinct from
	// /v1/health (liveness). Returns 200 when the server can serve
	// traffic; 503 with a structured error when an essential
	// dependency isn't ready. liveness (/v1/health) only checks "process
	// is alive"; readiness should fail when the process is alive but
	// not yet usable so k8s/orchestrators can withhold traffic.
	//
	// Criteria for 200:
	//  - schema migration is complete (s.store opened cleanly; if not,
	//    New() would have failed)
	//  - indexer is initialized (s.indexer != nil)
	//  - no first-time index pass blocking on a project with zero
	//    on-disk data yet (mid-pass is OK when prior index exists —
	//    we surface degraded state via _meta.index_in_progress (#925)
	//    rather than failing the readiness probe).
	if path == "ready" {
		ok := true
		var reasons []string
		if s.store == nil {
			ok = false
			reasons = append(reasons, "store not initialized")
		}
		if s.indexer == nil {
			ok = false
			reasons = append(reasons, "indexer not initialized")
		}
		// Schema migration check: if the store opened successfully, the
		// migration ran. A zero schema_version indicates a wedged DB.
		// Schema-migration sanity: if New() reached this code path the
		// migration must have run. The package-level CurrentSchemaVersion
		// returns >=1 by construction; we surface it in the response so
		// orchestrators can diff against expected versions across rolling
		// upgrades, but the comparison itself is informational.
		_ = db.CurrentSchemaVersion
		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{
			"ready":          ok,
			"version":        s.version,
			"schema_version": db.CurrentSchemaVersion(),
			"reasons":        reasons,
		})
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
		//
		// #556: serve with ETag (content hash) so a 304 short-circuits
		// the body when the browser has the same JS already. Plus gzip
		// when the client sent Accept-Encoding: gzip — typical 70-80%
		// size reduction on this asset.
		body := []byte(renderDashboardJS(s.effectivePrefix(r)))
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=600")
		s.writeDashboardSecurityHeaders(w, "default-src 'none'; frame-ancestors 'none'")
		writeAssetWithETagAndGzip(w, r, body)
		return
	}
	if path == "dashboard.css" && r.Method == http.MethodGet {
		body := []byte(renderDashboardCSS())
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=600")
		s.writeDashboardSecurityHeaders(w, "default-src 'none'; frame-ancestors 'none'")
		writeAssetWithETagAndGzip(w, r, body)
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
				"calls":        r0.Calls,
				"tokens_used":  r0.TokensUsed,
				"tokens_saved": r0.TokensSaved,
				"started_at":   r0.StartedAt.Format(time.RFC3339),
				"last_seen":    r0.LastSeen.Format(time.RFC3339),
			}
		}
		// All-time cumulative. No $-figures (#476 SAVINGS_HONESTY).
		var allTime map[string]any
		if atCalls, atUsed, atSaved, _, err := s.store.GetAllTimeSavings(); err == nil {
			allTime = map[string]any{
				"calls":        atCalls,
				"tokens_used":  atUsed,
				"tokens_saved": atSaved,
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
	// GET /v1/hook-stats — PreToolUse hook conversion-rate metrics
	// over the trailing 7 days. Backs the v0.37 dashboard headline
	// panel (#628). Returns the bounded percentage + raw counts so
	// the dashboard can render the small-N "no data yet" state.
	//
	// Telemetry is local-only (#626) — every byte returned here
	// originates in the user's pincher.db. Nothing phones home.
	if path == "hook-stats" && r.Method == http.MethodGet {
		pct, redirects, taken, err := s.store.HookConversionRate7d()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		// #629: triangulating panels — override rate (intentional reject vs
		// unresolved-yet) and per-tool breakdown so a low conversion rate
		// has somewhere to drill into.
		overridePct, overrides, resolved, err := s.store.HookOverrideRate7d()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		byTool, err := s.store.HookCountsByTool7d()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		if byTool == nil {
			byTool = map[string]map[string]int{}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"window":         "7d",
			"redirects":      redirects,
			"taken":          taken,
			"conversion_pct": pct,
			"resolved":       resolved,
			"overrides":      overrides,
			"override_pct":   overridePct,
			"by_tool":        byTool,
		})
		return
	}
	// GET /v1/sessions — per-session savings history for sparkline chart.
	if path == "sessions" && r.Method == http.MethodGet {
		// #531: ?limit= lifts the previously-hardcoded 90-session window.
		// Default 90 (preserves prior behavior), max 500. Sessions are
		// small per row (~150 B) so 500 is comfortably bounded.
		p := parsePageParams(r, 90, 500)
		sessions, err := s.store.GetSessions(p.Limit + p.Offset)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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
			})
		}
		page, total, hasMore := sliceWindow(rows, p)
		json.NewEncoder(w).Encode(map[string]any{
			"sessions": page,
			"total":    total,
			"has_more": hasMore,
		})
		return
	}
	// POST /v1/index-progress — live file progress for a running index job.
	// #535: alongside files_done/files_total/active we now return
	// started_at + elapsed_ms + files_per_sec + eta_ms so the dashboard
	// can render "Indexing… 1234/5678 (~2 min remaining)" instead of
	// just a static counter.
	//
	// Math: rate = done / elapsed; eta = (total - done) / rate. When
	// rate is 0 (no files done yet) eta_ms is null — clients render
	// "estimating..." rather than infinity. Same when an index is
	// inactive: started_at + elapsed_ms + eta_ms are all null because
	// the in-memory progress entry has been deleted.
	if path == "index-progress" && r.Method == http.MethodPost {
		var body struct {
			Project string `json:"project"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		projectID := body.Project
		if projectID == "" {
			projectID = s.sessionID
		}
		done, total, startedAtUnix, active := s.indexer.GetProgressDetail(projectID)
		resp := map[string]any{
			"project":        projectID,
			"files_done":     done,
			"files_total":    total,
			"active":         active,
			"started_at":     nil,
			"elapsed_ms":     nil,
			"files_per_sec":  nil,
			"eta_ms":         nil,
		}
		if startedAtUnix > 0 {
			startedAt := time.Unix(0, startedAtUnix)
			resp["started_at"] = startedAt.Format(time.RFC3339)
			elapsed := time.Since(startedAt)
			elapsedMs := elapsed.Milliseconds()
			resp["elapsed_ms"] = elapsedMs
			if done > 0 && elapsedMs > 0 {
				rate := float64(done) / elapsed.Seconds()
				resp["files_per_sec"] = rate
				if active && total > done {
					remaining := float64(total-done) / rate
					resp["eta_ms"] = int64(remaining * 1000)
				}
			}
		}
		json.NewEncoder(w).Encode(resp)
		return
	}
	// GET /v1/projects — list all indexed projects.
	// #530: pagination via ?limit=&offset=. Default 50, max 200. Returns
	// {projects, total, has_more} so the dashboard can render a "Load
	// more" button without re-counting.
	if path == "projects" && r.Method == http.MethodGet {
		projects, err := s.store.ListProjects()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		page, total, hasMore := sliceWindow(projects, parsePageParams(r, 50, 200))
		json.NewEncoder(w).Encode(map[string]any{
			"projects": page,
			"total":    total,
			"has_more": hasMore,
		})
		return
	}
	// DELETE /v1/projects/empty — bulk-delete every project with zero
	// symbols and zero edges. These accumulate from SessionStart hooks
	// firing in non-code directories and clutter the dashboard.
	if path == "projects/empty" && r.Method == http.MethodDelete {
		n, err := s.store.DeleteEmptyProjects()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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
			writeError(w, http.StatusBadRequest, "bad_request",
				`body must be {"id":"<project-id>"}`)
			return
		}
		if err := s.store.DeleteProject(body.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"deleted": body.ID})
		return
	}

	// availableTools builds the sorted live-registry tool list for 404
	// payloads. Defined here so both the non-POST and POST branches below
	// emit an identical "unknown tool" shape (#714).
	availableTools := func() []string {
		out := make([]string, 0, len(s.handlers))
		for name := range s.handlers {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}

	if r.Method != http.MethodPost {
		// #609: when the path is a known GET-only endpoint, surface a
		// targeted 405 with `Allow: GET, HEAD` instead of the generic
		// "use POST /v1/{tool}" — that message implied the endpoint
		// didn't exist as a tool, sending operators down the wrong
		// debugging path.
		if httpGetOnlyRoutes[path] {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("endpoint /v1/%s requires GET or HEAD", path))
			return
		}
		// #714: a non-POST request to a path that isn't even a known
		// tool is a 404, not a 405 — "use POST" misleads the caller into
		// a round-trip that would ALSO fail (POST on an unknown tool
		// 404s). Only paths that ARE known POST tools get the 405.
		if _, isKnownTool := s.handlers[path]; !isKnownTool {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("unknown tool %q", path),
				map[string]any{"available_tools": availableTools()},
			)
			return
		}
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"method not allowed — use POST /v1/{tool}")
		return
	}

	handler, ok := s.handlers[path]
	if !ok {
		// #609: POST against a known GET-only path must 405, not 404 —
		// the endpoint exists, the verb doesn't.
		if httpGetOnlyRoutes[path] {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("endpoint /v1/%s requires GET or HEAD", path))
			return
		}
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("unknown tool %q", path),
			map[string]any{"available_tools": availableTools()},
		)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	// #714: validate the body is well-formed JSON before handing it to
	// the tool handler. Pre-fix, malformed JSON fell through to
	// parseArgs, which logged a warning and returned an empty args map —
	// so the caller saw a misleading "query is required" (or similar
	// missing-field error) instead of "your JSON is broken." json.Valid
	// is allocation-free; cheap to run on every request.
	if !json.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid_json_body",
			"request body is not valid JSON — check your serialization")
		return
	}

	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      path,
			Arguments: json.RawMessage(body),
		},
	}

	result, err := handler(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// #586+#537: when the handler returns IsError, wrap the plain-text
	// error message in the standardized v0.25 envelope so the HTTP body
	// matches the OpenAPI Error schema referenced from every endpoint's
	// `default` response. Pre-v0.25 used `{error: string}`; v0.25
	// changed to `{error: {code, message, details?}}` so generated SDKs
	// and the dashboard JS can pattern-match on `error.code` instead of
	// substring-checking the message text.
	if result.IsError {
		msg := "empty error result"
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				msg = tc.Text
			}
		}
		// errResultRich (#709/#712) emits a JSON envelope
		// {"error": "...", "_meta": {"next_steps": [...]}} through
		// TextContent. Without unwrapping, that whole JSON string lands
		// verbatim in the standardized envelope's `message` field — a
		// double-encoded blob the client can't pattern-match on. Detect
		// the rich shape, lift the inner `error` string to `message`,
		// and carry next_steps through as `details` so HTTP clients
		// keep the remediation hints. Bare errResult text isn't JSON,
		// so json.Unmarshal fails and msg passes through unchanged.
		message := msg
		var details map[string]any
		var rich map[string]any
		if json.Unmarshal([]byte(msg), &rich) == nil {
			if inner, ok := rich["error"].(string); ok {
				message = inner
				if meta, ok := rich["_meta"].(map[string]any); ok {
					if ns, ok := meta["next_steps"]; ok {
						details = map[string]any{"next_steps": ns}
					}
				}
			}
		}
		if details != nil {
			writeError(w, http.StatusBadRequest, "tool_error", message, details)
		} else {
			writeError(w, http.StatusBadRequest, "tool_error", message)
		}
		return
	}

	// Success path: handlers build a JSON-encoded string explicitly
	// via jsonResultWithMeta and pass it through TextContent verbatim
	// — write through unchanged.
	if len(result.Content) > 0 {
		if tc, ok := result.Content[0].(*mcp.TextContent); ok {
			w.Write([]byte(tc.Text))
			return
		}
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "empty result from handler")
}

// openAPIComponentSchemas defines the shared schemas referenced via
// $ref from per-endpoint response bodies (#581). Two components:
//
//   - Meta: the `_meta` envelope every tool response carries —
//     baseline_method, tokens_used/saved, latency_ms, optional
//     next_steps, optional warnings, optional celebration.
//   - Error: the `{error: string}` shape every handler returns from
//     errResult(). Wired as the `default` response on every endpoint.
//
// Lifting these to components/schemas means a single source of truth
// for the envelope — when the envelope grows a field, the spec
// updates in one place instead of N.
func openAPIComponentSchemas() map[string]any {
	return map[string]any{
		"Meta": map[string]any{
			"type":        "object",
			"description": "Envelope present on every tool response. Tracks token accounting, latency, and (optionally) next-step recommendations + warnings + milestone celebrations.",
			"properties": map[string]any{
				"baseline_method": map[string]any{
					"type":        "string",
					"enum":        []any{"full_file_read", "partial_read", "none"},
					"description": "Which kind of work this tool replaced. `full_file_read` for tools that replace a Read of source files; `partial_read` for repeat-access; `none` for admin / orientation tools with no Read alternative.",
				},
				"tokens_used":      map[string]any{"type": "integer", "description": "Approx tokens spent producing this response."},
				"tokens_saved":     map[string]any{"type": []any{"integer", "null"}, "description": "Approx tokens saved vs reading the underlying source files. `null` when baseline_method=none."},
				"tokens_saved_pct": map[string]any{"type": []any{"number", "null"}, "description": "Bounded form of tokens_saved: percentage of total token cost (saved+used) avoided by this call. Capped at 100; can be negative when the response envelope cost more than the savings. `null` when baseline_method=none."},
				"latency_ms":       map[string]any{"type": "integer", "description": "Server-side handler latency in milliseconds."},
				"next_steps": map[string]any{
					"type":        "array",
					"description": "Suggested next tool calls to keep the agent oriented.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"tool": map[string]any{"type": "string"},
							"args": map[string]any{"type": "string", "description": "JSON-encoded arguments for the suggested tool call."},
							"why":  map[string]any{"type": "string"},
						},
					},
				},
				"warnings":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Non-fatal advisories — typo'd args, unknown property names, etc. (#473, #499, #501)."},
				"celebration": map[string]any{"type": "string", "description": "One-shot tier-milestone line, fired exactly once per installation per tier (#494). Opt-in: only present when PINCHER_CELEBRATIONS=1 is set (default off — #863)."},
				"request_id":  map[string]any{"type": "string", "description": "Correlation ID for end-to-end request tracing (#657). Echoes the inbound X-Request-ID header, or a freshly minted UUID v7 when the request carries none. Also returned in the X-Request-ID response header."},
			},
			"required": []any{"latency_ms", "tokens_used", "request_id"},
		},
		"Error": map[string]any{
			"type":        "object",
			"description": "v0.25 standardized error envelope (#537). Returned for every 4xx/5xx response. BREAKING CHANGE from v0.24: pre-v0.25 returned `{error: \"<text>\"}`; v0.25 returns `{error: {code, message, details?}}` so clients can pattern-match on the machine-readable code instead of substring-checking the text. Standard codes: bad_request, not_found, unauthorized, rate_limited, method_not_allowed, internal_error, tool_error.",
			"properties": map[string]any{
				"error": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"code":    map[string]any{"type": "string", "description": "Machine-readable identifier (snake_case)."},
						"message": map[string]any{"type": "string", "description": "Human-readable description."},
						"details": map[string]any{"type": "object", "description": "Optional structured context (e.g. {\"available_tools\": [...]} on not_found)."},
					},
					"required": []any{"code", "message"},
				},
			},
			"required": []any{"error"},
		},
	}
}

// openAPISpec returns a minimal OpenAPI 3.1 document describing every HTTP tool endpoint.
// Served at GET /v1/openapi.json so any client (Postman, Cursor, copilots) can auto-import.
//
// When deployed behind a reverse proxy, the path keys are prefixed with the
// effective basepath and a "servers" block is added so imported clients build
// the right base URL.
func (s *Server) openAPISpec(r *http.Request) map[string]any {
	prefix := s.effectivePrefix(r)
	// #558 Phase 1: build the path list dynamically from s.handlers.
	// Pre-fix this was a hardcoded slice that drifted every time a new
	// MCP tool was added — dead_code, guide, neighborhood, init were
	// invisible to OpenAPI consumers (Postman imports, copilots) even
	// though they're reachable via the generic /v1/<tool> dispatcher.
	// Iteration is over sorted names so the spec is deterministic
	// across requests and builds (otherwise map iteration order would
	// flip the output every fetch and break HTTP caching).
	names := make([]string, 0, len(s.handlers))
	for name := range s.handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	paths := map[string]any{}
	for _, t := range names {
		// Pull description + input schema from the registered mcp.Tool
		// so the OpenAPI spec mirrors what the agent sees in tools/list.
		// Keeps documentation in lockstep with behavior — no second
		// source of truth to maintain.
		summary := "Call the " + t + " tool"
		var requestSchema any = map[string]any{"type": "object"}
		if tool := s.tools[t]; tool != nil {
			if tool.Description != "" {
				summary = tool.Description
			}
			// InputSchema is typed as `any` on mcp.Tool but every
			// addTool site passes json.RawMessage. Type-assert and
			// re-parse so the OpenAPI consumer gets a real schema
			// (properties, required, enum) instead of the bare
			// {type: object} placeholder.
			if raw, ok := tool.InputSchema.(json.RawMessage); ok && len(raw) > 0 {
				var parsed any
				if err := json.Unmarshal(raw, &parsed); err == nil {
					requestSchema = parsed
				}
			}
		}
		// #581: per-tool response schema. Falls back to the bare
		// {type: object} placeholder when no OutputSchema was
		// registered. The gate test
		// TestOpenAPI_EveryToolHasNonPlaceholderOutputSchema fails
		// CI when a future tool is registered without one.
		var responseSchema any = map[string]any{"type": "object"}
		if raw, ok := s.outputSchemas[t]; ok && len(raw) > 0 {
			var parsed any
			if err := json.Unmarshal(raw, &parsed); err == nil {
				responseSchema = parsed
			}
		}
		// #650: per-tool x-pincher-tier annotation. Static classification
		// available at planning time so router-shaped consumers (detour-
		// shape model routing in particular) can decide which model
		// tier handles the next agent step before the call happens.
		// Also injected into _meta.complexity_tier on every response
		// for consumers reading at call time. Gate test enforces every
		// registered tool has a classification.
		tier := toolComplexityTier(t)
		paths[prefix+"/v1/"+t] = map[string]any{
			"post": map[string]any{
				"operationId":           t,
				"summary":               summary,
				"x-pincher-tier":        tier,
				"x-pincher-idempotent":  toolIsIdempotent(t),
				"requestBody": map[string]any{
					"required": true,
					"content":  map[string]any{"application/json": map[string]any{"schema": requestSchema}},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "Tool result",
						"content":     map[string]any{"application/json": map[string]any{"schema": responseSchema}},
					},
					"default": map[string]any{
						"description": "Error response",
						"content":     map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}},
					},
				},
			},
		}
	}
	paths[prefix+"/v1/health"] = map[string]any{
		"get": map[string]any{
			"operationId":    "health",
			"summary":        "Liveness probe",
			"x-pincher-tier": toolComplexityTier("health"),
			"responses":      map[string]any{"200": map[string]any{"description": "ok"}},
		},
	}
	// #660: GET /v1/ready — readiness probe distinct from /v1/health.
	// k8s deployments need to separate liveness ("process is alive,
	// don't restart me") from readiness ("can serve traffic, route to me").
	paths[prefix+"/v1/ready"] = map[string]any{
		"get": map[string]any{
			"operationId": "ready",
			"summary":     "Readiness probe",
			"description": "Returns 200 when the server can serve traffic; 503 when an essential dependency (store, indexer, schema migration) isn't ready. Use /v1/health for liveness; use /v1/ready for readiness gating in orchestrator manifests.",
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Ready to serve traffic",
					"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"ready":          map[string]any{"type": "boolean"},
							"version":        map[string]any{"type": "string"},
							"schema_version": map[string]any{"type": "integer"},
							"reasons":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
					}}},
				},
				"503": map[string]any{
					"description": "Not ready — see reasons[] for the failing dependency",
					"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"ready":          map[string]any{"type": "boolean"},
							"version":        map[string]any{"type": "string"},
							"schema_version": map[string]any{"type": "integer"},
							"reasons":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
					}}},
				},
			},
		},
	}
	// #654: GET /v1/events — Server-Sent Events stream. Declared as a
	// streaming endpoint so generated clients know the response is a
	// long-lived text/event-stream, not a single JSON body.
	paths[prefix+"/v1/events"] = map[string]any{
		"get": map[string]any{
			"operationId": "events",
			"summary":     "Subscribe to index lifecycle events via Server-Sent Events",
			"description": "Long-lived text/event-stream emitting index_started, index_complete, " +
				"and binary_drift events. On connect, a binary_drift snapshot is sent for every " +
				"currently-drifted project. Honors the --http-key bearer when set.",
			"parameters": []any{
				map[string]any{
					"name":        "project",
					"in":          "query",
					"required":    false,
					"schema":      map[string]any{"type": "string"},
					"description": "Filter the stream to a single project ID.",
				},
			},
			"responses": map[string]any{
				"200": map[string]any{
					"description": "Server-Sent Events stream",
					"content": map[string]any{
						"text/event-stream": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"type": map[string]any{
										"type": "string",
										"enum": []string{"index_started", "index_complete", "binary_drift"},
									},
									"project_id": map[string]any{"type": "string"},
								},
							},
						},
					},
				},
			},
		},
	}
	spec := map[string]any{
		"openapi":    "3.1.0",
		"info":       map[string]any{"title": "pincherMCP HTTP API", "version": s.version},
		"paths":      paths,
		"components": map[string]any{"schemas": openAPIComponentSchemas()},
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

// SetMCPHTTPPath enables the MCP streamable-HTTP transport on the existing
// HTTP server (#651, v0.54). Empty disables. Path is normalized to start
// with "/" and have no trailing slash; "/mcp" is the conventional default.
// Re-uses the HTTP server's --http-key auth, rate limiting, and basepath
// stripping — pincher only needs to be told *where* to mount, not how to
// secure it. Capability `streamable_http` is advertised in _meta.capabilities
// when this is set.
func (s *Server) SetMCPHTTPPath(p string) {
	s.mcpHTTPPath = normalizeBasePath(p)
	// Re-compute capabilities so streamable_http surfaces immediately
	// when the setter is called post-New (matches the New-time path
	// for callers that wire the flag before constructing the server).
	s.capabilities = computeCapabilities(s)
}

// streamableHTTPHandler returns the lazily-constructed MCP streamable-HTTP
// handler. The SDK's NewStreamableHTTPHandler takes a per-request resolver
// that returns the *mcp.Server to handle that request — we always return
// the same singleton, since pincher is a single-tenant primitive (no
// per-session state beyond the SDK's own session table). Built once via
// sync.Once so the SDK construction cost is paid the first time the
// transport is hit, not at startup.
func (s *Server) streamableHTTPHandler() http.Handler {
	s.mcpHTTPHandlerOnce.Do(func() {
		s.mcpHTTPHandler = mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return s.mcp },
			nil, // default options — text/event-stream responses, stateful sessions
		)
	})
	return s.mcpHTTPHandler
}

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
	// #401: cache the projectArg → ID resolution for 60s. Hot path
	// for every MCP call that passes an explicit project arg —
	// pre-fix this hit GetProject (1 SQL) and on miss fell through
	// to ListProjects (full scan). Cache miss returns to the same
	// path; cache hit skips both SQLs. handleIndex invalidates the
	// cache so a freshly-indexed project resolves on the next call
	// instead of waiting for TTL expiry.
	if cached, ok := s.lookupProjectIDCache(projectArg); ok {
		return cached, nil
	}
	// Accept either a name or ID
	p, err := s.store.GetProject(projectArg)
	if err != nil {
		return "", err
	}
	if p != nil {
		s.storeProjectIDCache(projectArg, p.ID)
		return p.ID, nil
	}
	// #997: if projectArg looks like an absolute path, try canonicalizing
	// it via ProjectIDFromPath and looking up the canonical form. On
	// case-insensitive filesystems (Windows NTFS, macOS APFS) a user can
	// reasonably pass either casing — e.g. `D:\ClaudeCode\pincher-repo`
	// or `d:\claudecode\pincher-repo` — but GetProject above is an
	// exact-string match against the stored ID. Without this fallback,
	// the lookup misses despite the directory being byte-identical.
	if filepath.IsAbs(projectArg) {
		if canonicalID := db.ProjectIDFromPath(projectArg); canonicalID != projectArg {
			if cp, cErr := s.store.GetProject(canonicalID); cErr == nil && cp != nil {
				s.storeProjectIDCache(projectArg, cp.ID)
				return cp.ID, nil
			}
		}
	}
	// Try matching by name. When multiple projects share the same
	// name (common after a project gets moved or renamed on disk —
	// the stale row stays in the DB until prune_dead), prefer one
	// whose path still exists on disk over a dead-on-disk
	// collision. Pre-fix, the first match wins regardless of liveness,
	// so a stale empty `D:\…\pincher` row routinely out-resolved the
	// live `D:\ClaudeCode\pincher-repo` and downstream tools returned
	// silently empty results. Caught dogfooding this session.
	all, err := s.store.ListProjects()
	if err != nil {
		return "", err
	}
	var liveMatch, deadMatch *db.Project
	for i := range all {
		proj := &all[i]
		if proj.Name != projectArg {
			continue
		}
		if _, statErr := os.Stat(proj.Path); statErr == nil {
			liveMatch = proj
			break
		}
		if deadMatch == nil {
			deadMatch = proj
		}
	}
	if liveMatch != nil {
		s.storeProjectIDCache(projectArg, liveMatch.ID)
		return liveMatch.ID, nil
	}
	if deadMatch != nil {
		// Live match not found; falling back to a dead-on-disk row.
		// The caller's project arg matched, but the directory is
		// gone — surface this in slog for ops dashboards.
		slog.Warn("pincher.resolve.dead_project_match",
			"project_arg", projectArg,
			"path", deadMatch.Path,
			"recommendation", "prune the dead row (list prune_dead=true) or re-index the intended path")
		s.storeProjectIDCache(projectArg, deadMatch.ID)
		return deadMatch.ID, nil
	}
	return "", fmt.Errorf("project %q not found — use `list` to see available projects", projectArg)
}

// lookupProjectIDCache returns the cached ID for projectArg if
// present and not yet expired. Misses (cold + expired) both
// return ("", false) so the caller falls through to the SQL path.
func (s *Server) lookupProjectIDCache(projectArg string) (string, bool) {
	v, ok := s.projectIDCache.Load(projectArg)
	if !ok {
		return "", false
	}
	entry, ok := v.(*projectIDCacheEntry)
	if !ok || time.Now().After(entry.expiresAt) {
		s.projectIDCache.Delete(projectArg)
		return "", false
	}
	return entry.id, true
}

// storeProjectIDCache records a successful resolution. Empty IDs
// are NOT cached — they would mean "not found", which we want to
// revalidate next call so a freshly-indexed project becomes
// visible immediately.
func (s *Server) storeProjectIDCache(projectArg, id string) {
	if id == "" {
		return
	}
	s.projectIDCache.Store(projectArg, &projectIDCacheEntry{
		id:        id,
		expiresAt: time.Now().Add(projectIDCacheTTL),
	})
}

// invalidateProjectIDCache clears the project resolution cache.
// Called from handleIndex after a successful index/re-index, since
// that's the operation that can introduce a new project name → ID
// mapping or change an existing one.
func (s *Server) invalidateProjectIDCache() {
	s.projectIDCache.Range(func(key, _ any) bool {
		s.projectIDCache.Delete(key)
		return true
	})
}

// Per-tool complexity tier classification (#650). Used by:
//   - openAPISpec to inject `x-pincher-tier` per-endpoint at planning time
//   - jsonResultWithMeta to inject `_meta.complexity_tier` per-response
//
// Tiers (router-routability framing — about response shape, not work cost):
//   - lite:     small structured response, fits in any model context window;
//               pure data; safe to consume from a 7B local model.
//   - standard: medium structured response, agent reasons over it; fine on
//               most frontier models; may strain small local models.
//   - heavy:    synthesis-style output requiring frontier-model parsing
//               (e.g. recommendation paragraphs from `guide`).
//
// Gate test capability_test.go enforces every registered tool has a
// classification — no missing entries, no defaults applied silently.
//
// Adding a new tool requires adding it here in the same PR; CI fails
// otherwise.
var toolComplexityTiers = map[string]string{
	// lite — pure-data, small response
	"search":      "lite",
	"symbol":      "lite",
	"symbols":     "lite",
	"health":      "lite",
	"stats":       "lite",
	"schema":      "lite",
	"list":        "lite",
	"doctor":      "lite",
	"adr":         "lite",
	"index":       "lite",
	"rebuild_fts": "lite",
	"init":        "lite",
	"self_test":   "lite",

	// standard — pure-data, medium response (agent reasons over)
	"context":      "standard",
	"trace":        "standard",
	"query":        "standard",
	"changes":      "standard",
	"neighborhood": "standard",
	"dead_code":    "standard",
	"architecture": "standard",
	"fetch":        "standard",

	// heavy — synthesis-style output requiring frontier parsing
	"guide": "heavy",
}

// toolComplexityTier returns the registered tier for a tool, or the
// empty string if none is registered. The empty case is what the
// gate test catches — production code should never see an unclassified
// tool.
func toolComplexityTier(tool string) string {
	return toolComplexityTiers[tool]
}

// toolIdempotent declares whether the tool is safe to retry without
// side-effects (#659). Routers/aggregators (zelos, bifrost, detour-
// shape gateways) consult this declaration before retrying a failed
// call: idempotent=true means "retry freely"; idempotent=false means
// "the call has side effects, do not retry blindly." Static, tool-
// level declaration — per-action ambiguity (adr set vs adr get) is
// resolved at the tool level as the conservative case (the tool
// EITHER mutates or it doesn't; mixed tools declare false).
//
// Gate test (TestToolIdempotency_EveryToolClassified) enforces every
// registered tool has an entry. Pre-fix this declaration was implicit
// in the code path; routers had to assume "not idempotent" and skip
// retries conservatively. Capability advertisement (#649) carries
// "idempotency_declared" so consumers know the data is available.
var toolIdempotent = map[string]bool{
	// Idempotent (true) — safe to retry.
	"search":       true,
	"symbol":       true,
	"symbols":      true,
	"context":      true,
	"trace":        true,
	"query":        true,
	"guide":        true,
	"changes":      true,
	"fetch":        true,
	"architecture": true,
	"dead_code":    true,
	"neighborhood": true,
	"list":         true,
	"health":       true,
	"stats":        true,
	"schema":       true,
	"doctor":       true,
	"self_test":    true, // read-only smoke-test; runs in a temp project that's cleaned before return

	// Not idempotent (false) — writes/mutations; routers should not blindly retry.
	"index":       false, // writes symbols + edges
	"rebuild_fts": false, // rebuilds storage
	"init":        false, // writes editor config files (write=true path)
	"adr":         false, // mixed: get/list idempotent, set/delete not — declare conservatively
}

// toolIsIdempotent returns the registered idempotency declaration for
// a tool. Used by the OpenAPI spec emitter to stamp
// x-pincher-idempotent on every endpoint.
func toolIsIdempotent(tool string) bool {
	return toolIdempotent[tool]
}

// computeCapabilities builds the per-server capability advertisement
// slice published in _meta.capabilities (#649). Each tag corresponds
// to a feature with a runtime probe in capability_test.go; the
// gate test enforces lockstep between tag and reality so we never
// advertise something the running binary doesn't actually support.
//
// Tag vocabulary kept stable — additions are minor SemVer events
// (routers can rely on absence == not-supported). Removals are major
// SemVer events (any router consuming the removed tag breaks).
//
// Conditional capabilities (those depending on Server-level flags
// rather than always-on binary features) consult the server fields
// at startup. Because capabilities is computed once and cached, a
// runtime flag change (e.g. SetHTTPKey called after New) is NOT
// reflected — a deliberate simplification. If we need runtime-mutable
// capabilities later, the field becomes a func that re-evaluates.
func computeCapabilities(s *Server) []string {
	caps := []string{
		// Schema version — always reflects the current migration head.
		// Routers can pin a minimum schema or refuse to talk to older
		// pincher binaries via this tag.
		"schema_v26",

		// PreToolUse hook intercept (#625, #626, #627, v0.36).
		// `pincher hook-check` is built into every binary post-v0.36;
		// agents wired via `pincher init --target=claude` get
		// runtime tool-call interception.
		"hook_check",

		// Supervised mode (#371, v0.11) — auto-restart-on-drift,
		// initialize-replay across respawn. Routers know the binary
		// can be swapped under them without losing the MCP session.
		"supervised",

		// All operator/admin tools agent-callable via MCP (v0.52,
		// reverses #624). Routers can call any of the 22 tools
		// through MCP without falling back to HTTP.
		"operator_tools_on_mcp",

		// Session counters survive supervised respawn (#420, v0.16).
		// Stats roll-forward across restart so dashboards aren't
		// confused by a fresh process zeroing out cumulative counts.
		"session_persistence",

		// Binary-version drift warning surfaces in _meta when the
		// running binary differs from a project's indexed-by version
		// (#620, v0.34, deduped per-session).
		"binary_drift_warning",

		// Per-call BPE token counts (cl100k_base) in _meta.tokens_used
		// — exact for Claude/OpenAI families, approximate for
		// Gemini/Llama. Stable since v0.4.
		"tokens_used_envelope",

		// Bounded percentage form of tokens_saved (#619, v0.34).
		"tokens_saved_pct",

		// Standardized error envelope on all 4xx/5xx HTTP responses
		// (#537, v0.25). Routers can rely on the {error: {code,
		// message, details}} shape rather than parsing a string.
		"standardized_error_envelope",

		// Per-tool complexity tier in OpenAPI (x-pincher-tier) and
		// _meta.complexity_tier (#650, v0.53). Routers (especially
		// detour-shape model-tier routers) consume to decide which
		// model handles the agent step that consumes the response.
		"complexity_tier",

		// SSE event stream at GET /v1/events (#654, v0.56). Dashboards
		// and CI bots can subscribe to index_started / index_complete /
		// binary_drift instead of polling /v1/health or
		// /v1/index-progress. Always wired when the HTTP gateway is up.
		"sse",

		// Per-tool x-pincher-idempotent declaration in OpenAPI spec
		// (#659, v0.58). Every tool endpoint stamps idempotent=true|false
		// so router/aggregator retry logic can act on a machine-readable
		// declaration rather than assuming "not idempotent" conservatively.
		"idempotency_declared",
	}

	// Conditional capability — present when the operator has wired
	// the bearer token. Routers behind an auth-aware ingress can
	// detect that pincher itself enforces auth and act accordingly.
	if s.httpKey != "" {
		caps = append(caps, "http_auth")
	}

	// Streamable-HTTP MCP transport (#651, v0.54). Present when the
	// operator has mounted the MCP transport on the HTTP server via
	// SetMCPHTTPPath. Routers (zelos, bifrost) deployed in k8s can
	// detect this and skip stdio sub-process spawning.
	if s.mcpHTTPPath != "" {
		caps = append(caps, "streamable_http")
	}

	// Closure-table fast-path for trace (#652 phase 1, v0.54). Present
	// when PINCHER_CLOSURE_TABLES=1 was set at indexer time AND the
	// closure table has rows for at least one project. Routers can
	// surface this to agents as "trace queries on this backend hit
	// closure-table speed (single SELECT, ~1ms) instead of recursive
	// CTE (5–50ms)". Phase 1 trade-off documented: closure rows don't
	// store per-hop edge kind, so the `via` field on trace results is
	// empty when the fast-path fires.
	if db.ClosureEnabled() {
		// Sample any project — we don't need exact-per-project here,
		// just a "is there closure data this binary could serve from"
		// signal. Cheap LIMIT 1 query.
		var n int64
		_ = s.store.DB().QueryRow("SELECT COUNT(*) FROM closure LIMIT 1").Scan(&n)
		if n > 0 {
			caps = append(caps, "closure_tables")
		}
	}

	return caps
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
	// #657: wrap once at the single registration chokepoint so every
	// tool — over stdio AND streamable-HTTP — stamps _meta.request_id.
	wrapped := s.withRequestID(handler)
	s.mcp.AddTool(tool, wrapped)
	s.handlers[tool.Name] = wrapped
	s.tools[tool.Name] = tool
}

// (v0.52: addOperatorTool + makeOperatorRedirectHandler removed. The
// v0.35 #624 surface narrowing — and the v0.51.1 #644 redirect-stub
// mechanism that compensated for it — both deleted. Aggregator-shaped
// deployments (zelos, bifrost, detour) collapsed the agent/operator
// distinction: every pincher tool is now agent-callable via MCP.
// `addTool` is the only registration path. HTTP routes preserved
// for ops automation that already integrates against /v1/<tool>.)

// toolArgKeysFor returns the set of argument keys declared in tool's
// InputSchema.properties — used by unknownArgs to detect typos / unknown
// args (#499). Lazily parses once per tool; cached on s.toolArgKeys.
// Returns nil when the tool isn't registered or its schema can't be
// parsed (caller treats nil as "skip the check, don't false-positive").
func (s *Server) toolArgKeysFor(tool string) map[string]bool {
	s.toolArgKeysOnce.Do(func() {
		s.toolArgKeys = make(map[string]map[string]bool, len(s.tools))
		for name, t := range s.tools {
			raw, ok := t.InputSchema.(json.RawMessage)
			if !ok {
				// All registered tools today supply json.RawMessage;
				// future helper-built schemas would land here. Fall
				// back to marshal-then-unmarshal rather than skip.
				b, err := json.Marshal(t.InputSchema)
				if err != nil {
					continue
				}
				raw = b
			}
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				continue
			}
			keys := make(map[string]bool, len(schema.Properties))
			for k := range schema.Properties {
				keys[k] = true
			}
			s.toolArgKeys[name] = keys
		}
	})
	return s.toolArgKeys[tool]
}

// unknownArgs returns warning strings for any args key NOT declared in
// the tool's InputSchema.properties (#499). The same failure-as-pedagogy
// pattern as #473's pinchQL warnings: silent ignore is the bug; surfacing
// the typo is the fix. Returns nil when the tool's schema can't be
// resolved (don't false-positive on schema parse errors).
func (s *Server) unknownArgs(tool string, args map[string]any) []string {
	allowed := s.toolArgKeysFor(tool)
	if allowed == nil || len(args) == 0 {
		return nil
	}
	var warnings []string
	for k := range args {
		// #622: `verbose` is a universal meta-arg accepted by every tool
		// — it controls _meta envelope shape (drops pedagogy-only
		// next_steps when false). Skipping it here keeps the per-tool
		// InputSchema definitions clean while still letting callers opt
		// into the full envelope on any call.
		if k == "verbose" {
			continue
		}
		if !allowed[k] {
			// Build a sorted hint of accepted keys so the warning is
			// actionable (agent can self-correct on the next call).
			accepted := make([]string, 0, len(allowed))
			for a := range allowed {
				accepted = append(accepted, a)
			}
			sort.Strings(accepted)
			warnings = append(warnings, fmt.Sprintf(
				"unknown arg %q for tool %q — accepted: %s",
				k, tool, strings.Join(accepted, ", "),
			))
		}
	}
	sort.Strings(warnings) // deterministic order for tests
	return warnings
}

func (s *Server) registerTools() {
	// 1. index — restored to MCP-visible in v0.51 (#645). The v0.35 #624
	// surface narrowing swept index into the operator-only bucket; real-
	// user feedback showed agents need it on the working set to (a) help
	// onboard fresh repos, (b) recover from binary-version drift surfaced
	// in `_meta.binary_version_warning`, (c) close the in-session-edit
	// race the watcher's 2s tick can't cover. HTTP route stays for backward
	// compat. See docs/REFERENCE.md → "Indexing modes" for the full
	// staleness model.
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
		Description: "**Use instead of repeated `symbol` calls** when you have several IDs. Batch fetches up to 100 symbols in a single SQL round trip + per-symbol byte-offset reads. Returns `[{id, source, signature, file_path, start_line}, ...]` in the same order as the input `ids`. Missing IDs surface as `{id, error: \"not found\"}` rather than failing the whole batch. Pass `fields=id,name,signature` to drop unused fields and skip the disk-read for source.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["ids"],"properties":{
				"ids":{"type":"array","items":{"type":"string"},"description":"Array of stable symbol IDs."},
				"project":{"type":"string"},
				"fields":{"type":"string","description":"Comma-separated fields to include per result, e.g. 'id,name,signature'. Omit for all fields. Skipping 'source' avoids the per-symbol disk read entirely — major win on 50+ ID batches where the agent only needs metadata."}
			}
		}`),
	}, s.handleSymbols)

	// 4. context
	s.addTool(&mcp.Tool{
		Name:        "context",
		Description: "**Use before editing a function** to read it together with everything it directly imports and calls — one shot, ~90% token reduction vs reading files. Returns `{symbol: {source, ...}, imports: [{source, ...}], callees: [{source, ...}]}` — `imports` is cross-package dependencies (IMPORTS edges), `callees` is the in-package helpers it directly calls (CALLS edges). De-duplicated so a symbol that's both imported and called only appears once. Prefer this over `symbol` whenever you need to understand how a function works in context, not just see its source. Pass `fields=symbol,callees` to drop sections you don't need. Pass `lite=true` for source-only retrieval — minimum-envelope shape used by the PreToolUse hook redirect when replacing a Read call.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"Symbol ID to fetch with its imports."},
				"project":{"type":"string"},
				"fields":{"type":"string","description":"Comma-separated top-level keys to include, e.g. 'symbol,callees' to drop imports. Omit for all. _meta is preserved unconditionally."},
				"lite":{"type":"boolean","description":"When true, return only {id, source} — no imports, no callees, no next_steps. Used by PreToolUse hook to land on minimum-envelope shape when redirecting a Read call. Default false."}
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
				"project":{"type":"string","description":"Project name or ID. Defaults to session project. Pass '*' to search across every indexed project (cross-repo lookups)."},
				"kind":{"type":"string","description":"Filter by symbol kind: Function|Method|Class|Interface|Enum|Type|Variable|Module|Setting|Section|Document|Resource|DataSource|Output|Local|Provider|Block"},
				"language":{"type":"string","description":"Filter by language: Go|Python|TypeScript|HCL|YAML|Markdown|etc"},
				"corpus":{"type":"string","enum":["","code","config","docs"],"description":"FTS5 corpus to search. Default (omitted or '') is 'code' — source-code identifiers (Function/Method/Class/etc). 'config' restricts to YAML/JSON/HCL/TOML Settings/Resources/Outputs; 'docs' to Markdown sections + fetched Documents. Use a specific corpus to avoid BM25 dilution from unrelated symbol kinds. (The legacy 'all' value was removed in v0.5; older callers passing 'all' are soft-redirected to 'code' with a deprecation log line.)"},
				"limit":{"type":"integer","description":"Max results returned in this page (default 20, max 500)."},
				"offset":{"type":"integer","description":"Skip this many BM25-ranked results before the page. Default 0, max 5000. Used for paginated 'Load more' UX (#532). Response includes total + has_more so callers can decide whether to page deeper."},
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
				"project":{"type":"string","description":"Project name or ID. Defaults to session project. Pass '*' to query across every indexed project (cross-repo graph lookups)."},
				"max_rows":{"type":"integer","description":"Max rows (default 200, max 10000)"},
				"min_confidence":{"type":"number","description":"Minimum extraction_confidence (0.0-1.0). Default 0.0 (no filter). Filters rows whose query selects an extraction_confidence column; rows from queries that don't return confidence are unaffected."}
			}
		}`),
	}, s.handleQuery)

	// 7. trace
	s.addTool(&mcp.Tool{
		Name:        "trace",
		Description: "**Use before changing behaviour** that other code depends on, to find callers (inbound) or what it calls (outbound). Risk labels: CRITICAL=direct callers, HIGH=2 hops, MEDIUM=3 hops. Pass `name` for the common case; when the name is ambiguous (multiple symbols share it) trace falls back to the first match and surfaces alternatives in `_meta.ambiguous_match`. To trace a specific alternative, pass `id=` with the exact symbol ID from search/symbols/query — that's the disambiguation escape hatch (#474). Default traversal follows CALLS-family edges; pass `kinds=READS,WRITES` to trace data-flow edges instead (or `kinds=CALLS,READS` to mix). Test files and testdata/ fixtures are filtered by default; pass `include_tests=true` to see test coverage of a symbol. When `depth` is omitted, the result is auto-trimmed to the smallest depth with ≥5 hops (so hotspots don't dump 100+ rows); `_meta.depth_used` reports the trim. Pass `depth=N` explicitly to skip the trim.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"name":{"type":"string","description":"Function name to trace (short name, e.g. 'ProcessOrder'). When ambiguous, trace picks the first match and surfaces alternatives via _meta.ambiguous_match — pass id= instead to trace a specific alternative."},
				"id":{"type":"string","description":"Exact symbol ID to trace (e.g. 'internal/db/db.go::db.Open#Function'). Use this when name resolution would be ambiguous, or when you already have the ID from search/symbols/query. Mutually exclusive with name (id wins). #474."},
				"project":{"type":"string"},
				"direction":{"type":"string","enum":["outbound","inbound","both"],"description":"outbound=what it calls, inbound=what calls it. Default: both"},
				"depth":{"type":"integer","description":"BFS depth 1-5 (default 3)"},
				"risk":{"type":"boolean","description":"Add CRITICAL/HIGH/MEDIUM/LOW risk labels (default true)"},
				"min_confidence":{"type":"number","description":"Minimum extraction_confidence (0.0-1.0). Default 0.0 (no filter). Hops whose target symbol scores below the threshold are excluded from the result."},
				"kinds":{"type":"string","description":"Comma-separated list of edge kinds to traverse (e.g. 'CALLS' or 'READS,WRITES'). Default: CALLS-family (CALLS,HTTP_CALLS,ASYNC_CALLS) — covers the typical 'who calls this' use case. Pass READS / WRITES (Go vars only, see #264/#265) to follow data-flow edges. Whitespace and case-insensitive."},
				"include_tests":{"type":"boolean","description":"If true, surface hops in test files (*_test.go, *.spec.ts, etc.) and testdata/ fixtures. Default false — tests flood inbound traces on hotspots without orientation value, so they're filtered like architecture's hotspot list. Set true when you genuinely want to see a symbol's test coverage."},
				"fields":{"type":"string","description":"Comma-separated top-level keys to include, e.g. 'hops,total' to drop risk_summary. Per-hop fields are NOT trimmed — pass shape via downstream symbol/symbols calls. _meta is preserved."}
			}
		}`),
	}, s.handleTrace)

	// 8. changes
	s.addTool(&mcp.Tool{
		Name:        "changes",
		Description: "**Use before final response after code edits** to surface the blast radius. Maps `git diff` to affected symbols, BFS-traces impact, returns `changed_symbols` + impacted callers tagged CRITICAL/HIGH/MEDIUM/LOW + summary counts + `tests_to_run` (test functions that exercise the changed symbols, ranked by overlap descending — re-run the top entries before pushing). Scopes: `unstaged` (default) / `staged` / `all` (includes untracked) / `base:<branch>` (committed-only diff vs <branch>'s merge-base — use this to preview a PR's blast radius before opening it).",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string"},
				"scope":{"type":"string","description":"Which diff to analyse: 'unstaged' (default), 'staged', 'all' (includes untracked), or 'base:<branch>' (committed diff vs merge-base of <branch>, e.g. 'base:master' for pre-PR blast radius)"},
				"depth":{"type":"integer","description":"Blast radius BFS depth 1-5 (default 3)"},
				"fields":{"type":"string","description":"Comma-separated top-level keys to include, e.g. 'summary,tests_to_run' to drop changed_symbols+impacted lists. Common shape on chained PR-prep flow: agent only needs the summary + which tests to run. _meta is preserved."}
			}
		}`),
	}, s.handleChanges)

	// 9. dead_code — agent-facing exploration tool. Restored to MCP-visible
	// in v0.52 (full reversal of #624) after the aggregator-deployment shape
	// (zelos, bifrost, detour) made the v0.35 narrowing argument obsolete.
	s.addTool(&mcp.Tool{
		Name:        "dead_code",
		Description: "**Find unreachable internal functions/methods** — symbols with zero inbound edges (CALLS/READS/WRITES/REFERENCES/IMPORTS) that aren't exported, aren't entry points, and aren't tests. The inverse of `architecture` hotspots. Defaults bias toward precision: `language=Go` (1.0-confidence AST extraction) + `kinds=Function,Method`. Lower `min_confidence` and broaden `kinds` at the cost of false positives from regex-tier extractors that under-resolve cross-file edges. Test fixtures under `testdata/` and `__fixtures__/` are post-filtered out — they have no test runners but aren't real code either.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string","description":"Project name or ID. Defaults to session project."},
				"language":{"type":"string","description":"Language filter, e.g. 'Go'. Default: empty (all languages). Recommended: 'Go' until non-Go AST extractors land — regex extractors under-resolve cross-file CALLS so non-Go results have higher false-positive rates."},
				"kinds":{"type":"string","description":"Comma-separated kinds to consider, e.g. 'Function,Method' (default), 'Function,Method,Class'. Setting/Variable/Section are excluded by default since dead-DATA has different semantics than dead-code."},
				"min_confidence":{"type":"number","description":"Minimum extraction_confidence (default 0.95 — biases to AST-extracted languages). Drop to 0.0 to include regex-tier languages (higher false-positive rate)."},
				"limit":{"type":"integer","description":"Max symbols returned (default 100, max 500)."}
			}
		}`),
	}, s.handleDeadCode)

	// 10. architecture — agent-facing orient tool. v0.52 reversal of #624.
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

	// 11. schema — agent-callable introspection. v0.52 reversal of #624.
	s.addTool(&mcp.Tool{
		Name:        "schema",
		Description: "**Use before writing a `query`** to see what node/edge kinds exist in this project. Returns node-kind counts (Function, Class, Method, …), edge-kind counts (CALLS, IMPORTS, …), and totals.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string"}
			}
		}`),
	}, s.handleSchema)

	// 12. list — cross-project enumeration. v0.52 reversal of #624.
	s.addTool(&mcp.Tool{
		Name:        "list",
		Description: "**Use to confirm which projects are indexed** before scoping a query with `project=`. Returns `[{name, path, files, symbols, edges, indexed_at}, ...]` for active projects. Paginated: defaults to 50 entries per call (limit/offset), with the next page surfaced in `_meta.next_steps` when more remain. Defaults filter out projects whose on-disk path no longer exists, whose last index is older than `active_within_days` (14 by default), or that have zero edges (typically empty worktrees). Pass `active=false`/`include_dead=true`/`min_edges=0` to widen the filter, `limit=0` for the legacy unbounded dump, `prune_dead=true` to physically remove dead-on-disk projects from the store.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"active":{"type":"boolean","description":"Filter to projects indexed within active_within_days. Default true."},
			"active_within_days":{"type":"integer","description":"Activity window for active=true. Default 14."},
			"include_dead":{"type":"boolean","description":"Include projects whose on-disk path no longer exists in the response. Default false. Orthogonal to prune_dead — combine for an audit + cleanup pass (#378)."},
			"prune_dead":{"type":"boolean","description":"Permanently delete projects whose on-disk path no longer exists from the store. Default false. Pruned ids returned in the pruned field. Combine with include_dead=true to see what got deleted in the same call."},
			"min_edges":{"type":"integer","description":"Drop projects whose edge count is below this threshold (#419). Default 1 — hides empty-graph worktrees and pre-extraction stubs. Pass 0 for the legacy unfiltered shape."},
			"limit":{"type":"integer","description":"Max rows returned per page. Default 50. Pass 0 for legacy unbounded behaviour."},
			"offset":{"type":"integer","description":"Skip the first N rows (default 0). Use the value from _meta.next_steps to walk pages."}
		}}`),
	}, s.handleList)

	// 12. adr — restored to MCP-visible in v0.51 (#645). Same v0.35 #624
	// over-correction as `index`: institutional memory tool the agent
	// reads + writes mid-session per global CLAUDE.md policy. Operator-
	// only access defeated that workflow. HTTP route preserved.
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

	// 13. health — drift signals + extraction coverage. v0.52 reversal of #624.
	s.addTool(&mcp.Tool{
		Name:        "health",
		Description: "**Use to verify extraction quality before trusting graph results**, or to detect a stale index. Returns schema version, index staleness, and per-language coverage with parser identity (AST vs Regex) and avg/p10/p50 confidence per (language, kind). A low p10 on a corpus you care about means `search` results in that area need a higher `min_confidence` to be reliable.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string","description":"Project to report on. Defaults to session project."}
			}
		}`),
	}, s.handleHealth)

	// 14. stats — session savings + cumulative counters. v0.52 reversal of #624.
	s.addTool(&mcp.Tool{
		Name:        "stats",
		Description: "**Use to track context-budget savings** for the current session and all-time. Returns tokens used, tokens saved (vs reading whole files), call count, plus per-project index size (files, symbols, edges). Useful as a sanity check that pincher tools are being preferred over `Read`/`Grep` — if `tokens_saved` is 0 after a chunk of work, the agent is probably bypassing the index.",
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

	// 17. neighborhood — graph view around a symbol. v0.52 reversal of #624.
	s.addTool(&mcp.Tool{
		Name:        "neighborhood",
		Description: "**Returns same-file symbols, NOT graph adjacency.** Despite the name (#498), this tool answers \"what other symbols live in the same file as the seed?\" — useful for in-file refactor planning. For graph adjacency (callers / callees / readers / writers), use `trace direction=both` instead. Given a seed symbol ID, returns every symbol in the same file (signatures + line ranges) ordered by source position. One round-trip vs N `symbol` calls or one whole-file `Read`. Paginated: defaults to 50 neighbors per call (limit/offset), with the next page surfaced in `_meta.next_steps` when the file has more. Default response excludes `source`; pass `include_source=true` to also fetch each neighbor's body.",
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

	// 18. init — agent-callable for new-repo onboarding via aggregator
	// (e.g., zelos surfacing "user opened repo X" → agent fires init).
	// v0.52 reversal of #624. Description leads with the side-effect
	// warning so agents confirm with the user before invoking.
	s.addTool(&mcp.Tool{
		Name:        "init",
		Description: "**Seed an editor's pincher usage policy file** without dropping into a separate shell. Same surface as `pincher init` CLI but defaults to dry-run for safety; pass `write=true` to actually mutate files. Targets: claude / cursor / cursor-legacy / windsurf / aider / codex / zed / gemini / warp / vscode (Copilot rules) / vscode-mcp (Copilot Chat MCP) / detect / all. The continue target is rejected (always-global, escapes project scope from an MCP context). Returns per-target {target, path, action, diff_preview, bytes_in, bytes_out}.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"target":{"type":"string","description":"Editor target: claude|cursor|cursor-legacy|windsurf|aider|codex|zed|gemini|warp|vscode|vscode-mcp|detect|all. Default: detect."},
				"write":{"type":"boolean","description":"If true, mutate target files. Default false (dry-run)."},
				"project_path":{"type":"string","description":"Project root override. Defaults to the session project root."}
			}
		}`),
	}, s.handleInit)

	// 19. doctor — drift + sanity diagnostics. v0.52 reversal of #624.
	s.addTool(&mcp.Tool{
		Name:        "doctor",
		Description: "**Diagnostic report from the local pincher database** — schema version, DB + WAL file sizes, per-project staleness, recent extraction failures, recent slow queries. Same data the `pincher doctor --json` CLI returns; exposed via MCP so dashboards and ops automations can poll without shelling out. Read-only; safe to call repeatedly.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"lookback_hours":{"type":"integer","description":"Hours of history to include in failures + slow-query lists. Default 168 (7 days)."},
				"top":{"type":"integer","description":"Maximum entries returned per section — caps the extraction-failures list, the slow-queries list, AND the projects list (#575, response-size bound on multi-project installs). Projects are sorted by symbol count desc, so the largest are kept; projects_truncated reports the count omitted. Use the list tool for full project enumeration. Default 10."}
			}
		}`),
	}, s.handleDoctor)

	// 20. rebuild_fts — agent-callable for "search feels broken, refresh
	// the FTS5 index." Slow on large projects; description warns. v0.52
	// reversal of #624.
	s.addTool(&mcp.Tool{
		Name:        "rebuild_fts",
		Description: "**Admin: rebuild every FTS5 index from source data.** Equivalent to `pincher rebuild-fts` CLI. Use after symptoms of FTS corruption (search results missing symbols you can confirm exist via `query`). Long-running on large indexes (~1 second per ~10k symbols). Mutates DB; requires confirm=true to actually run — without it, returns the projected work without touching anything.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"confirm":{"type":"boolean","description":"If true, perform the rebuild. Default false (dry-run — returns symbol counts only)."}
			}
		}`),
	}, s.handleRebuildFTS)

	// 21. self_test — internal probe (expensive). v0.52 reversal of #624.
	s.addTool(&mcp.Tool{
		Name:        "self_test",
		Description: "**Smoke-test the pincher install** by exercising the index → search → byte-offset-retrieve loop. Equivalent to `pincher self-test` CLI. Returns per-step pass/fail. Useful as a liveness check after a binary upgrade or in CI. Read-only; uses a temp project that's cleaned up before return.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{}
		}`),
	}, s.handleSelfTest)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool handlers
// ─────────────────────────────────────────────────────────────────────────────

// diagnoseEmptyIndex explains why a zero-symbol index result happened.
// Returns nil when result.Symbols > 0 (no diagnosis needed). The branches are
// ordered most-specific-first; #425 split the legacy "files processed but no
// symbols" bucket into a benign incremental case (symbol-neutral edits) vs the
// genuine extractor-missing case so callers can act on the difference.
func diagnoseEmptyIndex(result *index.IndexResult, force bool) map[string]any {
	if result == nil || result.Symbols != 0 {
		return nil
	}
	switch {
	case result.Files == 0 && result.Blocked == 0 && result.Skipped == 0:
		return map[string]any{
			"diagnosis": "no indexable source files found at this path",
			"hint":      "verify the path is a project root (contains code in a recognised language) or check `pincher health` for indexing failures",
		}
	case result.Files == 0 && result.Blocked > 0:
		return map[string]any{
			"diagnosis": fmt.Sprintf("all %d files were blocked by ast.ShouldSkip (lockfiles, minified bundles, source maps)", result.Blocked),
			"hint":      "expected for vendor-only or build-artifact-only directories; index a parent directory if your sources are nested elsewhere",
		}
	case result.Files == 0 && result.Skipped > 0 && !force:
		return map[string]any{
			"diagnosis": fmt.Sprintf("incremental index — all %d files unchanged since last run", result.Skipped),
			"hint":      "this is the expected fast path. Pass `force=true` if you suspect index corruption.",
		}
	case result.Files > 0 && result.Skipped > 0:
		// #425: incremental re-index where some files were reprocessed but
		// the edits were symbol-neutral (comments, whitespace, body changes
		// that didn't add/remove declarations). Not a bug — distinguish
		// from the genuine extractor-missing case below.
		return map[string]any{
			"diagnosis": fmt.Sprintf("incremental re-index: %d files unchanged, %d reprocessed without new or removed symbols", result.Skipped, result.Files),
			"hint":      "no action needed — symbol-neutral edits (comments, whitespace, body-only changes) produce zero net symbol delta",
		}
	default:
		return map[string]any{
			"diagnosis": "files were processed but no symbols extracted",
			"hint":      "language detection may be missing extension support; check `pincher health` per-language coverage",
		}
	}
}

func (s *Server) handleIndex(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	path := str(args, "path")
	if path == "" {
		path = s.sessionRoot
	}
	if path == "" {
		// #712: failure-as-pedagogy — index needs an absolute path; show
		// the shape and point at `list` to see what's already indexed.
		return s.errResultRich("path is required (no session root detected)", []map[string]string{
			{"tool": "index", "args": `{"path":"/abs/path/to/repo"}`,
				"why": "index walks a repo and builds the symbol graph — pass an absolute path"},
			{"tool": "list", "args": `{}`,
				"why": "see which projects are already indexed before re-indexing"},
		}), nil
	}
	force := boolArg(args, "force")

	// #790: refuse the catastrophic index targets (filesystem root,
	// $HOME) on the MCP surface too — pre-fix only the `pincher index`
	// CLI ran this guard, so an MCP `index` call could point at / or
	// $HOME and bloat the shared DB with millions of cache "symbols".
	// hookMode=false: an explicit `index` tool call is a deliberate
	// action, same trust level as a manual CLI invocation — only the
	// catastrophic cases trip, not the project-marker check.
	if trap, reason := index.IsBloatTrap(path, false); trap {
		return s.errResultRich(fmt.Sprintf("refusing to index %q (%s)", path, reason), []map[string]string{
			{"tool": "index", "args": `{"path":"/abs/path/to/repo"}`,
				"why": "point index at a project root, not a filesystem root or your home directory"},
			{"tool": "list", "args": `{}`,
				"why": "see which projects are already indexed"},
		}), nil
	}

	// F1: refuse re-index when the existing project was stamped by a
	// newer pincher binary. Running our older parsing logic over a
	// project a newer binary already understood would silently
	// reintroduce extraction regressions the newer one fixed. The
	// resolution is "upgrade pincher", not "let the older binary
	// rewrite the data".
	if existingID := db.ProjectIDFromPath(path); existingID != "" {
		if err := s.checkDriftForWrite(existingID); err != nil {
			return errResult(err.Error()), nil
		}
	}

	result, err := s.indexer.Index(ctx, path, force)
	if err != nil {
		// #712: failure-as-pedagogy — the most common index error is a
		// path that doesn't exist on disk; point at `list` so the caller
		// can check what IS indexed and confirm the path shape.
		return s.errResultRich(fmt.Sprintf("index error: %v", err), []map[string]string{
			{"tool": "list", "args": `{}`,
				"why": "confirm the path — list shows every indexed project's absolute path"},
		}), nil
	}

	// #401: a successful index/re-index can introduce new project
	// name → ID mappings or change existing ones. Clear the cache
	// so the next resolveProjectID call sees the fresh state
	// instead of waiting for the 60s TTL.
	s.invalidateProjectIDCache()

	// #734: IndexResult.Symbols/.Edges/.Files are per-run accumulators —
	// .Symbols/.Files count only files reprocessed this run, and .Edges is
	// further inflated by the whole-project resolve passes (resolveImports/
	// resolveCalls/resolveReads rebuild the entire cross-file edge set every
	// run and add their full count to totalEdges). Reporting the raw struct
	// fields made `index` disagree with `health` after every incremental
	// re-index (observed: edges 12275 vs health's 15032, symbols 0 vs 5126).
	// The CLI json/text path already fetches true totals from GraphStats
	// (see cmd/pinch/main.go) — do the same here so the MCP surface agrees.
	totalSyms, totalEdges, _, _, _ := s.store.GraphStats(result.ProjectID)

	data := map[string]any{
		"project":     result.Project,
		"path":        result.Path,
		"files":       result.Skipped + result.Files, // total files in the graph (matches health + CLI)
		"symbols":     totalSyms,                      // DB graph total, not this run's delta
		"edges":       totalEdges,                     // DB graph total, not this run's delta
		"reprocessed": result.Files,                   // files actually re-extracted this run
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
	if meta := diagnoseEmptyIndex(result, force); meta != nil {
		data["_meta"] = meta
	}
	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

func (s *Server) handleSymbol(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	id := str(args, "id")
	if id == "" {
		// #712: failure-as-pedagogy — point the caller at `search`, which
		// is where symbol IDs come from.
		return s.errResultRich("id is required", []map[string]string{
			{"tool": "search", "args": `{"query":"<symbol-name>"}`,
				"why": "search returns symbol IDs — pass one back as symbol's id"},
		}), nil
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
		// #704: not-found path teaches the next move. The natural
		// remediation is `search` by short name (handles typos, case,
		// and stale IDs that didn't make symbol_moves).
		return s.errResultRich(
			fmt.Sprintf("symbol %q not found", id),
			[]map[string]string{
				{"tool": "search", "args": fmt.Sprintf(`{"query":%q}`, shortNameFromID(id)),
					"why": "id resolution failed (also tried symbol_moves redirect) — search by short name"},
				{"tool": "list", "args": "{}",
					"why": "if no project matches, the right project may not be indexed"},
			},
		), nil
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

	// #766: for a Document the content is returned in `source`; its
	// Docstring holds the *same* bytes, so echoing both doubles the
	// payload. A Document has no doc-comment distinct from its content —
	// blank the docstring field and let `source` be the canonical text.
	docstring := sym.Docstring
	if sym.Kind == "Document" {
		docstring = ""
	}

	// Estimate token savings vs. reading the whole file.
	// Baseline: agent would read the entire file to find this symbol on
	// first access. On repeat access this session, the file is already in
	// context — charge 0 (#478).
	tokensSaved := 0
	if s.markFileAccessed(sym.ProjectID, sym.FilePath) {
		fileSizeBytes := avgFileSize // conservative fallback
		if root != "" {
			if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(sym.FilePath))); err == nil {
				fileSizeBytes = int(fi.Size())
			}
		}
		symbolBytes := sym.EndByte - sym.StartByte
		tokensSaved = max(0, fileSizeBytes-symbolBytes) / charsPerToken
	}

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
		"docstring":             docstring,
		"complexity":            sym.Complexity,
		"is_exported":           sym.IsExported,
		"extraction_confidence": sym.ExtractionConfidence,
		"source":                source,
	}

	// #908/#914: route through the shared projection-with-check helper
	// so unknown fields are warned instead of being included as null.
	// Pre-#908 `data[f] = allFields[f]` returned `nil` for an unknown
	// key — the response carried `{nonexistent_field: null}` which
	// lied about the field's existence. #914 hoisted the check pattern
	// out of this handler so trace / changes match the same shape.
	data := projectAndCheckFields(allFields, fieldSet)
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

// attachStalenessWarningsForPaths checks each unique file path against
// the on-disk hash and emits one _meta.warnings entry per stale file
// (#980). One re-index next_step is appended at most — `force=true`
// covers every stale file at once, so multiplying that hint per path
// would just spam. Used by handleContext for the imports/callees
// dependency files, which are read via the same byte-offset path as
// the seed and silently shipped stale bytes pre-fix.
func (s *Server) attachStalenessWarningsForPaths(data map[string]any, projectID string, paths []string, root string) {
	if root == "" || len(paths) == 0 {
		return
	}
	seen := make(map[string]bool, len(paths))
	var stale []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		stored := s.store.GetFileHash(projectID, p)
		if stored == "" {
			continue
		}
		live, ok := fileHashOnDisk(filepath.Join(root, filepath.FromSlash(p)))
		if !ok || live == stored {
			continue
		}
		stale = append(stale, p)
	}
	if len(stale) == 0 {
		return
	}
	meta, _ := data["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	warnings, _ := meta["warnings"].([]string)
	for _, p := range stale {
		warnings = append(warnings,
			fmt.Sprintf("dependency file %q modified since last index — source bytes may not match the symbol; re-index to refresh", p))
	}
	meta["warnings"] = warnings
	// Append a single force-reindex next_step only if one isn't already
	// present from the seed-staleness path (#317). A repeat would just
	// noise the response — one force=true covers all stale files.
	steps, _ := meta["next_steps"].([]map[string]string)
	hasReindex := false
	for _, st := range steps {
		if st["tool"] == "index" && strings.Contains(st["args"], `"force":true`) {
			hasReindex = true
			break
		}
	}
	if !hasReindex {
		steps = append(steps, map[string]string{
			"tool": "index",
			"args": nextStepArgs(map[string]any{"path": root, "force": true}),
			"why":  "dependency files changed since last index — re-index so byte offsets match the current source",
		})
		meta["next_steps"] = steps
	}
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

// changesMaxList caps the `impacted` and `tests_to_run` lists in a
// `changes` response. A change to a hot file (e.g. cypher/engine.go's
// parseQuery) blast-radiuses into 100+ symbols + 100+ tests; the full
// response then exceeds the MCP token limit and the tool fails by
// default. The summary keeps the true totals; the lists keep the
// highest-signal entries (impacted by risk, tests by overlap).
const changesMaxList = 50

// adrKeyMaxLen + adrValueMaxLen bound the ADR set action's input
// (#534). The dashboard's input/textarea elements set matching
// maxlength attributes so the bounds enforce uniformly across UI
// and server. 256-char keys are display labels (e.g.
// "auth-rewrite-2026"); 16 KB values cover real ADRs with room
// for embedded code blocks but reject paste-of-an-entire-transcript
// pathology that #534 motivated.
const (
	adrKeyMaxLen   = 256
	adrValueMaxLen = 16 * 1024
)

func (s *Server) handleSymbols(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	ids := strSlice(args, "ids")
	if len(ids) == 0 {
		// #712: failure-as-pedagogy — an empty batch is most often "I
		// don't have IDs yet". Point at the tools that produce them.
		return s.errResultRich("ids array is required", []map[string]string{
			{"tool": "search", "args": `{"query":"<symbol name>"}`,
				"why": "search returns symbol IDs — feed them back into symbols as the ids array"},
			{"tool": "query", "args": `{"pinchql":"MATCH (n:Function) RETURN n.id LIMIT 20"}`,
				"why": "a pinchQL query projecting n.id yields a batch of IDs to fetch"},
		}), nil
	}
	if len(ids) > maxBatchSymbols {
		return s.errResultRich(
			fmt.Sprintf("too many ids: max %d per call, got %d", maxBatchSymbols, len(ids)),
			[]map[string]string{
				{"tool": "symbols", "args": fmt.Sprintf(`{"ids":[ ...first %d... ]}`, maxBatchSymbols),
					"why": fmt.Sprintf("split the batch — call symbols repeatedly in pages of %d", maxBatchSymbols)},
			}), nil
	}

	projectArg := str(args, "project")
	// #400: per-entry field projection. Caller-driven cut applied
	// to each row in the `symbols` array. nil = all fields.
	fieldSet := parseFieldsArg(str(args, "fields"))
	// #908: validate the requested fields against the known entry shape
	// up-front. Pre-fix unknown fields were silently dropped by
	// projectFields, so a typo'd field name (`fields=id,naem`) gave no
	// signal that the caller's projection was malformed.
	var symbolsUnknownFields []string
	if fieldSet != nil {
		knownFields := map[string]bool{
			"id": true, "name": true, "qualified_name": true, "kind": true,
			"language": true, "file_path": true, "start_line": true, "end_line": true,
			"start_byte": true, "end_byte": true, "signature": true, "return_type": true,
			"docstring": true, "complexity": true, "is_exported": true,
			"extraction_confidence": true, "source": true, "error": true, "_meta": true,
		}
		for f := range fieldSet {
			if !knownFields[f] {
				symbolsUnknownFields = append(symbolsUnknownFields, f)
			}
		}
		sort.Strings(symbolsUnknownFields)
	}
	includeSource := fieldSet == nil || fieldSet["source"]
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
		if includeSource {
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
		}
		// #766: blank a Document's docstring — its text is already in
		// `source`; echoing both doubles the payload. Mirrors handleSymbol.
		docstring := sym.Docstring
		if sym.Kind == "Document" {
			docstring = ""
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
			"docstring":             docstring,
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
		// Apply per-entry projection. _meta (the staleness warning
		// attached just above) is preserved by projectFields.
		entry = projectFields(entry, fieldSet)
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
	// #908: batch-level warning for fields that didn't match any known
	// entry key. Single warning rather than per-entry to keep the
	// response shape clean — the projection failure mode is the same
	// across all rows since they share a schema.
	if len(symbolsUnknownFields) > 0 {
		knownKeys := []string{
			"complexity", "docstring", "end_byte", "end_line", "error",
			"extraction_confidence", "file_path", "id", "is_exported",
			"kind", "language", "name", "qualified_name", "return_type",
			"signature", "source", "start_byte", "start_line",
		}
		attachWarning(data, fmt.Sprintf(
			"fields %v matched no keys and were dropped; valid keys: %v",
			symbolsUnknownFields, knownKeys))
	}
	return s.jsonResultWithMeta(data, start, tool, args, s.savedVsFileSizesSession(resolvedProjectID, root, filePaths, responseJSON)), nil
}

func (s *Server) handleContext(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	id := str(args, "id")
	if id == "" {
		// #712: failure-as-pedagogy — context needs a symbol ID; search
		// is the tool that produces them.
		return s.errResultRich("id is required", []map[string]string{
			{"tool": "search", "args": `{"query":"<symbol-name>"}`,
				"why": "search returns symbol IDs — pass one back as context's id"},
		}), nil
	}
	// #400: response-level field projection. Caller-driven cut.
	fieldSet := parseFieldsArg(str(args, "fields"))

	sym, err := s.store.GetSymbol(id)
	if err != nil || sym == nil {
		return errResult(fmt.Sprintf("symbol %q not found", id)), nil
	}

	root, _ := s.resolveProjectRoot(sym.ProjectID)
	source, _ := index.ReadSymbolSource(root, *sym)

	// #623: lite=true short-circuit. Returns just {id, source} —
	// no imports, no callees, no staleness warning, no next_steps.
	// Used by the v0.36 PreToolUse hook redirect: when redirecting a
	// Read of a large indexed file to context, the agent gets exactly
	// the bytes Read would have given them, with the smallest possible
	// envelope. Same retrieval semantics as positional Read but with
	// byte-offset precision. Skips the IMPORTS/CALLS edge walks that
	// account for most of context's per-call latency on big symbols.
	if lite, _ := args["lite"].(bool); lite {
		liteData := map[string]any{
			"id":     sym.ID,
			"source": source,
		}
		// Apply field projection if requested — keeps the contract
		// consistent (callers already use `fields=` patterns elsewhere).
		liteData = projectFields(liteData, fieldSet)
		// Stale-bytes warning still applies in lite mode — minimum
		// envelope means "skip imports/callees/next_steps," not
		// "swallow correctness signals." Without this, an agent that
		// redirects from Read via lite=true gets stale source after
		// an edit with zero indication the bytes don't match the
		// current file (same silent-confidently-wrong family as #317
		// + #960). attachStalenessWarning is a hash compare — cheap
		// enough to keep on the lite path.
		if root != "" {
			s.attachStalenessWarning(liteData, sym.ProjectID, sym, root)
		}
		liteResponseJSON, _ := json.Marshal(liteData)
		return s.jsonResultWithMeta(liteData, start, tool, args,
			s.savedVsFileSizesSession(sym.ProjectID, root, []string{sym.FilePath}, liteResponseJSON)), nil
	}

	// #655: diff-encoded context for repeat reads (PINCHER_DIFF_CONTEXT=1).
	// On a repeat context(id=X) call this process, short-circuit on the
	// backing file's content hash. Unchanged → return {unchanged:true}
	// and skip the whole imports/callees rebuild (the agent already has
	// it). Changed → fall through, but ship the primary symbol's source
	// as a line diff against what we last served instead of the full
	// body. diffMode is "" (normal), "changed" by the end of this block.
	var diffMode, sinceHash string
	if s.diffContext && root != "" {
		diffKey := sym.ProjectID + "|" + sym.ID
		curHash, hashOK := fileHashOnDisk(filepath.Join(root, filepath.FromSlash(sym.FilePath)))
		if hashOK {
			if prevRaw, ok := s.contextDiffCache.Load(diffKey); ok {
				prev := prevRaw.(*contextDiffEntry)
				if prev.fileHash == curHash {
					// File untouched since the last fetch — the agent's
					// context window still holds everything we'd resend.
					unchanged := map[string]any{
						"symbol":     map[string]any{"id": sym.ID, "name": sym.Name, "kind": sym.Kind},
						"unchanged":  true,
						"since_hash": prev.fileHash,
					}
					return s.jsonResultWithMeta(unchanged, start, tool, args, db.ApproxTokens(prev.source)), nil
				}
				// File changed — ship the symbol body as a line diff
				// against what we last served, and refresh the cache so
				// the next call diffs from this revision.
				sinceHash = prev.fileHash
				diffMode = "changed"
				diff := lineDiff(prev.source, source)
				s.contextDiffCache.Store(diffKey, &contextDiffEntry{fileHash: curHash, source: source})
				source = diff
			} else {
				// First fetch this session — remember it for next time.
				s.contextDiffCache.Store(diffKey, &contextDiffEntry{fileHash: curHash, source: source})
			}
		}
	}

	// Find IMPORTS edges from this symbol — cross-package dependencies.
	importEdges, _ := s.store.EdgesFrom(sym.ID, []string{"IMPORTS"})
	// #332: zero-len init so JSON shape is stable when the symbol has
	// no imports (same fix as #328/#330).
	imports := []map[string]any{}
	var importPaths []string
	seen := map[string]bool{sym.ID: true}
	for _, e := range importEdges {
		if seen[e.ToID] {
			continue
		}
		imp, err := s.store.GetSymbol(e.ToID)
		if err != nil || imp == nil {
			continue
		}
		seen[e.ToID] = true
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

	// Find CALLS edges — the symbol's directly-called helpers (#381).
	// The tool description promises "everything it directly imports/calls";
	// pre-fix only IMPORTS were followed, so an in-file refactor that
	// reads `context` against a function calling 3 helpers got back zero
	// callees and had to chase each one with a separate tool call. Now
	// the response includes them in `callees`. De-duplicated against
	// `imports` so a callee that's also imported only appears once.
	callEdges, _ := s.store.EdgesFrom(sym.ID, []string{"CALLS"})
	callees := []map[string]any{}
	var calleePaths []string
	for _, e := range callEdges {
		if seen[e.ToID] {
			continue
		}
		callee, err := s.store.GetSymbol(e.ToID)
		if err != nil || callee == nil {
			continue
		}
		seen[e.ToID] = true
		calleeSource, _ := index.ReadSymbolSource(root, *callee)
		callees = append(callees, map[string]any{
			"id":        callee.ID,
			"name":      callee.Name,
			"kind":      callee.Kind,
			"file_path": callee.FilePath,
			"source":    calleeSource,
		})
		calleePaths = append(calleePaths, callee.FilePath)
	}

	// Savings = would have read the full source file + every import +
	// every callee file; gave only symbols. Include the primary symbol's
	// file in the baseline.
	allPaths := append([]string{sym.FilePath}, importPaths...)
	allPaths = append(allPaths, calleePaths...)
	symMap := map[string]any{"id": sym.ID, "name": sym.Name, "kind": sym.Kind}
	if diffMode == "changed" {
		// #655: `source` holds a line diff against the last-served body,
		// not the full source. Surface it under a distinct key + the hash
		// it diffs from so a consumer never mistakes a diff for source.
		symMap["diff"] = source
		symMap["since_hash"] = sinceHash
	} else {
		symMap["source"] = source
	}
	data := map[string]any{
		"symbol":  symMap,
		"imports": imports,
		"callees": callees,
	}
	// Context returns the symbol + its callees (the outbound direction). The
	// natural next move is the inbound direction — find the symbol's callers
	// before changing it. For non-callable kinds (Setting, Section), no
	// suggestion is offered: there's nothing further to chase.
	if next := suggestContextNextSteps(*sym); len(next) > 0 {
		data["_meta"] = map[string]any{"next_steps": next}
	}
	// #317: warn if the seed file changed since indexing.
	// #980: same check for imports/callees — they're read via the
	// same byte-offset path and pre-fix shipped stale bytes silently
	// when only the dependency file (not the seed) had been edited.
	// The original "checking imports multiplies cost without much
	// value" trade was invalidated by #978/#979's audit: stale-bytes
	// warnings are the contract for byte-offset reads, full stop.
	if root != "" {
		s.attachStalenessWarning(data, sym.ProjectID, sym, root)
		depPaths := make([]string, 0, len(importPaths)+len(calleePaths))
		depPaths = append(depPaths, importPaths...)
		depPaths = append(depPaths, calleePaths...)
		s.attachStalenessWarningsForPaths(data, sym.ProjectID, depPaths, root)
	}
	// #712 C.2: project, but if the caller's `fields` named keys that
	// don't exist on the response, warn instead of silently shipping a
	// `{_meta}`-only body. If *every* requested field was unknown the
	// projection is useless — fall back to the full unprojected data so
	// the call still returns something actionable.
	if fieldSet != nil {
		valid := projectableKeys(data)
		projected, unknown := projectFieldsChecked(data, fieldSet)
		if len(unknown) > 0 {
			realKeys := projectableKeys(projected)
			if len(realKeys) == 0 {
				// All requested fields were bogus — keep the full body.
				attachWarning(data, fmt.Sprintf(
					"fields=%v matched no keys; valid keys: %v — returning full response",
					unknown, valid))
			} else {
				attachWarning(projected, fmt.Sprintf(
					"fields %v matched no keys and were dropped; valid keys: %v",
					unknown, valid))
				data = projected
			}
		} else {
			data = projected
		}
	}
	responseJSON, _ := json.Marshal(data)
	return s.jsonResultWithMeta(data, start, tool, args, s.savedVsFileSizesSession(sym.ProjectID, root, allPaths, responseJSON)), nil
}

// lineDiff returns a compact line-level diff from oldSrc to newSrc for
// #655's diff-encoded context: unchanged lines are prefixed "  ",
// removals "- ", additions "+ ". It is not a full unified diff — there
// are no @@ hunk headers — because the consumer already has the symbol
// identified and only needs the delta. Classic LCS; O(m·n) time and
// space, which is fine for a single symbol body (bounded by extraction).
func lineDiff(oldSrc, newSrc string) string {
	o := strings.Split(oldSrc, "\n")
	n := strings.Split(newSrc, "\n")
	// lcs[i][j] = length of the longest common subsequence of o[i:] and n[j:].
	lcs := make([][]int, len(o)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(n)+1)
	}
	for i := len(o) - 1; i >= 0; i-- {
		for j := len(n) - 1; j >= 0; j-- {
			if o[i] == n[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var b strings.Builder
	i, j := 0, 0
	for i < len(o) && j < len(n) {
		switch {
		case o[i] == n[j]:
			b.WriteString("  " + o[i] + "\n")
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			b.WriteString("- " + o[i] + "\n")
			i++
		default:
			b.WriteString("+ " + n[j] + "\n")
			j++
		}
	}
	for ; i < len(o); i++ {
		b.WriteString("- " + o[i] + "\n")
	}
	for ; j < len(n); j++ {
		b.WriteString("+ " + n[j] + "\n")
	}
	return b.String()
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
		// #712: failure-as-pedagogy — show the caller what a valid query
		// looks like instead of just rejecting the empty one.
		return s.errResultRich("query is required (and must contain non-whitespace characters)", []map[string]string{
			{"tool": "search", "args": `{"query":"handleSearch"}`,
				"why": "exact identifier lookup — single token, no wildcards"},
			{"tool": "search", "args": `{"query":"auth*"}`,
				"why": "prefix match; also supports \"quoted phrases\" and AND/OR"},
			{"tool": "architecture", "args": `{}`,
				"why": "if you don't know what to search for, orient first"},
		}), nil
	}
	// #489: catch an unmatched double-quote before it leaks through to
	// FTS5 as "SQL logic error: unterminated string". Phrase queries
	// (`"login flow"`) are a real feature, so the natural agent retry
	// after a 0-result single-token search is to add quotes — surface
	// the matching-pair requirement instead of an SQL parser error.
	if strings.Count(query, `"`)%2 != 0 {
		return errResult(`unbalanced quote in query — phrase queries need a matched pair, e.g. query="login flow". To match a literal quote character, drop the surrounding quotes.`), nil
	}
	// #509: catch regex meta-patterns (".*", ".+", ".?") before they
	// leak past the #424 sanitizer to FTS5 as raw "syntax error near
	// '.'". The narrow check fires only on the "anything-wildcard"
	// signature — single `.` (e.g. `os.Stat`) is rescued by the
	// per-token sanitizer, and `auth*` is a valid FTS5 prefix wildcard.
	if seq := firstFTS5IncompatibleRegexChar(query); seq != "" {
		// #788: pinchQL's =~ takes a BARE regex, not slash-delimited.
		// A slash-wrapped query (the #786 case) must have its delimiters
		// stripped before it goes into the =~ example — otherwise the
		// redirect recommends `=~ '/handle.*/'`, which matches a literal
		// slash and returns zero rows.
		regexHint := query
		if len(regexHint) > 2 && regexHint[0] == '/' && regexHint[len(regexHint)-1] == '/' {
			regexHint = regexHint[1 : len(regexHint)-1]
		}
		return errResult(fmt.Sprintf(
			"query contains regex sequence %q that FTS5 doesn't understand. For pattern matching use the `query` tool: MATCH (n:Function) WHERE n.name =~ '%s' RETURN n.name. Or search a literal keyword instead.",
			seq, regexHint)), nil
	}
	// #736: a stem-less prefix wildcard ("*", "**") is not a valid FTS5
	// query — SQLite rejects it with the raw "unknown special query"
	// logic error. The natural agent instinct "search for everything"
	// lands here; surface the stem requirement and redirect to the
	// query tool for an actual list-all, instead of leaking SQL noise.
	if strings.Trim(query, "*") == "" {
		return errResult(fmt.Sprintf(
			"%q is not a valid search — FTS5 prefix wildcards need a stem, e.g. query=\"auth*\". To list everything, use the query tool: MATCH (n) RETURN n.name LIMIT 50.",
			query)), nil
	}
	projectArg := str(args, "project")
	kind := str(args, "kind")
	language := str(args, "language")
	corpus := str(args, "corpus")
	limit := intArg(args, "limit", 20)
	// #712: collect clamp warnings so the caller learns its input was
	// adjusted instead of silently getting a different page size than
	// it asked for. Surfaced in _meta.warnings below.
	var searchClampWarnings []string
	// #953: enum-value validation for kind/language. Pre-fix a typo'd
	// kind ("FunctionTypoKind") returned 0 rows and the diagnosis
	// recommended "drop the kind filter" — implying the value was
	// valid-but-selective. Same #473-family silent-quality-loss as the
	// pinchQL typo'd-property warning. Surface a warning naming the
	// unknown value and the canonical set so the typo is observable.
	// canonicalKindCase / canonicalLanguageCase already handle the
	// case-mismatch branch (#902/#910); we only warn for values that
	// don't match any known kind/language even case-insensitively.
	if kind != "" && canonicalKindCase(kind) == "" {
		searchClampWarnings = append(searchClampWarnings,
			fmt.Sprintf("kind=%q is not a known symbol kind — filter cannot match any rows. "+
				"Valid kinds: Function, Method, Class, Interface, Type, Variable, Module, "+
				"Constant, Field, Property, Enum, Trait, Section, Setting, Block, Resource, "+
				"DataSource, Provider, Output, Local, Heading, Document.", kind))
	}
	if language != "" && canonicalLanguageCase(language) == "" {
		searchClampWarnings = append(searchClampWarnings,
			fmt.Sprintf("language=%q is not a known language — filter cannot match any rows. "+
				"Valid languages: Go, Python, JavaScript, TypeScript, Rust, Java, Ruby, PHP, "+
				"C, C++, C#, Kotlin, Swift, Scala, Lua, Zig, Elixir, Haskell, Dart, R, YAML, "+
				"JSON, HCL, TOML, Bash, Markdown, HTML, Makefile, Jinja2, XML.", language))
	}
	// #935: "all" is deprecated and being removed (#106). Pre-fix
	// the soft-redirect logged a slog.Warn line — invisible to the
	// agent calling the tool. The agent passed corpus="all"
	// thinking it would search every corpus, got code-only results,
	// and had no signal that the override happened. Surface a
	// _meta.warnings entry alongside the redirect so the deprecation
	// is observable. Same failure-as-pedagogy shape as the
	// case-fix and unknown-property families.
	if corpus == "all" {
		slog.Warn("pincher.search.corpus_all_deprecated",
			"action", "redirected to 'code'",
			"recommendation", "call search once per corpus (code/config/docs); see #106")
		searchClampWarnings = append(searchClampWarnings,
			"corpus=\"all\" was removed in v0.5 — searched corpus=\"code\" instead. "+
				"To search config/docs, call search once per corpus (code/config/docs).")
		corpus = ""
	}
	rawLimit := limit
	// #532: cap limit at 500. Pre-cap a caller asking for limit=10000 would
	// pin a goroutine on FTS5 + the projection loop. Server-side cap also
	// bounds the "Load more" payload growth in the dashboard.
	if limit > 500 {
		limit = 500
	}
	if limit <= 0 {
		limit = 20
	}
	if rawLimit != limit && args["limit"] != nil {
		searchClampWarnings = append(searchClampWarnings,
			fmt.Sprintf("limit=%d clamped to %d (valid range 1-500)", rawLimit, limit))
	}
	// #532: offset for paginated "Load more" UX. Default 0, cap 5000 —
	// past 5000 results the BM25 tail isn't useful; agents should refine
	// the query instead of paging deeper.
	offset := intArg(args, "offset", 0)
	rawOffset := offset
	if offset < 0 {
		offset = 0
	}
	if offset > 5000 {
		offset = 5000
	}
	if rawOffset != offset && args["offset"] != nil {
		searchClampWarnings = append(searchClampWarnings,
			fmt.Sprintf("offset=%d clamped to %d (valid range 0-5000)", rawOffset, offset))
	}
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
	minConfidence := floatArg(args, "min_confidence", defaultMinConfidenceFor(query, corpus))
	minConfidence, searchMinConfWarn := clampMinConfidence(minConfidence)
	if searchMinConfWarn != "" {
		searchClampWarnings = append(searchClampWarnings, searchMinConfWarn)
	}

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
	//
	// #532: when offset > 0, include the offset in the fetch so we can
	// slice past it before applying the limit. Pagination semantics are
	// "BM25-ranked top-(offset+limit), serve [offset:offset+limit]" —
	// deeper offsets get the more-relevance-degraded long tail, which
	// is the correct trade-off for a "Load more" UX where the user
	// already saw the top-ranked results.
	fetchLimit := limit + offset
	if minConfidence > 0 {
		fetchLimit = (limit + offset) * 4
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

	// #453 AND→OR fallback. FTS5's default between bare tokens is
	// implicit AND; multi-token unquoted queries return 0 when no single
	// symbol matches every term. A common dogfood pattern is "Watch
	// poll Index" — the user means "find anything related to these
	// keywords" but FTS5 reads "find a symbol whose text contains all
	// three". When the AND path returns 0 (after corpus fallthrough)
	// AND the query is multi-token AND wasn't user-quoted AND has no
	// explicit FTS5 operator (so we know the user didn't ask for AND),
	// retry once joining the per-token-sanitised tokens with " OR ".
	// Surface `and_fallback_to_or=true` in _meta so the agent knows
	// the recovery happened.
	andFellThroughToOr := false
	orQuery := ""
	if len(results) == 0 && !strings.Contains(query, `"`) {
		tokens := strings.Fields(query)
		if len(tokens) > 1 && !containsBareFTS5Operator(tokens) {
			sanitised := make([]string, len(tokens))
			for i, t := range tokens {
				sanitised[i] = wrapTokenIfNeeded(t)
			}
			orQuery = strings.Join(sanitised, " OR ")
			retryCorpora := []string{corpus}
			if corpus == "" {
				retryCorpora = []string{"", db.CorpusConfig, db.CorpusDocs}
			}
			for _, fb := range retryCorpora {
				orResults, orErr := s.store.SearchSymbolsByCorpus(projectID, orQuery, kind, language, fb, fetchLimit)
				if orErr != nil {
					continue
				}
				if len(orResults) > 0 {
					results = orResults
					andFellThroughToOr = true
					if corpus == "" && fb != "" {
						fellthroughTo = fb
					}
					break
				}
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
	}

	// #532: pagination envelope. `searchTotal` = post-filter row count we
	// considered; `searchHasMore` = there's at least one row past the
	// current window. The window is [offset:offset+limit] of the BM25-
	// ranked, confidence-filtered, fallthrough-resolved result set.
	//
	// Note: searchTotal is a LOWER BOUND on the true match count for
	// queries that hit fetchLimit — FTS5 stops at fetchLimit so we don't
	// know how many more rows exist past it. The dashboard treats
	// has_more as the source of truth for the "Load more" button; total
	// is rendered as "Showing 50 of 1234+" when has_more is true.
	searchTotal := len(results)
	searchHasMore := false
	if offset >= len(results) {
		results = results[:0]
	} else {
		end := offset + limit
		if end > len(results) {
			end = len(results)
		}
		searchHasMore = end < searchTotal || searchTotal >= fetchLimit
		results = results[offset:end]
	}

	// Resolve project root once for single-project snippet reads.
	// #941: when project="*" (cross-repo search) projectID is "" and
	// resolveProjectRoot returns "" — pre-fix snippet stayed empty
	// for every cross-project result because the disk read short-
	// circuited on root=="". Per-symbol resolution + small cache fixes
	// the cross-project case without regressing single-project
	// performance (the loop below caches root by project so we hit
	// the DB once per project, not once per symbol).
	root, _ := s.resolveProjectRoot(projectID)
	projectRoots := map[string]string{}
	if projectID != "" && root != "" {
		projectRoots[projectID] = root
	}
	rootFor := func(pid string) string {
		if pid == "" {
			return root
		}
		if r, ok := projectRoots[pid]; ok {
			return r
		}
		r, _ := s.resolveProjectRoot(pid)
		projectRoots[pid] = r
		return r
	}

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
		// #947: end_line / start_byte / end_byte were silently dropped
		// from search results. Agents reading a result couldn't compute
		// the symbol's line range or byte span without a follow-up
		// `symbol` call, defeating the "search returns enough to answer
		// in one shot" contract. neighborhood / symbol / symbols all
		// surface these; search now matches.
		allFields["end_line"] = r.Symbol.EndLine
		allFields["start_byte"] = r.Symbol.StartByte
		allFields["end_byte"] = r.Symbol.EndByte
		allFields["signature"] = r.Symbol.Signature
		allFields["score"] = r.Score
		allFields["extraction_confidence"] = r.Symbol.ExtractionConfidence

		// Add a short snippet so Claude can often skip a follow-up symbol/context call.
		// Suppress for variables/types where the signature IS the content.
		// Skip the disk read entirely when the caller's fields= projection excludes
		// snippet — otherwise we'd read kilobytes per result and discard them.
		includeSnippet := fieldSet == nil || fieldSet["snippet"]
		snippet := ""
		if includeSnippet && r.Symbol.Kind != "Variable" && r.Symbol.Kind != "Type" {
			var src string
			if r.Symbol.Kind == "Document" {
				// #766: Documents (fetched URLs) have no on-disk file to
				// byte-seek — their text lives in Docstring. Without this
				// the snippet was always empty for Document hits, breaking
				// the "skip a follow-up call" contract search advertises.
				src = r.Symbol.Docstring
			} else if symRoot := rootFor(r.Symbol.ProjectID); symRoot != "" {
				if s, err := index.ReadSymbolSourceCapped(symRoot, r.Symbol, snippetReadCap); err == nil {
					src = s
				}
			}
			if src != "" {
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
	tokensSaved := s.savedVsFileSizesSession(projectID, root, filePaths, responseJSON)

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
	// #453: surface the AND→OR recovery so the agent knows the query
	// rewrite happened and can re-issue the OR form directly next time
	// (or refine to a narrower term set).
	if andFellThroughToOr {
		meta["and_fallback_to_or"] = true
		meta["effective_query"] = orQuery
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
	// #712: surface limit/offset clamps so the caller learns its page
	// params were adjusted. jsonResultWithMeta's unknown-args merge
	// appends to the same key — pre-seed, don't clobber.
	if len(searchClampWarnings) > 0 {
		existing, _ := meta["warnings"].([]string)
		meta["warnings"] = append(existing, searchClampWarnings...)
	}
	data := map[string]any{
		"results":  rows,
		"count":    len(rows),
		"query":    query,
		"total":    searchTotal,
		"has_more": searchHasMore,
		"offset":   offset,
		"limit":    limit,
		"_meta":    meta,
	}
	// F1: surface binary-version drift on read-class tools so the agent
	// knows results may reflect older parsing logic than what indexed
	// the project. No-op when versions match or either is dev/unstamped.
	s.attachDriftWarning(data, projectID)
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
			// #910: same canonical-case probe pattern as #902 for the
			// kind filter. Stored canonical values are Function /
			// Method / Class / etc. — case-sensitive equality at the
			// DB layer means "FuNcTiOn" returns zero. Teach the case
			// fix instead of recommending the filter be dropped, since
			// "drop the filter" over-broadens to all kinds.
			if canon := canonicalKindCase(kind); canon != "" && canon != kind {
				if cn, cerr := relax(query, canon, language, corpus); cerr == nil && cn > 0 {
					return fmt.Sprintf(
							"%d match(es) exist for %q but kind=%q is the wrong case — the canonical form is %q",
							cn, query, kind, canon,
						), []map[string]string{{
							"tool": "search",
							"args": nextStepArgs(map[string]any{"query": query, "kind": canon}),
							"why":  fmt.Sprintf("verified: kind=%q surfaces %d match(es)", canon, cn),
						}}, true
				}
			}
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
			// #902: before recommending "drop the filter entirely",
			// check if a case-normalised form of the user's language
			// would have matched. If so, teach the case fix — that's
			// what the user actually meant. Dropping the filter would
			// over-broaden to other languages and pollute results.
			if canon := canonicalLanguageCase(language); canon != "" && canon != language {
				if cn, cerr := relax(query, kind, canon, corpus); cerr == nil && cn > 0 {
					return fmt.Sprintf(
							"%d match(es) exist for %q but language=%q is the wrong case — the canonical form is %q",
							cn, query, language, canon,
						), []map[string]string{{
							"tool": "search",
							"args": nextStepArgs(map[string]any{"query": query, "language": canon}),
							"why":  fmt.Sprintf("verified: language=%q surfaces %d match(es)", canon, cn),
						}}, true
				}
			}
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
// contain characters FTS5 treats as syntactic. Without this, natural
// identifier queries like `os.Stat`, `parse(query)`, or `@deprecated`
// raise a raw "fts5: syntax error" the caller can't recover from
// without learning FTS5 quoting (#289, #424).
//
// Two passes:
//
//  1. Per-token wrapping for chars that are illegal inside a bare
//     token but harmless inside a phrase quote: `.`, `-`, `:` (between
//     alphanumerics), plus `(`, `)`, `,`, `[`, `]`, `{`, `}`, `@`,
//     `!`, `?`, `/`, `'` anywhere in the token (#424).
//
//  2. Whole-query wrapping when a bare FTS5 boolean operator (NOT,
//     AND, OR — uppercase, FTS5 is case-sensitive) appears as a
//     standalone token in a multi-token query. That's the only
//     reliable signal the user typed "handle AND NOT context" as
//     prose rather than as an operator expression. We quote the whole
//     thing as a phrase rather than try to surgically wrap operators —
//     a phrase search of the original text is what the user wanted.
//
// Preserved as-is:
//   - Explicit quoted phrases ("login flow") — early return on the
//     first `"` so anything quoted passes through verbatim.
//   - Wildcards (`auth*`, `os.Stat*` becomes `"os.Stat"*`).
//   - Column-prefix syntax (`name:value`) — the colon between
//     alphanumerics gets wrapped per token, which is fine; intentional
//     `colname:term` with no alphanum-colon-alphanum gap survives.
//   - Already-correct queries with no special chars.
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
	if len(tokens) > 1 && containsBareFTS5Operator(tokens) {
		// #452: distinguish deliberate FTS5 expressions from prose that
		// happens to contain capitalised operator words. A short query
		// where every non-operator token is identifier-shaped (e.g.
		// `Watch OR poll`, `Foo AND NOT Bar`) is a real FTS5 expression
		// the user typed on purpose — pass it through with per-token
		// wrapping so the operator semantics survive. The original
		// phrase-wrap fallback stays for the prose case (e.g.
		// `handle AND NOT context` looking for English text).
		if looksLikeDeliberateFTS5Expr(tokens) {
			for i, tok := range tokens {
				if tok == "NOT" || tok == "AND" || tok == "OR" {
					continue
				}
				tokens[i] = wrapTokenIfNeeded(tok)
			}
			return strings.Join(tokens, " ")
		}
		// Phrase-wrap the whole query so FTS5's operator parser stays
		// out of it. Strip apostrophes — they'd terminate the phrase
		// otherwise (#424 unterminated-string repro).
		safe := strings.ReplaceAll(q, `'`, "")
		return `"` + safe + `"`
	}
	for i, tok := range tokens {
		tokens[i] = wrapTokenIfNeeded(tok)
	}
	return strings.Join(tokens, " ")
}

// looksLikeDeliberateFTS5Expr reports whether a tokenised query looks
// like the user actually meant the operators as FTS5 operators rather
// than as English words inside prose. Signals required:
//  1. Short query (2-5 tokens).
//  2. Every non-operator token is identifier-shaped (alphanumerics + `.`/`-`/`_`).
//  3. At least one non-operator token carries a code-not-prose signal —
//     CamelCase, an identifier punctuation char (`.`/`-`/`_`), or a `*` suffix.
//
// All-lowercase prose (e.g. `foo OR bar`, `handle AND NOT context`)
// fails signal 3 and stays phrase-wrapped, preserving the original
// (#289 / #424) safety for natural-language queries that happen to
// capitalise AND/OR/NOT. CamelCase or punctuation-bearing names
// (`Watch OR poll`, `auth* OR oauth*`, `mod.Foo AND mod.Bar`) pass
// through with operator semantics intact.
func looksLikeDeliberateFTS5Expr(tokens []string) bool {
	if len(tokens) < 2 || len(tokens) > 5 {
		return false
	}
	hasOp := false
	hasCodeIdent := false
	for _, t := range tokens {
		if t == "NOT" || t == "AND" || t == "OR" {
			hasOp = true
			continue
		}
		if !looksLikeIdentToken(t) {
			return false
		}
		if looksLikeCodeIdent(t) {
			hasCodeIdent = true
		}
	}
	return hasOp && hasCodeIdent
}

// looksLikeCodeIdent reports whether a token reads as a source-code
// identifier rather than a plain English word. Identifier punctuation,
// a prefix wildcard, or mixed-case ASCII (camelCase OR PascalCase) all
// qualify.
//
// #919: pre-fix only PascalCase (uppercase-first + lowercase inside)
// counted; camelCase tokens like `handleSearch` were treated as prose
// and the entire query was phrase-wrapped, defeating #887's OR
// support. The `hasMixedCase` predicate unifies both shapes — same
// detector used by `needsQuoting` at the token-wrap layer.
func looksLikeCodeIdent(s string) bool {
	if strings.HasSuffix(s, "*") {
		return true
	}
	if strings.ContainsAny(s, "._-") {
		return true
	}
	return hasMixedCase(s)
}

// looksLikeIdentToken reports whether a token reads as a source-code
// identifier or prefix-wildcard. Permissive on intermediate `.`/`-`/`_`
// because those are common in package paths and qualified names.
func looksLikeIdentToken(s string) bool {
	if len(s) == 0 {
		return false
	}
	if strings.HasSuffix(s, "*") {
		s = s[:len(s)-1]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAlphanum(c) || c == '.' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// wrapTokenIfNeeded returns tok wrapped in FTS5 phrase quotes if it
// contains an FTS5-special character. Strips a trailing `*` before
// testing and re-adds it so prefix queries (`os.Stat*`) keep working.
// Apostrophes inside the wrapped span are stripped so they don't
// terminate the phrase (#424).
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
	core = strings.ReplaceAll(core, `'`, "")
	return `"` + core + `"` + suffix
}

func needsQuoting(s string) bool {
	if len(s) == 0 {
		return false
	}
	// Chars that are FTS5-syntactic anywhere in the token — parens
	// open/close groups, slash splits paths, @ is reserved, ! and ?
	// were proposed in #424, brackets/braces/comma all crash bare. An
	// apostrophe alone opens a phrase and crashes with unterminated
	// string. Wrapping in a phrase quote neutralises all of these.
	if strings.ContainsAny(s, "()[]{},/@!?'") {
		return true
	}
	// #887: phrase-quote mixed-case identifiers (CamelCase like
	// `handleSearch`). Without the quotes, FTS5 returns ZERO rows when
	// two CamelCase tokens are OR'd together — `handleSearch OR
	// handleQuery` matches nothing while either alone matches its
	// symbol, and `"handleSearch" OR "handleQuery"` returns both. The
	// quoted form is semantically identical for a single-word token
	// (the phrase has one token), but works around the bare-OR quirk
	// uniformly. Pure-uppercase tokens (`OR`, `AND`, `NOT` — the
	// FTS5 operators) are already skipped before this check fires.
	if hasMixedCase(s) {
		return true
	}
	// `.`, `-`, `:` only matter when they sit between alphanumerics —
	// `os.Stat`, `my-component`, `localhost:8080`. A bare `.` or `:`
	// at an edge is usually intentional (wildcard or column prefix).
	if len(s) < 3 {
		return false
	}
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '.' || s[i] == '-' || s[i] == ':' {
			if isAlphanum(s[i-1]) && isAlphanum(s[i+1]) {
				return true
			}
		}
	}
	return false
}

// containsBareFTS5Operator reports whether any token in tokens is a
// standalone uppercase FTS5 boolean operator (NOT, AND, OR). FTS5
// treats these as operators only when uppercase and unquoted, so the
// match is case-sensitive and exact.
func containsBareFTS5Operator(tokens []string) bool {
	for _, t := range tokens {
		if t == "NOT" || t == "AND" || t == "OR" {
			return true
		}
	}
	return false
}

// hasMixedCase reports whether s contains BOTH an upper-case and a
// lower-case ASCII letter — the shape of a CamelCase identifier like
// `handleSearch`. Used by needsQuoting to phrase-wrap such tokens
// (#887) so multi-CamelCase OR queries actually return rows.
func hasMixedCase(s string) bool {
	var upper, lower bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			upper = true
		} else if c >= 'a' && c <= 'z' {
			lower = true
		}
		if upper && lower {
			return true
		}
	}
	return false
}

func isAlphanum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// defaultMinConfidenceFor picks the right min_confidence default for a
// query that doesn't carry an explicit threshold (#247 #5 #379).
//
// The 0.71 baseline filters bottom-floor noise (README/CHANGELOG H1
// sections at 0.70) on wide keyword searches against the code corpus —
// necessary because a doc-section title can BM25-match an unrelated
// identifier query. But two cases break that defaulting:
//
//  1. Exact identifier queries (#247): `registerTools` can't share a
//     name with a doc symbol, so the floor is irrelevant. Default 0.0.
//
//  2. Explicit corpus=docs (#379): the caller is asking for Markdown /
//     fetched-document content, which is exactly what 0.71 was designed
//     to filter out. Markdown sections extract at 0.7-0.81, so the
//     default silently zero-results the caller's intended target.
//     When corpus=docs, default 0.0 — the caller's scope choice IS
//     the noise filter.
//
// Anything else (phrase, wildcard, multi-word against code/config) keeps
// the 0.71 baseline. Explicit min_confidence on the call wins.
func defaultMinConfidenceFor(query, corpus string) float64 {
	if corpus == "docs" {
		return 0.0
	}
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
	// #453: when the query is multi-token unquoted, the failure mode is
	// usually "no symbol matched all N tokens (AND) and none matched any
	// of them (OR)" — surface both halves so the agent stops thinking
	// about confidence/kind/language tweaks for a query that's just
	// missing the underlying name.
	isMultiTokenUnquoted := !strings.ContainsAny(query, "\"") && len(strings.Fields(query)) > 1
	switch {
	case minConfidence > 0:
		return fmt.Sprintf("no matches at min_confidence ≥ %.2f — bottom-floor symbols (lockfile keys, README headings) need min_confidence=0.0 to surface", minConfidence)
	case kind != "":
		return fmt.Sprintf("no matches with kind=%q — try without the kind filter to see all matching symbols", kind)
	case language != "":
		return fmt.Sprintf("no matches in language=%q — try without the language filter or check the project's actual language mix via `architecture`", language)
	case corpus != "" && corpus != "code":
		return fmt.Sprintf("no matches in corpus=%q — try corpus=code (the default for source identifiers) or omit corpus", corpus)
	case isMultiTokenUnquoted:
		return fmt.Sprintf("no symbol matched all of %q (FTS5 default = AND) and the OR-fallback also returned 0 — none of the terms exist as symbol names. Re-think the keyword set or `list` projects to confirm scope", query)
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
	cypherAlias := str(args, "cypher")
	// #712: when a caller passes BOTH `pinchql` and the legacy `cypher`
	// alias, `pinchql` wins and `cypher` is silently dropped. Silent
	// ignore is the bug (same class as #473's unknown-property warning) —
	// surface it so the caller learns which one ran.
	var queryWarnings []string
	if cql != "" && cypherAlias != "" {
		queryWarnings = append(queryWarnings,
			"both `pinchql` and the legacy `cypher` alias were passed — `pinchql` was used and `cypher` ignored; pass only one")
	}
	if cql == "" {
		cql = cypherAlias
	}
	// Surface a deprecation warning when ONLY the legacy `cypher`
	// alias is used. Pre-fix the alias was honored silently — agents
	// using it had no signal the migration window was closing, and
	// the day the alias is removed every cached call site breaks at
	// once with no advance notice. Matches the corpus="all"
	// deprecation pattern (#935): observable redirect beats silent
	// rewrite. Detect on the ORIGINAL args (str(args, "pinchql"))
	// rather than the post-fallback cql, since the fallback above has
	// already copied cypherAlias into cql when pinchql was empty.
	if str(args, "pinchql") == "" && cypherAlias != "" {
		slog.Warn("pincher.query.cypher_alias_deprecated",
			"action", "honored cypher arg",
			"recommendation", "rename the parameter to `pinchql`; the cypher alias will be removed in a future release")
		queryWarnings = append(queryWarnings,
			"the `cypher` parameter is a deprecated alias kept for one release — rename it to `pinchql` before the alias is removed")
	}
	if cql == "" {
		// #712: failure-as-pedagogy — pinchQL is unfamiliar; show a
		// working query + point at `schema` for the node/edge kinds.
		return s.errResultRich("pinchql query is required (parameter `pinchql`; legacy alias `cypher` also accepted)", []map[string]string{
			{"tool": "query", "args": `{"pinchql":"MATCH (f:Function) WHERE f.name=\"main\" RETURN f.file_path LIMIT 10"}`,
				"why": "pinchQL is a Cypher-shaped subset — MATCH/WHERE/RETURN/LIMIT"},
			{"tool": "schema", "args": `{}`,
				"why": "lists the node kinds (Function, Class, ...) and edge kinds (CALLS, ...) available to match on"},
		}), nil
	}
	maxRows := intArg(args, "max_rows", 200)
	// #879: clamp max_rows to the documented [1, 10000] range and warn.
	// Pre-fix `cypher.Executor.maxRows()` silently rewrote 0 / negative
	// to 200 and `> 10000` to 10000 — the caller got a different row
	// budget than they asked for with no signal.
	if rawMR, present := args["max_rows"]; present {
		if mr, ok := rawMR.(float64); ok {
			rawMRInt := int(mr)
			if rawMRInt < 1 {
				queryWarnings = append(queryWarnings,
					fmt.Sprintf("max_rows=%d clamped to 1 (valid range 1-10000)", rawMRInt))
				maxRows = 1
			} else if rawMRInt > 10000 {
				queryWarnings = append(queryWarnings,
					fmt.Sprintf("max_rows=%d clamped to 10000 (valid range 1-10000)", rawMRInt))
				maxRows = 10000
			}
		}
	}
	minConfidence := floatArg(args, "min_confidence", 0.0)
	minConfidence, queryMinConfWarn := clampMinConfidence(minConfidence)
	if queryMinConfWarn != "" {
		queryWarnings = append(queryWarnings, queryMinConfWarn)
	}

	// project=* opts in to cross-project pinchQL — same shape as
	// search's `project=*`. Useful for "where do I import lib X
	// across all my services?" queries when many projects are
	// indexed in the same store.
	projectArg := str(args, "project")
	var projectID string
	allowAllProjects := false
	if projectArg == "*" {
		allowAllProjects = true
	} else {
		var errRes *mcp.CallToolResult
		projectID, errRes = s.mustProject(args)
		if errRes != nil {
			return errRes, nil
		}
	}

	// Cypher queries are pure SELECTs — route to the reader pool (#51).
	exec := &cypher.Executor{DB: s.store.RO(), MaxRows: maxRows, ProjectID: projectID, AllowAllProjects: allowAllProjects}
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
		// Rich error envelope so cypher parse/syntax failures carry
		// recovery next_steps. Operator-hint family (BETWEEN, !=,
		// LIKE, IN, arithmetic) already teaches the supported
		// spelling inline in the message; the next_steps lead the
		// caller to schema/guide when the error is about something
		// else (unknown property, malformed regex, type mismatch).
		return s.errResultRich(
			fmt.Sprintf("cypher error: %v", err),
			[]map[string]string{
				{"tool": "schema", "args": `{}`,
					"why": "list valid node and edge kinds the query can match on"},
				{"tool": "query", "args": `{"pinchql":"MATCH (f:Function) WHERE f.name=\"main\" RETURN f.file_path LIMIT 10"}`,
					"why": "working pinchQL example — copy the shape, swap the predicate"},
				{"tool": "guide", "args": `{"task":"<describe what you're trying to find>"}`,
					"why": "guide proposes a query shape from a free-form task description"},
			}), nil
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
	meta := map[string]any{}
	// #873: only emit the confidence histogram when the query actually
	// projected an extraction_confidence column. A pinchQL query that
	// RETURNs only `n.name` carries no confidence data, but
	// confidenceDistribution([]) returns an all-zero map — emitting it
	// reads as "every result is confidence 0", the opposite of the truth.
	// Omit when there's no data to summarize.
	if len(confs) > 0 {
		meta["confidence_distribution"] = confidenceDistribution(confs)
	}
	// #473: surface unknown-property warnings from the cypher engine.
	// Typo'd property names (n.foo on a kind that has no foo column)
	// silently return 0 rows; surfacing the engine's warning gives the
	// agent a remediation instead of a misleading empty result.
	if len(result.Warnings) > 0 {
		queryWarnings = append(queryWarnings, result.Warnings...)
	}
	if len(queryWarnings) > 0 {
		meta["warnings"] = queryWarnings
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
	// Honest savings: when the query projected a file_path column, the
	// agent's alternative was reading those files. Harvest distinct
	// paths and use savedVsFileSizesSession. When no file_path column
	// was projected, we can't honestly claim a file-read alternative —
	// pass 0 rather than fabricating count × avgFileSize.
	queryPaths := harvestRowFilePaths(rows)
	tokensSaved := 0
	if len(queryPaths) > 0 && !allowAllProjects {
		root, _ := s.resolveProjectRoot(projectID)
		tokensSaved = s.savedVsFileSizesSession(projectID, root, queryPaths, responseJSON)
	}
	return s.jsonResultWithMeta(data, start, tool, args, tokensSaved), nil
}

// harvestRowFilePaths walks pinchQL result rows and collects distinct
// values from any column whose key looks like a file-path projection
// (`file_path` exact, or `<alias>.file_path` like `n.file_path`).
// Returns nil when no such column is present — caller treats that as
// "no honest file-read alternative" and passes tokensSaved=0.
func harvestRowFilePaths(rows []map[string]any) []string {
	if len(rows) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, r := range rows {
		for k, v := range r {
			if k == "file_path" || strings.HasSuffix(k, ".file_path") {
				if s, ok := v.(string); ok && s != "" {
					if _, dup := seen[s]; !dup {
						seen[s] = struct{}{}
						out = append(out, s)
					}
				}
			}
		}
	}
	return out
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
	// Bare key — non-pinchQL callers and pinchQL aggregates without a
	// variable prefix.
	if v, ok := row["extraction_confidence"]; ok {
		if f, ok := asFloat(v); ok {
			return f, true
		}
	}
	// #873: pinchQL projects with the variable prefix, so
	// `RETURN n.extraction_confidence` yields a row key
	// "n.extraction_confidence". The `confidence` short alias produces
	// "n.confidence". Match any key ending in either form so the
	// query-handler's min_confidence filter AND
	// confidence_distribution histogram actually see the data — pre-fix
	// rowConfidence's bare-key lookup never matched any pinchQL row,
	// silently disabling both.
	for k, v := range row {
		if strings.HasSuffix(k, ".extraction_confidence") || strings.HasSuffix(k, ".confidence") {
			if f, ok := asFloat(v); ok {
				return f, true
			}
		}
	}
	return 0, false
}

// asFloat converts a JSON-decoded number to float64 (the JSON unmarshal
// typically yields float64, but float32 / int variants are accepted for
// future-proofing against alternative scan paths).
func asFloat(v any) (float64, bool) {
	switch f := v.(type) {
	case float64:
		return f, true
	case float32:
		return float64(f), true
	case int:
		return float64(f), true
	case int64:
		return float64(f), true
	}
	return 0, false
}

func (s *Server) handleTrace(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	// `id` is the exact-symbol escape hatch (#474): when the caller already
	// has a symbol ID from search/symbols/query and wants to trace THAT
	// specific symbol (not whatever the name resolver picks first), pass
	// id= and skip the name lookup entirely. `name` remains supported for
	// the common case where the caller is asking by identifier alone.
	id := str(args, "id")
	name := str(args, "name")
	if id == "" && name == "" {
		// #712: failure-as-pedagogy — trace needs a seed; both shapes
		// (by-name and by-id) are valid, show each.
		return s.errResultRich("either `id` or `name` is required", []map[string]string{
			{"tool": "trace", "args": `{"name":"<function-name>"}`,
				"why": "trace by short name — the common case"},
			{"tool": "search", "args": `{"query":"<function-name>"}`,
				"why": "if the name is ambiguous, search first, then trace by the exact id"},
		}), nil
	}
	direction := str(args, "direction")
	if direction == "" {
		direction = "both"
	}
	// #839: validate direction. The DB layer's traceViaCTE only branches
	// on inbound/outbound/both — any other value falls through every
	// branch and silently returns 0 hops, indistinguishable from a
	// genuine "no callers" result. `callers`/`callees` are the obvious
	// words an agent reaches for (this tool's own description says "find
	// callers (inbound)"), so map those two synonyms with a warning; for
	// anything else, warn and fall back to `both`. Same failure-as-
	// pedagogy shape as the unknown-edge-kind warnings below.
	var traceDirWarning string
	switch direction {
	case "inbound", "outbound", "both":
		// canonical — no warning
	case "callers":
		traceDirWarning = `direction="callers" is not a valid value — interpreted as "inbound". Use direction="inbound" (the canonical term) to silence this warning.`
		direction = "inbound"
	case "callees":
		traceDirWarning = `direction="callees" is not a valid value — interpreted as "outbound". Use direction="outbound" (the canonical term) to silence this warning.`
		direction = "outbound"
	default:
		traceDirWarning = fmt.Sprintf(`direction=%q is not a valid value — falling back to "both". Valid values: inbound, outbound, both.`, direction)
		direction = "both"
	}
	depth := intArg(args, "depth", 3)
	// #703: clamp negative/zero depth. Schema declares depth 1-5; callers
	// passing depth<1 previously got `{hops:[], total:N>0, risk_summary
	// populated}` — the depth-grouping loop's `for d:=1; d<=depth` never
	// executed, but the upstream BFS output still populated `total` and
	// `riskCounts`. Internal-consistency violation. Clamp + emit
	// `_meta.warnings` so the caller learns instead of guessing.
	var traceDepthClampMsg string
	if depth < 1 {
		traceDepthClampMsg = fmt.Sprintf("trace: depth=%d clamped to 1 (valid range 1-5)", depth)
		depth = 1
	} else if depth > 5 {
		// #712: schema declares depth 1-5; depth>5 was silently honored,
		// which can dump very large traversals. Clamp to 5 + warn rather
		// than letting an off-by-typo (depth=50) pin a goroutine on a
		// hotspot BFS.
		traceDepthClampMsg = fmt.Sprintf("trace: depth=%d clamped to 5 (valid range 1-5)", depth)
		depth = 5
	}
	// #402: detect whether the caller passed depth explicitly. When
	// they did, honor it exactly. When they didn't (auto mode), the
	// post-filter below trims the result down to the smallest depth
	// that still has >= autoTraceDeepenThreshold hops — hotspot
	// traces don't dump 100+ hops the agent has to read. Pure
	// post-filter on the existing depth-3 trace; SQL cost is
	// unchanged. The bigger perf win (single-SQL closure-table
	// lookups instead of recursive CTEs) is tracked at #403.
	_, depthExplicit := args["depth"]
	addRisk := boolArgDefault(args, "risk", true)
	minConfidence := floatArg(args, "min_confidence", 0.0)
	minConfidence, traceMinConfWarn := clampMinConfidence(minConfidence)
	// #398: by default, drop hops in *_test.go and testdata/__fixtures__/
	// paths. Mirrors the architecture filter (#305 + #393): test
	// files have legitimate inbound edges from every test that
	// touches them but flood the BFS output with low-signal hops
	// (a single trace on a hotspot returns 100+ test functions).
	// Fixture paths (testdata/corpus/...) are worse: those symbols
	// aren't real code, just inputs to pincher's own snapshot tests.
	// `include_tests=true` opts back into the legacy mixed list when
	// the caller actually wants to see test coverage of a symbol.
	includeTests := boolArg(args, "include_tests")

	// kinds: comma-separated list of edge kinds to traverse (e.g.
	// "CALLS" or "READS,WRITES"). Empty/missing = default (CALLS
	// family). Whitespace and case differences are tolerated so a
	// caller passing "reads, writes" matches the same as "READS,WRITES".
	kindsArg := str(args, "kinds")
	var edgeKinds []string
	// #712: validate edge kinds. An unrecognized kind (typo, made-up
	// name) previously produced a silent 0-hop traversal — the caller
	// can't tell "no edges of this kind" from "I typo'd the kind". Warn
	// on unknown kinds so the failure teaches. knownEdgeKinds is the
	// full traversable set; keep in sync with the edge-kind taxonomy.
	knownEdgeKinds := map[string]bool{
		"CALLS": true, "HTTP_CALLS": true, "ASYNC_CALLS": true,
		"READS": true, "WRITES": true, "IMPORTS": true, "REFERENCES": true,
	}
	var traceKindWarnings []string
	if kindsArg != "" {
		for _, k := range strings.Split(kindsArg, ",") {
			k = strings.ToUpper(strings.TrimSpace(k))
			if k != "" {
				if !knownEdgeKinds[k] {
					traceKindWarnings = append(traceKindWarnings,
						fmt.Sprintf("unknown edge kind %q ignored — valid kinds: CALLS, HTTP_CALLS, ASYNC_CALLS, READS, WRITES, IMPORTS, REFERENCES", k))
					continue
				}
				edgeKinds = append(edgeKinds, k)
			}
		}
	}

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	// Resolve the start symbol. Two paths:
	//   - id=  : exact-symbol seed, no ambiguity, no GetSymbolsByName call.
	//            #474 escape hatch when name resolution picks the wrong target.
	//   - name=: keep the prior name-based behaviour — surface ambiguity in
	//            _meta so agents who hit a same-named symbol elsewhere
	//            (common: many `Run`, `Handler`, `Open` per project) know
	//            they need to refine via the new `id` parameter.
	var starts []db.Symbol
	var seedID string
	if id != "" {
		seed, lookupErr := s.store.GetSymbol(id)
		if lookupErr != nil || seed == nil {
			// #704: not-found errors carry remediation hints. Without
			// them the caller is stuck — the obvious next move is search
			// by short name, surfaced explicitly.
			return s.errResultRich(
				fmt.Sprintf("trace: symbol id %q not found", id),
				[]map[string]string{
					{"tool": "search", "args": fmt.Sprintf(`{"query":%q}`, shortNameFromID(id)),
						"why": "id resolution failed — search by short name to find similar / case-correct matches"},
					{"tool": "list", "args": "{}",
						"why": "if no project matches, the right project may not be indexed"},
				},
			), nil
		}
		starts = []db.Symbol{*seed}
		seedID = seed.ID
		name = seed.Name
	} else {
		var err error
		starts, err = s.store.GetSymbolsByName(projectID, name, 5)
		if err != nil {
			return errResult(fmt.Sprintf("trace lookup: %v", err)), nil
		}
		if len(starts) == 0 {
			// #704: same shape as the id-not-found path above.
			return s.errResultRich(
				fmt.Sprintf("symbol %q not found in project", name),
				[]map[string]string{
					{"tool": "search", "args": fmt.Sprintf(`{"query":%q}`, name),
						"why": "name resolution failed — search by short name to find similar / case-correct matches"},
					{"tool": "list", "args": "{}",
						"why": "if no project matches, the right project may not be indexed"},
				},
			), nil
		}
		// #319: rank candidates so the picked target is the most useful
		// trace seed. Precedence:
		//   1. Non-scratch, non-test files first (scratch_*.go, *_test.go)
		//   2. Callable kinds first (Function, Method) — Modules/Settings
		//      can match a name but they have no CALLS edges, so tracing
		//      them returns 0 hops and looks like a real empty result.
		//   3. Stable order from GetSymbolsByName for everything else.
		sortTraceCandidates(starts)
		seedID = starts[0].ID
	}
	hops, err := s.indexer.TraceByID(ctx, projectID, seedID, direction, depth, addRisk, edgeKinds...)
	if err != nil {
		return errResult(fmt.Sprintf("trace error: %v", err)), nil
	}

	// Filter by min_confidence — drop hops whose target falls below threshold.
	// Always collect confidences for the response distribution (regardless of
	// whether the threshold filter is active). #398: also filter test +
	// fixture hops by default; these flood inbound traces on hotspots
	// without adding orientation value.
	confs := make([]float64, 0, len(hops))
	filtered := hops[:0]
	// #898: count hops dropped by the default test/fixture filter so the
	// empty-trace branch can surface the include_tests=true escape hatch
	// instead of suggesting "this symbol is a leaf, read its own source".
	// For a symbol whose only inbound writes/calls come from test files,
	// the pre-fix advice was confidently wrong.
	testFilteredCount := 0
	for _, h := range hops {
		if minConfidence > 0 && h.Symbol.ExtractionConfidence < minConfidence {
			continue
		}
		if !includeTests && (isTestFile(h.Symbol.FilePath) || isTestFixturePath(h.Symbol.FilePath)) {
			testFilteredCount++
			continue
		}
		filtered = append(filtered, h)
		confs = append(confs, h.Symbol.ExtractionConfidence)
	}
	hops = filtered

	// #402: adaptive trace depth. When the caller didn't pass
	// `depth` explicitly, find the smallest D where hops with
	// d <= D total at least autoTraceDeepenThreshold. Trim the
	// result to that depth. The agent gets the answer "who calls
	// this directly?" with one round-trip when there are enough
	// direct callers; deeper hops only appear when shallower ones
	// don't satisfy the threshold. depthUsed surfaces in _meta.
	autoTraceDeepenThreshold := 5
	depthUsed := depth
	if !depthExplicit && len(hops) > 0 {
		for d := 1; d <= depth; d++ {
			count := 0
			for _, h := range hops {
				if h.Depth <= d {
					count++
				}
			}
			if count >= autoTraceDeepenThreshold {
				depthUsed = d
				break
			}
		}
		// Trim hops + confs to depthUsed.
		if depthUsed < depth {
			trimmed := hops[:0]
			trimmedConfs := confs[:0]
			for i, h := range hops {
				if h.Depth <= depthUsed {
					trimmed = append(trimmed, h)
					trimmedConfs = append(trimmedConfs, confs[i])
				}
			}
			hops = trimmed
			confs = trimmedConfs
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
	// #703: surface the depth clamp in _meta.warnings when negative/zero
	// depth was passed. jsonResultWithMeta's unknown-args merge appends to
	// this same key (see beneath); pre-seed here so it doesn't get clobbered.
	// #703 + #712: surface depth clamp and unknown-edge-kind warnings.
	// jsonResultWithMeta's unknown-args merge appends to this same key,
	// so pre-seed the slice rather than overwriting it later.
	var traceWarnings []string
	if traceDepthClampMsg != "" {
		traceWarnings = append(traceWarnings, traceDepthClampMsg)
	}
	if traceDirWarning != "" {
		traceWarnings = append(traceWarnings, traceDirWarning)
	}
	if traceMinConfWarn != "" {
		traceWarnings = append(traceWarnings, traceMinConfWarn)
	}
	traceWarnings = append(traceWarnings, traceKindWarnings...)
	if len(traceWarnings) > 0 {
		meta["warnings"] = traceWarnings
	}
	// #402: when auto-deepen trimmed the result below the requested
	// depth, tell the agent so depth_used vs depth_requested is
	// observable. Only emit when the trim actually shortened
	// something — a noisy explicit-depth=3 doesn't need to surface
	// "depth_used:3" on every call.
	if !depthExplicit && depthUsed < depth {
		meta["depth_used"] = depthUsed
		meta["depth_requested"] = depth
		meta["auto_deepened"] = false
		meta["depth_hint"] = fmt.Sprintf("trimmed to depth=%d (>=5 hops at this depth); pass depth=%d explicitly to see deeper hops", depthUsed, depth)
	}
	// Surface name-ambiguity so the agent can refine instead of trusting
	// the first-match heuristic silently. Records up to 5 alternative
	// matches (the GetSymbolsByName cap) with enough info to disambiguate.
	// Skipped when the caller pinned the seed via `id=` — there's no
	// ambiguity to surface (#474).
	if id == "" && len(starts) > 1 {
		alts := make([]map[string]any, 0, len(starts))
		for _, s := range starts {
			alts = append(alts, map[string]any{
				"id":             s.ID,
				"qualified_name": s.QualifiedName,
				"kind":           s.Kind,
				"file_path":      s.FilePath,
			})
		}
		// #474: hint references the real `id` parameter (just added to
		// this tool) instead of the fictional `TraceByID` MCP tool.
		altID := starts[0].ID
		if len(starts) > 1 {
			altID = starts[1].ID
		}
		meta["ambiguous_match"] = map[string]any{
			"resolved_to":  starts[0].ID,
			"alternatives": alts,
			"hint":         fmt.Sprintf(`name %q matched %d symbols; trace used the first (%s). To trace a specific alternative, call trace again with id="%s" (or any other alternative id above). To narrow by name, call search with kind/language filters first.`, name, len(starts), starts[0].ID, altID),
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
	} else if testFilteredCount > 0 {
		// #898: the trace IS connected — just only via test files. The
		// default include_tests=false dropped them silently, so the empty
		// result read as "this symbol is a leaf." Surface the escape
		// hatch with the count so a heavily-tested utility doesn't look
		// dead just because its writers are all in *_test.go.
		retryArgs := fmt.Sprintf(`{"name":%q,"include_tests":true`, name)
		if direction != "both" {
			retryArgs += fmt.Sprintf(`,"direction":%q`, direction)
		}
		if kindsArg != "" {
			retryArgs += fmt.Sprintf(`,"kinds":%q`, kindsArg)
		}
		retryArgs += "}"
		meta["next_steps"] = []map[string]string{
			{"tool": "trace", "args": retryArgs,
				"why": fmt.Sprintf("%d hop(s) were filtered as test/fixture paths — re-run with include_tests=true to see them", testFilteredCount)},
		}
		meta["diagnosis"] = fmt.Sprintf(
			"empty result, but %d hop(s) exist in test/fixture files (filtered by default include_tests=false). Pass include_tests=true if test coverage is what you're after.",
			testFilteredCount)
	} else {
		// Empty trace = no inbound/outbound CALLS edges. Likely a leaf
		// (no callers) or an entry point (no callees). Direct the agent
		// to the source itself.
		meta["next_steps"] = []map[string]string{
			{"tool": "context", "args": fmt.Sprintf(`{"id":"%s"}`, starts[0].ID),
				"why": "no call edges found at this depth — read the symbol's own source instead"},
		}
		// #858: distinguish "this symbol is a leaf" from "this language
		// has no edge graph at all." For C / TS / etc. an empty trace is
		// the second case — say so rather than letting it read like a
		// genuine leaf result.
		if gap := s.edgeCoverageGap(projectID); gap != "" {
			meta["diagnosis"] = gap
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
	// #400: response-level field projection. Per-hop fields are NOT
	// trimmed (caller wanting fewer fields per hop should pass the
	// trim shape via downstream `symbol`/`symbols` calls). Top-level
	// projection lets callers drop e.g. `risk_summary` when they
	// only want the hop list.
	data = projectAndCheckFields(data, parseFieldsArg(str(args, "fields")))
	return s.jsonResultWithMeta(data, start, tool, args, s.savedVsFileSizesSession(projectID, traceRoot, tracedPaths, responseJSON)), nil
}

func (s *Server) handleChanges(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	projectArg := str(args, "project")
	scope := str(args, "scope")
	if scope == "" {
		scope = "unstaged"
	}
	depth := intArg(args, "depth", 3)
	// #877: clamp depth to the documented 1-5 range. Pre-fix an out-of-
	// range value (depth=0, depth=99) was passed straight through to
	// TraceByID which silently coerced it to 3 — the user got a depth-3
	// blast radius while believing they got what they asked for. Mirrors
	// the trace depth clamp (#703/#712).
	var changesDepthClampMsg string
	if depth < 1 {
		changesDepthClampMsg = fmt.Sprintf("changes: depth=%d clamped to 1 (valid range 1-5)", depth)
		depth = 1
	} else if depth > 5 {
		changesDepthClampMsg = fmt.Sprintf("changes: depth=%d clamped to 5 (valid range 1-5)", depth)
		depth = 5
	}

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
		// Rich envelope so the agent learns the valid scopes instead of
		// staring at a bare "git diff failed". Most common cause is a
		// base-branch typo (`base:mian`) or a non-existent branch — the
		// next_steps lead them at the supported shapes.
		return s.errResultRich(
			fmt.Sprintf("git diff failed: %v", diffErr),
			[]map[string]string{
				{"tool": "changes", "args": `{"scope":"unstaged"}`,
					"why": "default: working-tree changes not yet staged"},
				{"tool": "changes", "args": `{"scope":"staged"}`,
					"why": "changes added via git add (pre-commit blast radius)"},
				{"tool": "changes", "args": `{"scope":"all"}`,
					"why": "every dirty path including untracked files"},
				{"tool": "changes", "args": `{"scope":"base:master"}`,
					"why": "committed-only diff vs master's merge-base — preview a PR's blast radius. Use the actual base branch name (master/main/develop/…)"},
			}), nil
	}

	// Parse changed files from diff
	changedFiles := parseGitDiffFiles(diffOutput)

	// #502: also fetch the unified diff so per-file hunk ranges can
	// intersect each symbol's [StartLine, EndLine]. Pre-fix, every
	// symbol in any changed file was treated as "changed" — adding
	// one function to a 6000-line file expanded the blast radius BFS
	// to half the codebase. The hunk fetch is best-effort: on error
	// we fall back to the pre-#502 behaviour (all symbols in changed
	// files) so the tool stays usable when git options change shape.
	hunkDiff, hunkErr := runGitDiffHunks(root, scope)
	var hunksByFile map[string][][2]int
	if hunkErr == nil {
		hunksByFile = parseGitDiffHunks(hunkDiff)
	}

	// Find symbols in changed files. When we have hunks for a file,
	// keep only symbols whose line range overlaps an actual edit.
	// When hunks aren't available for a file (untracked content,
	// rename without content change, parse miss), fall back to
	// "all symbols in file" — better to over-report than under-report
	// for the safety-check use case.
	var changedSymbols []db.Symbol
	for _, f := range changedFiles {
		syms, err := s.store.GetSymbolsForFile(projectID, f)
		if err != nil {
			continue
		}
		hunks, hasHunks := hunksByFile[f]
		if !hasHunks || len(hunks) == 0 {
			changedSymbols = append(changedSymbols, syms...)
			continue
		}
		for _, sym := range syms {
			if symbolOverlapsHunks(sym.StartLine, sym.EndLine, hunks) {
				changedSymbols = append(changedSymbols, sym)
			}
		}
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

	// Build risk summary — count the FULL impacted set before any trim.
	riskCounts := map[string]int{"CRITICAL": 0, "HIGH": 0, "MEDIUM": 0, "LOW": 0}
	for _, item := range impacted {
		if r, ok := item["risk"].(string); ok {
			riskCounts[r]++
		}
	}

	// #729 follow-up: a change to a hot file (e.g. cypher/engine.go's
	// parseQuery) can blast-radius into 100+ impacted symbols + 100+
	// tests — the full response then exceeds the MCP token limit and
	// `changes` fails by default, exactly when it's most needed. Cap
	// both lists like neighborhood (#293): keep the highest-signal
	// entries (impacted sorted by risk severity, tests already sorted
	// by overlap desc), surface the true totals in `summary`, and warn.
	fullImpacted, fullTests := len(impacted), len(testsToRun)
	riskRank := map[string]int{"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2, "LOW": 3}
	sort.SliceStable(impacted, func(i, j int) bool {
		ri := riskRank[fmt.Sprint(impacted[i]["risk"])]
		rj := riskRank[fmt.Sprint(impacted[j]["risk"])]
		if ri != rj {
			return ri < rj
		}
		return fmt.Sprint(impacted[i]["id"]) < fmt.Sprint(impacted[j]["id"])
	})
	var changesTrimWarnings []string
	if changesDepthClampMsg != "" {
		changesTrimWarnings = append(changesTrimWarnings, changesDepthClampMsg)
	}
	if len(impacted) > changesMaxList {
		changesTrimWarnings = append(changesTrimWarnings, fmt.Sprintf(
			"impacted trimmed to the %d highest-risk of %d — see summary.total_impacted for the full count, or pass fields=summary,tests_to_run to drop the lists",
			changesMaxList, fullImpacted))
		impacted = impacted[:changesMaxList]
	}
	if len(testsToRun) > changesMaxList {
		changesTrimWarnings = append(changesTrimWarnings, fmt.Sprintf(
			"tests_to_run trimmed to the %d highest-overlap of %d — re-run those first",
			changesMaxList, fullTests))
		testsToRun = testsToRun[:changesMaxList]
	}

	// #330: zero-len init so the JSON field is [] when no symbols changed.
	changedSymNames := []map[string]any{}
	for _, sym := range changedSymbols {
		changedSymNames = append(changedSymNames, map[string]any{
			"id": sym.ID, "name": sym.Name, "kind": sym.Kind, "file_path": sym.FilePath,
		})
	}
	// #740: #730 capped `impacted` and `tests_to_run` but missed
	// `changed_symbols` — on a large diff (a wide rename, or scope=all
	// over a tree with many untracked multi-symbol files) this list is
	// unbounded and reopens the same response-bloat problem #730 closed.
	// Cap it the same way: sort by (file_path, id) so the trim is
	// deterministic, keep the first changesMaxList, and warn.
	// summary.changed_symbols already carries the true full count.
	fullChangedSyms := len(changedSymNames)
	if fullChangedSyms > changesMaxList {
		sort.SliceStable(changedSymNames, func(i, j int) bool {
			fi, fj := fmt.Sprint(changedSymNames[i]["file_path"]), fmt.Sprint(changedSymNames[j]["file_path"])
			if fi != fj {
				return fi < fj
			}
			return fmt.Sprint(changedSymNames[i]["id"]) < fmt.Sprint(changedSymNames[j]["id"])
		})
		changesTrimWarnings = append(changesTrimWarnings, fmt.Sprintf(
			"changed_symbols trimmed to %d of %d — see summary.changed_symbols for the full count, or pass fields=summary,tests_to_run to drop the lists",
			changesMaxList, fullChangedSyms))
		changedSymNames = changedSymNames[:changesMaxList]
	}

	responseJSON, _ := json.Marshal(impacted)
	data := map[string]any{
		"changed_files":   changedFiles,
		"changed_symbols": changedSymNames,
		"impacted":        impacted,
		"tests_to_run":    testsToRun,
		"summary": map[string]any{
			"changed_files":   len(changedFiles),
			"changed_symbols": len(changedSymbols),
			// total_impacted / tests_to_run report the FULL counts even
			// when the lists themselves were trimmed for response budget.
			"total_impacted": fullImpacted,
			"tests_to_run":   fullTests,
			"critical":       riskCounts["CRITICAL"],
			"high":           riskCounts["HIGH"],
			"medium":         riskCounts["MEDIUM"],
			"low":            riskCounts["LOW"],
		},
	}
	// Suggest the next move based on what changes found. CRITICAL impact
	// → trace the affected callers to inspect the chain. Non-zero impact
	// without CRITICAL → read context on the most-impacted symbol.
	// No impact → the change is local; suggest writing tests.
	meta := map[string]any{}
	if nextSteps := suggestChangesNextSteps(impacted, changedSymNames, riskCounts); len(nextSteps) > 0 {
		meta["next_steps"] = nextSteps
	}
	if len(changesTrimWarnings) > 0 {
		meta["warnings"] = changesTrimWarnings
	}
	if len(meta) > 0 {
		data["_meta"] = meta
	}
	// #400: response-level field projection. Common shape on chained
	// PR-prep flow: caller wants summary + tests_to_run, skips
	// changed_symbols/impacted lists. `fields=summary,tests_to_run`
	// drops ~80% of the response when the diff impacts many symbols.
	data = projectAndCheckFields(data, parseFieldsArg(str(args, "fields")))
	// Honest savings baseline: the agent's alternative was reading every
	// changed file plus every transitively-impacted symbol's file. Sum
	// real file sizes (de-duped + per-session dedup'd) rather than the
	// fabricated count × avgFileSize savedVsFullRead used to claim.
	changedPaths := make([]string, 0, len(changedSymbols)+len(impacted))
	for _, sym := range changedSymbols {
		if sym.FilePath != "" {
			changedPaths = append(changedPaths, sym.FilePath)
		}
	}
	for _, item := range impacted {
		if fp, ok := item["file_path"].(string); ok && fp != "" {
			changedPaths = append(changedPaths, fp)
		}
	}
	return s.jsonResultWithMeta(data, start, tool, args, s.savedVsFileSizesSession(projectID, root, changedPaths, responseJSON)), nil
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

// edgeCoverageGap returns a one-line advisory when a graph-shaped tool
// (trace / dead_code) is about to return an empty result *because* the
// project's dominant language has no cross-file edge resolution (#858).
// resolveImports/Calls/Reads cover Go and Python; C / TypeScript / Rust
// / etc. extract symbols fine but produce a zero-edge graph, so the
// graph tools are silent no-ops on them — an empty result that reads
// like "nothing to find" but actually means "unsupported language."
//
// Returns "" when the project has edges, has no symbols, or its
// dominant language does have resolution — i.e. whenever an empty graph
// result is genuinely meaningful. Best-effort: any DB error → "".
func (s *Server) edgeCoverageGap(projectID string) string {
	symCount, edgeCount, _, _, err := s.store.GraphStats(projectID)
	if err != nil || symCount == 0 || edgeCount > 0 {
		return ""
	}
	var lang string
	var cnt int
	row := s.store.RO().QueryRow(
		`SELECT language, COUNT(*) c FROM symbols WHERE project_id=? GROUP BY language ORDER BY c DESC LIMIT 1`,
		projectID)
	if err := row.Scan(&lang, &cnt); err != nil || lang == "" {
		return ""
	}
	switch lang {
	case "Go", "Python":
		// These have cross-file edge resolution — a zero-edge graph
		// here is a real finding, not a coverage gap.
		return ""
	}
	return fmt.Sprintf("This project is predominantly %s. Cross-file edge resolution currently covers Go and Python only (#858) — trace and dead_code return empty results here because the edge graph itself is empty, not because there are no callers / no dead code. Use search and neighborhood for %s navigation.", lang, lang)
}

func (s *Server) handleDeadCode(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)
	_ = ctx

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	language := str(args, "language")
	kindsRaw := str(args, "kinds")
	// #738: allocate as []string{} not nil — a nil slice marshals to JSON
	// `null`, and `kinds` is echoed verbatim into filters.kinds. Consumers
	// iterating filters.kinds without a null-check break (and filters.language
	// already defaults to "" not null — keep the echo block consistent). This
	// is the recurring nil-slice-in-response class CLAUDE.md flags.
	kinds := []string{}
	if kindsRaw != "" {
		for _, k := range strings.Split(kindsRaw, ",") {
			if k = strings.TrimSpace(k); k != "" {
				kinds = append(kinds, k)
			}
		}
	}
	// #851: validate kinds against the symbol-kind taxonomy actually
	// present in the project, mirroring trace's edge-kind validation
	// (traceKindWarnings). A typo'd kind otherwise matches zero rows
	// silently — the caller can't tell "Funktion isn't a kind" from
	// "those kinds are all clean". Unknown kinds are dropped (not failed)
	// so the rest of the filter still works, exactly like trace.
	var kindWarnings []string
	if len(kinds) > 0 {
		if _, _, kindCounts, _, statErr := s.store.GraphStats(projectID); statErr == nil && len(kindCounts) > 0 {
			validKinds := make([]string, 0, len(kindCounts))
			for k := range kindCounts {
				validKinds = append(validKinds, k)
			}
			sort.Strings(validKinds)
			validated := []string{}
			for _, k := range kinds {
				if _, ok := kindCounts[k]; ok {
					validated = append(validated, k)
				} else {
					kindWarnings = append(kindWarnings, fmt.Sprintf(
						"unknown kind %q ignored — no symbol in this project has that kind; valid kinds: %s",
						k, strings.Join(validKinds, ", ")))
				}
			}
			kinds = validated
		}
	}
	// 0.95 default biases toward AST-extracted languages (Go=1.0,
	// JSON/YAML/HCL parser-backed). Caller can drop to 0.0 to include
	// regex-tier languages at known false-positive cost (their CALLS
	// edges under-resolve cross-file).
	minConfidence := floatArg(args, "min_confidence", 0.95)
	minConfidence, deadMinConfWarn := clampMinConfidence(minConfidence)
	if deadMinConfWarn != "" {
		kindWarnings = append(kindWarnings, deadMinConfWarn)
	}
	limit := intArg(args, "limit", 100)
	// #879: surface the limit clamp instead of silently dropping the
	// caller-requested page size. search/neighborhood already warn on
	// limit clamp; dead_code didn't. Also clamps negative / zero to 1
	// (pre-fix passed straight through to GetDeadCode and produced an
	// empty or all-rows result depending on the driver).
	if rawL, present := args["limit"]; present {
		if l, ok := rawL.(float64); ok {
			rawLInt := int(l)
			if rawLInt < 1 {
				kindWarnings = append(kindWarnings,
					fmt.Sprintf("limit=%d clamped to 1 (valid range 1-500)", rawLInt))
				limit = 1
			} else if rawLInt > 500 {
				kindWarnings = append(kindWarnings,
					fmt.Sprintf("limit=%d clamped to 500 (valid range 1-500)", rawLInt))
				limit = 500
			}
		}
	}

	rawDead, err := s.store.GetDeadCode(projectID, kinds, language, minConfidence, limit*2)
	if err != nil {
		return errResult(fmt.Sprintf("dead_code: %v", err)), nil
	}

	// Post-filter: testdata fixtures (#393) and developer scratch
	// paths shouldn't appear — they're either fixture inputs (not
	// real code) or known-dead noise the developer doesn't need
	// told. SQL can't filter these without a path-pattern column;
	// LIMIT*2 from SQL + trim in Go is cheaper than a new column.
	dead := []map[string]any{}
	for _, sym := range rawDead {
		if isDeveloperScratchPath(sym.FilePath) || isTestFixturePath(sym.FilePath) {
			continue
		}
		if isRuntimeInvokedGoSymbol(sym.Language, sym.Name) {
			continue
		}
		dead = append(dead, map[string]any{
			"id":         sym.ID,
			"name":       sym.Name,
			"kind":       sym.Kind,
			"language":   sym.Language,
			"file_path":  sym.FilePath,
			"start_line": sym.StartLine,
			"complexity": sym.Complexity,
		})
		if len(dead) >= limit {
			break
		}
	}

	data := map[string]any{
		"dead_symbols": dead,
		"total":        len(dead),
		"filters": map[string]any{
			"language":       language,
			"kinds":          kinds,
			"min_confidence": minConfidence,
		},
	}
	if len(dead) > 0 {
		// Surface the obvious next move: read the top dead symbol's
		// source to confirm before deleting. trace inbound is the
		// safety check — sometimes the graph misses a caller (regex
		// extractor under-resolution) and the agent should verify
		// before suggesting a deletion.
		topID, _ := dead[0]["id"].(string)
		topName, _ := dead[0]["name"].(string)
		data["_meta"] = map[string]any{
			"next_steps": []map[string]string{
				{"tool": "symbol", "args": fmt.Sprintf(`{"id":"%s"}`, topID),
					"why": "read the top dead symbol's source before recommending deletion — confirm the graph isn't missing an inbound edge"},
				{"tool": "trace", "args": fmt.Sprintf(`{"name":"%s","direction":"inbound"}`, topName),
					"why": "double-check inbound callers — name-based trace catches references the symbol-id graph might miss for regex-extracted languages"},
			},
		}
	} else if gap := s.edgeCoverageGap(projectID); gap != "" {
		// #858: the empty result isn't "no dead code" — it's "this
		// language has no edge graph, so dead_code can't run." Tell the
		// caller that instead of the misleading "lower min_confidence"
		// advice (there are no edges at any confidence to lower toward).
		data["_meta"] = map[string]any{
			"diagnosis": gap,
			"next_steps": []map[string]string{
				{"tool": "health", "args": "{}",
					"why": "see the per-language extraction + edge coverage breakdown for this project"},
			},
		}
	} else {
		// #712: the advice was inverted — "tighten min_confidence" RAISES
		// the floor, which surfaces FEWER candidates. dead_code's
		// min_confidence gates which symbols enter the candidate pool;
		// LOWERING it (e.g. 0.95 → 0.7) lets lower-confidence symbols
		// also be considered, surfacing more potential dead code.
		//
		// #896: don't suggest a floor that's the same as or higher than
		// the caller's current value — that's either a no-op suggestion
		// (caller already at 0.7) or an inversion (caller at 0.0, we
		// recommend 0.7 which NARROWS the pool). Pick the next-lower
		// step; when already at the widest floor, drop the
		// min_confidence hint entirely.
		suggested := suggestDeadCodeFloor(minConfidence)
		var diagnosis string
		nextSteps := []map[string]string{}
		if suggested >= 0 {
			diagnosis = fmt.Sprintf("no dead code at min_confidence ≥ %.2f — lower min_confidence (e.g. %.2f) or broaden kinds to surface more candidates", minConfidence, suggested)
			nextSteps = append(nextSteps, map[string]string{
				"tool": "dead_code",
				"args": fmt.Sprintf(`{"min_confidence":%g}`, suggested),
				"why":  "lower the confidence floor so regex-extracted (sub-1.0) symbols enter the candidate pool",
			})
		} else {
			diagnosis = fmt.Sprintf("no dead code at min_confidence ≥ %.2f — already at the widest floor; broaden kinds (e.g. Function,Method,Class) to surface more candidates, or this language genuinely has no unreferenced internal symbols", minConfidence)
		}
		data["_meta"] = map[string]any{
			"diagnosis":  diagnosis,
			"next_steps": nextSteps,
		}
	}

	// #851: attach unknown-kind warnings to whichever _meta map the
	// branches above built. jsonResultWithMeta's unknown-args merge
	// appends to this same key, so set rather than risk a clobber.
	if len(kindWarnings) > 0 {
		if m, ok := data["_meta"].(map[string]any); ok {
			m["warnings"] = kindWarnings
		}
	}

	responseJSON, _ := json.Marshal(dead)
	// Honest savings: the agent's alternative was reading every file
	// containing a dead symbol. Sum real file sizes (de-duped + per-
	// session dedup'd) instead of fabricating count × avgFileSize.
	root, _ := s.resolveProjectRoot(projectID)
	deadPaths := make([]string, 0, len(dead))
	for _, d := range dead {
		if fp, ok := d["file_path"].(string); ok && fp != "" {
			deadPaths = append(deadPaths, fp)
		}
	}
	return s.jsonResultWithMeta(data, start, tool, args, s.savedVsFileSizesSession(projectID, root, deadPaths, responseJSON)), nil
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
				if isTestFixturePath(fp) {
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
	// #380: also exclude non-code kinds (Variable, Setting, Section)
	// since their high in-degree comes from data references, not from
	// callers depending on them as a change-risk surface.
	// Fetch over-quota and post-filter so the top-10 stays at the
	// intended size after dropping tests + non-hotspot kinds.
	includeTests := boolArg(args, "include_tests")
	hotspotFetchLimit := 100
	if includeTests {
		hotspotFetchLimit = 50 // legacy path keeps tests; still filter kinds
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
		if isTestFixturePath(h.FilePath) {
			// Fixtures sit in testdata/ — their symbols are real but
			// they're not signal for "what's the most-called code in
			// this project?" Same rationale as the entry_points filter.
			continue
		}
		if !isHotspotKind(h.Kind) {
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
	//
	// F1: drift warning, same rationale as handleSearch.
	s.attachDriftWarning(data, projectID)
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
	// Empty-graph diagnosis. The most common cause is a name-collision
	// with a stale project — e.g. `schema project="pincher"` resolves
	// to a dead-on-disk `D:\...\pincher` row with 0 symbols rather
	// than the intended `pincher-repo`. Without a diagnosis the caller
	// sees `{symbols:0, edges:0, node_kinds:{}, edge_kinds:{}}` and
	// has no idea whether the project really is empty or whether they
	// resolved the wrong one. Same failure-as-pedagogy throughline as
	// #983 (list empty-state) and #974 (errResultRich progress).
	if symCount == 0 {
		meta := map[string]any{
			"diagnosis": "this project has 0 indexed symbols — either the project really is empty, or the project arg resolved to a stale name-collision (e.g. \"pincher\" matching a dead-on-disk row instead of \"pincher-repo\"). Confirm with `list` or re-index the intended path.",
			"next_steps": []map[string]string{
				{"tool": "list", "args": `{"include_dead":true}`,
					"why": "include dead-on-disk projects so a name-collision shows up — compare the path on the row that resolved here"},
				{"tool": "index", "args": `{"path":"/absolute/path/to/intended/project","force":true}`,
					"why": "re-index from the actual path you meant; the project arg matched by name and the intended one may not be indexed yet"},
			},
		}
		data["_meta"] = meta
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
	// #302/#378: explicit prune flag. When true, dead-on-disk projects
	// are physically removed from the store. Orthogonal to include_dead
	// — set both to audit + cleanup in one call (the pruned ids are
	// reported in the response so the caller sees what got deleted).
	pruneDead := false
	if v, ok := args["prune_dead"].(bool); ok {
		pruneDead = v
	}
	// #301: pagination. Pre-fix `limit=0` meant "all rows", which on
	// dev machines with 100+ indexed projects (worktree fan-out from
	// adjacent tools) returned a 10K-token response for what's almost
	// always a yes/no orientation lookup. Default to 50; the caller
	// can ask for more via explicit `limit`.
	// #712: collect input-clamp warnings so callers learn when a
	// negative arg was silently coerced. Surfaced in _meta.warnings.
	var listClampWarnings []string
	limit := 50
	if v, ok := args["limit"].(float64); ok {
		if v > 0 {
			limit = int(v)
		} else if v == 0 {
			// limit=0 is the documented "all rows" sentinel.
			limit = -1
		} else {
			// Negative is NOT the documented sentinel — almost certainly
			// a caller mistake. Treat as unbounded for back-compat but
			// warn so the caller learns the right shape.
			listClampWarnings = append(listClampWarnings,
				fmt.Sprintf("limit=%d treated as unbounded — pass limit=0 for the documented 'all rows' sentinel, or limit>0 for a page size", int(v)))
			limit = -1
		}
	}
	offset := 0
	if v, ok := args["offset"].(float64); ok {
		if v > 0 {
			offset = int(v)
		} else if v < 0 {
			listClampWarnings = append(listClampWarnings,
				fmt.Sprintf("offset=%d clamped to 0 (must be >= 0)", int(v)))
		}
	}
	// Default activity threshold: 14 days. Configurable per-call via
	// `active_within_days` for users who want the broader view.
	activeWithinDays := 14
	if v, ok := args["active_within_days"].(float64); ok {
		if v > 0 {
			activeWithinDays = int(v)
		} else {
			listClampWarnings = append(listClampWarnings,
				fmt.Sprintf("active_within_days=%d ignored (must be > 0) — using default 14", int(v)))
		}
	}
	cutoff := time.Now().Add(-time.Duration(activeWithinDays) * 24 * time.Hour)
	// #419: drop projects without a usable graph by default. On
	// developer machines with a worktree fan-out (`.claude/worktrees/`
	// adjective-scientist slugs from concurrent agent runs), `list`
	// defaulted to surfacing 30+ empty-graph entries that pushed the
	// real project out of the response window. min_edges=1 keeps the
	// orientation view useful; pass min_edges=0 to opt back into the
	// legacy unfiltered shape.
	minEdges := 1
	if v, ok := args["min_edges"].(float64); ok {
		minEdges = int(v)
	}

	// Filter first, paginate after — `count` reports the post-filter
	// total so the caller can decide whether the next page is worth
	// fetching. #334: zero-len init so list returns "projects":[] (not
	// null) when the store has no projects.
	//
	// #505: track WHY each filter dropped a project so the agent can
	// see what's hidden. Pre-fix, the lump-sum `dropped` counter was
	// opaque — agents had no signal whether to pass min_edges=0 or
	// include_dead=true or active=false to recover the missing entry.
	filtered := []map[string]any{}
	dropped := 0
	droppedDead := 0
	droppedInactive := 0
	droppedLowEdges := 0
	var pruned []string // #302: ids of dead-on-disk projects we deleted
	for _, p := range projects {
		// Stat once per project — used by both the include_dead filter
		// and the prune_dead deletion. include_dead and prune_dead are
		// orthogonal (#378): include_dead=true asks "show dead rows in
		// the response", prune_dead=true asks "delete dead rows from
		// the DB". Combining them = audit + cleanup in one call.
		// Pre-#378 the prune branch was nested inside !includeDead,
		// so include_dead=true silently no-op'd prune_dead.
		_, statErr := os.Stat(p.Path)
		dead := os.IsNotExist(statErr)

		if dead && pruneDead {
			// #302: delete the row so it doesn't keep appearing in
			// subsequent list calls. Failure to delete is non-fatal —
			// we still report it via dropped/included and let the next
			// call try again.
			if delErr := s.store.DeleteProject(p.ID); delErr == nil {
				pruned = append(pruned, p.ID)
			}
		}
		if dead && !includeDead {
			// Hide dead rows from the response unless the caller
			// explicitly opts in. The DB row may have been pruned
			// above; either way it doesn't appear here.
			dropped++
			droppedDead++
			continue
		}
		if activeOnly && p.IndexedAt.Before(cutoff) {
			dropped++
			droppedInactive++
			continue
		}
		// #419: edge-count gate. Empty-graph projects are usually
		// ephemeral worktrees or pre-extraction stubs that crowd the
		// orientation view without adding useful info. Caller can pass
		// min_edges=0 to see them anyway.
		if minEdges > 0 && p.EdgeCount < minEdges {
			dropped++
			droppedLowEdges++
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
		// #505: split the lump-sum dropped count into per-reason
		// buckets so the agent knows what knob recovers the missing
		// entry. Always present (zero values surface as 0, not omitted)
		// so downstream consumers can rely on the shape.
		"filtered_breakdown": map[string]any{
			"dead_path": droppedDead,
			"inactive":  droppedInactive,
			"low_edges": droppedLowEdges,
		},
		"page": map[string]any{
			"limit":    limit,
			"offset":   offset,
			"returned": len(rows),
		},
	}
	// #505: when a filter dropped anything, surface a one-line diagnosis
	// telling the agent which arg toggles to flip. Skipped when nothing
	// was filtered (no signal needed).
	if dropped > 0 {
		var hints []string
		if droppedDead > 0 {
			hints = append(hints, fmt.Sprintf("%d dead path (pass include_dead=true)", droppedDead))
		}
		if droppedInactive > 0 {
			hints = append(hints, fmt.Sprintf("%d inactive >%dd (pass active=false or active_within_days=N)", droppedInactive, activeWithinDays))
		}
		if droppedLowEdges > 0 {
			hints = append(hints, fmt.Sprintf("%d below min_edges=%d (pass min_edges=0)", droppedLowEdges, minEdges))
		}
		meta, _ := data["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["filter_diagnosis"] = "filtered: " + strings.Join(hints, ", ")
		data["_meta"] = meta
	}
	// #712: surface input-clamp warnings (negative limit/offset/
	// active_within_days). Merge into _meta.warnings, never clobber —
	// jsonResultWithMeta's unknown-args path appends to the same key.
	if len(listClampWarnings) > 0 {
		meta, _ := data["_meta"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		existing, _ := meta["warnings"].([]string)
		meta["warnings"] = append(existing, listClampWarnings...)
		data["_meta"] = meta
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
			// how to surface the suppressed rows. Diagnosis names the
			// dominant filter cause so the next_steps actually map to
			// the recovery args. Pre-fix the message hardcoded
			// "stale or dead-on-disk" even when min_edges was the
			// dominant reason (73 of 128 dropped on a fresh test) —
			// the recommended next_steps (active=false, include_dead)
			// wouldn't have recovered the result.
			causes := []string{}
			if droppedInactive > 0 {
				causes = append(causes, fmt.Sprintf("%d inactive >%dd", droppedInactive, activeWithinDays))
			}
			if droppedDead > 0 {
				causes = append(causes, fmt.Sprintf("%d dead-on-disk", droppedDead))
			}
			if droppedLowEdges > 0 {
				causes = append(causes, fmt.Sprintf("%d below min_edges=%d", droppedLowEdges, minEdges))
			}
			meta["diagnosis"] = fmt.Sprintf("no projects after filters: %s — pass the recovery args below to widen the scope", strings.Join(causes, ", "))
			recovery := []map[string]string{}
			if droppedInactive > 0 {
				recovery = append(recovery, map[string]string{"tool": "list", "args": `{"active":false}`,
					"why": "include projects whose last index is older than the activity window"})
			}
			if droppedDead > 0 {
				recovery = append(recovery, map[string]string{"tool": "list", "args": `{"include_dead":true}`,
					"why": "include projects whose on-disk path no longer exists (stale DB rows)"})
			}
			if droppedLowEdges > 0 {
				recovery = append(recovery, map[string]string{"tool": "list", "args": `{"min_edges":0}`,
					"why": "drop the edge-count floor — surface projects with empty or sparse graphs (regex-extracted languages, pre-extraction stubs)"})
			}
			meta["next_steps"] = recovery
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

	// #712: failure-as-pedagogy — every adr arg-validation error points
	// the caller at a concrete valid call shape instead of just naming
	// the missing field.
	adrUsageSteps := []map[string]string{
		{"tool": "adr", "args": `{"action":"list"}`,
			"why": "list all stored decisions/conventions for this project"},
		{"tool": "adr", "args": `{"action":"set","key":"STACK","value":"Go+SQLite"}`,
			"why": "store a decision — key is a short label, value is the body"},
		{"tool": "adr", "args": `{"action":"get","key":"STACK"}`,
			"why": "retrieve one entry by key"},
	}
	var data map[string]any
	switch action {
	case "get":
		if key == "" {
			return s.errResultRich("key is required for action=get", adrUsageSteps), nil
		}
		val, ok, err := s.store.GetADR(projectID, key)
		if err != nil {
			return errResult(err.Error()), nil
		}
		if !ok {
			// #712: failure-as-pedagogy — a missing key is most often a
			// typo or a wrong-project scope. `list` shows what keys DO
			// exist so the caller can correct rather than guess.
			return s.errResultRich(
				fmt.Sprintf("ADR key %q not found", key),
				[]map[string]string{
					{"tool": "adr", "args": `{"action":"list"}`,
						"why": "shows every key stored for this project — check for a typo or wrong-project scope"},
				}), nil
		}
		data = map[string]any{"key": key, "value": val}

	case "set":
		if key == "" || value == "" {
			return s.errResultRich("key and value are required for action=set", adrUsageSteps), nil
		}
		// #534: enforce length limits — pre-fix the form accepted
		// arbitrary-length input, and a paste-of-an-entire-transcript
		// blew up the row. Backend bounds match the form's maxlength
		// attributes so the validation message is consistent across
		// surfaces (UI rejects on submit, MCP/HTTP reject server-side).
		if len(key) > adrKeyMaxLen {
			return errResult(fmt.Sprintf(
				"key too long: %d chars exceeds limit of %d (#534). Use a shorter ADR key — keys are display labels, not the body.",
				len(key), adrKeyMaxLen)), nil
		}
		if len(value) > adrValueMaxLen {
			return errResult(fmt.Sprintf(
				"value too long: %d bytes exceeds limit of %d (#534). Trim the body or split across multiple ADR entries — long pastes are a smell that the value is doing the job of a separate document.",
				len(value), adrValueMaxLen)), nil
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
			return s.errResultRich("key is required for action=delete", adrUsageSteps), nil
		}
		if err := s.store.DeleteADR(projectID, key); err != nil {
			return errResult(err.Error()), nil
		}
		data = map[string]any{"key": key, "deleted": true}

	default:
		return s.errResultRich(
			fmt.Sprintf("unknown action %q — valid actions: get, set, list, delete", action),
			adrUsageSteps), nil
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
		// #944: Python has a regex/AST dispatcher behind the langAdapter.
		// The adapter registers confidence=0.85 (the regex fallback's
		// honesty), so the above loop labels Python "Regex" even when
		// extractPythonAST is actually running. PythonAvailable() probes
		// the same gate the dispatcher uses, so we can upgrade the label
		// to "AST" when the runtime conditions for AST extraction hold.
		// Honest signal to agents that filter by parser identity.
		if report.Coverage[i].Language == "Python" && ast.PythonAvailable() {
			report.Coverage[i].Parser = "AST"
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
	//
	// The hint text differs based on PINCHER_AUTO_RESTART_ON_DRIFT
	// (#352). When the env var is "1", the supervisor / MCP client
	// will respawn the server on the next tool call (typically
	// within ~100ms — see autoRestartExitDelay), so the manual
	// "/mcp reconnect" advice would mislead the caller. When unset,
	// the user has to drive the reconnect themselves, so we keep
	// the explicit `/mcp reconnect` hint.
	binaryReplaced := false
	if s.binaryPath != "" && !s.binaryStartMTime.IsZero() {
		if info, err := os.Stat(s.binaryPath); err == nil && info.ModTime().After(s.binaryStartMTime) {
			data["binary_stale"] = true
			if os.Getenv(autoRestartEnvVar) == "1" {
				data["binary_stale_message"] = "Newer pincher binary on disk; supervisor will respawn on the next tool call (PINCHER_AUTO_RESTART_ON_DRIFT=1)."
			} else {
				data["binary_stale_message"] = "Newer pincher binary on disk; restart the MCP server (/mcp reconnect) to pick up changes, or set PINCHER_AUTO_RESTART_ON_DRIFT=1 for hands-off swaps via `pincher supervised`."
			}
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
		}
	}

	// All-time savings summed across every persisted session. No $-figures
	// (#476 SAVINGS_HONESTY): we don't know the user's model or pricing.
	atCalls, atUsed, atSaved, _, _ := s.store.GetAllTimeSavings()

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
	// #619: bounded-percentage form alongside the absolute count. Capped
	// at 100% by construction; trivially comparable across sessions and
	// across users without ratio compounding. Suppressed when there's no
	// meaningful comparison to draw.
	pct := ""
	if baseline > 0 && tokensSaved > 0 {
		pct = fmt.Sprintf("  (%.0f%%)", float64(tokensSaved)/float64(baseline)*100)
	}

	var b strings.Builder
	b.WriteString("┌" + strings.Repeat("─", w) + "┐\n")
	b.WriteString(header("SESSION"))
	// #420: the SESSION counters are process-scoped — they reset every
	// time the supervised inner respawns (binary swap, crash recovery,
	// probe timeout). Surface the uptime so the agent can tell the
	// difference between "session=zero because nothing has happened"
	// and "session=low because the inner just respawned and the prior
	// session's counters rolled over into ALL-TIME". When uptime is
	// short relative to ALL-TIME activity, the agent knows a respawn
	// happened recently.
	b.WriteString(line("Process up:", humanDuration(time.Since(s.sessionStartedAt))))
	b.WriteString(line("Tool calls:", commify(calls)))
	b.WriteString(line("Without pincher:", "~"+commify(baseline)+" tokens"))
	b.WriteString(line("With pincher:", commify(tokensUsed)+" tokens"))
	b.WriteString(line("Saved:", "~"+commify(tokensSaved)+" tokens"+pct))
	b.WriteString(line("Avg latency:", fmt.Sprintf("%d ms", avgLatency)))

	// ALL-TIME section — only render when the DB has data (otherwise it's
	// just a row of zeros, noisy for first-use).
	if atCalls > 0 {
		b.WriteString(sep)
		b.WriteString(header("ALL-TIME"))
		b.WriteString(line("Tool calls:", commify(atCalls)))
		b.WriteString(line("Tokens used:", commify(atUsed)))
		b.WriteString(line("Tokens saved:", "~"+commify(atSaved)))
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

// humanDuration formats a duration with the smallest meaningful unit for
// the SESSION view: seconds < 1m, minutes < 1h, then h+m. Avoids the
// full "1h2m3s" Go default when sub-minute precision isn't useful.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}


// maxFetchBytes caps the HTTP response body read to 512 KB.
const maxFetchBytes = 512 * 1024

// maxDocstringBytes caps the extracted text stored per Document symbol to 32 KB.
const maxDocstringBytes = 32 * 1024

// maxFetchRedirects caps the redirect chain depth in handleFetch. Each hop
// is re-validated through validateFetchURL so a public-looking initial URL
// can't redirect into RFC1918 / loopback / link-local ranges.
const maxFetchRedirects = 5

// normalizeFetchURL canonicalizes a fetch URL so the same resource keyed
// different ways produces one Document symbol instead of duplicates.
// Found during v0.56 dogfooding: `fetch https://example.com` and
// `fetch https://example.com/` created two separate Document rows
// because the symbol ID is keyed on the raw input string.
//
// Normalization: lowercase scheme + host, drop the default port, empty
// path → "/", drop the fragment (never sent to the server). The query
// string is preserved — distinct queries are distinct resources. On
// parse failure the raw string is returned unchanged (the caller has
// already validated it; this is belt-and-suspenders).
func normalizeFetchURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "http" && strings.HasSuffix(u.Host, ":80")) ||
		(u.Scheme == "https" && strings.HasSuffix(u.Host, ":443")) {
		u.Host = u.Host[:strings.LastIndex(u.Host, ":")]
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.Fragment = ""
	return u.String()
}

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

// fetchDialControl is the net.Dialer.Control hook for handleFetch's HTTP
// client (#843). It runs after DNS resolution, immediately before
// connect(2), with the literal ip:port being dialed — closing the
// DNS-rebinding TOCTOU window between validateFetchURL's lookup and the
// transport's own lookup. The IP checked here IS the IP connected to.
func (s *Server) fetchDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("dial: cannot split %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("dial: cannot parse IP %q", host)
	}
	if reason := s.fetchIPBlockReason(ip); reason != "" {
		return fmt.Errorf("blocked at dial: %s (%s)", reason, ip)
	}
	return nil
}

func (s *Server) handleFetch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	rawURL := str(args, "url")
	if rawURL == "" {
		// #712: failure-as-pedagogy — show a concrete valid fetch call.
		return s.errResultRich("url is required", []map[string]string{
			{"tool": "fetch", "args": `{"url":"https://pkg.go.dev/net/http"}`,
				"why": "fetch pulls external docs/specs into the searchable knowledge base — http/https only"},
		}), nil
	}
	titleOverride := str(args, "title")

	if err := s.validateFetchURL(rawURL); err != nil {
		// #712: failure-as-pedagogy — the URL was rejected (bad scheme,
		// blocked address, ...); show the accepted shape.
		return s.errResultRich(fmt.Sprintf("invalid url %q: %v", rawURL, err), []map[string]string{
			{"tool": "fetch", "args": `{"url":"https://example.com/docs"}`,
				"why": "only public http/https URLs are accepted — loopback, link-local, and private addresses are blocked"},
		}), nil
	}

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	// #733: key the stored Document symbol on a normalized URL so that
	// trivially-different spellings of the same resource (case-folded
	// scheme/host, default :80/:443 port, missing trailing path,
	// fragment) collapse to one symbol instead of accumulating
	// duplicates. The raw URL is still used for the actual HTTP request.
	normURL := normalizeFetchURL(rawURL)

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
	//
	// #843: validateFetchURL and the transport's dial do INDEPENDENT DNS
	// lookups, so a host the attacker controls can answer the validation
	// lookup with a public IP and the dial lookup with a private one
	// (classic DNS-rebinding TOCTOU). The net.Dialer.Control hook runs
	// after resolution, immediately before connect(2), with the literal
	// ip:port being dialed — so the IP that is checked IS the IP that is
	// connected to. validateFetchURL + CheckRedirect stay as the fast,
	// friendly pre-flight; Control is the belt-and-suspenders that makes
	// rebinding impossible regardless of how many lookups happen.
	dialer := &net.Dialer{Control: s.fetchDialControl}
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
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
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

	// #579: Content-Type-aware extraction. text/html → strip tags +
	// <title>; text/markdown / text/plain → preserve verbatim, parse
	// first H1 as title for markdown. Pre-fix every fetched URL ran
	// through extractTextFromHTML unconditionally, which (a) eats
	// stray `>` characters even outside tags (arrows, generics,
	// blockquotes silently mangled), and (b) returns empty title for
	// non-HTML inputs since `<title>` doesn't exist in markdown.
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	var pageTitle, text string
	if strings.Contains(contentType, "text/markdown") || strings.Contains(contentType, "text/plain") || strings.HasSuffix(strings.ToLower(rawURL), ".md") {
		text = extractMarkdownText(string(rawBytes))
		pageTitle = firstMarkdownH1(string(rawBytes))
	} else {
		pageTitle, text = extractTextFromHTML(string(rawBytes))
	}
	if titleOverride != "" {
		pageTitle = titleOverride
	}
	if pageTitle == "" {
		pageTitle = normURL
	}
	if len(text) > maxDocstringBytes {
		text = text[:maxDocstringBytes] + "\n[truncated]"
	}

	symID := db.MakeSymbolID(normURL, normURL, "Document")
	sym := db.Symbol{
		ID:        symID,
		ProjectID: projectID,
		FilePath:  normURL,
		Name:      pageTitle,
		// QualifiedName must stay == normURL: it's the stable per-URL
		// key the #733 re-fetch dedup relies on (a page title can
		// change between fetches; the normalized URL can't).
		QualifiedName: normURL,
		Kind:          "Document",
		Language:      "text",
		Docstring:     text,
		// #766: signature == file_path == qualified_name was pure
		// redundancy. The page title gives `search` a meaningful
		// one-line signature instead of a third copy of the URL.
		Signature:            pageTitle,
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
		"url":       normURL,
		"title":     pageTitle,
		"text":      text,
		"raw_bytes": len(rawBytes),
		"stored":    true,
	}

	// #617: warn when the extracted text is suspiciously small relative
	// to the raw response — typical of JS-rendered SPAs (GitHub, Twitter,
	// Reddit, modern docs sites) where the static HTML is a near-empty
	// shell. Pre-fix the response looked successful (`stored: true`,
	// realistic `raw_bytes`, real `title`) but `text` was just the
	// inert accessibility skip-link. Threshold: > 10 KB raw AND text/raw
	// ratio < 0.5%. Skip the heuristic for markdown/plain inputs (their
	// "extraction" is verbatim copy and the ratio approaches 1).
	if !strings.Contains(contentType, "text/markdown") && !strings.Contains(contentType, "text/plain") {
		if len(rawBytes) > 10000 && len(text)*200 < len(rawBytes) {
			// #945: sanity-check by counting <p> tags in the raw HTML before
			// blaming JS rendering. If the page has many paragraph tags but
			// extraction produced near-zero text, the bug is in the
			// extractor (or this prefix-match family), not in the page. Pre-fix
			// Wikipedia (100+ <p> tags, fully static) was misdiagnosed as
			// JS-rendered — sending users in the wrong direction.
			pTags := bytes.Count(bytes.ToLower(rawBytes), []byte("<p>")) +
				bytes.Count(bytes.ToLower(rawBytes), []byte("<p "))
			meta, _ := data["_meta"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
			}
			warnings, _ := meta["warnings"].([]string)
			var msg string
			if pTags >= 5 {
				msg = fmt.Sprintf("fetched %d bytes but extracted only %d chars of text (%.2f%%) — page has %d <p> tags so the static HTML is not empty. The extractor failed on this page structure. File against pincher fetch with the URL and content-type.",
					len(rawBytes), len(text), float64(len(text))*100/float64(len(rawBytes)), pTags)
			} else {
				msg = fmt.Sprintf("fetched %d bytes but extracted only %d chars of text (%.2f%%) — page is likely JS-rendered. Static HTML extraction won't surface the visible content. For GitHub/GitLab repos try the raw README URL (e.g. https://raw.githubusercontent.com/<owner>/<repo>/<branch>/README.md); for other SPAs prefer the project's REST/JSON API or a markdown mirror.",
					len(rawBytes), len(text), float64(len(text))*100/float64(len(rawBytes)))
			}
			warnings = append(warnings, msg)
			meta["warnings"] = warnings
			data["_meta"] = meta
		}
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
	shapeAudit      guideShape = "audit"      // structural audit (#467) — undocumented/untested surveys (pinchQL predicate)
	shapeToolAudit  guideShape = "tool_audit" // empirical audit of a tool's OUTPUT quality (#497) — "find FPs in dead_code", "audit search results"
	shapeDeadCode   guideShape = "dead_code"  // unreachable / zero-caller / unused-function surveys — routes to the dead_code tool
	shapeUnknown    guideShape = "unknown"    // fallback
)

// pincherToolNames is the set of registered tool names a tool-output
// audit task might reference (#497). Used by classifyTaskShape and
// extractAuditedTool to recognize "find FPs in dead_code" / "audit
// search results" tasks. Order matters: longer names first so
// "dead_code" wins over a hypothetical "dead" prefix match.
var pincherToolNames = []string{
	"dead_code", "neighborhood", "architecture",
	"changes", "context", "fetch", "health", "index", "list",
	"query", "schema", "search", "stats", "symbol", "symbols",
	"trace", "guide", "adr",
}

// auditShapePattern matches the structural-audit phrasing #608
// flagged: "(find|list|count) [every|all|any] <noun> (without|missing|
// lacking|that lacks|that has no|that doesn't have|with no|with zero)
// ...". Anchored with `\b` so partial matches don't trigger; case-
// insensitive matching is the caller's job (input is already lowercased
// before the regex runs).
//
// The quantifier (every|all|any) is optional (#992): "find symbols that
// have no test coverage" is structurally an audit query but lacked the
// "every|all|any" prefix and pre-fix slipped through to shapeFind →
// BM25 search of "have no", which matches nothing. The absence-phrase
// alternation is the load-bearing audit signal; without it the regex
// can't match, so dropping the quantifier doesn't catch generic
// "find the X" phrasings (no absence word → no match).
var auditShapePattern = regexp.MustCompile(
	`\b(find|list|count|show|surface) (?:(every|all|any) )?\w+( \w+){0,3}?` +
		` (without|missing|lacking|lacks|has no|have no|doesn't have|does not have|with no|with zero|where there's no)\b`,
)

// auditThresholdPattern (#912) matches metric-audit phrasings like
// "find every function with complexity above 50" or "list all methods
// with more than 100 lines". Same structural-audit intent as the
// absence pattern above — the right tool is `query` with a pinchQL
// predicate, not BM25 text search.
//
// Allows 0-3 optional words between "with/having/whose" and the
// comparison so "with more than 100" (zero metric words before the
// multi-word comparison) and "with complexity above 50" (one metric
// word) both match. Trailing `\b` is omitted because some comparison
// alternatives end in non-word characters (>, >=, <, <=) which never
// satisfy `\b`.
//
// Lowercased-input invariant matches auditShapePattern's contract.
var auditThresholdPattern = regexp.MustCompile(
	`\b(find|list|count|show|surface) (every|all|any) \w+( \w+){0,4}?` +
		` (with|having|whose)( \w+){0,3}? ` +
		`(above|over|exceeding|greater than|larger than|bigger than|more than|below|under|less than|smaller than|at least|at most|exceeds|>=|<=|>|<)`,
)

// auditLooseThresholdPattern (#924) catches the more natural phrasings
// of threshold audits that drop the "every|all|any" article AND the
// "with|having|whose" clause — e.g. "find functions longer than 100
// lines". The adjective-form comparison ("longer/shorter/bigger than")
// is itself a strong audit signal, so the surrounding scaffold isn't
// required to disambiguate from prose.
var auditLooseThresholdPattern = regexp.MustCompile(
	`\b(find|list|count|show|surface) \w+( \w+){0,3}? ` +
		`(longer|shorter|bigger|smaller|larger|deeper|wider|heavier|slower|faster) than \b`,
)

// auditAdjectivePattern (#924) catches "find untested exported
// functions" — the standalone audit adjectives that mean "missing X"
// without using comparison or "with no X" scaffolding. Must run BEFORE
// shapeTest, otherwise "untested" matches the bare "test" substring
// check and routes to test-writing flow. "unused" is intentionally
// NOT in the adjective list — it's already routed to shapeDeadCode
// (more specific than a generic audit query).
var auditAdjectivePattern = regexp.MustCompile(
	`\b(find|list|count|show|surface)( \w+){0,2} ` +
		`(untested|undocumented|uncovered|untyped|unowned|unauthenticated|unvalidated|unhandled)\b`,
)

// auditBareThresholdPattern (#951) catches "find all functions over
// 100 lines" — threshold audits that drop the "with|having|whose"
// scaffold AND use a bare preposition (over/under/above/below/more
// than/less than/at least/at most) instead of a comparative adjective
// ("longer/bigger than"). Without this, the most common audit phrasing
// in code review falls through to BM25 text search on the literal
// words. Same #473-family silent-quality-loss as the others in this
// classifier.
//
// Requires a digit after the preposition to anchor "this is a metric
// threshold," not prose ("look over there"). 0-3 optional words between
// the subject noun and the preposition catches "find all Go functions
// over 100 lines."
var auditBareThresholdPattern = regexp.MustCompile(
	`\b(find|list|count|show|surface) \w+( \w+){0,3}? ` +
		`(over|under|above|below|more than|less than|at least|at most|>=|<=|>|<)\s*\d`,
)

// refactorExtractWord word-bounds the "extract" refactor verb so it
// doesn't substring-match the nouns "extraction" / "extractor" /
// "extracted" — all common in this codebase (#784). The other refactor
// keywords ("refactor", "rename", "restructure", "clean up") are
// distinctive enough to stay plain substring checks.
var refactorExtractWord = regexp.MustCompile(`\bextract\b`)

// reviewDiffWord (#937) word-bounds the "diff" review keyword so it
// doesn't substring-match "difference" / "different" / "differentiate".
// Same pattern as refactorExtractWord — bare `contains("diff")`
// caught "what's the difference between symbol and context" and
// routed it to shapeReview, recommending `changes` for a meta
// question about pincher's own tool surface.
var reviewDiffWord = regexp.MustCompile(`\bdiff\b`)

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
	// #497: tool-output audit — "find FPs in dead_code", "audit search
	// results", "characterize trace failures". The user is investigating
	// a tool's output quality, not generic bugs in its source. The
	// recipe is: run the tool, verify each result, cluster FPs.
	// Routed before shapeFix so "find false positives in dead_code"
	// doesn't match "find " (shapeFind) or "fix" — both would send the
	// agent down the wrong workflow.
	case extractAuditedTool(t) != "" &&
		contains("false positive", "false-positive", "fp ", "fps ",
			"audit", "noise", "noisy", "wrong result", "incorrect",
			"precision", "verify output", "characterize"):
		return shapeToolAudit
	// Dead-code surveys route to the `dead_code` tool — the canonical
	// answer for "which functions have zero callers". Checked before
	// shapeTest/shapeFind so "find unused functions with no test
	// coverage" doesn't match "coverage" and route to a search+context
	// flow that never mentions dead_code. The shapeToolAudit case above
	// already claimed "find FPs in dead_code" (it carries the tool name),
	// so a plain dead-code survey lands here.
	case contains("dead code", "dead-code", "unused function", "unused method",
		"unused code", "unreachable", "zero callers", "no callers",
		"never called", "uncalled",
		// #768: "no callers" misses the more technical phrasings a dev
		// actually types — "no inbound callers/edges" (the exact
		// language dead_code's own docs use) has a word between "no"
		// and "callers", so the substring check fell through to
		// shapeUnknown. "nothing calls" / "never used" are the same
		// survey intent.
		"no inbound caller", "no inbound edge", "nothing calls", "never used"):
		return shapeDeadCode
	// #608/#780: structural-audit pattern ("find every X without Y")
	// routes to a pinchQL audit query. Runs before shapeFix — otherwise
	// "find every handler that has no error return" matches `error` and
	// routes to fix. Must run AFTER shapeDeadCode: this regex also
	// matches "find all functions with no callers", but a task naming
	// callers/unused code is a dead-code survey, not a generic docstring
	// audit — pre-fix it grabbed those tasks and recommended the
	// hardcoded `docstring IS NULL` query regardless.
	case auditShapePattern.MatchString(t):
		return shapeAudit
	// #912: threshold/comparison audits ("find every function with
	// complexity above 50") are the same structural-audit intent as
	// the absence pattern — pinchQL with a numeric WHERE predicate
	// answers them, BM25 search of the literal phrase doesn't.
	case auditThresholdPattern.MatchString(t):
		return shapeAudit
	// #924: loose threshold form — "find functions longer than 100
	// lines" — drops the "every|all|any" article and the
	// "with|having|whose" clause. The adjective-form comparison is
	// itself the audit signal.
	case auditLooseThresholdPattern.MatchString(t):
		return shapeAudit
	// #951: bare-preposition threshold form — "find functions over
	// 100 lines" — drops both the comparative adjective AND the
	// "with/having" scaffold. Anchored on the digit after the
	// preposition so "look over there" doesn't false-trigger.
	case auditBareThresholdPattern.MatchString(t):
		return shapeAudit
	// #924: audit adjectives ("untested", "undocumented", ...) — must
	// run before the shapeTest case below, which would otherwise catch
	// "untested" via the bare "test" substring check and recommend a
	// test-writing flow instead of a coverage audit.
	case auditAdjectivePattern.MatchString(t):
		return shapeAudit
	case contains("test", "spec ", "coverage"):
		return shapeTest
	// #937: "diff" word-bounded so it doesn't substring-match
	// "difference" / "different" / "differentiate" — meta questions
	// about pincher's own tool surface ("what's the difference between
	// symbol and context") used to misroute to shapeReview and
	// recommend the `changes` tool.
	case contains("review", "before commit", "blast radius", "pre-commit", "impact") ||
		reviewDiffWord.MatchString(t):
		return shapeReview
	// #784: shapeRefactor runs before shapeFix. "refactor"/"rename"/
	// "restructure"/"extract" are explicit leading-verb intent signals;
	// shapeFix's keyword list includes the noun "error", which collides
	// with common refactor phrasings ("refactor the error handling",
	// "extract the error-wrapping helper"). Pre-fix those routed to
	// shapeFix and guide recommended a bug-hunt search flow.
	case contains("refactor", "rename", "restructure", "clean up") ||
		refactorExtractWord.MatchString(t):
		// Note: "split", "move" intentionally NOT in this list — both are
		// also nouns ("FTS5 split", "the move detector") and would over-
		// match. Lose those signal words rather than false-positive.
		// "extract" is word-bounded (refactorExtractWord) so it doesn't
		// catch "extraction"/"extractor" (#784).
		return shapeRefactor
	case contains("fix", "bug", "broken", "error", "regression", "crash", "wrong"):
		return shapeFix
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
	case contains("understand", "explain", "how does", "what is", "explore", "learn", "orient",
		"why does", "why is", "why are", "why do"):
		// #397: "why does X" is the canonical "explain this" phrasing
		// and was previously falling through to shapeUnknown — guide
		// then routed those tasks to a generic architecture+search
		// recommendation instead of the deeper context-read flow.
		return shapeUnderstand
	// #467: structural-audit shapes — "find undocumented exported APIs",
	// "list functions with no docstring", etc. The right tool is
	// `query` with a pinchQL predicate (n.docstring IS NULL,
	// n.is_test=true, etc.), not BM25 search of the literal phrase.
	// Must come before shapeFind so "find undocumented" doesn't fall
	// through to a useless `search query="undocumented"`.
	case contains("undocumented", "no docstring", "missing docstring", "missing comment", "without docstring", "without comment"):
		return shapeAudit
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

// domainConcept maps a substring pattern in a task description to a
// concept-aware starter recommendation that supersedes the generic
// shape-default first tool. The list is ordered: first match wins.
// All keys are lowercase; the matcher lowercases the task before
// looking. #397.
//
// The intent: when a user says "add a new MCP tool" or "schema
// migration", the guide should jump straight to the file/symbol
// where that concept lives, instead of generic "search for similar
// existing code". One concrete pointer is worth ten generic tool
// suggestions.
var domainConcepts = []struct {
	patterns []string
	tool     string
	args     string
	why      string
}{
	{
		patterns: []string{"mcp tool", "tool registration", "addtool", "registertools", "registertool", "tools/list", "tools/call", "tool surface"},
		tool:     "search",
		args:     `{"query":"registerTools"}`,
		why:      "MCP tools are registered in registerTools (internal/server/server.go); read that to see the registration pattern + InputSchema shape",
	},
	{
		patterns: []string{"schema migration", "schema change", "add a column", "schema bump", "schemamigrations"},
		tool:     "search",
		args:     `{"query":"schemaMigrations"}`,
		why:      "schema migrations live in the schemaMigrations slice in internal/db/db.go — append to add one; see CLAUDE.md for the 5-file checklist (corpus snapshots etc.)",
	},
	{
		patterns: []string{"new language", "new extractor", "language extractor", "ast extractor", "regex extractor", "extractor candidate"},
		tool:     "search",
		args:     `{"query":"registerExtractor"}`,
		why:      "extractors self-register in init() — see internal/ast/registry.go for the Extractor interface; existing extractors are the template",
	},
	{
		patterns: []string{"git diff", "blast radius", "changed_files", "scope=base", "compare branches"},
		tool:     "search",
		args:     `{"query":"runGitDiff"}`,
		why:      "git diff handling lives in runGitDiff / parseGitDiffFiles (internal/server/server.go) — that's where scope=base:<branch> dispatches",
	},
	{
		patterns: []string{"supervisor", "respawn", "supervised mode", "auto-restart", "auto_restart", "init replay", "tools/list_changed"},
		tool:     "search",
		args:     `{"query":"Supervisor"}`,
		why:      "the supervisor lives in internal/supervisor/supervisor.go — Supervisor type, respawn(), and the pump goroutines are the surface area",
	},
	{
		// #616: bare "pinchql" was matching tasks like "use pinchQL to
		// find X" — the user wanted a pinchQL *query* template, not a
		// pointer at the engine's source code. Tightened to phrases that
		// unambiguously signal "I'm investigating pinchQL internals".
		patterns: []string{"cypher engine", "pinchql engine", "pinchql implementation",
			"pinchql parser", "pinchql planner", "pinchql pushdown", "pinchql where",
			"how does pinchql", "how pinchql works", "explain pinchql",
			"match (a)", "where pushdown", "edge filter"},
		tool:     "search",
		args:     `{"query":"runJoinQuery"}`,
		why:      "pinchQL routing splits between runNodeScan / runJoinQuery / runBFS in internal/cypher/engine.go — Execute() is the dispatcher",
	},
	{
		// #616: when the user wants to USE pinchQL (not investigate it),
		// hand back a starter `query` recommendation with a self-join
		// template so they have something to adapt rather than starting
		// from a blank prompt.
		patterns: []string{"use pinchql", "via pinchql", "with pinchql", "in pinchql",
			"pinchql query", "pinchql to find", "pinchql to list", "pinchql to count"},
		tool:     "query",
		args:     `{"pinchql":"MATCH (n:Function) RETURN n.qualified_name LIMIT 20"}`,
		why:      "pinchQL is the right tool for structural questions — adapt the template to your filter (n.docstring IS NULL, n.is_test=true, etc.). See `schema` for available properties",
	},
	{
		patterns: []string{"fts5", "full-text search", "rebuild fts", "search index", "bm25"},
		tool:     "search",
		args:     `{"query":"symbols_fts"}`,
		why:      "FTS5 lives in the symbols_fts virtual table + 3 sync triggers (internal/db/db.go) — never sync manually, the triggers handle it",
	},
	{
		patterns: []string{"session stats", "token saving", "tokens_used", "tokens_saved"},
		tool:     "search",
		args:     `{"query":"jsonResultWithMeta"}`,
		why:      "every tool response wraps via jsonResultWithMeta which atomically increments session stats — read it before adding a new tool",
	},
	{
		// #616-style tighten: bare "trace" / "callers" matched any task
		// mentioning callers (e.g. "find functions with zero callers")
		// and wrongly prepended a trace-internals source pointer. These
		// patterns now only fire when the user is investigating trace's
		// OWN implementation, not just using the concept.
		patterns: []string{"traceviacte", "trace internals", "trace bfs",
			"how does trace", "trace implementation", "trace recursive cte"},
		tool:     "search",
		args:     `{"query":"traceViaCTE"}`,
		why:      "trace BFS uses a recursive CTE in internal/db/db.go (traceViaCTE) — that's the SQL doing the work",
	},
}

// domainConceptHint returns a concept-aware recommendation when the
// task references a known pincher domain concept, or nil when no
// concept matches (caller falls through to shape-default
// recommendations only).
func domainConceptHint(task string) *map[string]string {
	t := strings.ToLower(task)
	for _, c := range domainConcepts {
		for _, p := range c.patterns {
			if strings.Contains(t, p) {
				return &map[string]string{"tool": c.tool, "args": c.args, "why": c.why}
			}
		}
	}
	return nil
}

// extractAuditedTool scans a task string (already lowercased) for a
// pincher tool name and returns it. Returns "" when no tool name is
// present. Used by #497 tool-audit shape detection so a generic "find
// FPs" without a tool reference doesn't grab the audit recipe.
//
// Word-boundary check (surrounding non-letter chars) avoids matching
// inside other identifiers — "search" must not match in "researching".
func extractAuditedTool(taskLower string) string {
	if taskLower == "" {
		return ""
	}
	for _, name := range pincherToolNames {
		idx := 0
		for {
			pos := strings.Index(taskLower[idx:], name)
			if pos < 0 {
				break
			}
			absolutePos := idx + pos
			beforeOK := absolutePos == 0 || !isWordChar(taskLower[absolutePos-1])
			endPos := absolutePos + len(name)
			afterOK := endPos == len(taskLower) || !isWordChar(taskLower[endPos])
			if beforeOK && afterOK {
				return name
			}
			idx = absolutePos + 1
		}
	}
	return ""
}

// isWordChar — letters, digits, underscore. Anything else is a word
// boundary for extractAuditedTool's word-match check.
func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

// guideRecommendations returns the default 2-3 next-tool suggestions
// for a given task shape. The "args" slot is a best-effort template —
// the guide tool fills in anything it can extract from the task string
// (e.g. the most-likely symbol name) and leaves the rest as a
// placeholder the agent fills in.
// inferAuditPinchQL (#921) picks the right pinchQL audit template
// based on the task's keywords. Pre-fix shapeAudit always emitted the
// docstring/is_exported template (the canonical #467 example) no
// matter what audit the user described. Threshold tasks (#912) now
// get a complexity-shaped query that actually addresses their
// question. Returns the pinchql string and a short "why" rationale.
func inferAuditPinchQL(task string) (pinchql, why string) {
	t := strings.ToLower(task)
	switch {
	case strings.Contains(t, "complexity") || strings.Contains(t, "cyclomatic"):
		return `MATCH (n:Function) WHERE n.complexity > 20 RETURN n.name, n.file_path, n.complexity ORDER BY n.complexity DESC LIMIT 50`,
			"structural audit — pinchQL projects n.complexity directly. Adjust the threshold (currently 20) to match your task. ORDER BY complexity DESC surfaces the worst offenders first"
	case strings.Contains(t, "line") && (strings.Contains(t, "long") || strings.Contains(t, "above") ||
		strings.Contains(t, "over") || strings.Contains(t, "exceed") || strings.Contains(t, "more")):
		// #928: pinchQL doesn't yet support arithmetic operators
		// (`-`, `+`, `*`, `/`) in WHERE/RETURN, so the obvious
		// `(n.end_line - n.start_line) > 100` template crashes the
		// parser. Until #928 lands, emit a length-correlated query
		// that returns start_line + end_line and asks the caller to
		// compute the diff client-side. Adjust to use real arithmetic
		// once the engine supports it.
		return `MATCH (n:Function) WHERE n.is_test=false AND n.language='Go' RETURN n.name, n.file_path, n.start_line, n.end_line LIMIT 200`,
			"structural audit — pinchQL doesn't yet support arithmetic in WHERE/RETURN (#928), so the engine can't filter by (end_line - start_line) directly. The query returns start_line + end_line for every Go non-test function (capped at 200 to stay bounded); compute the diff and sort descending client-side. Once #928 ships, this template will become `WHERE (n.end_line - n.start_line) > N ORDER BY ... DESC LIMIT 50`."
	case strings.Contains(t, "untested") ||
		(strings.Contains(t, "test") && (strings.Contains(t, "coverage") || strings.Contains(t, "missing"))):
		// #923: scope to Go — regex-tier languages don't populate
		// is_test reliably, so they flood the result with false
		// positives.
		// #943: include Methods alongside Functions. The MCP server has
		// many handleX methods that should be audited the same way as
		// top-level functions. Pre-fix the template only matched
		// `(n:Function)` — Go's method-heavy idioms slipped through.
		return `MATCH (n) WHERE (n.kind="Function" OR n.kind="Method") AND n.is_exported=true AND n.is_test=false AND n.language='Go' RETURN n.name, n.file_path, n.kind LIMIT 50`,
			"structural audit — pinchQL lists exported non-test Go functions AND methods. Combine with trace(direction=inbound, include_tests=true) per result to spot test coverage"
	default:
		// Docstring audit is the canonical #467 example and still the
		// most common shapeAudit query — keep it as the fallback.
		// #923: scope to Go + non-test. Regex-tier languages don't
		// populate the docstring property (so 100% match the IS NULL
		// filter), and test functions don't need docstrings by
		// convention. Pre-fix the top results were JS handlers, Bash
		// helpers, and `TestDashboardJS_*` — pure noise.
		// #943: include Methods alongside Functions. On a method-heavy
		// codebase (Go MCP server pattern) the previous Function-only
		// template hid the majority of the coverage gap.
		return `MATCH (n) WHERE (n.kind="Function" OR n.kind="Method") AND n.docstring IS NULL AND n.is_exported=true AND n.is_test=false AND n.language='Go' RETURN n.name, n.file_path, n.kind LIMIT 50`,
			"structural audit — pinchQL filters on docstring/is_exported directly across Functions AND Methods. Scoped to Go (AST-extracted docstrings) and non-test symbols so regex-tier languages and tests don't flood the result. BM25 search of the literal phrase wouldn't surface anything"
	}
}

func guideRecommendations(shape guideShape, taskHint, auditedTool, fullTask string) []map[string]string {
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
	case shapeAudit:
		// #467 + #921: shape-aware audit. The canonical pinchQL
		// template depends on the kind of audit — docstring-coverage,
		// complexity threshold, line-count threshold, etc. Pre-#921
		// every audit got the docstring/is_exported template even
		// when the user asked about complexity above 20.
		// inferAuditPinchQL routes on keywords; the fallback is the
		// canonical #467 docstring example.
		pinchql, why := inferAuditPinchQL(fullTask)
		auditQuery := nextStepArgs(map[string]any{"pinchql": pinchql})
		return []map[string]string{
			{"tool": "query", "args": auditQuery, "why": why},
			{"tool": "context", "args": `{"id":"<from-query>"}`,
				"why": "read each candidate's surrounding context to confirm"},
		}
	case shapeDeadCode:
		// The dead_code tool IS the answer here — zero-inbound-edge
		// detection is exactly what it does. search/context would make
		// the agent reinvent it by hand. trace inbound on a candidate
		// confirms it (a true dead symbol has no callers; a false
		// positive has callers the static graph missed).
		return []map[string]string{
			{"tool": "dead_code", "args": `{"language":"Go"}`,
				"why": "purpose-built — finds non-exported functions/methods with zero inbound CALLS/READS/WRITES/REFERENCES edges"},
			{"tool": "trace", "args": `{"name":"<candidate-from-dead_code>","direction":"inbound"}`,
				"why": "verify a candidate — a real dead symbol traces to zero callers; callers here mean a missed edge (interface dispatch, reflection)"},
			{"tool": "context", "args": `{"id":"<candidate-from-dead_code>"}`,
				"why": "read the symbol before deleting — confirm it's not an entry point the graph can't see"},
		}
	case shapeToolAudit:
		// #497: tool-output audit recipe. The user is asking "is this
		// tool's output trustworthy?" — the right move is empirical:
		// run the tool on a known corpus, verify each result, cluster
		// the false positives by mechanism. Generic "search the source
		// of the tool" routes the agent to the wrong investigation.
		//
		// taskHint comes from taskHintFromString which often strips the
		// tool name (it's looking for the discriminating identifier).
		// auditedTool is passed through separately from handleGuide so
		// the recipe can name the tool the user asked about, not whatever
		// hint shard happened to survive.
		audited := auditedTool
		if audited == "" {
			audited = "<tool-name>"
		}
		toolArgs := nextStepArgs(map[string]any{}) // tool-specific; agent fills in
		return []map[string]string{
			{"tool": audited, "args": toolArgs,
				"why": "run the tool on a representative project — collect its actual output, not your assumption of what it does"},
			{"tool": "trace", "args": `{"id":"<from-` + audited + `>","direction":"inbound"}`,
				"why": "verify each result by tracing inbound callers — a true positive has no callers; a false positive has callers the static graph missed"},
			{"tool": "context", "args": `{"id":"<unexpected-result>"}`,
				"why": "for each FP cluster, read the symbol's source to identify the missed-edge mechanism (interface dispatch, runtime invocation, build-tag gate, etc.)"},
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
		"use":   true, "uses":  true, "used":  true, "using": true,
		"find":  true, "show":  true,
		"handle": true, "handles": true,
		// #933: call-family verbs / nouns. "what calls processPayment"
		// previously hinted "calls processPayment" — the trace
		// recommendation then templated `name="calls processPayment"`
		// which doesn't resolve. Same idea as "use/uses" above — the
		// shape detector (shapeTraceIn keyword "calls") owns these
		// tokens; the hint should be the bare identifier.
		"call": true, "calls": true, "called": true,
		"caller": true, "callers": true, "calling": true,
		"trace": true, "traces": true, "traced": true, "tracing": true,
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
		// #615: visibility / category nouns. Tasks like "find what calls
		// a private function" or "list every public method" use these
		// adjectives to scope a class of symbols, not name a single one.
		// Pre-fix the hint extractor would pick "private" / "public" as
		// the discriminator and template a useless `search query="private"`.
		// Drop them as stopwords so the actual subject (or no subject) wins.
		"private": true, "public": true, "exported": true, "unexported": true,
		"internal": true, "external": true, "global": true, "local": true,
		"stub":     true, "stubs":   true, "static":   true, "dynamic":  true,
		// auxiliary verbs + negation. "find symbols that have no test
		// coverage" used to extract "have no" as the hint (longest non-
		// stopword run), and the templated search recommendation searched
		// for the literal phrase "have no" — never the subject of any task.
		"have": true, "has": true, "had": true, "having": true,
		"no":   true, "not": true, "without": true,
	}
	// #942: strip apostrophes before tokenizing so contractions don't
	// leave stray single-letter tokens. Pre-fix "indexer's" split into
	// ["indexer", "s"] — the stray "s" survived stopword filtering and
	// corrupted the hint. Both ASCII (') and curly Unicode (’) handled.
	task = strings.ReplaceAll(task, "'", "")
	task = strings.ReplaceAll(task, "’", "")
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
	// #397: when run lengths tie, prefer runs with all-caps tokens
	// (acronyms like INI, MCP, FTS5, BPE) over lowercase runs of the
	// same length. Users mention acronyms as the subject of the task
	// ("INI file parsing" — the acronym IS the discriminator); the
	// neighbouring lowercase tokens are usually scope nouns.
	bestIdx := 0
	bestLen := len(runs[0])
	bestChars := totalLen(runs[0])
	bestAcronyms := acronymCount(runs[0])
	for i := 1; i < len(runs); i++ {
		runLen := len(runs[i])
		runChars := totalLen(runs[i])
		runAcronyms := acronymCount(runs[i])
		switch {
		case runLen > bestLen:
			bestIdx, bestLen, bestChars, bestAcronyms = i, runLen, runChars, runAcronyms
		case runLen == bestLen && runAcronyms > bestAcronyms:
			bestIdx, bestLen, bestChars, bestAcronyms = i, runLen, runChars, runAcronyms
		case runLen == bestLen && runAcronyms == bestAcronyms && runChars > bestChars:
			bestIdx, bestLen, bestChars, bestAcronyms = i, runLen, runChars, runAcronyms
		case runLen == bestLen && runAcronyms == bestAcronyms && runChars == bestChars:
			bestIdx = i // later wins on tie
		}
	}
	return strings.Join(runs[bestIdx], " ")
}

// acronymCount returns the number of all-caps tokens in toks. Used by
// taskHintFromString tie-break (#397) — acronyms are higher signal than
// neighbouring lowercase scope nouns.
func acronymCount(toks []string) int {
	n := 0
	for _, t := range toks {
		if len(t) >= 2 && strings.ToUpper(t) == t {
			n++
		}
	}
	return n
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
		// #712: failure-as-pedagogy — guide takes a free-form task; show
		// the shape so the caller doesn't have to guess.
		return s.errResultRich("task is required (free-form description of what you're trying to do)", []map[string]string{
			{"tool": "guide", "args": `{"task":"fix the login retry bug"}`,
				"why": "describe the goal in plain words — guide returns 2-3 recommended tool calls"},
			{"tool": "guide", "args": `{"task":"understand how indexing handles symlinks"}`,
				"why": "works for understanding tasks too, not just fixes"},
		}), nil
	}
	shape := classifyTaskShape(task)
	hint := taskHintFromString(task)
	if hint == "" {
		// Fall back to the first non-trivial token so search args isn't
		// completely empty. Edge case for very short or all-stop-word tasks.
		hint = task
	}
	// #497: scan the raw task once for an audited tool name. Passed to
	// guideRecommendations so shapeToolAudit can name the right tool —
	// the hint string usually drops it.
	auditedTool := extractAuditedTool(strings.ToLower(task))
	recommendations := guideRecommendations(shape, hint, auditedTool, task)
	// #397: if the task mentions a pincher-domain concept, prepend a
	// concept-aware starter that points at the actual file/symbol where
	// the concept lives. The shape-default recommendations follow as
	// the broader workflow.
	if cc := domainConceptHint(task); cc != nil {
		recommendations = append([]map[string]string{*cc}, recommendations...)
	}

	data := map[string]any{
		"task":                   task,
		"shape":                  string(shape),
		"hint":                   hint,
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
	//
	// #945: open is matched as a literal prefix, NOT a whole tag name. Pre-fix
	// `<head` matched both `<head>` (the document head) AND `<header>`. The
	// closing tag `</head>` did not match `</header>`, so the "no closing tag
	// found" branch fired and chopped raw[:si] — truncating everything from
	// the first <header> in the body to end-of-document. Wikipedia + most
	// modern HTML5 documents have <header> elements in body content, so this
	// reduced their extracted text to just the pre-<header> skip-link
	// (~15 chars from 400+ KB of raw HTML).
	//
	// Fix: require the byte after the tag name to be `>`, `/`, or whitespace
	// (an HTML tag-name terminator) before treating the match as our tag.
	// And when no closing tag is found, skip this open instead of truncating
	// the document.
	for _, tag := range []string{"script", "style", "head", "nav", "footer"} {
		open := "<" + tag
		close := "</" + tag + ">"
		searchFrom := 0
		for {
			lo := strings.ToLower(raw)
			if searchFrom >= len(lo) {
				break
			}
			si := strings.Index(lo[searchFrom:], open)
			if si < 0 {
				break
			}
			si += searchFrom
			// Tag-boundary check: char after the tag name must terminate it.
			next := si + len(open)
			if next < len(raw) {
				c := raw[next]
				if c != '>' && c != '/' && c != ' ' && c != '\t' && c != '\n' && c != '\r' {
					// Not our tag — e.g. <head matched against <header>.
					searchFrom = si + 1
					continue
				}
			}
			ei := strings.Index(lo[si:], close)
			if ei < 0 {
				// No closing tag found; skip past this open rather than
				// truncating the document body.
				searchFrom = si + len(open)
				continue
			}
			raw = raw[:si] + " " + raw[si+ei+len(close):]
			searchFrom = si + 1 // restart near where we cut
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
			// #579: only consume `>` when it closes a tag we entered.
			// Outside a tag (e.g. an arrow `=>` in a code sample, a
			// blockquote `>`, a generic `Vec<T>` mishandled because the
			// `<` already flipped state — anything where `>` is a
			// literal character) preserve it verbatim. Pre-fix every
			// `>` was silently consumed.
			if inTag {
				inTag = false
			} else {
				b.WriteByte('>')
			}
		case !inTag:
			b.WriteByte(raw[i])
		}
	}

	// Collapse whitespace.
	text = strings.Join(strings.Fields(b.String()), " ")
	return
}

// extractMarkdownText returns the markdown body verbatim with
// trailing whitespace trimmed and CRLF normalized to LF. Used for
// text/markdown / text/plain fetches where extractTextFromHTML's tag
// stripper would corrupt code samples and arrow operators (#579).
func extractMarkdownText(raw string) string {
	return strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
}

// firstMarkdownH1 returns the first `# Title` line of a markdown
// document, or "" if no H1 is found in the leading 200 lines. Used
// as the title when content-type is markdown — the URL fallback in
// the fetch handler is ugly when a real title is one line away
// (#579). Skips empty lines and YAML/TOML front-matter delimiters
// (`---`, `+++`).
func firstMarkdownH1(raw string) string {
	scanner := strings.SplitN(raw, "\n", 200)
	inFrontMatter := false
	for i, line := range scanner {
		t := strings.TrimSpace(line)
		if i == 0 && (t == "---" || t == "+++") {
			inFrontMatter = true
			continue
		}
		if inFrontMatter {
			if t == "---" || t == "+++" {
				inFrontMatter = false
			}
			continue
		}
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(t, "# "))
		}
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────────
// _meta envelope
// ─────────────────────────────────────────────────────────────────────────────

// avgFileSize is the estimated chars in a typical source file an agent would
// have to read to locate a symbol without pincherMCP. Real files in this repo
// average ~33KB; 20KB is a conservative cross-language estimate.
const avgFileSize = 20_000

// charsPerToken is the approximate number of source-code characters per BPE
// token. Used only for baseline estimates where we don't have the actual text.
const charsPerToken = 4

// savedVsFileSizes returns estimated tokens saved using actual file sizes looked
// up from the filesystem. Used by tests; production handlers should call
// savedVsFileSizesSession (the per-session-dedup variant, #478).
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

// markFileAccessed records that the agent has received content for (projectID,
// relPath) via a pincher tool this session. Returns true if this is the first
// access (the agent did NOT already have the file in context) and false
// otherwise. The caller decides what baseline to charge: full-file-read on
// fresh access, zero on repeat (file is already in context). See #478.
//
// projectID may be empty for tools that don't carry one (e.g. cross-project
// search); the empty key is still namespaced so it never collides with a real
// project ID.
func (s *Server) markFileAccessed(projectID, relPath string) bool {
	key := projectID + "|" + relPath
	_, loaded := s.accessedFiles.LoadOrStore(key, struct{}{})
	return !loaded
}

// savedVsFileSizesSession is the per-session-dedup variant of savedVsFileSizes.
// For each de-duplicated path that the agent has NOT yet received from a
// pincher tool this session, charge the full file size. For paths the agent
// has already received, charge zero — they are already in the context window
// and reading them again would not have meant another full Read.
//
// This closes the largest source of inflation in tokens_saved (ADR
// SAVINGS_HONESTY source #2). A 10-symbol-from-1-file workflow now claims a
// 1-file baseline, not 10×.
func (s *Server) savedVsFileSizesSession(projectID, root string, filePaths []string, payloadBytes []byte) int {
	total := 0
	seen := make(map[string]bool)
	for _, fp := range filePaths {
		if seen[fp] {
			continue
		}
		seen[fp] = true
		if !s.markFileAccessed(projectID, fp) {
			continue
		}
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

	// Merge into any pre-existing `_meta` rather than overwriting, so handlers
	// can attach handler-specific fields (e.g. `confidence_distribution` from
	// #34 Phase 3) before calling.
	meta, _ := data["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["tokens_used"] = tokensUsed
	// #477: stamp the baseline method so consumers can tell what kind
	// of work each tool replaces. "none" tools (architecture, schema,
	// list, ...) emit tokens_saved=null and don't accumulate to stats —
	// they have no Read/Grep alternative, so a numeric saving would be
	// fabricated, not measured. "full_file_read" / "partial_read" tools
	// stamp an honest int.
	baselineMethod := baselineMethodForTool[tool]
	if baselineMethod == "" {
		// Default for tools added without an explicit classification —
		// safer to assume the tool replaces a Read than to silently
		// suppress savings tracking. The classification gate test in
		// the test suite catches drift.
		baselineMethod = baselineMethodFullFileRead
	}
	meta["baseline_method"] = baselineMethod
	if baselineMethod == baselineMethodNone {
		meta["tokens_saved"] = nil
		meta["tokens_saved_pct"] = nil
	} else {
		meta["tokens_saved"] = tokensSaved
		// #619: percentage saved is the bounded form of the same number —
		// easier to reason about per-call (capped at 100%, negative when
		// the envelope cost more than the savings, no compounding across
		// sessions). Reported alongside the absolute count so consumers
		// have both shapes available. Formula: saved / (saved + used).
		// Negative results are kept as-is — a -15% reads as "this call
		// cost more than the Read it replaced," which is the right signal
		// when it happens; clamping to 0 would hide that.
		denom := tokensSaved + tokensUsed
		if denom > 0 {
			meta["tokens_saved_pct"] = math.Round(float64(tokensSaved)/float64(denom)*1000) / 10
		} else {
			meta["tokens_saved_pct"] = 0.0
		}
	}
	meta["latency_ms"] = latency

	// #925: surface index_in_progress when the session project's
	// indexer is mid-pass. Pre-fix, agents calling search/query/trace
	// during the 30-60s window after a binary swap got silently
	// incomplete results — no warning, no `_meta` flag, no diagnosis,
	// and the standard empty-result advisory pointed them at
	// min_confidence (wrong cause). With this flag, callers know to
	// retry once the pass completes. Cheap probe: GetProgress reads
	// in-memory atomic counters, no DB hit. Stamped only when
	// genuinely active so quiet calls don't pay the field weight.
	//
	// #993: phase the warning so files_done==files_total doesn't read
	// "mid-pass (55/55 files)" — the per-file walk has finished but the
	// cross-file resolvers (resolveImports/resolveCalls/resolveReads)
	// are still running. Pre-fix the text said "mid-pass" with both
	// counters equal, conflicting with what the numbers show; agents
	// see 100% file completion and an alarming "retry" suggestion.
	if s.indexer != nil && s.sessionID != "" {
		if done, total, active := s.indexer.GetProgress(s.sessionID); active {
			meta["index_in_progress"] = map[string]any{
				"files_done":  done,
				"files_total": total,
			}
			existing, _ := meta["warnings"].([]string)
			var warnText string
			if total > 0 && done >= total {
				warnText = fmt.Sprintf("indexer is finalizing (cross-file resolver running after %d/%d files extracted); results may still be incomplete — retry in a few seconds",
					done, total)
			} else {
				warnText = fmt.Sprintf("indexer is mid-pass (%d/%d files); results may be incomplete — retry after the pass completes",
					done, total)
			}
			meta["warnings"] = append(existing, warnText)
		}
	}

	// #649: capability advertisement. Stable per-server slice computed
	// at New() time. Routers consume to make integration decisions
	// (subscribe to SSE? use streamable-HTTP? expect operator tools
	// via MCP?) without scraping versions or doing trial-and-error
	// calls. Sized small (~10 short strings, < 200 bytes); cost
	// negligible vs the discoverability win.
	meta["capabilities"] = s.capabilities

	// #650: per-response complexity_tier — confirms the static
	// x-pincher-tier classification at call time. Routers reading
	// _meta can decide on the fly which model handles the next
	// agent step. Empty string when tool isn't classified (gate
	// test catches; production should never hit this branch).
	if tier := toolComplexityTier(tool); tier != "" {
		meta["complexity_tier"] = tier
	}

	// #499: surface unknown args from this call. The fix is to teach
	// the agent that pincher saw the typo'd / made-up arg and ignored
	// it, instead of silently behaving as if the arg didn't exist
	// (the same failure-as-pedagogy pattern as #473's pinchQL warnings).
	// Merge with any pre-existing warnings (the cypher engine puts its
	// own here) — never overwrite.
	if w := s.unknownArgs(tool, args); len(w) > 0 {
		existing, _ := meta["warnings"].([]string)
		meta["warnings"] = append(existing, w...)
	}
	// #619: prose `savings:` line dropped. The structured fields
	// (tokens_saved + tokens_saved_pct) carry the same information in
	// a form clients can render however they like — dashboard renders
	// the percentage as the headline, raw integer as supporting detail,
	// without ever parsing a human string.
	data["_meta"] = meta

	// Accumulate session stats. On the very first call, flush immediately so
	// the dashboard sees the new session within milliseconds, not after 10s.
	// #477: skip the tokens_saved increment for "none" tools — adding 0
	// would be a no-op anyway, but being explicit guards against future
	// callers passing a non-zero tokensSaved by mistake.
	newCalls := atomic.AddInt64(&s.statsCalls, 1)
	atomic.AddInt64(&s.statsTokensUsed, int64(tokensUsed))
	if baselineMethod != baselineMethodNone {
		atomic.AddInt64(&s.statsTokensSaved, int64(tokensSaved))
	}
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

	// #494: occasional dopamine. Cumulative all-time tokens_saved =
	// previously-flushed sessions + this in-flight session's running
	// total (statsTokensSaved). Without the in-flight component, the
	// celebration would lag by 10s (next flushSession tick), which is
	// long enough to land on a different unrelated tool call. Adding
	// it surfaces the milestone on the response that actually crossed
	// it. MaybeFireCelebration's PRIMARY KEY guarantees one-shot per
	// installation.
	if cel := s.maybeFormatCelebration(); cel != "" {
		meta["celebration"] = cel
	}

	// #622: drop pedagogy-shape next_steps on the success path. Most
	// next_steps entries on a happy-path response are workflow hints
	// ("you found Foo with search; now run context on its ID") — useful
	// once, then noise on every subsequent call. Suppressed when:
	//   - the caller didn't opt in to verbose=true
	//   - there are no warnings (warnings + next_steps together are the
	//     pedagogy that justifies the chrome — drop both, or keep both)
	//   - there's no diagnosis (zero-result advisory pairs with steps)
	//   - the next_steps don't include a continue-the-same-tool entry
	//     (pagination — `limit/offset` continuation IS load-bearing
	//     follow-up info, not pedagogy; preserve it)
	//
	// Gated on `tool != ""` so unit tests that call handlers directly
	// without setting req.Params.Name preserve their next_steps
	// assertions. In production every MCP invocation populates the
	// tool name, so the strip applies on every real call.
	verbose, _ := args["verbose"].(bool)
	if !verbose && tool != "" {
		_, hasWarn := meta["warnings"]
		_, hasDiag := meta["diagnosis"]
		if !hasWarn && !hasDiag {
			if steps, ok := meta["next_steps"].([]map[string]string); ok && len(steps) > 0 {
				keep := false
				for _, st := range steps {
					if st["tool"] == tool {
						// Same-tool entry signals pagination / continuation —
						// real follow-up, not pedagogy. Keep the whole list.
						keep = true
						break
					}
				}
				if !keep {
					delete(meta, "next_steps")
				}
			}
		}
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

// maybeFormatCelebration consults the store for a one-shot threshold
// crossing (#494) and returns a one-line celebration string when fired.
// Empty string on no crossing or any error — celebrations are best-effort,
// never fail a tool response over them.
//
// "Cumulative" = persisted sessions + the in-flight session's atomic
// counter. The persisted-only path lags by up to 10s (flushSession
// ticker) which would land the celebration on a stale tool call. Adding
// the in-flight delta gives the milestone to the response that actually
// crossed the line.
func (s *Server) maybeFormatCelebration() string {
	if s.store == nil {
		return ""
	}
	// #863: celebrations are opt-in, default off. They're one-shot per
	// threshold per installation by design — but any workflow that
	// spins up fresh DBs (the dogfood loop's per-temp-dir installs, CI,
	// throwaway indexes) re-fires every tier from zero each time, so the
	// "dopamine" line becomes recurring noise rather than a rare signal.
	// PINCHER_CELEBRATIONS=1 re-enables it for users who want the marker.
	if os.Getenv("PINCHER_CELEBRATIONS") != "1" {
		return ""
	}
	_, _, persisted, _, err := s.store.GetAllTimeSavings()
	if err != nil {
		return ""
	}
	cumulative := persisted + atomic.LoadInt64(&s.statsTokensSaved)
	threshold, fired, err := s.store.MaybeFireCelebration(cumulative)
	if err != nil || !fired {
		return ""
	}
	return formatCelebration(threshold, cumulative)
}

// formatCelebration produces the human-readable one-liner for a
// celebration. Pure function — easy to unit-test independently of the
// store. No $-figures (#476 SAVINGS_HONESTY): we don't know the user's
// model or pricing, and the token number is the dopamine carrier.
func formatCelebration(threshold, cumulativeTokensSaved int64) string {
	return fmt.Sprintf("🎯 Pincher has saved you %s tokens (just crossed %s) — that's the loop working.",
		humanInt(int(cumulativeTokensSaved)), humanInt(int(threshold)))
}

// textResultWithMeta performs the same session accounting as jsonResultWithMeta
// but returns a pre-formatted text string rather than a JSON object. Used by
// handleStats so the output is human-readable on the command line.
func (s *Server) textResultWithMeta(text string, start time.Time, tool string, args map[string]any, tokensSaved int) *mcp.CallToolResult {
	latency := time.Since(start).Milliseconds()
	s.maybeRecordSlowQuery(tool, args, latency)
	tokensUsed := db.ApproxTokens(text)

	// #477: same baseline-method gate as jsonResultWithMeta. Tools
	// stamped "none" (admin/orientation) skip the savings tracker.
	baselineMethod := baselineMethodForTool[tool]
	if baselineMethod == "" {
		baselineMethod = baselineMethodFullFileRead
	}

	newCalls := atomic.AddInt64(&s.statsCalls, 1)
	atomic.AddInt64(&s.statsTokensUsed, int64(tokensUsed))
	if baselineMethod != baselineMethodNone {
		atomic.AddInt64(&s.statsTokensSaved, int64(tokensSaved))
	}
	atomic.AddInt64(&s.statsLatencyMS, latency)

	if newCalls == 1 {
		go s.flushSession()
	}

	// Append a compact meta line so callers still see accounting info.
	// No $-figures (#476 SAVINGS_HONESTY): we don't know the user's model.
	// #477: render `tokens saved —` for "none" tools so the field is
	// distinguishable from a tool that genuinely saved zero on this call.
	var savedRender string
	if baselineMethod == baselineMethodNone {
		savedRender = "—"
	} else {
		savedRender = fmt.Sprintf("%-6d", tokensSaved)
	}
	full := text + fmt.Sprintf("\n  tokens used %-6d  tokens saved %s  latency %d ms\n", tokensUsed, savedRender, latency)
	result := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: full}},
	}
	// #364: same restart hook as jsonResultWithMeta — see comment there.
	s.checkAutoRestart()
	return result
}

// baselineMethod constants describe the kind of work each tool replaces
// for an LLM agent. Stamped on every response in `_meta.baseline_method`
// so consumers can distinguish honest savings from "no comparison
// possible" (#477).
const (
	// baselineMethodFullFileRead — the tool returns content the agent
	// would otherwise have read from one or more source files. Token
	// savings are computed against real file sizes (deduped per-session).
	baselineMethodFullFileRead = "full_file_read"
	// baselineMethodPartialRead — second access to the same file in
	// the session. The agent already has the file in context window;
	// the saving is incremental, not a full file replay. Tracked via
	// the per-session accessedFiles set added in #478. (Currently no
	// tools stamp this directly — savedVsFileSizesSession returns 0
	// for repeat hits, which is the same outcome.)
	baselineMethodPartialRead = "partial_read"
	// baselineMethodNone — admin / orientation tool with no Read or
	// Grep alternative. tokens_saved is null (not 0) and the savings
	// human-readable line is suppressed — there's no honest baseline
	// to draw against.
	baselineMethodNone = "none"
)

// baselineMethodForTool maps each registered tool name to the kind of
// work it replaces (#477). Adding a new tool MUST update this map; the
// classification gate test catches drift. Tools not present in the map
// fall back to baselineMethodFullFileRead — safer than silent suppression.
var baselineMethodForTool = map[string]string{
	// Tools that replace direct file reads.
	"symbol":       baselineMethodFullFileRead,
	"symbols":      baselineMethodFullFileRead,
	"context":      baselineMethodFullFileRead,
	"search":       baselineMethodFullFileRead,
	"query":        baselineMethodFullFileRead,
	"trace":        baselineMethodFullFileRead,
	"changes":      baselineMethodFullFileRead,
	"dead_code":    baselineMethodFullFileRead,
	"neighborhood": baselineMethodFullFileRead,
	// Admin / orientation / write-side tools — no Read/Grep alternative.
	"index":        baselineMethodNone,
	"architecture": baselineMethodNone,
	"schema":       baselineMethodNone,
	"list":         baselineMethodNone,
	"adr":          baselineMethodNone,
	"health":       baselineMethodNone,
	"stats":        baselineMethodNone,
	"fetch":        baselineMethodNone, // ingests external URL — not a Read replacement
	"guide":        baselineMethodNone,
	"init":         baselineMethodNone,
	"doctor":       baselineMethodNone, // diagnostic report — no Read alternative
	"rebuild_fts":  baselineMethodNone, // admin: rebuild FTS5 indexes
	"self_test":    baselineMethodNone, // smoke test — no Read alternative
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

// shortNameFromID extracts the bare name from a symbol ID for use in
// search-by-name remediation hints (#704). Symbol ID format is
// {file_path}::{qualified_name}#{kind}; the short name is the last
// dotted segment of the qualified name (e.g. `db.Open` → `Open`,
// `server.*Server.handleTrace` → `handleTrace`). Returns the input
// unchanged if parsing fails — better to over-search than to lose
// the only signal the caller had about what they were looking for.
func shortNameFromID(id string) string {
	if i := strings.Index(id, "::"); i >= 0 {
		qn := id[i+2:]
		if j := strings.Index(qn, "#"); j >= 0 {
			qn = qn[:j]
		}
		if k := strings.LastIndex(qn, "."); k >= 0 {
			return qn[k+1:]
		}
		return qn
	}
	return id
}

// errResultRich is errResult plus a JSON body carrying the error message
// and a _meta.next_steps remediation hint (#704). The bare errResult text
// envelope leaves agents staring at a wall — when a `symbol not found` is
// returnable, the obvious next move (`search` for the short name) belongs
// in the response so the failure teaches instead of stalls. Capabilities
// list is included so failure responses match success-response shape.
// Clients that only parse text content still see the error message — JSON
// is a superset of text in their renderers — but JSON-aware clients pick
// up the structured remediation.
//
// #974: also stamps `index_in_progress` when the session indexer is
// mid-pass, mirroring the success path in jsonResultWithMeta. A
// "symbol not found" surfaced during a binary-drift re-extract is
// almost always transient — the caller deserves the same signal a
// successful-but-incomplete response carries, with a retry-after-pass
// hint prepended to next_steps.
func (s *Server) errResultRich(msg string, nextSteps []map[string]string) *mcp.CallToolResult {
	meta := map[string]any{
		"capabilities": s.capabilities,
	}
	if s.indexer != nil && s.sessionID != "" {
		if done, total, active := s.indexer.GetProgress(s.sessionID); active {
			meta["index_in_progress"] = map[string]any{
				"files_done":  done,
				"files_total": total,
			}
			// #993: phase the warning so files_done==files_total doesn't
			// read "mid-pass (55/55 files)" — the per-file walk has
			// finished but the cross-file resolvers are still running.
			var warnText string
			if total > 0 && done >= total {
				warnText = fmt.Sprintf("indexer is finalizing (cross-file resolver running after %d/%d files extracted); result may be transient — retry in a few seconds",
					done, total)
			} else {
				warnText = fmt.Sprintf("indexer is mid-pass (%d/%d files); result may be transient — retry after the pass completes",
					done, total)
			}
			meta["warnings"] = []string{warnText}
			nextSteps = append([]map[string]string{{
				"tool":  "index",
				"args":  `{}`,
				"why":   "wait for the in-flight pass to finish (or call again in a few seconds) — the symbol may resolve once extraction catches up",
			}}, nextSteps...)
		}
	}
	meta["next_steps"] = nextSteps
	body := map[string]any{
		"error": msg,
		"_meta": meta,
	}
	b, _ := json.Marshal(body)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
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

// parseFieldsArg parses a "field1,field2,field3" comma-separated
// allow-list into a set. Empty or whitespace-only input returns
// nil (no projection — caller returns all fields).
//
// #400: same shape `search` has used since v0.4 for its `fields`
// param; promoted to a shared helper so symbol / symbols / context
// / trace / changes can all opt callers into the same field-trim
// semantics. Caller-driven cut, no fidelity loss — agent picks
// what they want, server strips the rest.
func parseFieldsArg(s string) map[string]bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := make(map[string]bool)
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out[f] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// projectFields returns a shallow copy of m containing only keys in
// allow. nil allow returns m untouched (no projection). Unknown keys
// in allow are silently skipped — same behaviour as search's existing
// `fields` semantics, which lets callers pass a future-proof field
// list without breaking when a server version doesn't have all of
// them yet.
//
// `_meta` is preserved unconditionally — it carries token-savings
// numbers, drift warnings, and next-step suggestions that would
// otherwise be invisible. Callers that want to drop _meta can pop
// it after this call.
func projectFields(m map[string]any, allow map[string]bool) map[string]any {
	if allow == nil {
		return m
	}
	out := make(map[string]any, len(allow)+1)
	for f := range allow {
		if v, ok := m[f]; ok {
			out[f] = v
		}
	}
	if v, ok := m["_meta"]; ok {
		out["_meta"] = v
	}
	return out
}

// projectAndCheckFields (#914) is the shared projection+check pattern
// used by every handler that exposes a `fields=` parameter. It runs
// `projectFieldsChecked`, decides between "drop unknowns + warn" and
// "all-unknown → keep full response + warn", and returns the chosen
// data map. Caller passes a no-op `fields` (empty allow map → nil
// returned by parseFieldsArg) and gets m unchanged.
//
// Pre-#914 the rule was applied only in `symbol` and `context`; trace,
// changes, and similar handlers used the plain `projectFields` and
// silently dropped typo'd field names with no signal.
func projectAndCheckFields(m map[string]any, allow map[string]bool) map[string]any {
	if allow == nil {
		return m
	}
	projected, unknown := projectFieldsChecked(m, allow)
	if len(unknown) == 0 {
		return projected
	}
	validKeys := projectableKeys(m)
	realKeys := projectableKeys(projected)
	if len(realKeys) == 0 {
		// Every requested field was bogus — keep the full body so the
		// call stays useful, but tell the caller their projection was
		// malformed.
		attachWarning(m, fmt.Sprintf(
			"fields=%v matched no keys; valid keys: %v — returning full response",
			unknown, validKeys))
		return m
	}
	attachWarning(projected, fmt.Sprintf(
		"fields %v matched no keys and were dropped; valid keys: %v",
		unknown, validKeys))
	return projected
}

// projectFieldsChecked is projectFields plus the list of requested
// field names that matched no key in m (#712 C.2). Callers surface
// those in _meta.warnings — a typo'd field name (`fields=id` on
// `context`, whose top-level keys are symbol/imports/callees) would
// otherwise silently produce a `{_meta-only}` empty response and the
// caller never learns why. `_meta` is never counted as unknown — it's
// always preserved and isn't a caller-projectable key.
func projectFieldsChecked(m map[string]any, allow map[string]bool) (projected map[string]any, unknown []string) {
	if allow == nil {
		return m, nil
	}
	out := make(map[string]any, len(allow)+1)
	for f := range allow {
		if v, ok := m[f]; ok {
			out[f] = v
		} else if f != "_meta" {
			unknown = append(unknown, f)
		}
	}
	if v, ok := m["_meta"]; ok {
		out["_meta"] = v
	}
	sort.Strings(unknown)
	return out, unknown
}

// projectableKeys returns m's caller-facing top-level keys (everything
// except the always-preserved _meta), sorted — used to build the
// "valid keys: ..." half of an unknown-field warning.
func projectableKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "_meta" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// attachWarning appends a non-fatal advisory to data["_meta"].warnings,
// creating the _meta map / warnings slice as needed. Mirrors the
// _meta.warnings convention used by search/list/trace clamp warnings.
func attachWarning(data map[string]any, msg string) {
	meta, _ := data["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		data["_meta"] = meta
	}
	warnings, _ := meta["warnings"].([]string)
	meta["warnings"] = append(warnings, msg)
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
// clampMinConfidence enforces the documented [0.0, 1.0] range for the
// min_confidence parameter (#875). Values > 1.0 used to filter every
// result silently — extraction_confidence is in [0,1], so `> 1.0` is
// never satisfied. Same silent-confidently-wrong class as the trace
// `depth` clamp (#703); same remediation shape (clamp + warn). Returns
// the clamped value and a warning string ("" when in range).
func clampMinConfidence(v float64) (float64, string) {
	if v > 1.0 {
		return 1.0, fmt.Sprintf("min_confidence=%g clamped to 1.0 (valid range 0.0-1.0; out-of-range silently filtered every result)", v)
	}
	return v, ""
}

// canonicalKindCase (#910) returns the canonical-case spelling of a
// known symbol kind for case-insensitive matching. Same pattern as
// canonicalLanguageCase — the stored canonical values are PascalCase
// per the extractor convention, and a case-mismatched filter input
// silently returns 0 rows. Returns "" for unknown kinds; the existing
// unknown-enum-value path handles those.
func canonicalKindCase(in string) string {
	known := []string{
		// Code symbols
		"Function", "Method", "Class", "Interface", "Type", "Variable",
		"Module", "Constant", "Field", "Property", "Enum", "Trait",
		// Config / docs symbols
		"Section", "Setting", "Block", "Resource", "DataSource", "Provider",
		"Output", "Local", "Heading", "Document",
	}
	lower := strings.ToLower(in)
	for _, k := range known {
		if strings.ToLower(k) == lower {
			return k
		}
	}
	return ""
}

// canonicalLanguageCase (#902) returns the canonical-case spelling of
// a known language for the case-insensitive match. Returns "" when the
// input doesn't match any known language at all (case-insensitive) —
// in which case the existing unknown-enum-value advisory handles it.
// Sourced from the same language taxonomy the ast registry self-
// registers; the list is short enough to keep inline. Update when new
// extractors land.
func canonicalLanguageCase(in string) string {
	known := []string{
		"Go", "Python", "JavaScript", "TypeScript", "Rust", "Java",
		"Ruby", "PHP", "C", "C++", "C#", "Kotlin", "Swift", "Scala",
		"Lua", "Zig", "Elixir", "Haskell", "Dart", "R",
		"YAML", "JSON", "HCL", "TOML", "Bash", "Markdown", "HTML",
		"Makefile", "Jinja2", "XML",
	}
	lower := strings.ToLower(in)
	for _, k := range known {
		if strings.ToLower(k) == lower {
			return k
		}
	}
	return ""
}

// suggestDeadCodeFloor (#896) picks the next-lower min_confidence floor
// for the dead_code empty-result advisory. Returns a negative sentinel
// when the caller is already at or below the widest floor (≤0.0), so
// the caller drops the min_confidence hint entirely instead of
// recommending a HIGHER value (which would narrow the candidate pool —
// the opposite of "find more dead code").
//
// Steps mirror the meaningful confidence tiers in the corpus:
//   - >0.95 (default 0.95 strict tier) → 0.7  (admit Regex-stable extractors)
//   - >0.7  (the regex-stable floor)   → 0.0  (admit approximate-regex tier)
//   - ≤0.0  (already widest)           → -1   (no further floor exists)
func suggestDeadCodeFloor(current float64) float64 {
	const eps = 1e-9
	switch {
	case current > 0.7+eps:
		return 0.7
	case current > 0.0+eps:
		return 0.0
	default:
		return -1
	}
}

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
	// `base:<branch>` scope: "what's on this branch that isn't on
	// <branch>" — three-dot syntax uses merge-base, which matches the
	// "PR diff" question agents actually ask before opening a PR.
	// Validation is strict: branch name must pass `git rev-parse
	// --verify --quiet` so we can return a clear error before
	// shelling out to `git diff` (where a bad ref produces a less
	// obvious failure). Validating also rejects `..` and `-` prefixed
	// strings that could otherwise look like flag injection — exec
	// without a shell already prevents shell injection, but extra
	// arg validation surfaces user error sooner.
	if strings.HasPrefix(scope, "base:") {
		branch := strings.TrimPrefix(scope, "base:")
		if err := validateGitRefName(branch); err != nil {
			return "", fmt.Errorf("invalid base branch %q: %w", branch, err)
		}
		verify := exec.Command("git", "rev-parse", "--verify", "--quiet", branch)
		verify.Dir = root
		if err := verify.Run(); err != nil {
			return "", fmt.Errorf("base branch %q not found in repo at %s", branch, root)
		}
		cmd := exec.Command("git", "diff", "--name-only", branch+"...HEAD")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			return "", err
		}
		// base:<branch> reports committed-only diff. Untracked
		// files on the working tree aren't part of "what does this
		// branch introduce vs base"; for that view, commit first
		// then re-run, or use scope=all separately.
		return string(out), nil
	}

	args := []string{"diff", "--name-only"}
	switch scope {
	case "", "unstaged":
		// default — bare `git diff`, working tree vs index
	case "staged":
		args = append(args, "--cached")
	case "all":
		args = append(args, "HEAD")
	default:
		// #437: typos like `unsage` used to fall through to a bare
		// `git diff`, returning an empty changeset that looks identical
		// to a clean working tree. Reject explicitly so the caller
		// knows their scope arg was wrong.
		return "", fmt.Errorf("unknown scope %q; must be unstaged / staged / all / base:<branch>", scope)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	// #422: `unstaged` reports working-tree-modified files only —
	// matching `git diff --name-only` semantics and the tool
	// description's documented scope ladder (`unstaged` / `staged` /
	// `all` (includes untracked)). Pre-fix, both `unstaged` and `all`
	// folded untracked files in, so an agent that asked for the
	// modified-files diff before a commit saw untracked dotfiles
	// instead — the `tests_to_run` list then read "nothing to test,
	// ship it" even when real edits were waiting on tracked files.
	//
	// `all` remains the scope that includes untracked, which is the
	// "what's NOT yet committed across the whole working tree?" view
	// the agent reaches for when finalising a commit.
	if scope == "all" {
		untracked, lsErr := runGitLsUntracked(root)
		if lsErr == nil && untracked != "" {
			return string(out) + untracked, nil
		}
	}
	return string(out), nil
}

// firstFTS5IncompatibleRegexChar returns a description of the first
// regex META-PATTERN in q that the existing #424 FTS5 sanitizer can't
// rescue. Returns "" when the query is safe. Pre-flight gate for
// #509 — the agent's natural reach for "match a pattern" is regex,
// and certain regex sequences leak past sanitization to FTS5 as raw
// "syntax error near".
//
// What's checked (narrow on purpose):
//   - `.*` / `.+` / `.?` — the "anything" / "one+" / "optional"
//     wildcards that are the unmistakable signature of a regex query
//     and never appear in a literal identifier.
//   - `/.../ ` — a query wrapped in slashes on both ends is a regex
//     literal (#786). No identifier or path search looks like that,
//     so the meta-chars inside (`[A-Z]`, `\w`) — which the sanitizer
//     would otherwise quietly mangle into zero-result token soup —
//     get the friendly redirect instead.
//
// What's NOT checked:
//   - Single `.` — handled by sanitizeFTS5Query (wraps `os.Stat` etc.)
//   - `(` `)` `[` `]` `{` `}` `?` `!` — sanitizeFTS5Query wraps these.
//     (A bare `[A-Z]` without slash delimiters stays the sanitizer's
//     job — only the slash-wrapped form is unambiguously regex.)
//   - `*` alone — FTS5 supports it as a prefix wildcard (`auth*`).
//   - Anything inside a quoted phrase ("...") — caller's choice.
//
// This narrowness avoids breaking the dotted-identifier and prefix-
// wildcard cases pinned by `TestHandleSearch_DottedIdentifier_DoesNotError`
// and the docs-corpus wildcard test.
func firstFTS5IncompatibleRegexChar(q string) string {
	// Slash-delimited regex literal: /.../ — unambiguous, check first.
	if len(q) > 2 && q[0] == '/' && q[len(q)-1] == '/' {
		return "/.../"
	}
	inQuote := false
	for i := 0; i < len(q)-1; i++ {
		c := q[i]
		if c == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		if c == '.' {
			next := q[i+1]
			if next == '*' || next == '+' || next == '?' {
				return string([]byte{c, next})
			}
		}
	}
	return ""
}

// validateGitRefName rejects branch names that could confuse `git`
// or look like flag injection. Permissive enough to allow real refs
// (alphanumerics, `/`, `-`, `_`, `.`, `@`, `{`, `}` for reflog refs
// like `HEAD@{1}`); strict enough to reject empty strings, leading
// `-` (flag injection), and `..` (range syntax that bypasses the
// rev-parse single-ref check). The downstream `git rev-parse
// --verify --quiet` is the real gate; this is a fast pre-check
// that fails clearly before the subprocess runs.
func validateGitRefName(name string) error {
	if name == "" {
		return fmt.Errorf("empty branch name")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("branch name cannot start with '-' (looks like a flag)")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("branch name cannot contain '..' (use the bare branch name, not a range)")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '/', r == '-', r == '_', r == '.',
			r == '@', r == '{', r == '}':
			continue
		default:
			return fmt.Errorf("branch name contains disallowed character %q", r)
		}
	}
	return nil
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

// runGitDiffHunks returns the unified-diff text for the same scope as
// runGitDiff, but WITHOUT --name-only. The output retains hunk headers
// (`@@ -old,N +new,M @@`) so parseGitDiffHunks can extract per-file
// changed-line ranges (#502 — fixes blast-radius inflation when a
// single-function edit lives in a 6000-line file).
//
// scope handling mirrors runGitDiff so the two stay in lockstep.
// Untracked files don't appear in the unified diff (git can't compare
// against nothing); for scope='all' we emit them as full-file ranges
// upstream so the symbol intersection still includes their symbols.
func runGitDiffHunks(root, scope string) (string, error) {
	if strings.HasPrefix(scope, "base:") {
		branch := strings.TrimPrefix(scope, "base:")
		if err := validateGitRefName(branch); err != nil {
			return "", fmt.Errorf("invalid base branch %q: %w", branch, err)
		}
		// rev-parse --verify already done by runGitDiff before this
		// path is reached; skip the duplicate check.
		cmd := exec.Command("git", "diff", "--unified=0", branch+"...HEAD")
		cmd.Dir = root
		out, err := cmd.Output()
		return string(out), err
	}
	args := []string{"diff", "--unified=0"}
	switch scope {
	case "", "unstaged":
	case "staged":
		args = append(args, "--cached")
	case "all":
		args = append(args, "HEAD")
	default:
		return "", fmt.Errorf("unknown scope %q; must be unstaged / staged / all / base:<branch>", scope)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	return string(out), err
}

// parseGitDiffHunks parses unified-diff output into per-file changed-line
// ranges (#502). Returns map keyed by file path. Each hunk emits a
// half-open [start, end] range using the NEW-side line numbers (the
// `+new,M` half of the @@ header) since the symbol's StartLine /
// EndLine are post-edit byte offsets.
//
// Hunks with N=0 (pure deletions) still emit a single-line range at
// the new-side cursor position so a symbol whose function body just
// got a line removed shows up.
//
// Skips hunks at line 0 (file creation marker) and ignores binary diffs.
func parseGitDiffHunks(diff string) map[string][][2]int {
	out := map[string][][2]int{}
	var currentFile string
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			currentFile = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+++ "):
			// Some renames or new files; strip whatever prefix git used.
			currentFile = strings.TrimPrefix(line, "+++ ")
			currentFile = strings.TrimPrefix(currentFile, "b/")
		case strings.HasPrefix(line, "@@"):
			if currentFile == "" {
				continue
			}
			start, count := parseHunkHeader(line)
			if start <= 0 {
				continue
			}
			end := start + count - 1
			if count == 0 {
				end = start
			}
			out[currentFile] = append(out[currentFile], [2]int{start, end})
		}
	}
	return out
}

// parseHunkHeader extracts the +new,N portion of a unified-diff hunk
// header. Returns (startLine, lineCount). On parse failure returns
// (0, 0) so the caller can skip the hunk gracefully.
//
// Hunk header shape: `@@ -oldstart[,oldcount] +newstart[,newcount] @@`
// When ,N is omitted, the count defaults to 1 per the unified-diff spec.
func parseHunkHeader(header string) (int, int) {
	plus := strings.Index(header, "+")
	if plus < 0 {
		return 0, 0
	}
	rest := header[plus+1:]
	end := strings.Index(rest, " ")
	if end < 0 {
		return 0, 0
	}
	spec := rest[:end] // e.g. "123,7" or "123"
	parts := strings.SplitN(spec, ",", 2)
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0
	}
	count := 1
	if len(parts) == 2 {
		c, err := strconv.Atoi(parts[1])
		if err != nil {
			return start, 1
		}
		count = c
	}
	return start, count
}

// symbolOverlapsHunks returns true when [symStart, symEnd] intersects
// any [hunkStart, hunkEnd] range in hunks. Used by handleChanges to
// drop unchanged sibling symbols in a partially-edited file (#502).
//
// Empty hunks slice → return false (the file appears in the diff but
// no extractable hunks; safer to treat as "no symbols touched" than
// to retain the pre-#502 noise).
func symbolOverlapsHunks(symStart, symEnd int, hunks [][2]int) bool {
	for _, h := range hunks {
		if symStart <= h[1] && symEnd >= h[0] {
			return true
		}
	}
	return false
}

func parseGitDiffFiles(diff string) []string {
	// #408: zero-len init so callers see [] in JSON, not null. Same
	// JSON-shape invariant the v0.7.0 audit (#330 class) applied to
	// every other slice field in tool responses; this one slipped
	// through because the function returns to handleChanges which
	// puts the value directly into the response map.
	files := []string{}
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			files = append(files, line)
		}
	}
	return files
}


