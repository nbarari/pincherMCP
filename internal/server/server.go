// Package server implements the pincherMCP MCP server with all 14 tools.
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pincherMCP/pincher/internal/cypher"
	"github.com/pincherMCP/pincher/internal/db"
	"github.com/pincherMCP/pincher/internal/index"
)

// sessionFlushInterval controls how often in-memory session stats are persisted to SQLite.
const sessionFlushInterval = 10 * time.Second

// Server is the pincherMCP MCP server.
type Server struct {
	mcp      *mcp.Server
	store    *db.Store
	indexer  *index.Indexer
	handlers map[string]mcp.ToolHandler
	version  string
	httpKey  string // optional bearer token; empty = no auth required

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

	// mcpConnected is set to 1 when an MCP client fires onInit.
	// Sessions are only flushed to DB when an MCP client is connected — this
	// prevents the HTTP-only dashboard process from recording its own tool
	// calls (POST /v1/architecture etc.) as fake MCP sessions in the DB.
	mcpConnected int32
}

// New creates and registers all 14 MCP tools.
func New(store *db.Store, indexer *index.Indexer, version string) *Server {
	now := time.Now()
	s := &Server{
		store:               store,
		indexer:             indexer,
		handlers:            make(map[string]mcp.ToolHandler),
		version:             version,
		persistentSessionID: fmt.Sprintf("sess-%d", now.UnixNano()),
		sessionStartedAt:    now,
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

// StartSessionFlusher launches a background goroutine that persists session
// stats to SQLite every sessionFlushInterval. Call this after New().
func (s *Server) StartSessionFlusher(ctx context.Context) {
	go func() {
		t := time.NewTicker(sessionFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.flushSession() // final flush on shutdown
				return
			case <-t.C:
				s.flushSession()
			}
		}
	}()
}

// flushSession persists current in-memory session stats to the sessions table.
// Only runs when an MCP client has connected (mcpConnected=1). This prevents
// the HTTP-only dashboard process from writing its own tool calls as sessions.
func (s *Server) flushSession() {
	if atomic.LoadInt32(&s.mcpConnected) == 0 {
		return // no MCP client — HTTP-only process, don't record fake sessions
	}
	calls := atomic.LoadInt64(&s.statsCalls)
	if calls == 0 {
		return // nothing to record yet
	}
	tokensUsed := atomic.LoadInt64(&s.statsTokensUsed)
	tokensSaved := atomic.LoadInt64(&s.statsTokensSaved)
	costAvoided := float64(tokensSaved) / 1_000_000.0 * baseCostPer1M
	if err := s.store.RecordSession(s.persistentSessionID, s.sessionStartedAt, calls, tokensUsed, tokensSaved, costAvoided); err != nil {
		slog.Warn("pincher.session.flush.err", "err", err)
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
	// regression test in TestAuth_ConstantTime_FunctionalEquivalence
	// asserts behaviour on same-length and different-length mismatches.
	if s.httpKey != "" {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(tok))
		want := sha256.Sum256([]byte(s.httpKey))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
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
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": s.version})
		return
	}
	if path == "openapi.json" {
		json.NewEncoder(w).Encode(s.openAPISpec(r))
		return
	}
	if path == "dashboard" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Defense-in-depth CSP. The dashboard is an inline-only page
		// (HTML + inline <style> + inline <script> + fetch() to /v1/*),
		// so 'unsafe-inline' is required for both script-src and
		// style-src. We compensate by:
		//   - frame-ancestors 'none' — prevents clickjacking
		//   - default-src 'self' — restricts everything else to origin
		//   - connect-src 'self' — only the bundled fetch()es work
		//   - img-src 'self' data: — inline pixel art / favicons
		//   - object-src 'none' — no Flash/Java/etc.
		//   - base-uri 'self' — no <base> hijack
		//   - form-action 'self' — there are no forms today, but
		//     guards against future ones being targeted off-origin
		//
		// A future PR can extract the inline JS to /v1/dashboard.js and
		// drop unsafe-inline; the rest of the policy stays.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"object-src 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'; "+
				"frame-ancestors 'none'")
		// X-Content-Type-Options stops MIME-sniffing-based attacks where
		// a crafted response body is interpreted as a different type.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// X-Frame-Options is the legacy header for the same protection
		// frame-ancestors gives in the CSP — kept for older browsers.
		w.Header().Set("X-Frame-Options", "DENY")
		// Referrer-Policy keeps the dashboard's URL from leaking to
		// any third-party origin via outbound links.
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Write([]byte(renderDashboard(s.effectivePrefix(r))))
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
		var rows []map[string]any
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
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"error": fmt.Sprintf("unknown tool %q — available: index, symbol, symbols, context, search, query, trace, changes, architecture, schema, list, adr, health, stats", path),
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
}

func (s *Server) registerTools() {
	// 1. index
	s.addTool(&mcp.Tool{
		Name:        "index",
		Description: "Index a repository. Extracts symbols with byte offsets, builds a knowledge graph, and populates FTS5 search — all in one pass. Incremental by default (content-hash checks skip unchanged files). Set force=true to re-parse everything. Call this once per project before using any other tool.",
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
		Description: "Retrieve full source code for one symbol by stable ID using O(1) byte-offset seeking — never re-parses the file. Use `search` first to find the ID. Format: '{file_path}::{qualified_name}#{kind}'. Prefer `context` when you also need the symbol's dependencies, or `symbols` for batching multiple lookups.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"Stable symbol ID. Format: '{file_path}::{qualified_name}#{kind}'"},
				"project":{"type":"string","description":"Project name or ID. Defaults to session project."}
			}
		}`),
	}, s.handleSymbol)

	// 3. symbols (batch)
	s.addTool(&mcp.Tool{
		Name:        "symbols",
		Description: "Batch retrieve source code for multiple symbols in one call. Always prefer this over calling `symbol` in a loop — one round trip instead of N. Max 100 IDs per call. Returns array of {id, source, signature, file_path, start_line}.",
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
		Description: "PREFERRED for understanding a function: returns the symbol's full source PLUS all functions it directly imports/calls — in one shot. ~90% token reduction vs reading files. Use this instead of `symbol` whenever you need to understand how a function works in context. Returns {symbol: {source, ...}, imports: [{source, ...}]}.",
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
		Description: "Find symbols by name or content. Always start here when you don't know the exact symbol ID. Returns signature + a 5-line snippet for each result — often enough to answer without a follow-up call. Uses FTS5 BM25 ranking. Examples: search 'processOrder' to find a function, 'auth*' for prefix, '\"token validation\"' for a phrase. Filter by kind=Function or language=Go to narrow results. Use `context` on the result ID only if you need the full source + dependencies.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["query"],"properties":{
				"query":{"type":"string","description":"FTS5 search query. Supports: prefix (auth*), phrase (\"login flow\"), AND/OR."},
				"project":{"type":"string"},
				"kind":{"type":"string","description":"Filter by symbol kind: Function|Method|Class|Interface|Enum|Type|Variable|Module"},
				"language":{"type":"string","description":"Filter by language: Go|Python|TypeScript|etc"},
				"limit":{"type":"integer","description":"Max results (default 20)"},
				"fields":{"type":"string","description":"Comma-separated fields to include in each result, e.g. 'id,name,file_path'. Omit for all fields. Use to reduce token usage when you only need IDs or signatures."}
			}
		}`),
	}, s.handleSearch)

	// 6. query
	s.addTool(&mcp.Tool{
		Name:        "query",
		Description: "Graph query using Cypher syntax — use when you need structural relationships, not text matches. Examples: find all callers 'MATCH (a)-[:CALLS]->(b) WHERE b.name=\"Open\" RETURN a.name'; find classes in a file 'MATCH (n:Class) WHERE n.file_path CONTAINS \"server\" RETURN n.name'; multi-hop 'MATCH (a)-[:CALLS*1..3]->(b) WHERE a.name=\"main\" RETURN b.name'. Use `search` instead for name/text lookups.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["cypher"],"properties":{
				"cypher":{"type":"string","description":"Cypher query. Example: MATCH (f:Function)-[:CALLS]->(g) WHERE f.name='main' RETURN g.name LIMIT 20"},
				"project":{"type":"string"},
				"max_rows":{"type":"integer","description":"Max rows (default 200, max 10000)"}
			}
		}`),
	}, s.handleQuery)

	// 7. trace
	s.addTool(&mcp.Tool{
		Name:        "trace",
		Description: "Trace call relationships from a function — who calls it (inbound) or what it calls (outbound). Use for impact analysis before editing a function, or to understand a call chain. Risk labels show blast radius: CRITICAL=direct callers, HIGH=2 hops, MEDIUM=3 hops. Use `search` first to confirm the exact function name.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["name"],"properties":{
				"name":{"type":"string","description":"Function name to trace (short name, e.g. 'ProcessOrder')"},
				"project":{"type":"string"},
				"direction":{"type":"string","enum":["outbound","inbound","both"],"description":"outbound=what it calls, inbound=what calls it. Default: both"},
				"depth":{"type":"integer","description":"BFS depth 1-5 (default 3)"},
				"risk":{"type":"boolean","description":"Add CRITICAL/HIGH/MEDIUM/LOW risk labels (default true)"}
			}
		}`),
	}, s.handleTrace)

	// 8. changes
	s.addTool(&mcp.Tool{
		Name:        "changes",
		Description: "Pre-commit safety check: maps your git diff to affected symbols and computes blast radius. Use before committing to understand what you might have broken. Runs git diff, finds changed symbols, BFS-traces impact. Returns changed_symbols, impacted callers (with CRITICAL/HIGH/MEDIUM/LOW risk), and summary counts.",
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
		Description: "Codebase orientation in one call — call this first on any unfamiliar project. Returns: language breakdown, entry points, hotspot functions (most-called = highest change risk), and graph statistics. Much cheaper than reading files to understand the structure.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string"}
			}
		}`),
	}, s.handleArchitecture)

	// 10. schema
	s.addTool(&mcp.Tool{
		Name:        "schema",
		Description: "Graph schema overview: node kind counts (Function, Class, Method…), edge kind counts (CALLS, IMPORTS…), totals. Use before writing a `query` to see what node/edge kinds exist in this project.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string"}
			}
		}`),
	}, s.handleSchema)

	// 11. list
	s.addTool(&mcp.Tool{
		Name:        "list",
		Description: "List all indexed projects with stats: name, path, file count, symbol count, edge count, last indexed timestamp.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, s.handleList)

	// 12. adr
	s.addTool(&mcp.Tool{
		Name:        "adr",
		Description: "Persistent project knowledge store — survives across sessions. Store architectural decisions, team conventions, known gotchas. Actions: set (store), get (retrieve), list (all entries), delete. Examples: adr set PURPOSE 'payment processing service'; adr set STACK 'Go+SQLite+Redis'; adr list to recall everything stored.",
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
		Description: "Diagnostic report: schema version, index staleness (time since last index), and per-language extraction coverage (parser type + confidence score). Use to detect stale indexes or verify extraction quality before trusting graph results.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string","description":"Project to report on. Defaults to session project."}
			}
		}`),
	}, s.handleHealth)

	// 14. stats
	s.addTool(&mcp.Tool{
		Name:        "stats",
		Description: "Session savings summary: cumulative tokens used, tokens saved, cost avoided, and call count since the server started. Also shows per-project index size (files, symbols, edges). Useful for tracking how much context budget pincherMCP has saved.",
		InputSchema: json.RawMessage(`{
			"type":"object","properties":{
				"project":{"type":"string","description":"Project to include in index size breakdown. Defaults to session project."}
			}
		}`),
	}, s.handleStats)

	// 15. fetch
	s.addTool(&mcp.Tool{
		Name:        "fetch",
		Description: "Fetch a URL, extract its text content, and store it in the project knowledge base as a searchable Document. Use for API docs, library READMEs, specs, or any reference material you want to query later. After fetching, use `search` with kind=Document to find it, or `symbol` with the returned ID to retrieve the full text.",
		InputSchema: json.RawMessage(`{
			"type":"object","required":["url"],"properties":{
				"url":{"type":"string","description":"HTTP or HTTPS URL to fetch"},
				"project":{"type":"string","description":"Project to attach the document to. Defaults to session project."},
				"title":{"type":"string","description":"Override the page title used as the document name."}
			}
		}`),
	}, s.handleFetch)
}

// ─────────────────────────────────────────────────────────────────────────────
// Tool handlers
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) handleIndex(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

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
		"project":    result.Project,
		"path":       result.Path,
		"files":      result.Files,
		"symbols":    result.Symbols,
		"edges":      result.Edges,
		"skipped":    result.Skipped,
		"duration_ms": result.DurationMS,
	}
	return s.jsonResultWithMeta(data, start, 0), nil
}

func (s *Server) handleSymbol(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

	id := str(args, "id")
	if id == "" {
		return errResult("id is required"), nil
	}
	projectArg := str(args, "project")

	sym, err := s.store.GetSymbol(id)
	if err != nil {
		return errResult(fmt.Sprintf("db error: %v", err)), nil
	}
	if sym == nil {
		// Stale ID? Check symbol_moves for a redirect (handles file renames/moves).
		if newID, ok := s.store.ResolveStaleID(s.sessionID, id); ok {
			sym, err = s.store.GetSymbol(newID)
			if err != nil {
				return errResult(fmt.Sprintf("db error resolving stale id: %v", err)), nil
			}
		}
	}
	if sym == nil {
		return errResult(fmt.Sprintf("symbol %q not found", id)), nil
	}

	// Resolve project root for byte-offset seek
	projectID := sym.ProjectID
	if projectArg != "" {
		if pid, err := s.resolveProjectID(projectArg); err == nil {
			projectID = pid
		}
	}
	root, err := s.resolveProjectRoot(projectID)
	if err != nil {
		root = s.sessionRoot
	}

	// O(1) byte-offset retrieval — the pincherMCP core innovation
	source := ""
	if root != "" {
		source, _ = index.ReadSymbolSource(root, *sym)
	}
	// Document symbols (fetched URLs) store their content in Docstring — no local file to seek.
	if source == "" && sym.Kind == "Document" {
		source = sym.Docstring
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

	data := map[string]any{
		"id":            sym.ID,
		"name":          sym.Name,
		"qualified_name": sym.QualifiedName,
		"kind":          sym.Kind,
		"language":      sym.Language,
		"file_path":     sym.FilePath,
		"start_line":    sym.StartLine,
		"end_line":      sym.EndLine,
		"start_byte":    sym.StartByte,
		"end_byte":      sym.EndByte,
		"signature":     sym.Signature,
		"return_type":   sym.ReturnType,
		"docstring":     sym.Docstring,
		"complexity":             sym.Complexity,
		"is_exported":            sym.IsExported,
		"extraction_confidence":  sym.ExtractionConfidence,
		"source":                 source,
	}
	return s.jsonResultWithMeta(data, start, tokensSaved), nil
}

// maxBatchSymbols caps the number of IDs accepted by the symbols batch tool
// to prevent unbounded DB query loops and excessive memory usage.
const maxBatchSymbols = 100

func (s *Server) handleSymbols(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

	ids := strSlice(args, "ids")
	if len(ids) == 0 {
		return errResult("ids array is required"), nil
	}
	if len(ids) > maxBatchSymbols {
		return errResult(fmt.Sprintf("too many ids: max %d per call, got %d", maxBatchSymbols, len(ids))), nil
	}

	projectArg := str(args, "project")
	root := s.sessionRoot
	if projectArg != "" {
		if pid, err := s.resolveProjectID(projectArg); err == nil {
			if r, err := s.resolveProjectRoot(pid); err == nil {
				root = r
			}
		}
	}

	var results []map[string]any
	for _, id := range ids {
		sym, err := s.store.GetSymbol(id)
		if err != nil || sym == nil {
			results = append(results, map[string]any{"id": id, "error": "not found"})
			continue
		}
		source := ""
		if root != "" {
			source, _ = index.ReadSymbolSource(root, *sym)
		}
		results = append(results, map[string]any{
			"id":         sym.ID,
			"name":       sym.Name,
			"kind":       sym.Kind,
			"file_path":  sym.FilePath,
			"start_line": sym.StartLine,
			"signature":  sym.Signature,
			"source":     source,
		})
	}

	responseJSON, _ := json.Marshal(results)
	data := map[string]any{
		"symbols": results,
		"count":   len(results),
	}
	return s.jsonResultWithMeta(data, start, savedVsFullRead(len(results), responseJSON)), nil
}

func (s *Server) handleContext(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

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
	var imports []map[string]any
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
	responseJSON, _ := json.Marshal(data)
	return s.jsonResultWithMeta(data, start, savedVsFileSizes(root, allPaths, responseJSON)), nil
}

func (s *Server) handleSearch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

	query := str(args, "query")
	if query == "" {
		return errResult("query is required"), nil
	}
	projectArg := str(args, "project")
	kind := str(args, "kind")
	language := str(args, "language")
	limit := intArg(args, "limit", 20)
	fieldsArg := str(args, "fields")

	// project=* searches all indexed projects — no project filter applied.
	var projectID string
	if projectArg != "*" {
		var err error
		projectID, err = s.resolveProjectID(projectArg)
		if err != nil {
			return errResult(err.Error()), nil
		}
	}

	results, err := s.store.SearchSymbols(projectID, query, kind, language, limit)
	if err != nil {
		return errResult(fmt.Sprintf("search error: %v", err)), nil
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
	var rows []map[string]any
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

	data := map[string]any{
		"results": rows,
		"count":   len(rows),
		"query":   query,
	}
	return s.jsonResultWithMeta(data, start, tokensSaved), nil
}

func (s *Server) handleQuery(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

	cql := str(args, "cypher")
	if cql == "" {
		return errResult("cypher query is required"), nil
	}
	maxRows := intArg(args, "max_rows", 200)

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

	responseJSON, _ := json.Marshal(result.Rows)
	data := map[string]any{
		"columns": result.Columns,
		"rows":    result.Rows,
		"total":   result.Total,
	}
	return s.jsonResultWithMeta(data, start, savedVsFullRead(result.Total, responseJSON)), nil
}

func (s *Server) handleTrace(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

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

	projectID, errRes := s.mustProject(args)
	if errRes != nil {
		return errRes, nil
	}

	hops, err := s.indexer.Trace(ctx, projectID, name, direction, depth, addRisk)
	if err != nil {
		return errResult(fmt.Sprintf("trace error: %v", err)), nil
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

	var hopsList []map[string]any
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
	data := map[string]any{
		"root":      name,
		"direction": direction,
		"hops":      hopsList,
		"total":     len(hops),
	}
	if addRisk {
		data["risk_summary"] = riskCounts
	}
	return s.jsonResultWithMeta(data, start, savedVsFileSizes(traceRoot, tracedPaths, responseJSON)), nil
}

func (s *Server) handleChanges(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

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

	// BFS trace for blast radius
	var impacted []map[string]any
	seen := make(map[string]bool)
	for _, sym := range changedSymbols {
		hops, err := s.indexer.Trace(ctx, projectID, sym.Name, "inbound", depth, true)
		if err != nil {
			continue
		}
		for _, h := range hops {
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

	// Build risk summary
	riskCounts := map[string]int{"CRITICAL": 0, "HIGH": 0, "MEDIUM": 0, "LOW": 0}
	for _, item := range impacted {
		if r, ok := item["risk"].(string); ok {
			riskCounts[r]++
		}
	}

	var changedSymNames []map[string]any
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
		"summary": map[string]any{
			"changed_files":   len(changedFiles),
			"changed_symbols": len(changedSymbols),
			"total_impacted":  len(impacted),
			"critical":        riskCounts["CRITICAL"],
			"high":            riskCounts["HIGH"],
			"medium":          riskCounts["MEDIUM"],
			"low":             riskCounts["LOW"],
		},
	}
	return s.jsonResultWithMeta(data, start, savedVsFullRead(totalTracedSyms, responseJSON)), nil
}

func (s *Server) handleArchitecture(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

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

	// Entry points
	var entryPoints []map[string]any
	if epRows, err := s.store.RO().QueryContext(ctx,
		`SELECT name, file_path, start_line FROM symbols WHERE project_id=? AND is_entry_point=1 LIMIT 20`,
		projectID); err == nil {
		defer epRows.Close()
		for epRows.Next() {
			var name, fp string
			var line int
			if scanErr := epRows.Scan(&name, &fp, &line); scanErr == nil {
				entryPoints = append(entryPoints, map[string]any{"name": name, "file_path": fp, "start_line": line})
			}
		}
		_ = epRows.Err()
	}

	// Hotspots (most-called)
	hotspots, _ := s.store.GetHotspots(projectID, 10)
	var hotspotMaps []map[string]any
	for _, h := range hotspots {
		hotspotMaps = append(hotspotMaps, map[string]any{
			"name": h.Name, "kind": h.Kind, "file_path": h.FilePath,
		})
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
	// Architecture replaces reading every file to orient in the codebase.
	// Savings = all symbols in project × avgSymbolContext − this payload.
	symCount := 0
	if p != nil {
		symCount = p.SymCount
	}
	responseJSON, _ := json.Marshal(data)
	return s.jsonResultWithMeta(data, start, savedVsFullRead(symCount, responseJSON)), nil
}

func (s *Server) handleSchema(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)

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
	return s.jsonResultWithMeta(data, start, 0), nil
}

func (s *Server) handleList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start := time.Now()
	projects, err := s.store.ListProjects()
	if err != nil {
		return errResult(fmt.Sprintf("list error: %v", err)), nil
	}
	var rows []map[string]any
	for _, p := range projects {
		rows = append(rows, map[string]any{
			"id":         p.ID,
			"name":       p.Name,
			"path":       p.Path,
			"files":      p.FileCount,
			"symbols":    p.SymCount,
			"edges":      p.EdgeCount,
			"indexed_at": p.IndexedAt.Format(time.RFC3339),
		})
	}
	data := map[string]any{"projects": rows, "count": len(rows)}
	return s.jsonResultWithMeta(data, start, 0), nil
}

func (s *Server) handleADR(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)
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

	return s.jsonResultWithMeta(data, start, 0), nil
}

func (s *Server) handleHealth(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, args := beginCall(req)
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

	data := map[string]any{
		"schema_version": report.SchemaVersion,
		"db_path":        report.DBPath,
	}
	if report.Project != nil {
		data["project"] = map[string]any{
			"name":             report.Project.Name,
			"path":             report.Project.Path,
			"files":            report.Project.FileCount,
			"symbols":          report.Project.SymCount,
			"edges":            report.Project.EdgeCount,
			"indexed_at":       report.Project.IndexedAt.Format(time.RFC3339),
			"staleness_human":  report.StalenessHuman,
			"staleness_seconds": report.StalenessSecs,
		}
		data["extraction_coverage"] = report.Coverage
	}
	return s.jsonResultWithMeta(data, start, 0), nil
}

func (s *Server) handleStats(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, _ := beginCall(req)

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
	return s.textResultWithMeta(b.String(), start, 0), nil
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
	start, args := beginCall(req)

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
	return s.jsonResultWithMeta(data, start, tokensSaved), nil
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

func (s *Server) jsonResultWithMeta(data map[string]any, start time.Time, tokensSaved int) *mcp.CallToolResult {
	latency := time.Since(start).Milliseconds()

	// Estimate tokens in this response
	b, _ := json.Marshal(data)
	tokensUsed := db.ApproxTokens(string(b))

	// Cost avoided by not sending tokensSaved tokens to the model
	costAvoided := float64(tokensSaved) / 1_000_000.0 * baseCostPer1M

	data["_meta"] = map[string]any{
		"tokens_used":  tokensUsed,
		"tokens_saved": tokensSaved,
		"latency_ms":   latency,
		"cost_avoided": fmt.Sprintf("$%.4f", costAvoided),
	}

	// Accumulate session stats. On the very first call, flush immediately so
	// the dashboard sees the new session within milliseconds, not after 10s.
	newCalls := atomic.AddInt64(&s.statsCalls, 1)
	atomic.AddInt64(&s.statsTokensUsed, int64(tokensUsed))
	atomic.AddInt64(&s.statsTokensSaved, int64(tokensSaved))
	atomic.AddInt64(&s.statsLatencyMS, latency)

	// First call of a new session: flush immediately so the dashboard sees
	// the session within milliseconds rather than waiting for the 10s ticker.
	if newCalls == 1 {
		go s.flushSession()
	}

	out, _ := json.MarshalIndent(data, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
	}
}

// textResultWithMeta performs the same session accounting as jsonResultWithMeta
// but returns a pre-formatted text string rather than a JSON object. Used by
// handleStats so the output is human-readable on the command line.
func (s *Server) textResultWithMeta(text string, start time.Time, tokensSaved int) *mcp.CallToolResult {
	latency := time.Since(start).Milliseconds()
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
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: full}},
	}
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
func beginCall(req *mcp.CallToolRequest) (time.Time, map[string]any) {
	return time.Now(), parseArgs(req)
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


