# v1.0 "What's new" video — skeleton (record at v0.98)

Placeholder for [#1538](https://github.com/kwad77/pincher/issues/1538) (FILE-T).

A longer-form (3-5 min) explainer video, recorded at v0.98 when copy is locked. Distinguishes itself from `demo-script.md` (90s screencast) by being talking-head + screen-share + narration.

## Target shape

| Section | Time | Content |
|---|---|---|
| Intro | 0:00-0:30 | What pincher is in one sentence. Why v1.0. The frozen-surface guarantee. |
| The cost picture | 0:30-1:30 | Walk through the FILE-A methodology + FILE-B comparator results. Show ratio numbers on screen, anchored to the methodology doc URL. |
| The architecture | 1:30-2:30 | Three-layer storage (byte-offset / knowledge graph / FTS5). Why SQLite. Single-binary deployment. |
| The composites | 2:30-3:30 | Phase 4 composites (`investigate_failure`, `plan_change`, `audit_unused`, `onboard_module`, `why_empty`). One screen-share per. |
| Hosts | 3:30-4:00 | The host matrix, current conformance status, link to per-host tutorial. |
| Migration + close | 4:00-4:30 | v0 → v1 migration story. Where to file issues. Close. |

## Recording setup

- Lighting: indirect daylight or 5500K key + fill. No on-camera ring-light reflection on glasses.
- Audio: explicit USB mic, NOT laptop mic. Test against a phone playback before final take.
- Screen capture: 1920×1080. Pin the same terminal font as `demo-script.md` for cross-asset consistency.
- B-roll: have one screen recording per composite ready to drop in over narration.

## When this file becomes content

v0.97: lock the section-by-section outline against the real v0.98 binary's UX. v0.98: record. v0.99 RC: re-record any section where the surface shifted.

## Don't do this in this PR

Record. Setup is not the bottleneck; what to say at v1.0 is — and we don't know that yet.
