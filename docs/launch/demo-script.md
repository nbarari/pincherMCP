# v1.0 demo script — skeleton (record at v0.99 RC)

Placeholder for [#1538](https://github.com/kwad77/pincher/issues/1538) (FILE-T).

The v1.0 launch demo (~90 second screencast) lives here as a shot list and narration script. Recorded against the **v0.99 RC binary**, not before.

## Shot list (target order, ~90s total)

1. **(0-5s)** Fresh terminal. `git clone` a target repo (pincher itself or a small public Go repo). Background: white-on-black, no IDE noise.
2. **(5-15s)** `pincher init && pincher index .` — show the index complete line + symbol count.
3. **(15-35s)** `pincher search "OAuth login"` (or similar agent-sounding query). Show top result. Voice-over: "no setup, no schema config, just BM25 over the code corpus."
4. **(35-55s)** `pincher context id:<top result>` — show source + imports + callees in one response. Voice-over: "one round-trip — the function plus what it depends on."
5. **(55-75s)** `pincher trace id:<id> direction:in` — show inbound callers with risk labels. Voice-over: "what's the blast radius — before you touch it."
6. **(75-90s)** `pincher --http :8080` running in the background; agent (Claude Code or similar) connects via MCP, asks "what calls OAuth login," gets the response. Voice-over: "and the same surface your agent talks to."

## Narration tone

- No marketing adjectives. Verbs only.
- "Saves tokens" → "uses fewer bytes." Per `feedback_no_dollar_figures.md` (memory): no dollar figures.
- Don't name competitors. Per `feedback_no_comparisons_pre_1_0.md`.

## Recording setup

- Terminal font: explicit named ligature font (consistency across replay platforms).
- Window size: 1280×720 minimum (4× zoom on mobile is the realistic viewer experience).
- Capture: asciinema for terminal-only flow + screen-capture for the host-handshake bit. Splice in post.

## When this file becomes content

v0.97 release-prep: lock the shot list against the real binary's UX. v0.98: record once. v0.99 RC: re-record against the RC binary if any tool surface shifted.

## Don't do this in this PR

Record anything. The binary will shift between now and v0.99; the recording would be obsolete.
