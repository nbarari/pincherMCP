# Tutorial: pincher HTTP dashboard

About 10 minutes. By the end you'll have a pincher process serving the dashboard at `http://localhost:7777/v1/dashboard`, you'll have hit the REST API with `curl`, and you'll know how to point any HTTP-capable client at pincher.

This walkthrough assumes nothing about pincher's internals. For the long-form manual, see [`docs/REFERENCE.md`](../REFERENCE.md).

## What you need

- **Go 1.25+** on your `PATH` (or a [release binary](https://github.com/kwad77/pincher/releases/latest))
- A Git repository to point pincher at
- `curl` and a browser

## 1. Install and index

```bash
go install github.com/kwad77/pincher/cmd/pinch@latest
cd ~/code/your-project
pincher index
# indexed 42 files, 1238 symbols, 6711 edges in 187ms
```

## 2. Start pincher with HTTP enabled

Two flavours — pick one.

### Local-only (no auth needed)

```bash
pincher --http :7777
```

Loopback binds need no auth — pincher refuses to bind a non-loopback interface without `--http-key` (default-deny remote HTTP, see #199). Open a second terminal for the rest of the tutorial.

### Remote / shared (auth required)

```bash
pincher --http 0.0.0.0:7777 --http-key "$(openssl rand -hex 32)"
```

Note the key — you'll need it on every request:

```bash
export PINCHER_KEY=<the-key-you-just-generated>
curl -H "Authorization: Bearer $PINCHER_KEY" http://your-host:7777/v1/projects
```

If you want the key out of argv, use the `PINCHER_HTTP_KEY` env var instead of `--http-key`. For deployments behind a trusted reverse proxy with its own auth, the explicit escape hatch is `--http-allow-open` (or `PINCHER_HTTP_ALLOW_OPEN=1`).

## 3. Open the dashboard

In your browser:

```
http://localhost:7777/v1/dashboard
```

Or have pincher print the URL for you (useful in scripts or when the port was OS-picked via `:0`):

```bash
pincher web
# http://localhost:7777/v1/dashboard
pincher web --json
# {"url":"http://localhost:7777/v1/dashboard","base":"http://localhost:7777","pid":12345,"started_by":"manual"}
```

The dashboard panels (5 tabs):

- **Overview** — current-session and all-time token-savings cards plus a sparkline of tokens-saved-per-session history.
- **Projects** — every indexed project with file / symbol / edge counts. Filter by name, hide-empty toggle, click a card to expand its languages / hotspots / entry-points panel.
- **Search** — symbol-search form (kind + project filters); results link to project context.
- **ADRs** — Architecture Decision Records, browsable per project; add new entries inline.
- **Sessions** — historical session table with running totals; pulls from the shared `sessions` table that both the MCP stdio process and the HTTP server flush to (so totals reflect every pincher process touching the same data dir).

Header badges show health, last-refresh time, and an Auth button (only required when pincher was started with `--http-key`). The footer links to the OpenAPI spec at `/v1/openapi.json` — for an API explorer beyond the Search tab, that JSON is what you'd point a Postman / Bruno / Insomnia at.

## 4. Hit the REST API directly

Every MCP tool is also a `POST /v1/{tool}`. The body matches the tool's MCP InputSchema; the response is the same shape as the MCP response, including the `_meta` envelope.

### Search for a function

```bash
curl -s http://localhost:7777/v1/search \
  -H 'Content-Type: application/json' \
  -d '{"query": "processPayment", "limit": 5}' | jq
```

```json
{
  "results": [
    {
      "id": "internal/payments/charge.go::ProcessPayment#Function",
      "name": "ProcessPayment",
      "kind": "Function",
      "score": 7.42
    }
  ],
  "_meta": {
    "tokens_used": 312,
    "tokens_saved": 14500,
    "latency_ms": 2,
    "cost_avoided": "$0.0435"
  }
}
```

### Fetch the symbol's source

```bash
curl -s http://localhost:7777/v1/symbol \
  -H 'Content-Type: application/json' \
  -d '{"id": "internal/payments/charge.go::ProcessPayment#Function"}' | jq -r .source
```

That's an O(1) byte-offset seek — pincher never re-parses the file.

### Trace callers

```bash
curl -s http://localhost:7777/v1/trace \
  -H 'Content-Type: application/json' \
  -d '{"id": "internal/payments/charge.go::ProcessPayment#Function", "depth": 2}' | jq
```

## 5. Authenticated requests

If you started pincher with `--http-key`, every request needs an `Authorization: Bearer <key>` header:

```bash
curl -s -H "Authorization: Bearer $PINCHER_KEY" \
  http://localhost:7777/v1/projects | jq
```

The dashboard prompts for the key on first load and stores it in browser-local storage.

## 6. Behind a reverse proxy

For nginx-style fronting with HTTPS at the edge, pass `--basepath` and `--trust-proxy`:

```bash
pincher --http :7777 --http-key "$KEY" --basepath /pincher --trust-proxy
```

Both `/pincher/v1/*` and `/v1/*` route to the same handler; the dashboard URLs respect `X-Forwarded-Prefix` / `X-Forwarded-Proto` / `X-Forwarded-Host`. See [REFERENCE.md → CLI flags](../REFERENCE.md#cli-flags) for the full table.

## What to read next

- **[REFERENCE.md → HTTP REST API](../REFERENCE.md#http-rest-api)** — full request/response examples per tool
- **[REFERENCE.md → CLI flags](../REFERENCE.md#cli-flags)** — every `--http-*` knob
- **[Tutorial: Claude Code](claude-code.md)** — same indexing, MCP stdio transport
- **[Tutorial: Cursor](cursor.md)** — same indexing, Cursor rules file
- **[`packaging/README.md`](../../packaging/README.md)** — Homebrew, systemd, launchd, Windows service, Docker
