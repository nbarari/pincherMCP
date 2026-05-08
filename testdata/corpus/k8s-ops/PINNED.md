# Pinned corpus: `k8s-ops`

Small synthetic IaC layout used by the corpus-snapshot tooling (#33) to
exercise the YAML/JSON Setting extractor (PR #23) at the scale where it
matters: multi-document Kubernetes manifests, Helm `values.yaml`, and
docker-compose.

Hand-crafted rather than vendored from a real Helm chart so:

- Setting counts are eyeball-verifiable (every key in this corpus traces
  back to a specific construction here).
- The corpus stays stable across upstream chart upgrades.
- Multi-document YAML (`---` separators) gets a deterministic test —
  multi-doc files produce one `docN` prefix per doc, and the snapshot
  pins both docs' Settings.

## Layout

- `compose.yaml` — services, ports, environment. Single-document.
- `helm/values.yaml` — Helm-style nested mapping with image tags + ports.
- `manifests/deployment.yaml` — multi-document YAML (Deployment + Service)
  to exercise the `docN`-prefix path in the YAML extractor.

## What the snapshot pins

- `Setting` symbol count by qualified-name shape (`services.web.image`,
  `helm.values.image.tag`, etc.)
- Total `Setting` count across the corpus
- `extraction_confidence` stays at 1.0 for the YAML extractor
- File counts: 3 indexed, 0 blocked (none of these are lockfiles)
