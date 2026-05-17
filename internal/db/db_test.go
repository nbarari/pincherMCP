package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper: in-memory store
// ─────────────────────────────────────────────────────────────────────────────

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testProject(id string) Project {
	return Project{
		ID:        id,
		Path:      "/tmp/" + id,
		Name:      id,
		IndexedAt: time.Now().Truncate(time.Second),
	}
}

func testSymbol(id, name, kind, projectID, filePath string) Symbol {
	return Symbol{
		ID:            id,
		ProjectID:     projectID,
		FilePath:      filePath,
		Name:          name,
		QualifiedName: name,
		Kind:          kind,
		Language:      "Go",
		StartByte:     0,
		EndByte:       100,
		StartLine:     1,
		EndLine:       10,
		IsExported:    true,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DataDir
// ─────────────────────────────────────────────────────────────────────────────

func TestDataDir(t *testing.T) {
	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if dir == "" {
		t.Error("DataDir returned empty string")
	}
	// Should be a valid directory
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("DataDir %q does not exist: %v", dir, err)
	}
	if !info.IsDir() {
		t.Errorf("DataDir %q is not a directory", dir)
	}
}

// TestDataDir_PincherDataDirEnv covers the env-var override that lets a
// dev shell pin pincher to a separate data dir from the user's stable
// install (so dev migrations can't taint the stable DB). The env var
// is the full path — no `pincherMCP` suffix appended — to match how
// `--data-dir` already works.
func TestDataDir_PincherDataDirEnv(t *testing.T) {
	want := t.TempDir()
	t.Setenv("PINCHER_DATA_DIR", want)
	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if got != want {
		t.Errorf("DataDir() = %q, want %q", got, want)
	}
}

// TestDataDir_EmptyEnvIsIgnored covers the trim-then-empty case: an
// unset or whitespace-only PINCHER_DATA_DIR must NOT short-circuit to
// the empty string. Falls through to the platform default.
func TestDataDir_EmptyEnvIsIgnored(t *testing.T) {
	t.Setenv("PINCHER_DATA_DIR", "   ")
	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if got == "" || got == "   " {
		t.Errorf("DataDir() with whitespace env = %q, want platform default", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Open / migrate
// ─────────────────────────────────────────────────────────────────────────────

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if s.Path != filepath.Join(dir, "pincher.db") {
		t.Errorf("Path = %q, want %q", s.Path, filepath.Join(dir, "pincher.db"))
	}
}

// #830: Open must create a not-yet-existing data dir, the same way
// DataDir() does for the default + PINCHER_DATA_DIR paths. Before this,
// a `--data-dir` flag pointing at a missing dir reached Open uncreated
// and SQLite failed with a misleading SQLITE_CANTOPEN.
func TestOpen_CreatesMissingDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested", "datadir")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open on missing data dir should create it, got: %v", err)
	}
	defer s.Close()
	if _, statErr := os.Stat(filepath.Join(dir, "pincher.db")); statErr != nil {
		t.Errorf("pincher.db not created in the auto-created data dir: %v", statErr)
	}
}

// TestOpen_DSNPragmasApplied is the regression test for the DSN-syntax bug
// where modernc/sqlite-style `_pragma=name(value)` was written as the
// mattn-style `_name=value` and silently ignored. If WAL ever falls back
// to journal_mode=delete or busy_timeout to 0, queries serialize at the
// file level and contention cascades — this test fails loud.
func TestOpen_DSNPragmasApplied(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	checks := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "1"},   // NORMAL = 1
		{"foreign_keys", "1"},  // ON
		{"busy_timeout", "5000"},
		{"cache_size", "-65536"},
	}
	for _, c := range checks {
		var got string
		if err := s.db.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil {
			t.Errorf("PRAGMA %s: %v", c.pragma, err)
			continue
		}
		if !strings.EqualFold(got, c.want) {
			t.Errorf("PRAGMA %s = %q, want %q", c.pragma, got, c.want)
		}
	}
}

func TestOpen_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	s1.Close()

	// Second open should succeed (migrate is idempotent)
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	s2.Close()
}

// TestOpen_WALGuardrailEngaged pins the WAL bounding behavior added in PR #22.
// `journal_size_limit` is a silent no-op when journal_mode != WAL, so a
// regression that breaks WAL would also break this assertion at the same
// place — surfacing both failures at once. The 256 MiB cap is the documented
// upper bound; any future change to the literal needs to update both this
// test and the comment in db.go's Open() simultaneously.
func TestOpen_WALGuardrailEngaged(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const want = int64(256 * 1024 * 1024) // 256 MiB
	var got int64
	if err := s.db.QueryRow("PRAGMA journal_size_limit").Scan(&got); err != nil {
		t.Fatalf("PRAGMA journal_size_limit: %v", err)
	}
	if got != want {
		t.Errorf("journal_size_limit = %d, want %d (WAL guardrail not engaged)", got, want)
	}
}

// TestVacuum_Idempotent ensures Vacuum is safe to call repeatedly and on
// an empty DB — the `pincher vacuum` CLI is a deliberate user step but
// must not error if there's nothing to reclaim.
func TestVacuum_Idempotent(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Vacuum(); err != nil {
			t.Fatalf("Vacuum (call %d): %v", i+1, err)
		}
	}
}

// TestVacuum_ReclaimsAfterDelete is the core #732 guarantee: after a
// large DeleteProject the freed pages sit in the file until VACUUM
// rewrites it. Seed a project with enough symbols to grow the file,
// delete it, and assert Vacuum shrinks the file back down.
func TestVacuum_ReclaimsAfterDelete(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(testProject("bloat")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	syms := make([]Symbol, 0, 5000)
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("bloat::sym%d#Function", i)
		syms = append(syms, testSymbol(id, fmt.Sprintf("sym%d", i), "Function", "bloat", "internal/x/x.go"))
	}
	if err := s.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	if err := s.CheckpointTruncate(); err != nil {
		t.Fatalf("CheckpointTruncate: %v", err)
	}
	grown := dbFileSizeForTest(t, s.Path)

	if err := s.DeleteProject("bloat"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := s.Vacuum(); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	shrunk := dbFileSizeForTest(t, s.Path)

	if shrunk >= grown {
		t.Errorf("Vacuum did not reclaim space: grown=%d, after vacuum=%d", grown, shrunk)
	}
}

func dbFileSizeForTest(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat db file %q: %v", path, err)
	}
	return fi.Size()
}

// TestCheckpointTruncate_Idempotent ensures CheckpointTruncate is safe to
// call repeatedly — the indexer invokes it at every Index() tail, so a
// silent failure here would compound across re-indexes.
func TestCheckpointTruncate_Idempotent(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.CheckpointTruncate(); err != nil {
			t.Fatalf("CheckpointTruncate (call %d): %v", i+1, err)
		}
	}
	// Optimize is also called at the tail of Index(); same idempotency
	// expectation — once is fine, three is fine, no work between calls.
	for i := 0; i < 3; i++ {
		if err := s.Optimize(); err != nil {
			t.Fatalf("Optimize (call %d): %v", i+1, err)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Reader pool (#51) — single-writer preservation + RO enforcement
// ─────────────────────────────────────────────────────────────────────────────

// TestStore_WriterPoolStaysSingleWriter pins the SQLite single-writer
// invariant: the writer pool MUST have MaxOpenConns(1). A future
// "let's just bump this for parallelism" change would break correctness
// silently — this test fails loud.
func TestStore_WriterPoolStaysSingleWriter(t *testing.T) {
	s := newTestStore(t)
	got := s.db.Stats().MaxOpenConnections
	if got != 1 {
		t.Errorf("writer pool MaxOpenConnections = %d, want 1 (single-writer invariant)", got)
	}
}

// TestStore_ReaderPoolHasMultipleConns confirms the reader pool is
// actually multi-conn (the whole point of the split). If this returned
// 1, we'd just be paying for a second pool with no parallelism benefit.
func TestStore_ReaderPoolHasMultipleConns(t *testing.T) {
	s := newTestStore(t)
	got := s.ro.Stats().MaxOpenConnections
	if got <= 1 {
		t.Errorf("reader pool MaxOpenConnections = %d, want >1 (concurrent-read parallelism)", got)
	}
}

// TestStore_ReaderPoolRejectsWrites is the security gate. Even if a
// future routing bug sends a write through s.ro, SQLite enforces RO
// at the file level and refuses with "attempt to write a readonly
// database". This catches both the routing bug AND a hypothetical
// SQLite version that loosens the check.
func TestStore_ReaderPoolRejectsWrites(t *testing.T) {
	s := newTestStore(t)
	_, err := s.ro.Exec(`INSERT INTO projects(id, path, name, indexed_at) VALUES ('x','/x','x',0)`)
	if err == nil {
		t.Fatal("expected reader pool to refuse INSERT, got nil error")
	}
	// SQLite's exact error wording: "attempt to write a readonly database"
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Errorf("expected readonly-database error, got: %v", err)
	}
}

// TestStore_ReadsViaReaderPool_WhileWriteInProgress is the impact gate.
// While a heavy write transaction holds the writer connection, a read
// through s.ro MUST still complete promptly. Pre-fix (single pool with
// MaxOpenConns=1), the read would queue behind the writer.
func TestStore_ReadsViaReaderPool_WhileWriteInProgress(t *testing.T) {
	s := newTestStore(t)
	// Seed a project so the read has something to find.
	if err := s.UpsertProject(testProject("read-during-write")); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Hold a write transaction in a background goroutine.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO projects(id,path,name,indexed_at) VALUES('blocking','/b','b',0)`); err != nil {
		t.Fatalf("write inside tx: %v", err)
	}
	// Note: tx is HELD — we don't commit. The writer pool is "occupied".

	// Concurrent read MUST succeed via reader pool.
	done := make(chan error, 1)
	go func() {
		_, err := s.GetProject("read-during-write")
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("concurrent read failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("concurrent read blocked > 2s while writer tx held — reader pool not relieving contention")
	}
}

// TestStore_CloseClosesBothPools verifies the lifecycle: Close() must
// release BOTH pools, not just the writer. A leaked reader-pool
// connection would prevent the SQLite file from being released and
// cause "database is locked" on subsequent open attempts.
func TestStore_CloseClosesBothPools(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := s.db.Stats().OpenConnections; got != 0 {
		t.Errorf("writer pool OpenConnections after Close = %d, want 0", got)
	}
	if got := s.ro.Stats().OpenConnections; got != 0 {
		t.Errorf("reader pool OpenConnections after Close = %d, want 0", got)
	}
}

// TestStore_ROFallsBackToWriter pins the defensive RO() fallback: if
// the reader pool failed to open for any reason, RO() returns the
// writer pool so callers don't crash. Functionally correct (slower)
// instead of broken.
func TestStore_ROFallsBackToWriter(t *testing.T) {
	s := newTestStore(t)
	// Force the fallback path by clearing s.ro.
	s.ro.Close()
	s.ro = nil

	got := s.RO()
	if got != s.db {
		t.Error("RO() with nil ro pool should fall back to writer pool")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Reader pool tunable + classification gate (#51 part 2)
// ─────────────────────────────────────────────────────────────────────────────

func TestStore_OpenWithReaders_RespectsCustomSize(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenWithReaders(dir, 8)
	if err != nil {
		t.Fatalf("OpenWithReaders: %v", err)
	}
	defer s.Close()
	if got := s.ro.Stats().MaxOpenConnections; got != 8 {
		t.Errorf("MaxOpenConnections = %d, want 8", got)
	}
}

func TestStore_OpenWithReaders_ClampsAboveMax(t *testing.T) {
	// Pathological caller asks for 1000 — clamps to MaxReaderPoolSize.
	dir := t.TempDir()
	s, err := OpenWithReaders(dir, 1000)
	if err != nil {
		t.Fatalf("OpenWithReaders: %v", err)
	}
	defer s.Close()
	if got := s.ro.Stats().MaxOpenConnections; got != MaxReaderPoolSize {
		t.Errorf("MaxOpenConnections = %d, want clamped to %d", got, MaxReaderPoolSize)
	}
}

func TestStore_OpenWithReaders_ClampsBelowMin(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenWithReaders(dir, -5)
	if err != nil {
		t.Fatalf("OpenWithReaders: %v", err)
	}
	defer s.Close()
	// Negative falls through SetReaderPoolSize's "<= 0 → default" branch.
	// Documented behaviour: pass 0 or negative to get the default.
	if got := s.ro.Stats().MaxOpenConnections; got != DefaultReaderPoolSize {
		t.Errorf("MaxOpenConnections = %d, want %d (default)", got, DefaultReaderPoolSize)
	}
}

func TestStore_SetReaderPoolSize_RuntimeAdjust(t *testing.T) {
	s := newTestStore(t)
	if got := s.ro.Stats().MaxOpenConnections; got != DefaultReaderPoolSize {
		t.Fatalf("initial pool size = %d, want %d", got, DefaultReaderPoolSize)
	}
	if err := s.SetReaderPoolSize(16); err != nil {
		t.Fatalf("SetReaderPoolSize: %v", err)
	}
	if got := s.ro.Stats().MaxOpenConnections; got != 16 {
		t.Errorf("after SetReaderPoolSize(16): MaxOpenConnections = %d, want 16", got)
	}
}

// readerRoutedStoreMethods + writerRoutedStoreMethods enumerate every
// exported *Store method by intent. The reflection-enumeration test
// below verifies every method is in EXACTLY one of these sets — adding
// a new method without classifying it fails the test loud, forcing the
// PR reviewer to think about which pool it should use.
//
// SECURITY-RELEVANT: misclassifying a writer as a reader would either
// fail at runtime ("attempt to write a readonly database") OR succeed
// against a future relaxation of the RO mode. Misclassifying a reader
// as a writer wastes a writer-pool slot but isn't unsafe. Erring
// toward writer-routing is safe; toward reader-routing requires care.
var readerRoutedStoreMethods = map[string]bool{
	// Pure SELECTs (use s.ro).
	"GetSymbol":               true,
	"GetSymbolsByIDs":         true,
	"GetSymbolsByName":        true,
	"GetSymbolsByQN":          true,
	"GetSymbolsForFile":       true,
	"GetHotspots":             true,
	"GetDeadCode":             true,
	"SearchSymbols":           true,
	"SearchSymbolsByCorpus":   true,
	"EdgesFrom":               true,
	"EdgesTo":                 true,
	"GraphStats":              true,
	"AvgConfidenceByKind":     true,
	"GetProject":              true,
	"ListProjects":            true,
	"ProjectsContainingPath":  true,
	"GetADR":                  true,
	"ListADRs":                true,
	"GetFileHash":             true,
	"ListFilesForProject":     true,
	"ListSymbolFilePaths":     true,
	"SymbolCountsByFile":      true, // #1231 parity check (pure SELECT, reader pool)
	"FilesWithEdgesToFile":    true,
	"LoadPendingEdges":        true,
	"LoadStructFields":        true,
	"LoadInterfaceMethods":    true,
	"ListExtractionFailures":         true,
	"ListRecentExtractionFailuresAcrossProjects":  true, // #1205 doctor cross-project query
	"CountRecentExtractionFailuresAcrossProjects": true, // #1205 doctor truncation-count
	"EstimateProjectBytes":                        true, // #1220 doctor per-project byte estimate (pure SELECT)
	"ExtractionFailureCountsByReason":             true,
	"ListSlowQueries":         true,
	// HealthCheck is pure SELECT — previously misclassified under writer
	// "for transactional consistency" but there is no write path. Moved
	// to reader so health probes don't block on indexer write contention.
	"HealthCheck":             true,
	"GetAllTimeSavings":         true,
	"GetAllTimeCallsByLanguage": true,
	"GetAllTimeQueryMetrics":    true,
	"GetSessions":               true,
	"GetSessionByID":            true,
	"GetLatestHTTPSession":      true,
	"ResolveStaleID":          true,
	"TraceViaCTE":             true,
	"TraceViaCTEScoped":       true,
	"TraceViaClosure":         true, // #652 phase 1
	"ClosureRowCount":         true, // #652 phase 1
	"GetSymbolScoped":         true,
	// v0.36 hook telemetry helpers (#626).
	"IsFileIndexed":           true,
	"CountSymbolsInFile":      true,
	"LargestSymbolInFile":     true,
	"HookConversionRate7d":    true,
	"HookOverrideRate7d":      true,
	"HookCountsByTool7d":      true,
	// Accessors that return the underlying *sql.DB. RO() returns the
	// reader pool by definition; DB() returns the writer (semantic
	// belongs to writer-routed since callers may write through it).
	"RO": true,

	// #635 v0.64: session_tool_calls per-call detail (dashboard
	// triangulating panels). RecentToolCallsForSession is pure
	// SELECT — reader-routed.
	"RecentToolCallsForSession": true,
	// #635 v0.67: per-tool aggregate over the trailing window. Pure
	// SELECT with GROUP BY — reader-routed.
	"ToolCallStatsByTool": true,
	// #635 v0.67 panel 2: per-tier aggregate, same shape as above.
	"ToolCallStatsByTier": true,
	// #635 v0.67 panel 3: per-tool payload-size distribution (min/avg/max
	// response_bytes) — outlier finder. Reader-routed.
	"ToolCallPayloadSizeByTool": true,
	// #1263 v0.68 bench persistence: list runs + per-run results.
	"ListBenchRuns":   true,
	"GetBenchResults": true,
}

var writerRoutedStoreMethods = map[string]bool{
	// Mutations (use s.db).
	"UpsertProject":            true,
	"UpsertProjectMeta":        true,
	"UpdateProjectCounts":      true,
	"DeleteProject":            true,
	"DeleteEmptyProjects":      true,
	"BulkUpsertSymbols":        true,
	"DeleteSymbolsForFile":     true,
	"BulkUpsertEdges":            true,
	"DeleteEdgesByKindAndSource": true,
	"MaybeFireCelebration":       true,
	"ReplacePendingEdgesForFile":      true,
	"DeletePendingEdgesForFile":       true,
	"ReplaceStructFieldsForFile":      true,
	"ReplaceInterfaceMethodsForFile":  true,
	"RecordSymbolMove":         true,
	"DetectAndRecordMoves":     true,
	"SetFileHash":              true,
	"DeleteFileHash":           true,
	"SetADR":                   true,
	"DeleteADR":                true,
	"RecordSession":            true,
	"RecordSessionWithMetrics": true,
	"ResetSessions":            true,
	"RecordExtractionFailure":  true,
	"ClearExtractionFailures":  true,
	"RecordSlowQuery":          true,
	// v0.36 hook telemetry writers (#626).
	"LogHookInvocation":               true,
	"ResolveHookInvocationsForSession": true,
	// Mixed read+write — kept on writer for transactional consistency.
	"BuildClosure": true, // #652 phase 1 — DELETE + INSERT in a tx
	// Pragmas / lifecycle (writer-pool by definition).
	"Optimize":           true,
	"CheckpointTruncate": true,
	"RebuildFTS":         true,
	"Vacuum":             true,
	"Close":              true,
	"DB":                 true,
	// Configuration (operates on the reader pool but is itself a write
	// to the *Store — classified writer).
	"SetReaderPoolSize": true,

	// #635 v0.64: bulk-insert per-call events into session_tool_calls.
	// Writer-routed (mutates table).
	"RecordToolCalls": true,

	// #1263 v0.68 bench persistence: writes one bench_runs row + N
	// bench_results rows in one transaction. Writer-routed.
	"RecordBenchRun": true,
}

// TestStore_AllExportedMethodsClassified is the routing classification
// gate. Every exported method on *Store MUST appear in exactly one of
// readerRoutedStoreMethods or writerRoutedStoreMethods. New methods
// added without classification fail this test at the next CI run.
//
// Why this gate matters: without it, a new method authored under
// time pressure could silently land using the WRITER pool for what
// should be a reader-routed operation (or vice versa). The first form
// is a perf regression that's hard to spot; the second is a runtime
// failure waiting for the right deployment.
func TestStore_AllExportedMethodsClassified(t *testing.T) {
	storeType := reflect.TypeOf(&Store{})
	classified := 0
	for i := 0; i < storeType.NumMethod(); i++ {
		name := storeType.Method(i).Name
		// Reflection's NumMethod includes only exported methods on
		// pointer receivers, which is what we want.
		inReader := readerRoutedStoreMethods[name]
		inWriter := writerRoutedStoreMethods[name]

		if !inReader && !inWriter {
			t.Errorf("exported *Store.%s is unclassified — add it to readerRoutedStoreMethods or writerRoutedStoreMethods in db_test.go (#51 part 2)", name)
			continue
		}
		if inReader && inWriter {
			t.Errorf("exported *Store.%s appears in BOTH allowlists — pick one", name)
			continue
		}
		classified++
	}
	if classified == 0 {
		t.Fatal("reflection found zero exported methods on *Store; allowlists may be stale")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// extraction_failures (#42 part 1)
// ─────────────────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────────────────
// slow_queries (#42 part 2)
// ─────────────────────────────────────────────────────────────────────────────

func TestSlowQuery_RecordAndList(t *testing.T) {
	s := newTestStore(t)

	// Cross-project tool (no project_id) and project-scoped tool both work.
	cases := []struct {
		tool, projectID, args string
		duration              int64
	}{
		{"search", "p1", `{"query":"open"}`, 220},
		{"list", "", `{}`, 80},
		{"trace", "p2", `{"name":"main"}`, 1500},
	}
	for _, c := range cases {
		if err := s.RecordSlowQuery(c.tool, c.projectID, c.duration, c.args); err != nil {
			t.Fatalf("Record (%s): %v", c.tool, err)
		}
	}

	got, err := s.ListSlowQueries(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(cases) {
		t.Fatalf("got %d rows, want %d", len(got), len(cases))
	}
	// Default order is occurred_at DESC; most-recent insert first.
	if got[0].Tool != "trace" {
		t.Errorf("most-recent row = %q, want trace", got[0].Tool)
	}
	// project_id MUST round-trip: empty string in, empty string out (NOT NULL).
	for _, sq := range got {
		if sq.Tool == "list" && sq.ProjectID != "" {
			t.Errorf("cross-project tool's project_id should be empty, got %q", sq.ProjectID)
		}
	}
}

func TestSlowQuery_LimitRespected(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		if err := s.RecordSlowQuery("search", "p1", int64(100+i), "{}"); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	got, err := s.ListSlowQueries(2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("limit=2 returned %d rows", len(got))
	}
}

func TestExtractionFailure_RecordAndList(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(testProject("p1")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	// Record three failures spanning all four reason kinds (one is duplicated
	// to verify the UNIQUE constraint upserts rather than inserting).
	cases := []struct{ file, lang, reason, details string }{
		{"src/foo.go", "Go", "extractor_panicked", "panic: runtime error"},
		{"compose.yaml", "YAML", "byte_range_negative", "end_byte=10 <= start_byte=10"},
		{"src/bar.py", "Python", "parse_error", "expected ':' at line 5"},
	}
	for _, c := range cases {
		if err := s.RecordExtractionFailure("p1", c.file, c.lang, c.reason, c.details); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	got, err := s.ListExtractionFailures("p1", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(cases) {
		t.Fatalf("got %d rows, want %d", len(got), len(cases))
	}
	for _, f := range got {
		if f.FirstSeenAt.IsZero() || f.LastSeenAt.IsZero() {
			t.Errorf("zero timestamps on row %+v", f)
		}
	}
}

// TestExtractionFailure_RepeatedRecordUpdatesLastSeen pins the UNIQUE-conflict
// upsert behaviour. A file that fails repeatedly across re-indexes MUST NOT
// multiply rows; instead last_seen_at advances. Without this, every Watch
// tick on a broken file would add a new row.
func TestExtractionFailure_RepeatedRecordUpdatesLastSeen(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(testProject("p1")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := s.RecordExtractionFailure("p1", "broken.yaml", "YAML", "parse_error", "yaml: mapping values are not allowed"); err != nil {
			t.Fatalf("Record (call %d): %v", i, err)
		}
	}
	got, err := s.ListExtractionFailures("p1", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected exactly 1 row after 5 records of the same (file, reason); got %d", len(got))
	}
}

func TestExtractionFailure_DetailsTruncated(t *testing.T) {
	// Pathological details strings (a 50KB error message) MUST NOT bloat
	// the table; the implementation truncates at extractionFailureDetailsCap.
	s := newTestStore(t)
	if err := s.UpsertProject(testProject("p1")); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	huge := strings.Repeat("x", 50*1024)
	if err := s.RecordExtractionFailure("p1", "huge.go", "Go", "parse_error", huge); err != nil {
		t.Fatalf("Record: %v", err)
	}
	got, err := s.ListExtractionFailures("p1", 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("List: err=%v len=%d", err, len(got))
	}
	if len(got[0].Details) > extractionFailureDetailsCap+50 {
		t.Errorf("details persisted at %d chars, want capped near %d", len(got[0].Details), extractionFailureDetailsCap)
	}
	if !strings.HasSuffix(got[0].Details, "[truncated]") {
		t.Errorf("expected truncation marker, got: %q", got[0].Details[len(got[0].Details)-30:])
	}
}

// TestExtractionFailure_ProjectScopeIsolated guards against cross-project leak.
// A failure recorded under project A MUST NOT appear in project B's listing.
// SearchSymbols / Cypher scoping bugs in #41 set the precedent for this gate.
func TestExtractionFailure_ProjectScopeIsolated(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertProject(testProject("a")); err != nil {
		t.Fatalf("UpsertProject a: %v", err)
	}
	if err := s.UpsertProject(testProject("b")); err != nil {
		t.Fatalf("UpsertProject b: %v", err)
	}
	if err := s.RecordExtractionFailure("a", "x.go", "Go", "parse_error", "in A only"); err != nil {
		t.Fatalf("Record A: %v", err)
	}

	bRows, err := s.ListExtractionFailures("b", 0)
	if err != nil {
		t.Fatalf("List b: %v", err)
	}
	if len(bRows) != 0 {
		t.Errorf("project B sees %d rows from project A — scope leak", len(bRows))
	}
}

func TestMigrate_UpgradeFromV1(t *testing.T) {
	// Simulate a pre-versioning database that is at schema v1 (baseline only,
	// no extraction_confidence column, no symbol_moves table).
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pincher.db")

	// Build a v1-era database using a raw connection — apply the baseline schema
	// then pin schema_version to 1 (before any migrations ran).
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec(schema); err != nil {
		raw.Close()
		t.Fatalf("baseline schema: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		raw.Close()
		t.Fatalf("create schema_version: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version(version) VALUES(1)`); err != nil {
		raw.Close()
		t.Fatalf("seed schema_version: %v", err)
	}
	raw.Close()

	// Now open via the normal path — migrate() must detect v1 and apply
	// all pending migrations (v1→v2, v2→v3, …).
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after v1 seed: %v", err)
	}
	defer s.Close()

	// Verify the final version equals 1 + len(schemaMigrations).
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	want := 1 + len(schemaMigrations)
	if version != want {
		t.Errorf("version = %d, want %d", version, want)
	}

	// Seed a project so the symbol/symbol_moves inserts below satisfy FK
	// constraints. Pre-fix, foreign_keys=ON was silently ignored by the
	// (mis-formed) DSN so these inserts succeeded against an empty projects
	// table; now that the DSN is correct, FK is properly enforced.
	if _, err := s.db.Exec(`INSERT INTO projects(id,path,name,indexed_at) VALUES('p','/tmp/p','p',0)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Spot-check: extraction_confidence column must exist (migration 0).
	if _, err := s.db.Exec(`INSERT INTO symbols(id,project_id,file_path,name,qualified_name,kind,language,start_byte,end_byte,start_line,end_line,extraction_confidence) VALUES('x','p','f.go','X','X','func','Go',0,1,1,1,0.85)`); err != nil {
		t.Errorf("extraction_confidence column missing after migration: %v", err)
	}

	// Spot-check: symbol_moves table must exist (migration 1).
	if _, err := s.db.Exec(`INSERT INTO symbol_moves(old_id,new_id,project_id,moved_at) VALUES('old','new','p',0)`); err != nil {
		t.Errorf("symbol_moves table missing after migration: %v", err)
	}

	// Spot-check: the v5→v6 generated `symbol_id` column must exist and
	// mirror `id` so FTS5 content lookups (issue #19) can succeed.
	var symID string
	if err := s.db.QueryRow(`SELECT symbol_id FROM symbols WHERE id='x'`).Scan(&symID); err != nil {
		t.Errorf("symbol_id column missing after migration: %v", err)
	} else if symID != "x" {
		t.Errorf("symbol_id = %q, want %q (must mirror id)", symID, "x")
	}
}

// TestFTS_ContentLookup is the regression test for issue #19: the symbols_fts
// vtab declares first column `symbol_id` but the underlying `symbols` table
// column is `id`. FTS5 ops that need a content lookup (integrity-check,
// optimize, snippet/highlight, bare reads) issue `SELECT symbol_id, ...
// FROM symbols WHERE rowid = ?` and fail without the v5→v6 generated column.
//
// SearchSymbols' query plan hand-joins on rowid and reads from the source
// table directly so it never triggers content lookup — which is why the bug
// stayed latent. integrity-check exercises the broken path explicitly.
func TestFTS_ContentLookup(t *testing.T) {
	s := newTestStore(t)

	// Insert a project + symbol so FTS has content to verify.
	if _, err := s.db.Exec(`INSERT INTO projects(id,path,name,indexed_at) VALUES('p','/tmp/p','p',0)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO symbols(id,project_id,file_path,name,qualified_name,kind,language,start_byte,end_byte,start_line,end_line) VALUES('s1','p','f.go','Foo','pkg.Foo','Function','Go',0,1,1,1)`); err != nil {
		t.Fatalf("seed symbol: %v", err)
	}

	// integrity-check forces FTS5 to read original content from `symbols`
	// using the FTS column names. Pre-fix this errored with
	// `no such column: T.symbol_id`. Post-fix it returns 'ok'.
	// (Now run against `symbols_code_fts`; the legacy `symbols_fts` was
	// removed in #106's v12 migration.)
	if _, err := s.db.Exec(`INSERT INTO symbols_code_fts(symbols_code_fts) VALUES('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity-check failed (issue #19 regression): %v", err)
	}

	// COUNT(*) on the vtab also routes through content lookup on some
	// FTS5 paths — verify it succeeds.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM symbols_code_fts`).Scan(&n); err != nil {
		t.Fatalf("COUNT(*) FROM symbols_code_fts: %v", err)
	}
	if n != 1 {
		t.Errorf("symbols_fts row count = %d, want 1", n)
	}
}

func TestMigrate_VersionTracked(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	// Version must be 1 (baseline) + number of migrations applied.
	want := 1 + len(schemaMigrations)
	if version != want {
		t.Errorf("schema_version = %d, want %d", version, want)
	}
}

// TestMigrate_RejectsNewerDB verifies that an older binary refuses to open
// a database whose schema version is higher than any migration it knows
// about. Without this guard, the binary would silently proceed and could
// read/write rows using its older schema understanding.
func TestMigrate_RejectsNewerDB(t *testing.T) {
	dir := t.TempDir()

	// First Open: this binary creates the DB and migrates it to its current
	// max version.
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	// Simulate a future pincher version by bumping schema_version past
	// everything this binary knows about.
	futureVersion := 1 + len(schemaMigrations) + 1
	if _, err := s1.db.Exec(`UPDATE schema_version SET version = ?`, futureVersion); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	s1.Close()

	// Re-opening with the same binary must fail with a clear message. We
	// don't check the exact wording, just that the error mentions the
	// version mismatch so users know what to do.
	_, err = Open(dir)
	if err == nil {
		t.Fatal("Open should have refused a database at a newer schema version")
	}
	msg := err.Error()
	if !strings.Contains(msg, "newer than this binary") && !strings.Contains(msg, "upgrade pincher") {
		t.Errorf("error should point at the version mismatch, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Project CRUD
// ─────────────────────────────────────────────────────────────────────────────

func TestUpsertProject(t *testing.T) {
	s := newTestStore(t)
	p := testProject("proj1")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	got, err := s.GetProject("proj1")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got == nil {
		t.Fatal("GetProject returned nil")
	}
	if got.Name != "proj1" {
		t.Errorf("Name = %q, want proj1", got.Name)
	}
}

// TestProjectsContainingPath_NestedHit covers the canonical case for
// #235: a target path that's a strict descendant of a registered
// project should turn up as containing.
func TestProjectsContainingPath_NestedHit(t *testing.T) {
	s := newTestStore(t)
	parent := testProject("parent")
	parent.Path = filepath.Join(t.TempDir(), "thinksmart")
	if err := os.MkdirAll(parent.Path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := s.UpsertProject(parent); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	target := filepath.Join(parent.Path, "kernel")
	got, err := s.ProjectsContainingPath(target)
	if err != nil {
		t.Fatalf("ProjectsContainingPath: %v", err)
	}
	if len(got) != 1 || got[0].ID != "parent" {
		t.Errorf("got %#v, want one project [parent]", got)
	}
}

// TestProjectsContainingPath_SiblingMiss covers a sibling directory —
// must NOT match because Rel() would return "../sibling".
func TestProjectsContainingPath_SiblingMiss(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	parent := testProject("parent")
	parent.Path = filepath.Join(root, "alpha")
	os.MkdirAll(parent.Path, 0o755)
	s.UpsertProject(parent)

	target := filepath.Join(root, "beta") // sibling, not nested
	got, err := s.ProjectsContainingPath(target)
	if err != nil {
		t.Fatalf("ProjectsContainingPath: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no parents for sibling path; got %#v", got)
	}
}

// TestProjectsContainingPath_SamePathMiss covers the equality case —
// indexing a project's own root should not warn (Rel returns ".").
func TestProjectsContainingPath_SamePathMiss(t *testing.T) {
	s := newTestStore(t)
	parent := testProject("parent")
	parent.Path = filepath.Join(t.TempDir(), "self")
	os.MkdirAll(parent.Path, 0o755)
	s.UpsertProject(parent)

	got, err := s.ProjectsContainingPath(parent.Path)
	if err != nil {
		t.Fatalf("ProjectsContainingPath: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no parents for self-path; got %#v", got)
	}
}

// TestProjectsContainingPath_DeepNesting covers a multi-level descent
// (parent/a/b/c) and the case where a child project is also indexed —
// then both parent and intermediate child show up as ancestors of the
// deepest target.
func TestProjectsContainingPath_DeepNesting(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	parent := testProject("parent")
	parent.Path = filepath.Join(root, "monorepo")
	os.MkdirAll(parent.Path, 0o755)
	s.UpsertProject(parent)

	mid := testProject("mid")
	mid.Path = filepath.Join(parent.Path, "services")
	os.MkdirAll(mid.Path, 0o755)
	s.UpsertProject(mid)

	target := filepath.Join(mid.Path, "auth")
	got, err := s.ProjectsContainingPath(target)
	if err != nil {
		t.Fatalf("ProjectsContainingPath: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 ancestors (parent + mid), got %d: %#v", len(got), got)
	}
}

func TestUpsertProject_Update(t *testing.T) {
	s := newTestStore(t)
	p := testProject("proj1")
	s.UpsertProject(p)

	p.FileCount = 42
	p.SymCount = 100
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject update: %v", err)
	}

	got, _ := s.GetProject("proj1")
	if got.FileCount != 42 {
		t.Errorf("FileCount = %d, want 42", got.FileCount)
	}
}

// #724: a stale process (older binary → older schema) Watch()ing a
// shared DB must not stomp schema_version_at_index / binary_version
// back to its values. The monotonic guard in UpsertProject's ON
// CONFLICT clause blocks the downgrade.
func TestUpsertProject_MonotonicMetadataGuard(t *testing.T) {
	s := newTestStore(t)
	p := testProject("guard-proj")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	// Simulate a *newer* binary having indexed this project: bump
	// schema_version_at_index above anything UpsertProject would pass,
	// and set a distinctive binary_version.
	if _, err := s.db.Exec(
		`UPDATE projects SET schema_version_at_index=999, binary_version='9.9.9-newer' WHERE id=?`,
		"guard-proj"); err != nil {
		t.Fatalf("seed newer row: %v", err)
	}
	// Now a stale process re-upserts. UpsertProject passes the *current*
	// (lower) schema. The guard must keep the newer metadata.
	p.BinaryVersion = "0.1.0-ancient-orphan"
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject (stale writer): %v", err)
	}
	got, _ := s.GetProject("guard-proj")
	if got.SchemaVersionAtIndex == nil || *got.SchemaVersionAtIndex != 999 {
		t.Errorf("schema_version_at_index = %v, want 999 (stale writer must not downgrade it)", got.SchemaVersionAtIndex)
	}
	if got.BinaryVersion != "9.9.9-newer" {
		t.Errorf("binary_version = %q, want %q (stale writer must not stomp it)", got.BinaryVersion, "9.9.9-newer")
	}

	// Reverse direction: a project stuck at a *low* schema must still be
	// upgraded by a current-binary re-index.
	if _, err := s.db.Exec(
		`UPDATE projects SET schema_version_at_index=1, binary_version='0.1.0-stale' WHERE id=?`,
		"guard-proj"); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	p.BinaryVersion = "0.55.0-current"
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject (current writer): %v", err)
	}
	got, _ = s.GetProject("guard-proj")
	if got.SchemaVersionAtIndex == nil || *got.SchemaVersionAtIndex <= 1 {
		t.Errorf("schema_version_at_index = %v, want > 1 (current writer must upgrade it)", got.SchemaVersionAtIndex)
	}
	if got.BinaryVersion != "0.55.0-current" {
		t.Errorf("binary_version = %q, want %q (current writer must update it)", got.BinaryVersion, "0.55.0-current")
	}
}

func TestUpdateProjectCounts(t *testing.T) {
	s := newTestStore(t)
	p := testProject("counts-proj")
	p.FileCount = 5
	p.SymCount = 50
	p.EdgeCount = 25
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	// UpdateProjectCounts overwrites only the cached count columns,
	// leaving path/name/indexed_at intact.
	if err := s.UpdateProjectCounts("counts-proj", 12, 200, 80); err != nil {
		t.Fatalf("UpdateProjectCounts: %v", err)
	}

	got, _ := s.GetProject("counts-proj")
	if got == nil {
		t.Fatal("project disappeared after UpdateProjectCounts")
	}
	if got.FileCount != 12 || got.SymCount != 200 || got.EdgeCount != 80 {
		t.Errorf("counts after update = (%d,%d,%d), want (12,200,80)",
			got.FileCount, got.SymCount, got.EdgeCount)
	}
	// Other fields untouched
	if got.Path != p.Path || got.Name != p.Name {
		t.Errorf("non-count fields changed: path=%q name=%q", got.Path, got.Name)
	}
}

func TestUpdateProjectCounts_NonExistent_NoError(t *testing.T) {
	s := newTestStore(t)
	// Updating a project that doesn't exist is a silent no-op (UPDATE on
	// zero rows is not an error in SQLite). The caller doesn't need to
	// gate on existence — Index() upserts the project before any flush.
	if err := s.UpdateProjectCounts("never-existed", 1, 1, 1); err != nil {
		t.Errorf("UpdateProjectCounts on missing project should not error, got %v", err)
	}
}

func TestGetProject_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetProject("nonexistent")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent project")
	}
}

func TestListProjects(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("a"))
	s.UpsertProject(testProject("b"))
	s.UpsertProject(testProject("c"))

	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 3 {
		t.Errorf("expected 3 projects, got %d", len(projects))
	}
}

func TestDeleteProject(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	sym := testSymbol("s1", "Foo", "Function", "p1", "foo.go")
	s.BulkUpsertSymbols([]Symbol{sym})

	if err := s.DeleteProject("p1"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	got, _ := s.GetProject("p1")
	if got != nil {
		t.Error("project should be deleted")
	}
	// Symbols should also be deleted
	fetched, _ := s.GetSymbol("s1")
	if fetched != nil {
		t.Error("symbols should be deleted with project")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Symbol CRUD
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkUpsertSymbols(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("proj1"))

	syms := []Symbol{
		testSymbol("s1", "Foo", "Function", "proj1", "foo.go"),
		testSymbol("s2", "Bar", "Function", "proj1", "foo.go"),
		testSymbol("s3", "Baz", "Class", "proj1", "baz.go"),
	}
	if err := s.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	got, err := s.GetSymbol("s1")
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if got == nil {
		t.Fatal("symbol not found after upsert")
	}
	if got.Name != "Foo" {
		t.Errorf("Name = %q, want Foo", got.Name)
	}
}

func TestGetSymbol_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetSymbol("nonexistent")
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent symbol")
	}
}

func TestGetSymbolsByName(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("s1", "Process", "Function", "p1", "a.go"),
		testSymbol("s2", "Process", "Method", "p1", "b.go"),
		testSymbol("s3", "Other", "Function", "p1", "c.go"),
	})

	results, err := s.GetSymbolsByName("p1", "Process", 10)
	if err != nil {
		t.Fatalf("GetSymbolsByName: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
}

func TestGetSymbolsForFile(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("s1", "A", "Function", "p1", "target.go"),
		testSymbol("s2", "B", "Function", "p1", "target.go"),
		testSymbol("s3", "C", "Function", "p1", "other.go"),
	})

	results, err := s.GetSymbolsForFile("p1", "target.go")
	if err != nil {
		t.Fatalf("GetSymbolsForFile: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 symbols in target.go, got %d", len(results))
	}
}

func TestDeleteSymbolsForFile(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("s1", "A", "Function", "p1", "target.go"),
		testSymbol("s2", "B", "Function", "p1", "other.go"),
	})
	s.BulkUpsertEdges([]Edge{{ProjectID: "p1", FromID: "s1", ToID: "s2", Kind: "CALLS", Confidence: 1.0}})

	if err := s.DeleteSymbolsForFile("p1", "target.go"); err != nil {
		t.Fatalf("DeleteSymbolsForFile: %v", err)
	}

	got, _ := s.GetSymbol("s1")
	if got != nil {
		t.Error("s1 should be deleted")
	}
	got, _ = s.GetSymbol("s2")
	if got == nil {
		t.Error("s2 in other.go should survive")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// FTS5 search
// ─────────────────────────────────────────────────────────────────────────────

func TestSearchSymbols(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	syms := []Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "auth.go", Name: "AuthService",
			QualifiedName: "auth.AuthService", Kind: "Class", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 20},
		{ID: "s2", ProjectID: "p1", FilePath: "user.go", Name: "UserService",
			QualifiedName: "user.UserService", Kind: "Class", Language: "Go",
			StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 20},
		{ID: "s3", ProjectID: "p1", FilePath: "auth.go", Name: "Login",
			QualifiedName: "auth.Login", Kind: "Function", Language: "Go",
			StartByte: 200, EndByte: 300, StartLine: 30, EndLine: 45},
	}
	s.BulkUpsertSymbols(syms)

	results, err := s.SearchSymbols("p1", "auth*", "", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected results for 'auth*'")
	}
}

func TestSearchSymbols_KindFilter(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "f1", ProjectID: "p1", FilePath: "a.go", Name: "processOrder",
			QualifiedName: "pkg.processOrder", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
		{ID: "c1", ProjectID: "p1", FilePath: "a.go", Name: "processOrder",
			QualifiedName: "pkg.OrderProcessor", Kind: "Class", Language: "Go",
			StartByte: 60, EndByte: 200, StartLine: 10, EndLine: 30},
	})

	results, err := s.SearchSymbols("p1", "process*", "Function", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	for _, r := range results {
		if r.Symbol.Kind != "Function" {
			t.Errorf("kind filter failed: got %q", r.Symbol.Kind)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Edge operations
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkUpsertEdges(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("a", "A", "Function", "p1", "a.go"),
		testSymbol("b", "B", "Function", "p1", "b.go"),
	})

	edges := []Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
	}
	if err := s.BulkUpsertEdges(edges); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}

	from, err := s.EdgesFrom("a", nil)
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(from) != 1 {
		t.Errorf("expected 1 edge from a, got %d", len(from))
	}

	to, err := s.EdgesTo("b", nil)
	if err != nil {
		t.Fatalf("EdgesTo: %v", err)
	}
	if len(to) != 1 {
		t.Errorf("expected 1 edge to b, got %d", len(to))
	}
}

func TestEdgesFrom_KindFilter(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("a", "A", "Function", "p1", "a.go"),
		testSymbol("b", "B", "Function", "p1", "b.go"),
		testSymbol("c", "C", "Class", "p1", "c.go"),
	})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "a", ToID: "c", Kind: "IMPORTS", Confidence: 1.0},
	})

	calls, _ := s.EdgesFrom("a", []string{"CALLS"})
	if len(calls) != 1 {
		t.Errorf("expected 1 CALLS edge, got %d", len(calls))
	}

	all, _ := s.EdgesFrom("a", nil)
	if len(all) != 2 {
		t.Errorf("expected 2 total edges, got %d", len(all))
	}
}

func TestBulkUpsertEdges_Idempotent(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("a", "A", "Function", "p1", "a.go"),
		testSymbol("b", "B", "Function", "p1", "b.go"),
	})

	edge := []Edge{{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0}}
	s.BulkUpsertEdges(edge)
	s.BulkUpsertEdges(edge) // second insert should be ignored

	from, _ := s.EdgesFrom("a", nil)
	if len(from) != 1 {
		t.Errorf("duplicate insert should be ignored, got %d edges", len(from))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// File hash operations
// ─────────────────────────────────────────────────────────────────────────────

func TestFileHash(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	// Initially no hash
	h := s.GetFileHash("p1", "file.go")
	if h != "" {
		t.Errorf("expected empty hash, got %q", h)
	}

	if err := s.SetFileHash("p1", "file.go", "abc123"); err != nil {
		t.Fatalf("SetFileHash: %v", err)
	}

	h = s.GetFileHash("p1", "file.go")
	if h != "abc123" {
		t.Errorf("GetFileHash = %q, want abc123", h)
	}
}

func TestDeleteFileHash(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.SetFileHash("p1", "file.go", "abc123")

	if err := s.DeleteFileHash("p1", "file.go"); err != nil {
		t.Fatalf("DeleteFileHash: %v", err)
	}

	h := s.GetFileHash("p1", "file.go")
	if h != "" {
		t.Errorf("expected empty hash after delete, got %q", h)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ADR operations
// ─────────────────────────────────────────────────────────────────────────────

func TestADR_SetGet(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	if err := s.SetADR("p1", "STACK", "Go + SQLite"); err != nil {
		t.Fatalf("SetADR: %v", err)
	}

	val, ok, err := s.GetADR("p1", "STACK")
	if err != nil {
		t.Fatalf("GetADR: %v", err)
	}
	if !ok {
		t.Error("expected ADR to exist")
	}
	if val != "Go + SQLite" {
		t.Errorf("value = %q, want 'Go + SQLite'", val)
	}
}

func TestADR_NotFound(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	_, ok, err := s.GetADR("p1", "NONEXISTENT")
	if err != nil {
		t.Fatalf("GetADR: %v", err)
	}
	if ok {
		t.Error("expected ADR not to exist")
	}
}

func TestADR_List(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.SetADR("p1", "A", "val-a")
	s.SetADR("p1", "B", "val-b")

	entries, err := s.ListADRs("p1")
	if err != nil {
		t.Fatalf("ListADRs: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 ADR entries, got %d", len(entries))
	}
	if entries["A"] != "val-a" || entries["B"] != "val-b" {
		t.Errorf("unexpected entries: %v", entries)
	}
}

func TestADR_Delete(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.SetADR("p1", "KEY", "value")

	rows, err := s.DeleteADR("p1", "KEY")
	if err != nil {
		t.Fatalf("DeleteADR: %v", err)
	}
	if rows != 1 {
		t.Errorf("DeleteADR rows = %d, want 1", rows)
	}

	_, ok, _ := s.GetADR("p1", "KEY")
	if ok {
		t.Error("ADR should be deleted")
	}
}

// #1019: deleting a key that never existed must return rows=0, not 1
// — the handler relies on the count to distinguish "deleted" from
// "no-op" and emit the right envelope shape.
func TestADR_Delete_NonexistentKey_ReturnsZeroRows(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	rows, err := s.DeleteADR("p1", "NEVER_EXISTED")
	if err != nil {
		t.Fatalf("DeleteADR: %v", err)
	}
	if rows != 0 {
		t.Errorf("DeleteADR rows for nonexistent key = %d, want 0", rows)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Graph stats
// ─────────────────────────────────────────────────────────────────────────────

func TestGraphStats(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("f1", "Foo", "Function", "p1", "a.go"),
		testSymbol("f2", "Bar", "Function", "p1", "a.go"),
		testSymbol("c1", "MyClass", "Class", "p1", "b.go"),
	})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "f1", ToID: "f2", Kind: "CALLS", Confidence: 1.0},
	})

	symCount, edgeCount, kindCounts, edgeKindCounts, err := s.GraphStats("p1")
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 3 {
		t.Errorf("symCount = %d, want 3", symCount)
	}
	if edgeCount != 1 {
		t.Errorf("edgeCount = %d, want 1", edgeCount)
	}
	if kindCounts["Function"] != 2 {
		t.Errorf("Function count = %d, want 2", kindCounts["Function"])
	}
	if edgeKindCounts["CALLS"] != 1 {
		t.Errorf("CALLS edge count = %d, want 1", edgeKindCounts["CALLS"])
	}
}

func TestGetHotspots(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		testSymbol("a", "A", "Function", "p1", "a.go"),
		testSymbol("b", "B", "Function", "p1", "b.go"),
		testSymbol("c", "C", "Function", "p1", "c.go"),
	})
	// B is called by A and C → hotspot
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "c", ToID: "b", Kind: "CALLS", Confidence: 1.0},
	})

	hotspots, err := s.GetHotspots("p1", 5)
	if err != nil {
		t.Fatalf("GetHotspots: %v", err)
	}
	if len(hotspots) == 0 {
		t.Error("expected at least 1 hotspot")
	}
	if hotspots[0].Name != "B" {
		t.Errorf("top hotspot = %q, want B", hotspots[0].Name)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Utility functions
// ─────────────────────────────────────────────────────────────────────────────

func TestMakeSymbolID(t *testing.T) {
	id := MakeSymbolID("internal/db/db.go", "db.Open", "Function")
	want := "internal/db/db.go::db.Open#Function"
	if id != want {
		t.Errorf("MakeSymbolID = %q, want %q", id, want)
	}
}

func TestProjectNameFromPath(t *testing.T) {
	cases := []struct {
		path, want string
	}{
		{"/home/user/myproject", "myproject"},
		{"/home/user/myproject/", "myproject"},
		{"C:\\Users\\foo\\bar", "bar"},
	}
	for _, c := range cases {
		got := ProjectNameFromPath(c.path)
		if got != c.want {
			t.Errorf("ProjectNameFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestApproxTokens(t *testing.T) {
	// Counts verified against cl100k_base BPE (same tokenizer family as Claude).
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abcd", 1},        // single BPE token
		{"abcde", 2},       // splits at boundary
		{"abcdefgh", 1},    // BPE merges entire sequence
		{"hello world", 2}, // ["hello", " world"]
	}
	for _, c := range cases {
		got := ApproxTokens(c.s)
		if got != c.want {
			t.Errorf("ApproxTokens(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := []struct {
		bytes int
		want  string
	}{
		{500, "500 B"},
		{1500, "1.5 KB"},
		{2000000, "1.9 MB"},
	}
	for _, c := range cases {
		got := FormatSize(c.bytes)
		if got != c.want {
			t.Errorf("FormatSize(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DB accessor
// ─────────────────────────────────────────────────────────────────────────────

func TestDB_Accessor(t *testing.T) {
	store := newTestStore(t)
	if store.DB() == nil {
		t.Error("DB() should return non-nil *sql.DB")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetSymbolsByQN
// ─────────────────────────────────────────────────────────────────────────────

func TestGetSymbolsByQN(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-qn"
	store.UpsertProject(Project{ID: pid, Path: "/tmp/qn", Name: "qn"})
	store.BulkUpsertSymbols([]Symbol{
		{ID: "qn1", ProjectID: pid, FilePath: "a.go", Name: "Foo",
			QualifiedName: "pkg.Foo", Kind: "Function", Language: "Go",
			StartByte: 0, EndByte: 50, StartLine: 1, EndLine: 5},
	})
	results, err := store.GetSymbolsByQN(pid, "pkg.Foo")
	if err != nil {
		t.Fatalf("GetSymbolsByQN: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected to find symbol by qualified name")
	}
}

func TestGetSymbolsByQN_NotFound(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-qn2"
	store.UpsertProject(Project{ID: pid, Path: "/tmp/qn2", Name: "qn2"})
	results, err := store.GetSymbolsByQN(pid, "pkg.NonExistent")
	if err != nil {
		t.Fatalf("GetSymbolsByQN: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProjectIDFromPath
// ─────────────────────────────────────────────────────────────────────────────

func TestProjectIDFromPath(t *testing.T) {
	id := ProjectIDFromPath("/home/user/myproject")
	if id == "" {
		t.Error("ProjectIDFromPath returned empty string")
	}
	id2 := ProjectIDFromPath("/home/user/myproject")
	if id != id2 {
		t.Error("ProjectIDFromPath should be deterministic")
	}
	id3 := ProjectIDFromPath("/home/user/otherproject")
	if id == id3 {
		t.Error("different paths should give different IDs")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DataDir
// ─────────────────────────────────────────────────────────────────────────────

func TestDataDir_ReturnsPath(t *testing.T) {
	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if dir == "" {
		t.Error("DataDir returned empty string")
	}
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		t.Errorf("DataDir %q does not exist after call", dir)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BulkUpsertEdges
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkUpsertEdges_WithProperties(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-edge-props"
	store.UpsertProject(testProject(pid))
	store.BulkUpsertSymbols([]Symbol{
		{ID: "ep1", ProjectID: pid, FilePath: "a.go", Name: "A", QualifiedName: "pkg.A", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
		{ID: "ep2", ProjectID: pid, FilePath: "b.go", Name: "B", QualifiedName: "pkg.B", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
	})
	edges := []Edge{
		{ProjectID: pid, FromID: "ep1", ToID: "ep2", Kind: "CALLS", Confidence: 0.9,
			Properties: map[string]any{"line": 5}},
	}
	if err := store.BulkUpsertEdges(edges); err != nil {
		t.Fatalf("BulkUpsertEdges with properties: %v", err)
	}
	got, err := store.EdgesFrom("ep1", []string{"CALLS"})
	if err != nil {
		t.Fatalf("EdgesFrom: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(got))
	}
}

func TestBulkUpsertEdges_Empty(t *testing.T) {
	store := newTestStore(t)
	if err := store.BulkUpsertEdges(nil); err != nil {
		t.Fatalf("BulkUpsertEdges(nil): %v", err)
	}
	if err := store.BulkUpsertEdges([]Edge{}); err != nil {
		t.Fatalf("BulkUpsertEdges([]): %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteProject
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteProject_RemovesAll(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-del"
	store.UpsertProject(testProject(pid))
	store.BulkUpsertSymbols([]Symbol{
		{ID: "dp1", ProjectID: pid, FilePath: "a.go", Name: "A", QualifiedName: "pkg.A", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
		{ID: "dp2", ProjectID: pid, FilePath: "b.go", Name: "B", QualifiedName: "pkg.B", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
	})
	store.BulkUpsertEdges([]Edge{
		{ProjectID: pid, FromID: "dp1", ToID: "dp2", Kind: "CALLS", Confidence: 1.0},
	})
	if err := store.DeleteProject(pid); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	p, err := store.GetProject(pid)
	if err != nil {
		t.Fatalf("GetProject after delete: %v", err)
	}
	if p != nil {
		t.Error("project should be nil after deletion")
	}
	syms, _ := store.GetSymbolsForFile(pid, "a.go")
	if len(syms) != 0 {
		t.Errorf("expected 0 symbols after deletion, got %d", len(syms))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteSymbolsForFile
// ─────────────────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────────────────
// GraphStats
// ─────────────────────────────────────────────────────────────────────────────

func TestGraphStats_WithData(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-gs"
	store.UpsertProject(testProject(pid))
	store.BulkUpsertSymbols([]Symbol{
		{ID: "gs1", ProjectID: pid, FilePath: "a.go", Name: "Fn1", QualifiedName: "p.Fn1", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
		{ID: "gs2", ProjectID: pid, FilePath: "a.go", Name: "T1", QualifiedName: "p.T1", Kind: "Class", Language: "Go", StartByte: 20, EndByte: 50, StartLine: 5, EndLine: 10},
		{ID: "gs3", ProjectID: pid, FilePath: "b.go", Name: "Fn2", QualifiedName: "p.Fn2", Kind: "Function", Language: "Go", StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 2},
	})
	store.BulkUpsertEdges([]Edge{
		{ProjectID: pid, FromID: "gs1", ToID: "gs3", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: pid, FromID: "gs2", ToID: "gs1", Kind: "CALLS", Confidence: 1.0},
	})
	symCount, edgeCount, kindCounts, edgeKindCounts, err := store.GraphStats(pid)
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 3 {
		t.Errorf("expected 3 symbols, got %d", symCount)
	}
	if edgeCount != 2 {
		t.Errorf("expected 2 edges, got %d", edgeCount)
	}
	if kindCounts["Function"] != 2 {
		t.Errorf("expected 2 Function kinds, got %d", kindCounts["Function"])
	}
	if kindCounts["Class"] != 1 {
		t.Errorf("expected 1 Class kind, got %d", kindCounts["Class"])
	}
	if edgeKindCounts["CALLS"] != 2 {
		t.Errorf("expected 2 CALLS edges, got %d", edgeKindCounts["CALLS"])
	}
}

func TestGraphStats_EmptyProject(t *testing.T) {
	store := newTestStore(t)
	pid := "proj-gs-empty"
	store.UpsertProject(testProject(pid))
	symCount, edgeCount, kindCounts, edgeKindCounts, err := store.GraphStats(pid)
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 0 || edgeCount != 0 {
		t.Errorf("expected 0 counts, got sym=%d edge=%d", symCount, edgeCount)
	}
	if len(kindCounts) != 0 || len(edgeKindCounts) != 0 {
		t.Error("expected empty kind maps for empty project")
	}
}


// ─────────────────────────────────────────────────────────────────────────────
// TraceViaCTE
// ─────────────────────────────────────────────────────────────────────────────

func TestTraceViaCTE_Outbound(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	caller := testSymbol("s1", "Caller", "Function", "p1", "a.go")
	callee := testSymbol("s2", "Callee", "Function", "p1", "b.go")
	s.BulkUpsertSymbols([]Symbol{caller, callee})
	s.BulkUpsertEdges([]Edge{{ProjectID: "p1", FromID: "s1", ToID: "s2", Kind: "CALLS", Confidence: 1.0}})

	results, err := s.TraceViaCTE("s1", "outbound", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	if len(results) != 1 || results[0].SymbolID != "s2" {
		t.Errorf("expected [s2], got %v", results)
	}
	if results[0].Depth != 1 {
		t.Errorf("expected depth 1, got %d", results[0].Depth)
	}
	if results[0].ViaKind != "CALLS" {
		t.Errorf("expected via CALLS, got %q", results[0].ViaKind)
	}
}

func TestTraceViaCTE_Inbound(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	caller := testSymbol("s1", "Caller", "Function", "p1", "a.go")
	callee := testSymbol("s2", "Callee", "Function", "p1", "b.go")
	s.BulkUpsertSymbols([]Symbol{caller, callee})
	s.BulkUpsertEdges([]Edge{{ProjectID: "p1", FromID: "s1", ToID: "s2", Kind: "CALLS", Confidence: 1.0}})

	results, err := s.TraceViaCTE("s2", "inbound", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	if len(results) != 1 || results[0].SymbolID != "s1" {
		t.Errorf("expected [s1], got %v", results)
	}
}

func TestTraceViaCTE_Both(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	root := testSymbol("root", "Root", "Function", "p1", "root.go")
	caller := testSymbol("caller", "Caller", "Function", "p1", "caller.go")
	callee := testSymbol("callee", "Callee", "Function", "p1", "callee.go")
	s.BulkUpsertSymbols([]Symbol{root, caller, callee})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "caller", ToID: "root", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "root", ToID: "callee", Kind: "CALLS", Confidence: 1.0},
	})

	results, err := s.TraceViaCTE("root", "both", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range results {
		ids[r.SymbolID] = true
	}
	if !ids["caller"] {
		t.Error("expected caller in both-direction trace")
	}
	if !ids["callee"] {
		t.Error("expected callee in both-direction trace")
	}
}

func TestTraceViaCTE_MultiHop(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	a := testSymbol("a", "A", "Function", "p1", "a.go")
	b := testSymbol("b", "B", "Function", "p1", "b.go")
	c := testSymbol("c", "C", "Function", "p1", "c.go")
	s.BulkUpsertSymbols([]Symbol{a, b, c})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "b", ToID: "c", Kind: "CALLS", Confidence: 1.0},
	})

	results, err := s.TraceViaCTE("a", "outbound", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	ids := map[string]int{}
	for _, r := range results {
		ids[r.SymbolID] = r.Depth
	}
	if ids["b"] != 1 {
		t.Errorf("expected B at depth 1, got %d", ids["b"])
	}
	if ids["c"] != 2 {
		t.Errorf("expected C at depth 2, got %d", ids["c"])
	}
}

func TestTraceViaCTE_DepthLimit(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	a := testSymbol("a", "A", "Function", "p1", "a.go")
	b := testSymbol("b", "B", "Function", "p1", "b.go")
	c := testSymbol("c", "C", "Function", "p1", "c.go")
	s.BulkUpsertSymbols([]Symbol{a, b, c})
	s.BulkUpsertEdges([]Edge{
		{ProjectID: "p1", FromID: "a", ToID: "b", Kind: "CALLS", Confidence: 1.0},
		{ProjectID: "p1", FromID: "b", ToID: "c", Kind: "CALLS", Confidence: 1.0},
	})

	// maxDepth=1 should only find b, not c
	results, err := s.TraceViaCTE("a", "outbound", []string{"CALLS"}, 1)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	for _, r := range results {
		if r.SymbolID == "c" {
			t.Error("c should be out of reach at maxDepth=1")
		}
	}
}

func TestTraceViaCTE_NoEdges(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	sym := testSymbol("iso", "Isolated", "Function", "p1", "iso.go")
	s.BulkUpsertSymbols([]Symbol{sym})

	results, err := s.TraceViaCTE("iso", "both", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for isolated node, got %d", len(results))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Symbol move tracking
// ─────────────────────────────────────────────────────────────────────────────

func TestRecordSymbolMove_Basic(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	if err := s.RecordSymbolMove("p1", "old-id", "new-id"); err != nil {
		t.Fatalf("RecordSymbolMove: %v", err)
	}
	newID, ok := s.ResolveStaleID("p1", "old-id")
	if !ok {
		t.Fatal("expected stale ID to resolve")
	}
	if newID != "new-id" {
		t.Errorf("expected new-id, got %q", newID)
	}
}

func TestResolveStaleID_NotFound(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	_, ok := s.ResolveStaleID("p1", "nonexistent")
	if ok {
		t.Error("expected false for nonexistent stale ID")
	}
}

func TestRecordSymbolMove_Upsert(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.RecordSymbolMove("p1", "old-id", "new-id-1")
	// Second call should update new_id
	if err := s.RecordSymbolMove("p1", "old-id", "new-id-2"); err != nil {
		t.Fatalf("RecordSymbolMove upsert: %v", err)
	}
	newID, _ := s.ResolveStaleID("p1", "old-id")
	if newID != "new-id-2" {
		t.Errorf("expected updated new-id-2, got %q", newID)
	}
}

func TestDetectAndRecordMoves_DetectsMove(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	// Original symbol at old path
	old := testSymbol("old/path.go::MyFn#Function", "MyFn", "Function", "p1", "old/path.go")
	old.QualifiedName = "MyFn"
	s.BulkUpsertSymbols([]Symbol{old})

	// Same qualified name + kind, new path (file moved)
	newSym := testSymbol("new/path.go::MyFn#Function", "MyFn", "Function", "p1", "new/path.go")
	newSym.QualifiedName = "MyFn"

	if err := s.DetectAndRecordMoves("p1", []Symbol{newSym}); err != nil {
		t.Fatalf("DetectAndRecordMoves: %v", err)
	}

	newID, ok := s.ResolveStaleID("p1", old.ID)
	if !ok {
		t.Fatal("expected move to be recorded")
	}
	if newID != newSym.ID {
		t.Errorf("expected %q, got %q", newSym.ID, newID)
	}
}

func TestDetectAndRecordMoves_NoMove(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))

	// Brand new symbol — no existing symbol with same QN+kind
	newSym := testSymbol("path.go::BrandNew#Function", "BrandNew", "Function", "p1", "path.go")
	newSym.QualifiedName = "BrandNew"

	if err := s.DetectAndRecordMoves("p1", []Symbol{newSym}); err != nil {
		t.Fatalf("DetectAndRecordMoves: %v", err)
	}
	_, ok := s.ResolveStaleID("p1", newSym.ID)
	if ok {
		t.Error("no move should be recorded for brand-new symbol")
	}
}

func TestDetectAndRecordMoves_Empty(t *testing.T) {
	s := newTestStore(t)
	if err := s.DetectAndRecordMoves("p1", nil); err != nil {
		t.Fatalf("DetectAndRecordMoves empty: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteSymbolsForFile
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteSymbolsForFile_RemovesSymbolsAndEdges(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("dsf"))

	a := testSymbol("dsf::A#Function", "A", "Function", "dsf", "a.go")
	b := testSymbol("dsf::B#Function", "B", "Function", "dsf", "b.go")
	if err := s.BulkUpsertSymbols([]Symbol{a, b}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "dsf", FromID: a.ID, ToID: b.ID, Kind: "CALLS"},
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	if err := s.DeleteSymbolsForFile("dsf", "a.go"); err != nil {
		t.Fatalf("DeleteSymbolsForFile: %v", err)
	}

	// Symbol A should be gone
	got, err := s.GetSymbol(a.ID)
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if got != nil {
		t.Error("symbol A still exists after DeleteSymbolsForFile")
	}

	// Symbol B should still exist (different file)
	gotB, err := s.GetSymbol(b.ID)
	if err != nil {
		t.Fatalf("GetSymbol B: %v", err)
	}
	if gotB == nil {
		t.Error("symbol B should still exist")
	}

	// Edge from A should be gone
	var edgeCount int
	s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE from_id=? OR to_id=?`, a.ID, a.ID).Scan(&edgeCount)
	if edgeCount != 0 {
		t.Errorf("edges referencing deleted symbol still exist: count=%d", edgeCount)
	}
}

func TestDeleteSymbolsForFile_EmptyFile(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("dsf2"))
	// Deleting symbols for a file that has no symbols should not error.
	if err := s.DeleteSymbolsForFile("dsf2", "nonexistent.go"); err != nil {
		t.Errorf("DeleteSymbolsForFile nonexistent: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GraphStats
// ─────────────────────────────────────────────────────────────────────────────

func TestGraphStats_Kinds(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("gs"))

	fn := testSymbol("gs::Fn#Function", "Fn", "Function", "gs", "f.go")
	cl := testSymbol("gs::Cls#Class", "Cls", "Class", "gs", "f.go")
	cl.Kind = "Class"
	if err := s.BulkUpsertSymbols([]Symbol{fn, cl}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "gs", FromID: fn.ID, ToID: cl.ID, Kind: "CALLS"},
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	symCount, edgeCount, kindCounts, edgeKindCounts, err := s.GraphStats("gs")
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 2 {
		t.Errorf("symCount=%d, want 2", symCount)
	}
	if edgeCount != 1 {
		t.Errorf("edgeCount=%d, want 1", edgeCount)
	}
	if kindCounts["Function"] != 1 {
		t.Errorf("Function kind count=%d, want 1", kindCounts["Function"])
	}
	if kindCounts["Class"] != 1 {
		t.Errorf("Class kind count=%d, want 1", kindCounts["Class"])
	}
	if edgeKindCounts["CALLS"] != 1 {
		t.Errorf("CALLS edge count=%d, want 1", edgeKindCounts["CALLS"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SearchSymbols — kind and language filters
// ─────────────────────────────────────────────────────────────────────────────

func TestSearchSymbols_KindFilterCombined(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("ssk"))

	fn := testSymbol("ssk::ProcessOrder#Function", "ProcessOrder", "Function", "ssk", "f.go")
	cl := testSymbol("ssk::OrderService#Class", "OrderService", "Class", "ssk", "f.go")
	cl.Kind = "Class"
	cl.Name = "OrderService"
	cl.QualifiedName = "OrderService"

	if err := s.BulkUpsertSymbols([]Symbol{fn, cl}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Search with kind=Function should only return Function kinds
	results, err := s.SearchSymbols("ssk", "Order*", "Function", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	for _, r := range results {
		if r.Symbol.Kind != "Function" {
			t.Errorf("kind filter violated: got kind=%q", r.Symbol.Kind)
		}
	}

	// Search with kind=Class should only return Class kinds
	results, err = s.SearchSymbols("ssk", "Order*", "Class", "", 10)
	if err != nil {
		t.Fatalf("SearchSymbols Class: %v", err)
	}
	for _, r := range results {
		if r.Symbol.Kind != "Class" {
			t.Errorf("Class filter violated: got kind=%q", r.Symbol.Kind)
		}
	}
}

func TestSearchSymbols_LanguageFilter(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("ssl"))

	goSym := testSymbol("ssl::Handler#Function", "Handler", "Function", "ssl", "f.go")
	goSym.Language = "Go"

	pySym := testSymbol("ssl::handler#Function", "handler", "Function", "ssl", "f.py")
	pySym.Language = "Python"
	pySym.Name = "handler"
	pySym.QualifiedName = "handler"

	if err := s.BulkUpsertSymbols([]Symbol{goSym, pySym}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	results, err := s.SearchSymbols("ssl", "handler*", "", "Go", 10)
	if err != nil {
		t.Fatalf("SearchSymbols: %v", err)
	}
	for _, r := range results {
		if r.Symbol.Language != "Go" {
			t.Errorf("language filter violated: got language=%q", r.Symbol.Language)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetHotspots
// ─────────────────────────────────────────────────────────────────────────────

func TestGetHotspots_TopCalled(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("hot2"))

	caller := testSymbol("hot2::Caller#Function", "Caller", "Function", "hot2", "f.go")
	callee := testSymbol("hot2::Callee#Function", "Callee", "Function", "hot2", "f.go")
	if err := s.BulkUpsertSymbols([]Symbol{caller, callee}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// callee is called twice
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "hot2", FromID: caller.ID, ToID: callee.ID, Kind: "CALLS"},
		{ProjectID: "hot2", FromID: callee.ID, ToID: callee.ID, Kind: "CALLS"}, // self-call
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	hotspots, err := s.GetHotspots("hot2", 5)
	if err != nil {
		t.Fatalf("GetHotspots: %v", err)
	}
	if len(hotspots) == 0 {
		t.Fatal("expected at least one hotspot")
	}
	// Callee has 2 in-edges, so it should be top hotspot
	if hotspots[0].Name != "Callee" {
		t.Errorf("top hotspot=%q, want Callee", hotspots[0].Name)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TraceViaCTE — direction variants
// ─────────────────────────────────────────────────────────────────────────────

func TestTraceViaCTE_InboundDirection(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("tr"))

	caller := testSymbol("tr::Caller#Function", "Caller", "Function", "tr", "f.go")
	callee := testSymbol("tr::Callee#Function", "Callee", "Function", "tr", "f.go")
	if err := s.BulkUpsertSymbols([]Symbol{caller, callee}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "tr", FromID: caller.ID, ToID: callee.ID, Kind: "CALLS"},
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	// Inbound trace from callee — should find caller
	results, err := s.TraceViaCTE(callee.ID, "inbound", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE inbound: %v", err)
	}
	found := false
	for _, r := range results {
		if r.SymbolID == caller.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("inbound trace from Callee should find Caller, got %v", results)
	}
}

func TestTraceViaCTE_BothDirections(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("trb"))

	a := testSymbol("trb::A#Function", "A", "Function", "trb", "f.go")
	b := testSymbol("trb::B#Function", "B", "Function", "trb", "f.go")
	c := testSymbol("trb::C#Function", "C", "Function", "trb", "f.go")
	if err := s.BulkUpsertSymbols([]Symbol{a, b, c}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.BulkUpsertEdges([]Edge{
		{ProjectID: "trb", FromID: a.ID, ToID: b.ID, Kind: "CALLS"}, // A calls B
		{ProjectID: "trb", FromID: c.ID, ToID: b.ID, Kind: "CALLS"}, // C calls B
	}); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	// Both directions from B — outbound: nothing, inbound: A and C
	results, err := s.TraceViaCTE(b.ID, "both", []string{"CALLS"}, 3)
	if err != nil {
		t.Fatalf("TraceViaCTE both: %v", err)
	}
	ids := make(map[string]bool)
	for _, r := range results {
		ids[r.SymbolID] = true
	}
	if !ids[a.ID] {
		t.Error("both trace should find A (inbound caller)")
	}
	if !ids[c.ID] {
		t.Error("both trace should find C (inbound caller)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BulkUpsertSymbols — ExtractionConfidence default
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkUpsertSymbols_DefaultConfidence(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("buc"))

	sym := testSymbol("buc::Fn#Function", "Fn", "Function", "buc", "f.go")
	sym.ExtractionConfidence = 0 // should default to 1.0

	if err := s.BulkUpsertSymbols([]Symbol{sym}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	got, err := s.GetSymbol(sym.ID)
	if err != nil {
		t.Fatalf("GetSymbol: %v", err)
	}
	if got == nil {
		t.Fatal("symbol not found")
	}
	if got.ExtractionConfidence != 1.0 {
		t.Errorf("ExtractionConfidence=%f, want 1.0 (default)", got.ExtractionConfidence)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ListADRs — multiple entries
// ─────────────────────────────────────────────────────────────────────────────

func TestListADRs_MultipleEntries(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("adr"))

	entries := map[string]string{
		"PURPOSE": "code intelligence MCP",
		"STACK":   "Go + SQLite",
		"TEAM":    "platform",
	}
	for k, v := range entries {
		if err := s.SetADR("adr", k, v); err != nil {
			t.Fatalf("SetADR %s: %v", k, v)
		}
	}

	got, err := s.ListADRs("adr")
	if err != nil {
		t.Fatalf("ListADRs: %v", err)
	}
	if len(got) != len(entries) {
		t.Errorf("ListADRs count=%d, want %d", len(got), len(entries))
	}
	for k, want := range entries {
		if got[k] != want {
			t.Errorf("ADR[%q]=%q, want %q", k, got[k], want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ListProjects — multiple projects
// ─────────────────────────────────────────────────────────────────────────────

func TestListProjects_Multiple(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"lp1", "lp2", "lp3"} {
		if err := s.UpsertProject(testProject(id)); err != nil {
			t.Fatalf("UpsertProject %s: %v", id, err)
		}
	}

	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) < 3 {
		t.Errorf("ListProjects count=%d, want >=3", len(projects))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetProject — not found returns nil
// ─────────────────────────────────────────────────────────────────────────────

func TestGetProject_MissingID(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetProject("nonexistent-project-xyz")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got != nil {
		t.Error("GetProject nonexistent should return nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HealthCheck + formatStaleness
// ─────────────────────────────────────────────────────────────────────────────

func TestHealthCheck_NoProject(t *testing.T) {
	s := newTestStore(t)
	report, err := s.HealthCheck("")
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if report == nil {
		t.Fatal("report is nil")
	}
	if report.SchemaVersion <= 0 {
		t.Errorf("SchemaVersion=%d, want >0", report.SchemaVersion)
	}
	if report.Project != nil {
		t.Errorf("Project should be nil for empty projectID")
	}
	// #328: Coverage must be a non-nil empty slice so JSON shape is stable.
	if report.Coverage == nil {
		t.Error("Coverage is nil; want non-nil empty slice for stable JSON shape (marshals to [] not null)")
	}
}

// #328: An indexed project with zero symbols still returns Coverage as
// an empty slice, never nil. JSON consumers can `range` it without
// null-checking.
func TestHealthCheck_EmptyProject_CoverageIsEmptySliceNotNil(t *testing.T) {
	s := newTestStore(t)
	p := testProject("empty1")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	report, err := s.HealthCheck("empty1")
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if report.Coverage == nil {
		t.Fatal("Coverage is nil for empty project; want non-nil empty slice")
	}
	if len(report.Coverage) != 0 {
		t.Errorf("Coverage length = %d, want 0 for project with no symbols", len(report.Coverage))
	}
}

func TestHealthCheck_WithProject(t *testing.T) {
	s := newTestStore(t)
	p := testProject("hp1")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	// Insert a Go symbol so coverage has something to aggregate.
	sym := testSymbol("hp1::Fn#Function", "Fn", "Function", "hp1", "main.go")
	sym.ExtractionConfidence = 1.0
	if err := s.BulkUpsertSymbols([]Symbol{sym}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	report, err := s.HealthCheck("hp1")
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if report.Project == nil {
		t.Fatal("expected Project in report")
	}
	if report.Project.ID != "hp1" {
		t.Errorf("Project.ID=%q, want hp1", report.Project.ID)
	}
	if report.StalenessHuman == "" {
		t.Error("StalenessHuman should not be empty")
	}
	if len(report.Coverage) == 0 {
		t.Error("Coverage should have at least one entry")
	}
	// Go symbols with confidence=1.0 should be AST-parsed
	for _, lc := range report.Coverage {
		if lc.Language == "Go" && lc.Parser != "AST" {
			t.Errorf("Go Parser=%q, want AST", lc.Parser)
		}
	}
}

func TestHealthCheck_RegexLanguageCoverage(t *testing.T) {
	s := newTestStore(t)
	p := testProject("hpr")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	sym := testSymbol("hpr::Fn#Function", "Fn", "Function", "hpr", "main.py")
	sym.Language = "Python"
	sym.ExtractionConfidence = 0.85 // regex extraction
	if err := s.BulkUpsertSymbols([]Symbol{sym}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	report, err := s.HealthCheck("hpr")
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	for _, lc := range report.Coverage {
		if lc.Language == "Python" && lc.Parser != "Regex" {
			t.Errorf("Python Parser=%q, want Regex (confidence=0.85)", lc.Parser)
		}
	}
}

func TestFormatStaleness(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{10 * time.Second, "10s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}
	for _, tc := range cases {
		got := formatStaleness(tc.d)
		if got != tc.want {
			t.Errorf("formatStaleness(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DataDir
// ─────────────────────────────────────────────────────────────────────────────

func TestDataDir_ReturnsNonEmpty(t *testing.T) {
	dir, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if dir == "" {
		t.Error("DataDir returned empty string")
	}
	// Must be a directory that exists after the call
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("DataDir path does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("DataDir %q is not a directory", dir)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteProject
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteProject_ClearsSymbols(t *testing.T) {
	s := newTestStore(t)
	p := testProject("del-syms")
	if err := s.UpsertProject(p); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	sym := testSymbol("del-syms::Fn#Function", "Fn", "Function", "del-syms", "f.go")
	if err := s.BulkUpsertSymbols([]Symbol{sym}); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}

	if err := s.DeleteProject("del-syms"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	// Project should be gone
	got, err := s.GetProject("del-syms")
	if err != nil {
		t.Fatalf("GetProject after delete: %v", err)
	}
	if got != nil {
		t.Error("project still exists after DeleteProject")
	}

	// Symbols should be gone
	sym2, err := s.GetSymbol(sym.ID)
	if err != nil {
		t.Fatalf("GetSymbol after delete: %v", err)
	}
	if sym2 != nil {
		t.Error("symbol still exists after DeleteProject")
	}
}

func TestDeleteProject_NonExistent(t *testing.T) {
	s := newTestStore(t)
	// Deleting a project that doesn't exist should not error.
	if err := s.DeleteProject("does-not-exist"); err != nil {
		t.Errorf("DeleteProject nonexistent: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// migrate — schema version tracking
// ─────────────────────────────────────────────────────────────────────────────

func TestMigrate_SchemaVersion(t *testing.T) {
	s := newTestStore(t)
	// After Open(), schema_version should be at the latest version.
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	expected := 1 + len(schemaMigrations)
	if version != expected {
		t.Errorf("schema version=%d, want %d", version, expected)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	s := newTestStore(t)
	// Running migrate() again on an already-migrated DB should be safe.
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate(): %v", err)
	}
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	expected := 1 + len(schemaMigrations)
	if version != expected {
		t.Errorf("schema version after re-migrate=%d, want %d", version, expected)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Session persistence tests (RecordSession, GetSessions, GetAllTimeSavings)
// ─────────────────────────────────────────────────────────────────────────────

func TestRecordSession_Basic(t *testing.T) {
	s := newTestStore(t)
	start := time.Now().Add(-5 * time.Minute)
	if err := s.RecordSession("sess-001", start, 10, 500, 12000, 0.036, "", 0, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	rows, err := s.GetSessions(10)
	if err != nil {
		t.Fatalf("GetSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d sessions, want 1", len(rows))
	}
	r := rows[0]
	if r.SessionID != "sess-001" {
		t.Errorf("session_id=%q, want sess-001", r.SessionID)
	}
	if r.Calls != 10 {
		t.Errorf("calls=%d, want 10", r.Calls)
	}
	if r.TokensUsed != 500 {
		t.Errorf("tokens_used=%d, want 500", r.TokensUsed)
	}
	if r.TokensSaved != 12000 {
		t.Errorf("tokens_saved=%d, want 12000", r.TokensSaved)
	}
}

func TestRecordSession_Upsert(t *testing.T) {
	s := newTestStore(t)
	start := time.Now().Add(-10 * time.Minute)
	// First write
	if err := s.RecordSession("sess-abc", start, 5, 200, 4000, 0.012, "", 0, ""); err != nil {
		t.Fatalf("first RecordSession: %v", err)
	}
	// Upsert with updated stats (same session_id)
	if err := s.RecordSession("sess-abc", start, 20, 900, 18000, 0.054, "", 0, ""); err != nil {
		t.Fatalf("second RecordSession: %v", err)
	}
	rows, err := s.GetSessions(10)
	if err != nil {
		t.Fatalf("GetSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d sessions after upsert, want 1", len(rows))
	}
	if rows[0].Calls != 20 {
		t.Errorf("calls after upsert=%d, want 20", rows[0].Calls)
	}
}

func TestGetSessions_OrderAndLimit(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	// Insert 3 sessions with different start times
	for i, offset := range []time.Duration{-3 * time.Hour, -2 * time.Hour, -1 * time.Hour} {
		id := "sess-" + string(rune('a'+i))
		if err := s.RecordSession(id, now.Add(offset), int64(i+1)*5, 100, 1000, 0.003, "", 0, ""); err != nil {
			t.Fatalf("RecordSession %d: %v", i, err)
		}
	}
	// Limit 2 — should get the 2 most recent
	rows, err := s.GetSessions(2)
	if err != nil {
		t.Fatalf("GetSessions(2): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d sessions, want 2", len(rows))
	}
	// Most recent first
	if rows[0].SessionID != "sess-c" {
		t.Errorf("first result=%q, want sess-c (most recent)", rows[0].SessionID)
	}
	if rows[1].SessionID != "sess-b" {
		t.Errorf("second result=%q, want sess-b", rows[1].SessionID)
	}
}

func TestGetSessions_Empty(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.GetSessions(10)
	if err != nil {
		t.Fatalf("GetSessions on empty DB: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows on empty DB, want 0", len(rows))
	}
}

func TestGetAllTimeSavings_Empty(t *testing.T) {
	s := newTestStore(t)
	calls, used, saved, cost, err := s.GetAllTimeSavings()
	if err != nil {
		t.Fatalf("GetAllTimeSavings on empty DB: %v", err)
	}
	if calls != 0 || used != 0 || saved != 0 || cost != 0 {
		t.Errorf("expected all zeros on empty DB, got calls=%d used=%d saved=%d cost=%v", calls, used, saved, cost)
	}
}

func TestGetAllTimeSavings_Aggregates(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	if err := s.RecordSession("sess-x", now.Add(-2*time.Hour), 10, 300, 5000, 0.015, "", 0, ""); err != nil {
		t.Fatalf("RecordSession 1: %v", err)
	}
	if err := s.RecordSession("sess-y", now.Add(-1*time.Hour), 20, 600, 10000, 0.030, "", 0, ""); err != nil {
		t.Fatalf("RecordSession 2: %v", err)
	}
	calls, used, saved, cost, err := s.GetAllTimeSavings()
	if err != nil {
		t.Fatalf("GetAllTimeSavings: %v", err)
	}
	if calls != 30 {
		t.Errorf("total calls=%d, want 30", calls)
	}
	if used != 900 {
		t.Errorf("total tokens_used=%d, want 900", used)
	}
	if saved != 15000 {
		t.Errorf("total tokens_saved=%d, want 15000", saved)
	}
	if cost < 0.044 || cost > 0.046 {
		t.Errorf("total cost_avoided=%v, want ~0.045", cost)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetAllTimeCallsByLanguage (#240)
// ─────────────────────────────────────────────────────────────────────────────

func TestGetAllTimeCallsByLanguage_Empty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetAllTimeCallsByLanguage()
	if err != nil {
		t.Fatalf("GetAllTimeCallsByLanguage on empty DB: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries on empty DB, want 0 (got %v)", len(got), got)
	}
}

// Sessions persisted without a calls_by_language payload (empty string
// → SQL NULL) must not surface in the aggregate. This pins the NULL
// filtering in the WHERE clause: a regression that scanned NULLs would
// either error or inflate counts with default-zero entries.
func TestGetAllTimeCallsByLanguage_NullColumnSkipped(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordSession("legacy", time.Unix(1, 0), 5, 50, 500, 0.05, "", 0, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	got, err := s.GetAllTimeCallsByLanguage()
	if err != nil {
		t.Fatalf("GetAllTimeCallsByLanguage: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0 — sessions without language data must not appear", len(got))
	}
}

// Multiple sessions with overlapping languages must sum per-language.
// This is the load-bearing diagnostic property: the user's surfaced
// per-language tally is a sum across every session that recorded data.
func TestGetAllTimeCallsByLanguage_SumsAcrossSessions(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordSession("a", time.Unix(1, 0), 10, 100, 1000, 0.1, "", 0, `{"Go":7,"Markdown":3}`); err != nil {
		t.Fatalf("RecordSession a: %v", err)
	}
	if err := s.RecordSession("b", time.Unix(2, 0), 4, 40, 400, 0.04, "", 0, `{"Go":2,"Python":4}`); err != nil {
		t.Fatalf("RecordSession b: %v", err)
	}
	got, err := s.GetAllTimeCallsByLanguage()
	if err != nil {
		t.Fatalf("GetAllTimeCallsByLanguage: %v", err)
	}
	want := map[string]int64{"Go": 9, "Markdown": 3, "Python": 4}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("aggregate mismatch:\n  got  %v\n  want %v", got, want)
	}
}

// A session whose JSON payload is malformed must be skipped silently —
// one corrupted row should not blank out every other session's data.
// Defensive against forward-compat: a future writer that ships a
// non-flat-int shape shouldn't error this read path.
func TestGetAllTimeCallsByLanguage_MalformedJSONSkipped(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordSession("good", time.Unix(1, 0), 10, 100, 1000, 0.1, "", 0, `{"Go":5}`); err != nil {
		t.Fatalf("RecordSession good: %v", err)
	}
	if err := s.RecordSession("bad", time.Unix(2, 0), 4, 40, 400, 0.04, "", 0, `{not valid json`); err != nil {
		t.Fatalf("RecordSession bad: %v", err)
	}
	got, err := s.GetAllTimeCallsByLanguage()
	if err != nil {
		t.Fatalf("GetAllTimeCallsByLanguage: %v", err)
	}
	want := map[string]int64{"Go": 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("malformed-row aggregate mismatch:\n  got  %v\n  want %v", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetAllTimeQueryMetrics (#241)
// ─────────────────────────────────────────────────────────────────────────────

// Empty database returns a zero-value QueryMetrics — no rows, no
// counters. Pre-v17 sessions sum to zero; ensure the aggregator
// doesn't error on the bare schema either.
func TestGetAllTimeQueryMetrics_Empty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetAllTimeQueryMetrics()
	if err != nil {
		t.Fatalf("GetAllTimeQueryMetrics on empty DB: %v", err)
	}
	want := QueryMetrics{}
	if got != want {
		t.Errorf("empty aggregate = %+v, want %+v", got, want)
	}
}

// Multiple sessions with v17 metrics sum across rows. This is the
// load-bearing aggregate path that `pincher stats` consumes.
func TestGetAllTimeQueryMetrics_SumsAcrossSessions(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordSessionWithMetrics("a", time.Unix(1, 0), 5, 50, 500, 0.05, "", 0, "",
		QueryMetrics{QueriesTotal: 10, QueriesZeroResult: 3, QueriesRetriedSucceeded: 2, TokensBurnedOnFailures: 600}); err != nil {
		t.Fatalf("RecordSessionWithMetrics a: %v", err)
	}
	if err := s.RecordSessionWithMetrics("b", time.Unix(2, 0), 7, 70, 700, 0.07, "", 0, "",
		QueryMetrics{QueriesTotal: 4, QueriesZeroResult: 1, QueriesRetriedSucceeded: 1, TokensBurnedOnFailures: 200}); err != nil {
		t.Fatalf("RecordSessionWithMetrics b: %v", err)
	}
	got, err := s.GetAllTimeQueryMetrics()
	if err != nil {
		t.Fatalf("GetAllTimeQueryMetrics: %v", err)
	}
	want := QueryMetrics{
		QueriesTotal:            14,
		QueriesZeroResult:       4,
		QueriesRetriedSucceeded: 3,
		TokensBurnedOnFailures:  800,
	}
	if got != want {
		t.Errorf("aggregate mismatch:\n  got  %+v\n  want %+v", got, want)
	}
}

// Pre-v17 sessions (recorded via RecordSession with no metrics) hold
// zero on every counter; mixing them with v17 sessions must not
// corrupt the aggregate. Defensive against the upgrade path where a
// long-running database carries a mix of pre-v17 and v17 rows.
func TestGetAllTimeQueryMetrics_PreV17RowsContributeZero(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordSession("legacy", time.Unix(1, 0), 5, 50, 500, 0.05, "", 0, ""); err != nil {
		t.Fatalf("RecordSession legacy: %v", err)
	}
	if err := s.RecordSessionWithMetrics("modern", time.Unix(2, 0), 5, 50, 500, 0.05, "", 0, "",
		QueryMetrics{QueriesTotal: 10, QueriesZeroResult: 2, QueriesRetriedSucceeded: 1, TokensBurnedOnFailures: 400}); err != nil {
		t.Fatalf("RecordSessionWithMetrics modern: %v", err)
	}
	got, err := s.GetAllTimeQueryMetrics()
	if err != nil {
		t.Fatalf("GetAllTimeQueryMetrics: %v", err)
	}
	want := QueryMetrics{QueriesTotal: 10, QueriesZeroResult: 2, QueriesRetriedSucceeded: 1, TokensBurnedOnFailures: 400}
	if got != want {
		t.Errorf("mixed aggregate mismatch:\n  got  %+v\n  want %+v", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// withTx and migrate coverage tests
// ─────────────────────────────────────────────────────────────────────────────

func TestWithTx_RollbackOnError(t *testing.T) {
	s := newTestStore(t)
	// Insert a project, then roll it back via an error in withTx.
	intentionalErr := fmt.Errorf("intentional rollback")
	err := s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO projects(id, path, name, indexed_at) VALUES(?,?,?,?)`,
			"proj-rollback", "/tmp/rollback", "rollback", 0); err != nil {
			return err
		}
		return intentionalErr // triggers rollback
	})
	if err != intentionalErr {
		t.Fatalf("withTx returned %v, want intentionalErr", err)
	}
	// Project must NOT be in DB (rollback succeeded)
	p, _ := s.GetProject("proj-rollback")
	if p != nil {
		t.Error("project survived rollback — withTx did not roll back correctly")
	}
}

func TestWithTx_CommitOnSuccess(t *testing.T) {
	s := newTestStore(t)
	err := s.withTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects(id, path, name, indexed_at) VALUES(?,?,?,?)`,
			"proj-commit", "/tmp/commit", "commit", 0)
		return err
	})
	if err != nil {
		t.Fatalf("withTx commit: %v", err)
	}
	p, err := s.GetProject("proj-commit")
	if err != nil {
		t.Fatalf("GetProject after commit: %v", err)
	}
	if p == nil {
		t.Error("project not found after successful withTx commit")
	}
}

func TestMigrate_SchemaVersionAfterReopen(t *testing.T) {
	dir := t.TempDir()
	// Open once — migrates to latest version
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	var v1 int
	_ = s1.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v1)
	s1.Close()

	// Reopen — migrate() must be idempotent; version must stay the same
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer s2.Close()
	var v2 int
	_ = s2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v2)

	if v2 != v1 {
		t.Errorf("version after reopen=%d, want %d (same as first open)", v2, v1)
	}
}

func TestMigrate_AnalyzeOnFreshDB(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// migrate() seeds sqlite_stat1 with one ANALYZE on a brand-new DB so
	// PRAGMA optimize has somewhere to write stats from the first index
	// onwards. The table must exist after Open even though the DB is empty.
	var name string
	err = s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='sqlite_stat1'`,
	).Scan(&name)
	if err != nil {
		t.Errorf("sqlite_stat1 not created on fresh DB: %v", err)
	}
}

func TestOpen_WALGuardrailApplied(t *testing.T) {
	s := newTestStore(t)

	// journal_size_limit is session-local in SQLite. With pool=1, this
	// connection's setting is what subsequent queries observe.
	var sizeLimit int64
	if err := s.db.QueryRow("PRAGMA journal_size_limit").Scan(&sizeLimit); err != nil {
		t.Fatalf("read journal_size_limit: %v", err)
	}
	if sizeLimit != 268435456 {
		t.Errorf("journal_size_limit = %d, want 268435456", sizeLimit)
	}

	// wal_autocheckpoint is left at the SQLite default (1000 pages). An
	// earlier version of this branch lowered it to 100; that change cost
	// 14.5× on heavy single-writer indexing and was reverted.
	var checkpoint int
	if err := s.db.QueryRow("PRAGMA wal_autocheckpoint").Scan(&checkpoint); err != nil {
		t.Fatalf("read wal_autocheckpoint: %v", err)
	}
	if checkpoint != 1000 {
		t.Errorf("wal_autocheckpoint = %d, want default 1000", checkpoint)
	}
}

func TestCheckpointTruncate_DoesNotError(t *testing.T) {
	s := newTestStore(t)

	// CheckpointTruncate must succeed on an empty DB. It may also succeed
	// when WAL is not engaged (running an older binary against a delete-
	// mode DB) — in that case PRAGMA wal_checkpoint(TRUNCATE) is a quiet
	// no-op and we still want no error.
	if err := s.CheckpointTruncate(); err != nil {
		t.Errorf("CheckpointTruncate on empty DB: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DataDir — idempotency
// ─────────────────────────────────────────────────────────────────────────────

func TestDataDir_CalledTwice_Idempotent(t *testing.T) {
	d1, err1 := DataDir()
	d2, err2 := DataDir()
	if err1 != nil || err2 != nil {
		t.Fatalf("DataDir errors: %v, %v", err1, err2)
	}
	if d1 != d2 {
		t.Errorf("DataDir not idempotent: %q != %q", d1, d2)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProjectIDFromPath
// ─────────────────────────────────────────────────────────────────────────────

func TestProjectIDFromPath_AbsoluteCanonical(t *testing.T) {
	// Closes #84: ProjectIDFromPath now canonicalises (symlinks resolved,
	// casing folded on case-insensitive FSes) rather than returning the
	// input path verbatim. The contract: result is absolute, equal to
	// CanonicalProjectPath(filepath.Abs(input)). Idempotence on
	// equivalent input forms is covered by TestProjectIDFromPath_Idempotent.
	dir := t.TempDir()
	id := ProjectIDFromPath(dir)
	if !filepath.IsAbs(id) {
		t.Errorf("ProjectIDFromPath(%q) = %q, want absolute", dir, id)
	}
	abs, _ := filepath.Abs(dir)
	want := CanonicalProjectPath(abs)
	if id != want {
		t.Errorf("ProjectIDFromPath(%q) = %q, want canonical %q", dir, id, want)
	}
}

func TestProjectIDFromPath_RelativeResolved(t *testing.T) {
	// A relative path must be resolved to an absolute path
	id := ProjectIDFromPath(".")
	if id == "." {
		t.Error("ProjectIDFromPath('.') returned '.', want absolute path")
	}
	if !filepath.IsAbs(id) {
		t.Errorf("ProjectIDFromPath('.') = %q, want absolute", id)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ListADRs — empty project
// ─────────────────────────────────────────────────────────────────────────────

func TestListADRs_EmptyProject(t *testing.T) {
	store := newTestStore(t)
	adrs, err := store.ListADRs("no-such-project")
	if err != nil {
		t.Fatalf("ListADRs: %v", err)
	}
	if len(adrs) != 0 {
		t.Errorf("ListADRs nonexistent project: got %d, want 0", len(adrs))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GraphStats
// ─────────────────────────────────────────────────────────────────────────────

func TestGraphStats_Empty(t *testing.T) {
	store := newTestStore(t)
	symCount, edgeCount, kindCounts, edgeKindCounts, err := store.GraphStats("no-project")
	if err != nil {
		t.Fatalf("GraphStats: %v", err)
	}
	if symCount != 0 || edgeCount != 0 {
		t.Errorf("GraphStats empty: symCount=%d edgeCount=%d, want 0", symCount, edgeCount)
	}
	if len(kindCounts) != 0 || len(edgeKindCounts) != 0 {
		t.Errorf("GraphStats empty: kindCounts=%v edgeKindCounts=%v, want empty", kindCounts, edgeKindCounts)
	}
}

func TestGraphStats_WithMixedKinds(t *testing.T) {
	store := newTestStore(t)
	pid := "gstat-proj"
	store.UpsertProject(testProject(pid))

	syms := []Symbol{
		testSymbol("f1", "FuncA", "Function", pid, "a.go"),
		testSymbol("f2", "FuncB", "Function", pid, "a.go"),
		testSymbol("c1", "MyClass", "Class", pid, "b.go"),
	}
	if err := store.BulkUpsertSymbols(syms); err != nil {
		t.Fatalf("BulkUpsertSymbols: %v", err)
	}
	edges := []Edge{
		{FromID: "f1", ToID: "f2", Kind: "CALLS", ProjectID: pid},
		{FromID: "c1", ToID: "f1", Kind: "IMPORTS", ProjectID: pid},
	}
	if err := store.BulkUpsertEdges(edges); err != nil {
		t.Fatalf("BulkUpsertEdges: %v", err)
	}

	symCount, edgeCount, kindCounts, edgeKindCounts, err := store.GraphStats(pid)
	if err != nil {
		t.Fatalf("GraphStats with data: %v", err)
	}
	if symCount != 3 {
		t.Errorf("symCount=%d, want 3", symCount)
	}
	if edgeCount != 2 {
		t.Errorf("edgeCount=%d, want 2", edgeCount)
	}
	if kindCounts["Function"] != 2 {
		t.Errorf("kindCounts[Function]=%d, want 2", kindCounts["Function"])
	}
	if kindCounts["Class"] != 1 {
		t.Errorf("kindCounts[Class]=%d, want 1", kindCounts["Class"])
	}
	if edgeKindCounts["CALLS"] != 1 {
		t.Errorf("edgeKindCounts[CALLS]=%d, want 1", edgeKindCounts["CALLS"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteSymbolsForFile
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteSymbolsForFile_ClearsEdges(t *testing.T) {
	store := newTestStore(t)
	pid := "dsff-proj"
	store.UpsertProject(testProject(pid))

	syms := []Symbol{
		testSymbol("dsff-a", "Alpha", "Function", pid, "alpha.go"),
		testSymbol("dsff-b", "Beta", "Function", pid, "beta.go"),
	}
	store.BulkUpsertSymbols(syms)
	edges := []Edge{
		{FromID: "dsff-a", ToID: "dsff-b", Kind: "CALLS", ProjectID: pid},
	}
	store.BulkUpsertEdges(edges)

	// Delete symbols for alpha.go
	if err := store.DeleteSymbolsForFile(pid, "alpha.go"); err != nil {
		t.Fatalf("DeleteSymbolsForFile: %v", err)
	}

	// dsff-a should be gone
	sym, err := store.GetSymbol("dsff-a")
	if err != nil {
		t.Fatalf("GetSymbol after delete: %v", err)
	}
	if sym != nil {
		t.Error("dsff-a still exists after DeleteSymbolsForFile")
	}
	// dsff-b should still exist
	sym2, err := store.GetSymbol("dsff-b")
	if err != nil {
		t.Fatalf("GetSymbol dsff-b: %v", err)
	}
	if sym2 == nil {
		t.Error("dsff-b was incorrectly deleted")
	}
}

func TestDeleteSymbolsForFile_NonExistent_NoError(t *testing.T) {
	store := newTestStore(t)
	// Deleting symbols for a file that doesn't exist should not error
	if err := store.DeleteSymbolsForFile("any-proj", "nonexistent.go"); err != nil {
		t.Errorf("DeleteSymbolsForFile nonexistent: %v", err)
	}
}

func TestDeleteEmptyProjects(t *testing.T) {
	s := newTestStore(t)

	// Three projects: one real, one empty (ghost), one with only edges.
	real := testProject("real")
	real.SymCount = 42
	real.EdgeCount = 7
	s.UpsertProject(real)

	ghost := testProject("ghost")
	ghost.SymCount = 0
	ghost.EdgeCount = 0
	s.UpsertProject(ghost)

	edgesOnly := testProject("edges-only")
	edgesOnly.SymCount = 0
	edgesOnly.EdgeCount = 3
	s.UpsertProject(edgesOnly)

	n, err := s.DeleteEmptyProjects()
	if err != nil {
		t.Fatalf("DeleteEmptyProjects: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted count = %d, want 1", n)
	}

	// Only the ghost should be gone.
	if got, _ := s.GetProject("ghost"); got != nil {
		t.Error("ghost project should be deleted")
	}
	if got, _ := s.GetProject("real"); got == nil {
		t.Error("real project should survive")
	}
	if got, _ := s.GetProject("edges-only"); got == nil {
		t.Error("edges-only project should survive (has edges)")
	}

	// Idempotent: a second call has nothing to do.
	n2, err := s.DeleteEmptyProjects()
	if err != nil {
		t.Fatalf("second DeleteEmptyProjects: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second call deleted count = %d, want 0", n2)
	}
}

// TestRebuildFTS_HappyPath asserts that calling RebuildFTS on a healthy
// index returns the symbol count and leaves search working. This is the
// no-op case — proves the rebuild path produces an equivalent index.
func TestRebuildFTS_HappyPath(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "auth.go", Name: "AuthService",
			QualifiedName: "auth.AuthService", Kind: "Class", Language: "Go"},
		{ID: "s2", ProjectID: "p1", FilePath: "user.go", Name: "UserService",
			QualifiedName: "user.UserService", Kind: "Class", Language: "Go"},
		{ID: "s3", ProjectID: "p1", FilePath: "auth.go", Name: "Login",
			QualifiedName: "auth.Login", Kind: "Function", Language: "Go"},
	})

	rows, err := s.RebuildFTS()
	if err != nil {
		t.Fatalf("RebuildFTS: %v", err)
	}
	if rows != 3 {
		t.Errorf("rebuilt rows = %d, want 3", rows)
	}

	results, err := s.SearchSymbols("p1", "auth*", "", "", 10)
	if err != nil {
		t.Fatalf("post-rebuild SearchSymbols: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected post-rebuild results for 'auth*'")
	}
}

// TestRebuildFTS_RestoresAfterCorruption simulates a drift scenario where
// the FTS5 index has rows that no longer match `symbols`, then verifies
// rebuild restores parity. This is the actual escape-hatch use case.
//
// We simulate corruption by deleting symbols rows directly via raw SQL
// (bypassing the trigger contract) — the FTS5 index is left holding
// stale entries pointing at removed rows. Search then returns ghost
// hits, which is exactly the symptom users would see in a real bug.
func TestRebuildFTS_RestoresAfterCorruption(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "auth.go", Name: "Login",
			QualifiedName: "auth.Login", Kind: "Function", Language: "Go"},
		{ID: "s2", ProjectID: "p1", FilePath: "auth.go", Name: "Logout",
			QualifiedName: "auth.Logout", Kind: "Function", Language: "Go"},
	})

	// Confirm baseline: both rows match.
	pre, err := s.SearchSymbols("p1", "Log*", "", "", 10)
	if err != nil {
		t.Fatalf("baseline SearchSymbols: %v", err)
	}
	if len(pre) != 2 {
		t.Fatalf("baseline rows = %d, want 2", len(pre))
	}

	// Disable triggers and remove a symbol — this leaves the FTS5 index
	// holding a ghost entry. Real-world corruption is structurally similar:
	// the FTS shadow table holds rowids that the symbols table no longer has.
	if _, err := s.DB().Exec(`DROP TRIGGER sym_fts_corpus_delete`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := s.DB().Exec(`DELETE FROM symbols WHERE id='s2'`); err != nil {
		t.Fatalf("orphan delete: %v", err)
	}

	// Rebuild restores the FTS index from the canonical symbols table.
	rows, err := s.RebuildFTS()
	if err != nil {
		t.Fatalf("RebuildFTS: %v", err)
	}
	if rows != 1 {
		t.Errorf("rebuilt rows = %d, want 1", rows)
	}

	post, err := s.SearchSymbols("p1", "Log*", "", "", 10)
	if err != nil {
		t.Fatalf("post-rebuild SearchSymbols: %v", err)
	}
	if len(post) != 1 {
		t.Errorf("post-rebuild rows = %d, want 1", len(post))
	}
	if len(post) == 1 && post[0].Symbol.ID != "s1" {
		t.Errorf("post-rebuild kept wrong row: %s", post[0].Symbol.ID)
	}
}

// TestRebuildFTS_TriggersRestored confirms that after rebuild, the
// auto-sync triggers are reinstalled — subsequent inserts must show up
// in search without a second rebuild.
func TestRebuildFTS_TriggersRestored(t *testing.T) {
	s := newTestStore(t)
	s.UpsertProject(testProject("p1"))
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s1", ProjectID: "p1", FilePath: "auth.go", Name: "Login",
			QualifiedName: "auth.Login", Kind: "Function", Language: "Go"},
	})

	if _, err := s.RebuildFTS(); err != nil {
		t.Fatalf("RebuildFTS: %v", err)
	}

	// Insert a new symbol AFTER rebuild — the triggers should fire and
	// it should be searchable.
	s.BulkUpsertSymbols([]Symbol{
		{ID: "s2", ProjectID: "p1", FilePath: "user.go", Name: "Register",
			QualifiedName: "user.Register", Kind: "Function", Language: "Go"},
	})

	results, err := s.SearchSymbols("p1", "Register", "", "", 10)
	if err != nil {
		t.Fatalf("post-rebuild SearchSymbols: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("triggers not restored — got %d hits, want 1", len(results))
	}
}

// TestRebuildFTS_EmptyDB asserts the no-symbols case returns 0, no error.
// Edge case: a fresh install with no projects yet should still rebuild
// cleanly without panicking.
func TestRebuildFTS_EmptyDB(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.RebuildFTS()
	if err != nil {
		t.Fatalf("RebuildFTS on empty: %v", err)
	}
	if rows != 0 {
		t.Errorf("empty rebuild rows = %d, want 0", rows)
	}
}
