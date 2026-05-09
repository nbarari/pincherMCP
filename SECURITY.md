# Security Policy

pincherMCP is a local-first code intelligence server. It runs on a developer's
machine, indexes local repositories, and exposes that index to coding agents
over MCP (stdio) or HTTP. The threat surface is small but not zero — this
document describes what we treat as in-scope and how to report a vulnerability.

## Supported versions

Pre-1.0, only the latest tagged release receives security fixes. We also
respond to vulnerabilities found in `master`. Older tags are point-in-time
snapshots and are not patched.

| Version | Supported |
|---------|-----------|
| `master` | ✅ |
| latest tagged release | ✅ |
| earlier tags | ❌ — upgrade |

Once 1.0 ships, the `1.x` line will receive security fixes and a deprecation
window will be defined for prior majors.

## Reporting a vulnerability

**Do not open a public issue for security reports.** Instead, use one of:

- **GitHub Security Advisory** (preferred): open a private advisory at
  <https://github.com/kwad77/pincherMCP/security/advisories/new>. This routes
  directly to the maintainer and lets us collaborate on a fix before disclosure.
- **Email**: `kevinwaddell@gmail.com` with `[pincher security]` in the subject.

Include enough information for the issue to be reproduced:

- Affected version (output of `pincher --version` or commit SHA)
- A minimal reproducer (corpus shape, command line, MCP / HTTP request)
- Observed vs expected behaviour
- Whether you've already shared this with anyone else

We aim to acknowledge reports within 72 hours and ship a fix or workaround
within two weeks for high-severity issues. We will credit reporters in the
release notes unless they prefer to remain anonymous.

## In scope

- Code execution from a malformed corpus (a crafted file that crashes or
  RCEs the indexer or extractor)
- HTTP authentication bypass (`--http-key` / bearer-token enforcement)
- Path traversal or arbitrary-file-read via tool arguments
- SQL injection via tool arguments (the Cypher executor, search filters)
- Resource exhaustion (panic, hang, unbounded memory) reproducible from
  pathological but well-formed input. The 4 MB per-file size cap (`#111`)
  and 2-second Jinja2 parse timeout are explicit defenses; gaps are bugs.
- Information disclosure across project boundaries (`#2` / `#7` / `#92`).
  The defence-in-depth is project-scoped lookups and per-project edges.
  A path that surfaces another project's row is a security report.
- Dashboard XSS or CSP bypass (`script-src 'self'` is enforced;
  `data-action*` event delegation replaces all inline handlers).

## Out of scope

- Findings that require local code execution on the same machine pincher
  is running on. Pincher is single-tenant local software; an attacker
  who can already read your filesystem or exec commands as you can do
  arbitrary things regardless.
- Rate-limiting or DoS via repeated valid requests against an open HTTP
  port. Run `--http-key` (or front the server with a reverse proxy that
  does auth/rate-limiting) if you expose pincher to a network.
- The optional Homebrew formula or Docker image. Report those to the
  respective package maintainers.

## Hardening defaults

The server ships with conservative defaults documented in `CLAUDE.md`:

- HTTP API requires `--http-key` for production deployments. Without it,
  the API is open and intended only for local-loopback or dev use.
- The bloat-trap (`internal/index/bloat_trap.go`) refuses to index `/`,
  `$HOME`, or paths without project markers in hook mode.
- The indexer caps per-file reads at 4 MB by default
  (`--max-file-size-mb`).
- The cross-process project lockfile prevents concurrent indexers from
  contending on the SQLite WAL.

If a vulnerability hinges on disabling these defaults, please mention the
flag(s) in your report — we may treat it as a documentation issue rather
than a code change.
