# Helm chart (prototype, #661)

pincher ships a single-tenant Helm chart under `packaging/helm/pincher`. It
mirrors the documented v1.0 deployment boundary: **one Deployment per release,
one persistent SQLite + WAL volume, HTTP gateway on a ClusterIP Service**.

> **Status: prototype.** The chart works against vanilla Kubernetes 1.27+ and
> has been smoke-tested on `kind`. Production hardening (NetworkPolicy,
> PodDisruptionBudget, HorizontalPodAutoscaler) is intentionally out of scope
> — pincher's SQLite store is single-writer (#51), so HPA is unsafe by
> design. Front the Service with your own ingress / mTLS sidecar.

## Install

```bash
# Local prototype install from the repo
helm install pincher ./packaging/helm/pincher \
  --namespace pincher --create-namespace

# With auth (Secret must already exist in the namespace, key: http-key)
kubectl -n pincher create secret generic pincher-http \
  --from-literal=http-key="$(openssl rand -hex 32)"
helm install pincher ./packaging/helm/pincher \
  --namespace pincher \
  --set auth.enabled=true \
  --set auth.secretName=pincher-http

# With the streamable-HTTP MCP transport (#651) mounted on the same Service
helm install pincher ./packaging/helm/pincher \
  --namespace pincher \
  --set mcpHTTPPath=/mcp
```

## Health & readiness (#660)

The chart wires Kubernetes probes to the dedicated endpoints introduced in
#660:

| Probe | Path | Meaning |
|---|---|---|
| `livenessProbe` | `/v1/health` | "Process alive, DB reachable" — restart on failure. |
| `readinessProbe` | `/v1/ready` | "Index drained, accepting query traffic" — withhold traffic during initial scan. |

Splitting the two prevents the indexer's first-pass scan (can be minutes on a
fresh PVC of a large monorepo) from triggering a CrashLoopBackOff while
still keeping the pod off the Service `Endpoints` list until it can answer
queries.

## Single-writer reminder

Do not scale `replicaCount` above 1 unless you've sharded the PVC per
replica — out of scope for the prototype. The chart's `strategy: Recreate`
makes the two-pod overlap impossible during rolling updates.

## Supervised mode (#352)

Set `supervised.enabled=true` to enable auto-restart-on-drift inside the
container. Combined with an image-tag bump on the Deployment, the
container's entrypoint detects a freshly-installed pincher.exe in PATH and
respawns transparently on the next MCP tool call — no manual `/mcp`
reconnect required. Most clusters won't need this (they'll roll the pod via
`helm upgrade` instead), but it's wired for parity with the local dogfood
loop.

## Values reference

See `packaging/helm/pincher/values.yaml` for the full schema with comments.
The toggles you'll touch most:

- `image.tag` — defaults to the chart's `appVersion`.
- `persistence.size` — PVC size for `/var/lib/pincher` (SQLite + WAL).
- `persistence.storageClassName` — leave empty to use the cluster default.
- `auth.enabled` + `auth.secretName` — bearer-token gateway protection.
- `mcpHTTPPath` — set non-empty to expose streamable-HTTP MCP (#651).
- `resources` — defaults sized for a single-tenant repo of ~100k symbols.

## Uninstall

```bash
helm uninstall pincher --namespace pincher
# PVC is preserved by default — delete explicitly if you don't need the
# indexed state.
kubectl -n pincher delete pvc pincher-data
```
