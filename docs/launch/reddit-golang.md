# v1.0 r/golang post — skeleton (post day-of-tag)

Placeholder for [#1538](https://github.com/kwad77/pincher/issues/1538) (FILE-T).

A self-text Reddit submission for r/golang, posted day-of-v1.0-tag.

## Target shape

- **Title** — `pincher 1.0: <one-line value prop>` (no clickbait, no "I built").
- **Opening** — what pincher is (1 sentence), what just shipped (1 sentence), link to blog.
- **The technical interesting bit** — Go-specific deep dive: the SQLite-WAL bounding work, the AST extractor architecture, the `buildmode=plugin` deferral rationale (#1540 → ADR-0003). r/golang wants substance, not a marketing post.
- **Open invitation** — issues link, contribute link.
- **NOT** — links to Twitter, dollar-figures, comparisons against named alternatives.

## Tone

Technical-peer voice. Per `feedback_repositioning_tone.md`: forward-looking, no "we were wrong before." Honest about what's NOT in v1.0 (deferred plugin API, team mode).

## When this file becomes content

v0.97 release-prep: lock the technical-interesting-bit selection. v1.0 tag day: post.

## Don't do this in this PR

Write the post. The technical-deep-dive can only be locked once the v1.0 surface itself is locked.
