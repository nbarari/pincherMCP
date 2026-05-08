# docs-site corpus

Hand-crafted Markdown corpus exercising the goldmark-backed extractor
(#81) and the docs FTS5 corpus (#32 part 1).

## Why this corpus

The other pinned corpora (`go-project`, `k8s-ops`, `node-monorepo`)
are dominated by code or config. None had enough Markdown content to
exercise:

- Hierarchical heading qualified names (`api_reference.endpoints.get_users_me`)
- Multi-document multi-level structure (`README.md` → `docs/` → `docs/reference/`)
- Cross-document links in prose (resolved by humans, not by pincher)
- Docs-corpus FTS5 routing (`corpus=docs` in `search`)

Five files, ~2 KB total. Every file is hand-written prose with a
realistic heading hierarchy.

## Layout

```
docs-site/
├── README.md
├── CHANGELOG.md
└── docs/
    ├── getting-started.md
    └── reference/
        ├── api.md
        └── cli.md
```

## Pinned at

This corpus is hand-authored content, not pinned to an upstream commit.
Modifications go through `make corpus-snapshot-update` like the other
corpora; the snapshot diff is the rationale.
