# Troubleshooting

Top recurring friction items from the dogfood log, with remediation. If your issue isn't here, file at https://github.com/kwad77/pincher/issues with `pincher --version` and `pincher doctor` output.

## "Tool not appearing in my MCP client"

**Symptom:** A new pincher tool is in the binary but the client's tool palette doesn't list it.

**Cause:** the client cached the previous `tools/list` payload at session start; pincher's `tools_list_changed` notification didn't reach it.

**Fix:** reconnect the MCP server in the client (e.g. `/mcp` in Claude Code) so the client re-issues `tools/list`. After v0.55+ the supervised binary auto-detects on-disk drift and respawns; older binaries need manual reconnect.

## "Search / trace returns 0 rows on this project but I'm sure the symbol is there"

**Symptom:** `search query="MyFunc"` or `trace name=MyFunc` returns nothing even though the symbol clearly exists in the source.

**Cause:** index is stale (file changed since last index), OR the project hasn't been indexed at all, OR the running binary is newer than the index's schema_version.

**Fix:** run `health` to confirm staleness:

```
mcp__pincher__health project=MyProject
```

Look at the `index_drift` field. If true → re-index:

```
mcp__pincher__index path=/abs/path/to/MyProject force=true
```

The `force=true` flag bypasses the per-file content-hash cache so every file re-extracts.

## "Trace / dead_code returns empty on a TypeScript / C / Rust / etc. project"

**Symptom:** Graph-shaped tools (`trace`, `dead_code`, `neighborhood`'s graph view) return empty-but-valid responses on non-Go projects.

**Cause:** Cross-file edge resolution covers Go (full) and Python (partial). Other languages extract symbols fine but produce a zero-edge graph — `trace` / `dead_code` are silent no-ops there. See `_meta.empty_reason == "cross_file_unavailable"` on the response.

**Fix:** Use `search` / `symbol` / `neighborhood` (file-scope) for navigation on non-Go projects. Cross-file edges for TS/Rust/Java are tracked in #1177/#1182/#1183.

## "Dashboard panels say 'No tool calls recorded in the last 7 days' even though I've made calls"

**Symptom:** Overview tab's `Tool Call Breakdown` / `Calls by Complexity Tier` / `Response Payload Size` panels are empty.

**Cause:** the SessionFlusher hasn't flushed yet — events are buffered and flushed every 10 seconds. OR the project hasn't accumulated 7 days of data.

**Fix:** wait 10–15 seconds and refresh. If it persists, check `pincher stats` from CLI to confirm events are being recorded.

## "WAL file (`pincher.db-wal`) is much larger than the DB"

**Symptom:** `pincher.db` is e.g. 200 MB, but `pincher.db-wal` is 2 GB. Disk pressure climbs.

**Cause:** SQLite WAL bounding (`journal_size_limit=256 MB` + `CheckpointTruncate()` at every index pass) is a SOFT cap — if checkpoints can't complete because readers pin the WAL across the truncate, the WAL grows past the limit. The `doctor` advisory `walBloatAdvisory` fires when WAL > 512 MB or > 10% of DB.

**Fix:**

```bash
pincher vacuum
```

Rewrites the DB file and truncates the WAL. Heavy operation (file-size proportional) but reclaims the space.

## "Database is unusually large (multi-GB) but I only indexed a few projects"

**Symptom:** `~/AppData/Roaming/pincherMCP/pincher.db` (Windows) or `~/.pincher/pincher.db` is 5+ GB. `pincher doctor` surfaces the `largeDBAdvisory`.

**Cause:** stale projects accumulated by older binaries — projects whose on-disk path no longer exists, OR projects indexed by an obsolete schema and untouched for 30+ days.

**Fix:**

```bash
# Remove projects whose on-disk path is gone:
mcp__pincher__list prune_dead=true

# Drop projects indexed by an old schema and untouched for 30+ days:
pincher project prune-stale

# Reclaim disk after pruning (SQLite doesn't shrink the file on row deletion):
pincher vacuum
```

## "context.callees lists a method that the source clearly doesn't call"

**Symptom:** `context` on a Go function reports a callee that's actually a struct-field access in the source, not a method invocation.

**Cause:** local-variable type-inference gap. If the receiver's type comes from a same-name struct field on another type, the resolver false-binds. Largely fixed in #1134 (v0.69) for the `for _, x := range receiver.Field` pattern; other shapes may still surface.

**Fix:** treat the callee list as informational; double-check by reading the source via `context`'s primary symbol body.

## "`pincher doctor` says my project is indexed but `health` says `index_drift=true`"

**Symptom:** Project is on disk, indexed, but `health` reports a binary-version drift.

**Cause:** the running pincher binary was upgraded since the project was last indexed; some resolution-dependent edges may reflect older rules.

**Fix:** re-index to refresh:

```
mcp__pincher__index path=<project path> force=true
```

Or wait for the auto-drift-re-index to fire on the next file change in that project.

## "OTLP traces aren't reaching my collector"

**Symptom:** `OTEL_EXPORTER_OTLP_ENDPOINT` is set, but spans don't appear in the collector.

**Diagnostic ladder:**

1. `mcp__pincher__health` → look at `observability.traces_otlp`. If it shows `"off"`, the env var wasn't visible to the MCP child — set it in the parent shell that launched the client, or in the client's MCP server config.
2. If it shows `"on (OTLP/HTTP → http://...)"`, the exporter initialized successfully. Spans are emitted batched; wait ~10s for the first batch.
3. Check that the collector endpoint accepts OTLP/HTTP (not gRPC) — pincher uses `otlptracehttp`. Set `OTEL_EXPORTER_OTLP_TRACES_INSECURE=1` for plain-HTTP collectors in dev.
4. Confirm the `traces_otlp` capability appears in `_meta.capabilities` on a tool response — that's the runtime confirmation the tracer is wired.

## "How do I see what pincher is doing right now?"

- **Live indexing progress:** `mcp__pincher__index path=... ` returns progress in the response envelope.
- **Live event stream:** `curl http://localhost:8080/v1/events` (SSE) for `index_started` / `index_complete` / `binary_drift` events as they happen.
- **Dashboard:** start the HTTP server (`pincher --http :8080`) and open `http://localhost:8080/v1/dashboard`. Auto-refreshes every 30s.

## "How do I export pincher's metrics for Prometheus scraping?"

Already wired. With `pincher --http :8080` running:

```
http://localhost:8080/v1/metrics
```

Returns Prometheus text exposition format with the standard counters / histograms / gauges documented in [REFERENCE.md → Observability](REFERENCE.md#observability-1163-654-628).

## Still stuck?

- `pincher doctor` output is the canonical first-look at install state.
- `pincher health` is the canonical first-look at running-server state.
- File at https://github.com/kwad77/pincher/issues with both outputs attached.
