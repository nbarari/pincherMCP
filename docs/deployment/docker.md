# Docker

Single-container pincher with the HTTP REST gateway exposed. Suitable for
sidecar deployments in coding agent containers, local docker-compose
stacks, or as the base layer for the [Helm chart](./helm.md).

Image: `ghcr.io/kwad77/pinchermcp:latest` (tracks master) or
`ghcr.io/kwad77/pinchermcp:v0.84.0` (pinned). Built from
[`Dockerfile`](../../Dockerfile) at the repo root; pure-Go binary on
`gcr.io/distroless/static-debian12:nonroot` (~15 MB image).

## Quickstart

```bash
docker run -d --name pincher \
  -v pincher-data:/data \
  -p 8080:8080 \
  -e PINCHER_HTTP_ADDR=:8080 \
  -e PINCHER_HTTP_KEY="$(openssl rand -hex 32)" \
  ghcr.io/kwad77/pinchermcp:latest
```

Verify:

```bash
curl -H "Authorization: Bearer $PINCHER_HTTP_KEY" \
  http://localhost:8080/v1/health
```

The container runs `pincher --http :8080` as the default command. SQLite
DB + WAL live in the `/data` volume.

## Indexing a host directory

Pincher needs read access to the source tree it indexes. Mount the host
directory read-only and pass its path through the `index` tool:

```bash
docker run -d --name pincher \
  -v pincher-data:/data \
  -v /path/to/your/repo:/workspace:ro \
  -p 8080:8080 \
  -e PINCHER_HTTP_ADDR=:8080 \
  ghcr.io/kwad77/pinchermcp:latest

curl -X POST -H "Content-Type: application/json" \
  -d '{"path":"/workspace"}' \
  http://localhost:8080/v1/index
```

For multiple repos, mount each under a distinct path and call `index`
once per path. Pincher's per-project isolation handles them independently.

## docker-compose

```yaml
# docker-compose.yml
services:
  pincher:
    image: ghcr.io/kwad77/pinchermcp:latest
    container_name: pincher
    ports:
      - "8080:8080"
    environment:
      - PINCHER_HTTP_ADDR=:8080
      - PINCHER_HTTP_KEY=${PINCHER_HTTP_KEY}
    volumes:
      - pincher-data:/data
      - ${REPO_PATH:-./}:/workspace:ro
    restart: unless-stopped

volumes:
  pincher-data:
```

## MCP transport (streamable-HTTP)

To make the running container reachable as an MCP server (not just the
REST gateway), enable the streamable-HTTP transport via env var:

```bash
docker run -d --name pincher \
  -v pincher-data:/data \
  -p 8080:8080 \
  -e PINCHER_HTTP_ADDR=:8080 \
  -e PINCHER_MCP_HTTP_PATH=/mcp \
  ghcr.io/kwad77/pinchermcp:latest
```

The MCP transport mounts at `http://localhost:8080/mcp`. See
[`docs/streamable-http.md`](../streamable-http.md) for the per-host
client wiring.

## Resource sizing

Pincher's memory footprint is dominated by the running SQLite cache
plus per-index batches. On the bench corpus (pincher-repo itself,
~685 files, ~6,931 symbols):

| Resource | Idle | Indexing | Steady-state queries |
|---|---|---|---|
| RAM (RSS) | ~25 MB | ~110 MB peak | ~45 MB |
| Disk (DB + WAL) | ~14 MB | grows during pass | ~14 MB after checkpoint |
| CPU | <1% | 1–2 cores for ~5 s | <5% per call |

Larger codebases scale roughly linearly with file count. The
`pincher doctor` advisory fires if WAL size exceeds 256 MB (see
[`docs/troubleshooting.md`](../troubleshooting.md)).

## Upgrading

```bash
docker pull ghcr.io/kwad77/pinchermcp:latest
docker stop pincher && docker rm pincher
# rerun the docker run from above
```

The on-disk SQLite schema migrates automatically on startup. Index
state survives upgrades; schema-version bumps trigger a one-shot
re-index pass on the next `index` call.

## Health monitoring

The container exposes these endpoints:

| Endpoint | Purpose | Auth required |
|---|---|---|
| `/v1/health` | Schema version, per-language coverage, advisory list | No (lightweight) |
| `/v1/healthz` | Kubernetes liveness probe (always 200 if process up) | No |
| `/v1/readyz` | Kubernetes readiness (200 once index is queryable) | No |
| `/v1/doctor` | Full diagnostic — DB size, WAL, recent failures | Yes (if `PINCHER_HTTP_KEY` set) |

## Related

- [Helm chart](./helm.md) — production-shaped Kubernetes deployment
- [`docs/streamable-http.md`](../streamable-http.md) — MCP transport wiring
- [`docs/troubleshooting.md`](../troubleshooting.md) — common operational issues
- [`packaging/README.md`](../../packaging/README.md) — every supported install path

---

_Last reviewed: v0.75 (#1334)._
