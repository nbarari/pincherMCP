package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pincherMCP/pincher/internal/db"
	"github.com/pincherMCP/pincher/internal/index"
	"github.com/pincherMCP/pincher/internal/server"
)

// version is overridden at build time via -ldflags="-X main.version=...".
var version = "0.1.0"

func main() {
	// Subcommand dispatch — must happen before flag.Parse() since the global
	// flagset doesn't know about subcommand flags.
	if len(os.Args) > 1 && os.Args[1] == "index" {
		runIndexCLI(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		runDoctorCLI(os.Args[2:])
		return
	}

	var (
		showVersion = flag.Bool("version", false, "Print version and exit")
		dataDir     = flag.String("data-dir", "", "Override data directory (default: platform-appropriate)")
		verbose     = flag.Bool("verbose", false, "Enable verbose logging")
		httpAddr    = flag.String("http", "", "Also listen for HTTP requests on this address (e.g. :8080, or :0 to let the OS pick a free port). Falls back to $PINCHER_HTTP_ADDR. Enables any HTTP client to call all tools via POST /v1/{tool}.")
		httpKey     = flag.String("http-key", "", "Require this bearer token on all HTTP requests (recommended for non-localhost deployments). Falls back to $PINCHER_HTTP_KEY.")
		httpRate    = flag.Int("http-rate", 0, "Max HTTP requests per IP per minute. 0 = unlimited.")
		basePath    = flag.String("basepath", "", "External URL prefix when behind a reverse proxy (e.g. /pincher). Both /pincher/v1/* and /v1/* will route. Falls back to $PINCHER_BASEPATH.")
		trustProxy  = flag.Bool("trust-proxy", false, "Honor X-Forwarded-Prefix / X-Forwarded-Proto / X-Forwarded-Host headers. Only enable when behind a trusted proxy. Falls back to $PINCHER_TRUST_PROXY=1.")
		slowQueryMS = flag.Int64("slow-query-ms", 0, "Persist tool calls slower than N ms to the slow_queries table for `pincher doctor` to surface (#42). 0 = disabled (zero overhead).")
	)
	flag.Parse()

	// Env fallbacks for the HTTP knobs so install-time configuration
	// (Docker -e, systemd EnvironmentFile, launchd, k8s) doesn't need
	// to rewrite argv.
	if *httpAddr == "" {
		*httpAddr = os.Getenv("PINCHER_HTTP_ADDR")
	}
	if *httpKey == "" {
		*httpKey = os.Getenv("PINCHER_HTTP_KEY")
	}
	if *basePath == "" {
		*basePath = os.Getenv("PINCHER_BASEPATH")
	}
	if !*trustProxy && os.Getenv("PINCHER_TRUST_PROXY") == "1" {
		*trustProxy = true
	}

	if *showVersion {
		fmt.Printf("pincherMCP v%s\n", version)
		os.Exit(0)
	}

	if !*verbose {
		log.SetOutput(os.Stderr)
		log.SetFlags(0)
	}

	// Determine data directory
	dir := *dataDir
	if dir == "" {
		var err error
		dir, err = db.DataDir()
		if err != nil {
			log.Fatalf("pincherMCP: failed to determine data directory: %v", err)
		}
	}

	// Open SQLite store
	store, err := db.Open(dir)
	if err != nil {
		log.Fatalf("pincherMCP: failed to open database: %v", err)
	}
	defer store.Close()

	// Build indexer
	idx := index.New(store)

	// Build MCP server with all 15 tools
	srv := server.New(store, idx, version)

	// Slow-query capture (#42 part 2) applies to BOTH MCP stdio calls and
	// HTTP requests — must be set before either transport starts.
	if *slowQueryMS > 0 {
		srv.SetSlowQueryThreshold(*slowQueryMS)
	}

	// Context with graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start background file watcher and session persistence flusher
	go idx.Watch(ctx)
	srv.StartSessionFlusher(ctx)

	// Optionally run HTTP server for platform-agnostic access.
	if *httpAddr != "" {
		if *httpKey != "" {
			srv.SetHTTPKey(*httpKey)
		}
		if *httpRate > 0 {
			srv.SetRateLimit(*httpRate, time.Minute)
		}
		if *basePath != "" {
			srv.SetBasePath(*basePath)
		}
		if *trustProxy {
			srv.SetTrustProxy(true)
		}
		go func() {
			if err := srv.ListenAndServeHTTP(ctx, *httpAddr); err != nil {
				log.Printf("pincherMCP: http server error: %v", err)
			}
		}()
	}

	// Run MCP server over stdio
	if err := srv.MCPServer().Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatalf("pincherMCP: server error: %v", err)
	}
}

// runIndexCLI implements "pincher index [--force] [--hook] [--data-dir DIR] [PATH]".
//
// When PATH is omitted the current working directory is used, making it
// suitable as a zero-argument SessionStart hook:
//
//	"C:\\tools\\pincher.exe" index --hook
//
// With --hook the output is a Claude Code hook JSON envelope that injects
// the index summary as additionalContext so Claude knows the project is ready.
// Without --hook a human-readable one-line summary is printed instead.
func runIndexCLI(args []string) {
	// Silence the DB/indexer log output — callers only want the result line.
	log.SetOutput(io.Discard)

	fs := flag.NewFlagSet("index", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	force := fs.Bool("force", false, "Re-parse all files regardless of content hash")
	hookMode := fs.Bool("hook", false, "Output Claude Code SessionStart hook JSON instead of plain text")
	jsonSummary := fs.Bool("json-summary", false, "Emit a structured snapshot JSON to stdout (used by corpus-snapshot tooling, #33)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher index [--force] [--hook] [--json-summary] [--data-dir DIR] [PATH]")
		fmt.Fprintln(os.Stderr, "  Indexes PATH (default: current directory) into the pincher knowledge graph.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	path := fs.Arg(0)
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pincher: failed to get working directory: %v\n", err)
			os.Exit(1)
		}
	}

	// Refuse known bloat traps: indexing $HOME directly produces millions
	// of low-signal cache symbols, and SessionStart hook fires from a
	// non-project directory have no useful index to build. Hook mode
	// exits 0 silently so the SessionStart hook doesn't fail loudly;
	// manual mode exits 1 with a clear message.
	if trap, reason := isBloatTrap(path, *hookMode); trap {
		fmt.Fprintf(os.Stderr, "pincher: refusing to index %q (%s)\n", path, reason)
		if *hookMode {
			os.Exit(0)
		}
		os.Exit(1)
	}

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

	idx := index.New(store)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	result, err := idx.Index(ctx, path, *force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher: index error: %v\n", err)
		os.Exit(1)
	}

	// Fetch totals from DB (IndexResult only has delta counts for this run).
	totalSyms, totalEdges, _, _, _ := store.GraphStats(result.ProjectID)

	// Count uncommitted changed files via git (best-effort; ignored on error).
	changedFiles := gitChangedCount(path)

	if *jsonSummary {
		emitSnapshotJSON(store, result, dir)
		return
	}

	if *hookMode {
		var parts []string
		parts = append(parts, fmt.Sprintf("project '%s' — %d symbols, %d edges across %d files (%dms, %d unchanged)",
			result.Project, totalSyms, totalEdges, result.Skipped+result.Files, result.DurationMS, result.Skipped))
		if changedFiles > 0 {
			parts = append(parts, fmt.Sprintf("%d file(s) have uncommitted changes — call mcp__pincher__changes to see blast radius", changedFiles))
		}
		parts = append(parts, "call mcp__pincher__stats for session savings · mcp__pincher__changes for git diff · use pincher tools before Read/Grep")

		msg := "Pincher ready: " + parts[0] + ". "
		if changedFiles > 0 {
			msg += parts[1] + ". "
		}
		msg += parts[len(parts)-1] + "."

		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "SessionStart",
				"additionalContext": msg,
			},
		}
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		blockedFrag := ""
		if result.Blocked > 0 {
			blockedFrag = fmt.Sprintf(", %d blocked", result.Blocked)
		}
		fmt.Printf("indexed %s: %d total symbols, %d total edges, %d files (%d unchanged%s, %dms)\n",
			result.Project, totalSyms, totalEdges, result.Skipped+result.Files,
			result.Skipped, blockedFrag, result.DurationMS)
		if changedFiles > 0 {
			fmt.Printf("  %d file(s) with uncommitted changes\n", changedFiles)
		}
	}
}

// emitSnapshotJSON writes the corpus-snapshot shape (#33) to stdout. Counts
// come from the canonical sources: GraphStats for symbol/edge totals and
// per-kind groupings, AvgConfidenceByKind for signal-quality drift, and
// os.Stat on the DB file for storage cost. Stable JSON ordering via
// json.Marshal's alphabetical map iteration.
func emitSnapshotJSON(store *db.Store, result *index.IndexResult, dataDir string) {
	_, _, kindCounts, edgeKindCounts, _ := store.GraphStats(result.ProjectID)
	avgConf, _ := store.AvgConfidenceByKind(result.ProjectID)

	// Round confidence to 4 decimals so floating-point noise across runs
	// (e.g. yaml.v3 producing slightly different mappings on different
	// platforms) doesn't churn the snapshot diff. Human-readable too.
	roundedConf := make(map[string]float64, len(avgConf))
	for k, v := range avgConf {
		roundedConf[k] = math.Round(v*10000) / 10000
	}

	dbSizeKB := int64(0)
	if info, err := os.Stat(store.Path); err == nil {
		dbSizeKB = info.Size() / 1024
	}

	summary := map[string]any{
		"schema_version":         dbSchemaVersion(store),
		"files_seen":             result.Files + result.Skipped + result.Blocked,
		"files_indexed":          result.Files + result.Skipped, // Skipped == hash-skip but still "indexed" prior runs
		"files_blocked":          result.Blocked,
		"symbol_count_by_kind":   kindCounts,
		"edge_count_by_kind":     edgeKindCounts,
		"avg_confidence_by_kind": roundedConf,
		"db_size_kb":             dbSizeKB,
		"duration_ms":            result.DurationMS,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(summary)
}

// dbSchemaVersion reads the current schema_version. Best-effort; returns 0
// on any error (the snapshot diff will still surface the discrepancy).
func dbSchemaVersion(store *db.Store) int {
	var v int
	_ = store.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&v)
	return v
}

// gitChangedCount returns the number of files with uncommitted changes
// (staged + unstaged) in the given directory. Returns 0 on any error.
func gitChangedCount(dir string) int {
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	count := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if line := sc.Text(); len(line) >= 2 && line[0] != '?' {
			count++
		}
	}
	return count
}
