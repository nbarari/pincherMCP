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
	"strconv"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pincherMCP/pincher/internal/db"
	"github.com/pincherMCP/pincher/internal/index"
	"github.com/pincherMCP/pincher/internal/server"
)

// version is overridden at build time via -ldflags="-X main.version=...".
// Default tracks the latest released tag so binaries built without -ldflags
// from a clone at HEAD report a version that matches the GitHub release.
var version = "0.3.0"

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
	if len(os.Args) > 1 && os.Args[1] == "rebuild-fts" {
		runRebuildFTSCLI(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "self-test" {
		runSelfTestCLI(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "stats" {
		runStatsCLI(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "update" {
		runUpdateCLI(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "web" {
		runWebCLI(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "init" {
		runInitCLI(os.Args[2:])
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
		dbReaders   = flag.Int("db-readers", db.DefaultReaderPoolSize, "Maximum concurrent SQLite read connections. Higher = more parallel tool calls behind a busy server; capped at 32. Falls back to $PINCHER_DB_READERS.")
		maxFileMB   = flag.Int("max-file-size-mb", int(index.DefaultMaxFileSize/(1024*1024)), "Per-file size cap during indexing (MB). Files larger than this are recorded as `file_too_large` failures and skipped without being read into memory (#111). 0 disables the cap. Falls back to $PINCHER_MAX_FILE_SIZE_MB.")
	)
	// Custom usage banner: subcommand summary + the standard flag list.
	// Without this, `pincher --help` only shows flags — and a new user has
	// no way to discover that `pincher doctor`, `pincher index`, etc. exist
	// without reading the README or source.
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		printHelpBanner(out)
		flag.PrintDefaults()
	}
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

	// Env fallback for --db-readers so install-time configuration can
	// tune it without rewriting argv. Pass through unchanged if not set.
	if envReaders := os.Getenv("PINCHER_DB_READERS"); envReaders != "" && *dbReaders == db.DefaultReaderPoolSize {
		if v, parseErr := strconv.Atoi(envReaders); parseErr == nil && v > 0 {
			*dbReaders = v
		}
	}

	// Env fallback for --max-file-size-mb (#111). Default-comparison gate
	// matches the --db-readers pattern: env only wins when the flag is at
	// its built-in default, so an explicit `--max-file-size-mb 0` survives.
	if env := os.Getenv("PINCHER_MAX_FILE_SIZE_MB"); env != "" && *maxFileMB == int(index.DefaultMaxFileSize/(1024*1024)) {
		if v, parseErr := strconv.Atoi(env); parseErr == nil && v >= 0 {
			*maxFileMB = v
		}
	}

	// Open SQLite store with the configured reader pool size.
	store, err := db.OpenWithReaders(dir, *dbReaders)
	if err != nil {
		log.Fatalf("pincherMCP: failed to open database: %v", err)
	}
	defer store.Close()

	// Build indexer with the configured per-file cap (#111).
	idx := index.New(store)
	idx.SetMaxFileSize(int64(*maxFileMB) * 1024 * 1024)

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

// printHelpBanner writes the subcommand summary that prefixes
// `pincher --help`. Pulled out of main()'s flag.Usage closure so it's
// directly testable without invoking the CLI binary.
func printHelpBanner(out io.Writer) {
	fmt.Fprintln(out, "pincherMCP — local code intelligence MCP server")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  pincher                        Run as MCP stdio server (Claude Code, etc.)")
	fmt.Fprintln(out, "  pincher --http :PORT           Run as MCP stdio + HTTP REST server")
	fmt.Fprintln(out, "  pincher index PATH             Index a repository from the CLI")
	fmt.Fprintln(out, "  pincher doctor                 Diagnostic report (schema, staleness, failures)")
	fmt.Fprintln(out, "  pincher self-test              Smoke-test the install end-to-end")
	fmt.Fprintln(out, "  pincher rebuild-fts            Drop + recreate the FTS5 search indexes")
	fmt.Fprintln(out, "  pincher stats                  Persisted savings + per-project counts (--json, --reset)")
	fmt.Fprintln(out, "  pincher update                 Update pincher in place (git pull + build, or release asset)")
	fmt.Fprintln(out, "  pincher web                    Print dashboard URL of running HTTP server (auto-starts one if none)")
	fmt.Fprintln(out, "  pincher init [--global]        Inject the pincher usage policy block into CLAUDE.md")
	fmt.Fprintln(out, "  pincher --version              Print version and exit")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Each subcommand accepts its own --help, e.g. `pincher doctor --help`.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Flags (apply to the no-subcommand form — running as MCP server):")
}

func runIndexCLI(args []string) {
	// Silence the DB/indexer log output — callers only want the result line.
	log.SetOutput(io.Discard)

	fs := flag.NewFlagSet("index", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	force := fs.Bool("force", false, "Re-parse all files regardless of content hash")
	hookMode := fs.Bool("hook", false, "Output Claude Code SessionStart hook JSON instead of plain text")
	jsonSummary := fs.Bool("json-summary", false, "Emit a structured snapshot JSON to stdout (used by corpus-snapshot tooling, #33)")
	maxFileMB := fs.Int("max-file-size-mb", int(index.DefaultMaxFileSize/(1024*1024)), "Per-file size cap during indexing (MB). 0 disables the cap. Falls back to $PINCHER_MAX_FILE_SIZE_MB. (#111)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher index [--force] [--hook] [--json-summary] [--max-file-size-mb MB] [--data-dir DIR] [PATH]")
		fmt.Fprintln(os.Stderr, "  Indexes PATH (default: current directory) into the pincher knowledge graph.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if env := os.Getenv("PINCHER_MAX_FILE_SIZE_MB"); env != "" && *maxFileMB == int(index.DefaultMaxFileSize/(1024*1024)) {
		if v, parseErr := strconv.Atoi(env); parseErr == nil && v >= 0 {
			*maxFileMB = v
		}
	}

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
	idx.SetMaxFileSize(int64(*maxFileMB) * 1024 * 1024)

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

	// extraction_failures_by_reason: per-corpus count of each
	// extraction_failure reason. The cross-cutting QN-collision gate.
	//
	// **Why this is in every snapshot**: every recent extractor bug
	// (#69, #74, #79, #80) reduced to "extractor produced a duplicate
	// qualified_name." Each was caught only AFTER nbarari hit it on a
	// real corpus. With this field in every snapshot, a future variant
	// of the same bug class fails CI at PR time — the snapshot diff
	// surfaces the new non-zero count immediately, instead of slipping
	// through to a daily-DB report weeks later.
	//
	// Always emitted (even when the map is empty) so the diff against a
	// "0 failures" snapshot is unambiguous: `{}` → `{ qualified_name_collision: 1 }`
	// is a clear regression signal. The map type makes the diff
	// human-readable in PR review.
	failures, _ := store.ExtractionFailureCountsByReason(result.ProjectID)
	if failures == nil {
		failures = map[string]int{}
	}
	summary["extraction_failures_by_reason"] = failures

	// search_relevance: per-corpus curated query → top-hit-kind + qualified
	// name. The relevance regression gate that prerequisites #32 (per-corpus
	// FTS5 split). Without this, switching FTS architecture could silently
	// shift BM25 ranking and we'd never know.
	if rel := computeSearchRelevance(store, result); rel != nil {
		summary["search_relevance"] = rel
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(summary)
}

// searchRelevanceQuery captures one curated query plus the FTS5 corpus
// that should answer it. Each query is paired with the corpus it tests
// because, after the #32 part-3 default flip, an unparameterized search
// against a YAML-only corpus would return zero hits — pinning the
// corpus per query makes the snapshot semantically meaningful (it
// answers "for THIS corpus, does THIS query rank the right symbol on top?").
type searchRelevanceQuery struct {
	Query  string
	Corpus string
}

// searchRelevanceQueries maps a corpus name (== project name) to the
// curated query set whose top-hit metadata is locked into the snapshot.
// Adding a query = a new line here + `make corpus-snapshot-update` to
// record the current top-hit. A future PR that shifts ranking produces
// a snapshot diff that surfaces every shift explicitly for review.
//
// Each entry is a curated representative: a query whose intended top hit
// is unambiguous on its corpus. Avoid queries that match multiple symbols
// equally well (the BM25 tiebreak is implementation-defined and flaky).
var searchRelevanceQueries = map[string][]searchRelevanceQuery{
	"go-project": {
		// Code corpus — Go identifiers.
		{Query: "Open", Corpus: db.CorpusCode},   // Function in internal/auth/auth.go
		{Query: "Greet", Corpus: db.CorpusCode},  // Function in cmd/cli/main.go
		{Query: "User", Corpus: db.CorpusCode},   // Method on Session
	},
	"k8s-ops": {
		// Config corpus — YAML Settings.
		{Query: "image", Corpus: db.CorpusConfig},        // services.web.image / helm.values.image
		{Query: "replicaCount", Corpus: db.CorpusConfig}, // helm/values.yaml
		{Query: "deployment", Corpus: db.CorpusConfig},   // deployment.yaml metadata
	},
	"node-monorepo": {
		// Mixed: Greeter is a code Class; compilerOptions is a JSON Setting.
		{Query: "Greeter", Corpus: db.CorpusCode},
		{Query: "compilerOptions", Corpus: db.CorpusConfig},
		{Query: "makeGreeter", Corpus: db.CorpusCode},
	},
	"docs-site": {
		// Docs corpus — Markdown headings extracted by the goldmark-backed
		// extractor (#81). Each query targets a hierarchical qualified name
		// produced by the heading walker.
		{Query: "Authentication", Corpus: db.CorpusDocs},   // api_reference.authentication
		{Query: "Installation", Corpus: db.CorpusDocs},     // getting_started.installation
		{Query: "Endpoints", Corpus: db.CorpusDocs},        // api_reference.endpoints
	},
	"terraform-stack": {
		// Config corpus — HCL block/attribute symbols (#189).
		// Each query targets a representative symbol kind from the
		// HCL extractor: Variable, Resource, Module. Single-token
		// queries — FTS5 treats `.` as an operator, so dotted
		// identifiers like `aws_instance.web` don't match cleanly.
		{Query: "stack_name", Corpus: db.CorpusConfig},     // Variable in variables.tf
		{Query: "aws_security_group", Corpus: db.CorpusConfig}, // Resource in main.tf
		{Query: "network", Corpus: db.CorpusConfig},        // Module call in main.tf
	},
}

// SearchRelevanceHit is the per-query record persisted to the snapshot.
type SearchRelevanceHit struct {
	Query       string `json:"query"`
	Corpus      string `json:"corpus,omitempty"`
	TopHitKind  string `json:"top_hit_kind,omitempty"`
	TopHitQN    string `json:"top_hit_qn,omitempty"`
	NoMatch     bool   `json:"no_match,omitempty"`
}

// defaultMinConfidence mirrors the MCP `search` tool's default
// (#34 Phase 4). Threading it through the snapshot's search_relevance
// computation ensures the committed snapshot matches what an actual
// `search` call returns. A future PR that changes the default surfaces
// in the snapshot diff at PR time.
const defaultMinConfidence = 0.7

// computeSearchRelevance runs the curated query set for the given corpus
// and returns the top-hit metadata for each. Mirrors handleSearch's
// post-fetch filtering: pulls extra rows then applies min_confidence
// before slicing to the limit.
func computeSearchRelevance(store *db.Store, result *index.IndexResult) []SearchRelevanceHit {
	queries, ok := searchRelevanceQueries[result.Project]
	if !ok {
		return nil
	}
	out := make([]SearchRelevanceHit, 0, len(queries))
	for _, q := range queries {
		hit := SearchRelevanceHit{Query: q.Query, Corpus: q.Corpus}
		// Fetch extra so post-filter top-hit selection is robust.
		results, err := store.SearchSymbolsByCorpus(result.ProjectID, q.Query, "", "", q.Corpus, 10)
		if err != nil || len(results) == 0 {
			hit.NoMatch = true
			out = append(out, hit)
			continue
		}
		// Apply default min_confidence — same threshold the MCP search tool
		// uses by default. The first surviving hit is the snapshot top.
		topIdx := -1
		for i := range results {
			if results[i].Symbol.ExtractionConfidence >= defaultMinConfidence {
				topIdx = i
				break
			}
		}
		if topIdx < 0 {
			hit.NoMatch = true
			out = append(out, hit)
			continue
		}
		hit.TopHitKind = results[topIdx].Symbol.Kind
		hit.TopHitQN = results[topIdx].Symbol.QualifiedName
		out = append(out, hit)
	}
	return out
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
