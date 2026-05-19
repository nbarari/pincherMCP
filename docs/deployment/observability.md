# Observability — operator's guide

This is the single source for operators running pincher in production. Three surfaces — metrics, traces, structured logs — each independently enabled, each interoperable with the standard Go/OTel/Prometheus ecosystem. None of them are required for pincher to function; they're optional production hardening.

## Surface 1 — Prometheus metrics

**Endpoint:** `GET /v1/metrics` (always on when the HTTP gateway is enabled via `--http :PORT`).

**Format:** Standard Prometheus text exposition. Scraped by any Prometheus-compatible collector (Prometheus, VictoriaMetrics, Grafana Agent, OpenTelemetry Collector, …).

**Auth:** Honors `--http-key` if set (pass `Authorization: Bearer <key>` on the scrape).

**Metric inventory:**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `pincher_tool_calls_total` | counter | `tool`, `outcome` | Per-tool call count. `outcome` is `ok` / `error` / `empty`. |
| `pincher_tool_latency_seconds` | summary | `tool` | Per-tool wall-clock latency. Sum + count for rate(); percentiles via Prometheus `histogram_quantile` over the count rate (the exposition emits sum + count, not pre-aggregated quantiles, so a single global view scales to any window). |
| `pincher_tool_tokens_saved_total` | counter | `tool` | Cumulative tokens saved per the methodology in `docs/methodology/token-savings.md` (FILE-A). |
| `pincher_index_files_total` | counter | `outcome` | Files processed by `index`. `outcome` ∈ {indexed, skipped, blocked, deleted}. |
| `pincher_index_symbols_total` | counter | (none) | Cumulative symbols extracted across all index runs. |
| `pincher_db_size_bytes` | gauge | (none) | SQLite DB file size. Refreshed by a background ticker every 30s. |
| `pincher_wal_size_bytes` | gauge | (none) | SQLite WAL file size. Refreshed alongside `pincher_db_size_bytes`. |

The list above is the v1.0 surface declaration per [ADR-0002](../adr/0002-v1-frozen-surface.md). Adding a metric in a 1.x minor is non-breaking. Renaming or removing one is a 2.0 breaking change.

**Example scrape config (Prometheus):**

```yaml
scrape_configs:
  - job_name: pincher
    metrics_path: /v1/metrics
    scrape_interval: 30s
    static_configs:
      - targets: ['localhost:8080']
    authorization:
      type: Bearer
      credentials: <your-http-key>
```

**Dashboard:** The Grafana JSON for the recommended panel set lives at [`docs/deployment/grafana-dashboard.json`](grafana-dashboard.json) (forthcoming with FILE-G's follow-up).

## Surface 2 — OTLP traces

**Default:** Off. Pincher does not emit traces unless the operator opts in.

**Enable:** Set `OTEL_EXPORTER_OTLP_ENDPOINT` to your collector's OTLP/HTTP endpoint before pincher starts. Standard OTel SDK env-var contract — pincher honors `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL` (must be `http/protobuf`), `OTEL_EXPORTER_OTLP_HEADERS` for auth tokens.

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=https://otel-collector.example.com:4318
export OTEL_EXPORTER_OTLP_HEADERS='authorization=Bearer my-token'
pincher supervised --http :8080
```

**What gets traced:**

- Every MCP tool call becomes one root span: `pincher.tool.<name>`. Attributes: `tool`, `project_id`, `outcome`, `tokens_saved`, `tokens_used`, `latency_ms`.
- Every SQL query inside a tool call becomes a child span: `db.query.<operation>` (`select`, `update`, `migrate`). Slow queries (above the threshold per `pincher doctor`'s slow-query log) carry `slow=true`.
- Index passes become root spans `pincher.index.run` with per-file child spans when in verbose mode (`PINCHER_TRACE_INDEX_FILES=1` — verbose mode adds a span per file, can blow up the trace volume on large repos; opt-in).

When `OTEL_EXPORTER_OTLP_ENDPOINT` is unset, `pincher health` reports `traces_otlp: off`. When it's set, `pincher health` reports `traces_otlp: on (endpoint=...)`.

**Auth contract:** Same as the standard OTel SDK. Headers via `OTEL_EXPORTER_OTLP_HEADERS=key=value,key2=value2`.

**Sampling:** Honored via `OTEL_TRACES_SAMPLER=parentbased_traceidratio` + `OTEL_TRACES_SAMPLER_ARG=0.1`. Default is always-on (1.0); production deployments should lower this.

## Surface 3 — structured logs (slog)

**Format:** JSON when `pincher` runs with `PINCHER_LOG_FORMAT=json` (recommended for production); text otherwise.

**Level:** Configurable via `PINCHER_LOG_LEVEL` ∈ {`debug`, `info`, `warn`, `error`}. Default is `info`.

**Log shape:**

```json
{
  "time": "2026-05-18T16:30:00Z",
  "level": "INFO",
  "msg": "pincher.tool.completed",
  "tool": "search",
  "project_id": "abc123",
  "outcome": "ok",
  "tokens_saved": 4321,
  "latency_ms": 12
}
```

Standard slog message-name pattern: `pincher.<area>.<event>`. Areas include `tool`, `index`, `watch`, `db`, `auto_restart`, `auth`, `cancellation`. Events are past-tense for completions (`completed`, `started`) and noun-form for state checks (`drift_detected`).

**Recommended deployment:**

```bash
export PINCHER_LOG_FORMAT=json
export PINCHER_LOG_LEVEL=info
pincher supervised --http :8080 --http-key "$PINCHER_HTTP_KEY"
```

Ship the JSON log stream to your log aggregator (Loki, Datadog, Splunk, fluentd, etc.). Every record has a `time` (RFC3339), `level`, and `msg` field — the standard slog JSON contract — so any pipeline that understands slog or zerolog works.

## What's NOT in v1.0 observability

- **`metrics_prometheus` capability auto-discovery via SRV records.** Service-discovery integration is out of scope; configure scrape targets explicitly.
- **Tail-based sampling.** Pincher only does head-based (rate-controlled at span creation). Tail sampling lives at the OTel Collector layer if needed.
- **Distributed-tracing context propagation across pincher → external service boundaries.** Pincher's `fetch` tool doesn't propagate the parent trace context as an outgoing HTTP header. v1.x consideration.
- **Per-request tracing in the dashboard UI.** Dashboard panels show metrics + recent slow-query log; trace IDs aren't displayed inline. Click-through to your OTel UI (Jaeger, Grafana Tempo) is the recommended path.

## Quick "is observability working?" check

```bash
# Metrics endpoint reachable + emitting?
curl -fsS http://localhost:8080/v1/metrics | grep '^pincher_'

# Traces enabled + exporting?
pincher health --json | jq '.capabilities | grep traces_otlp'

# Logs at the expected level?
pincher --version 2>&1 | head -1   # confirms binary; logs go to stderr at start
```

If `pincher health`'s `capabilities` block doesn't list `metrics_prometheus`, the HTTP gateway isn't running — start with `--http :PORT`. If `traces_otlp` shows `off`, the OTLP env var wasn't set at start.

## Refs

- Issue: [#1526 (FILE-G)](https://github.com/kwad77/pincher/issues/1526)
- Sibling: [#1163](https://github.com/kwad77/pincher/issues/1163) — original OTLP traces + Prometheus exporter
- [`internal/server/metrics.go`](../../internal/server/metrics.go) — metric definitions
- [ADR-0002](../adr/0002-v1-frozen-surface.md) — frozen-surface declaration including the metric inventory
