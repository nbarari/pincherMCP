# Pinned corpus: `node-monorepo`

Small synthetic Node project used by the corpus-snapshot tooling (#33) to
exercise the file-level blocklist (PR #24) and prove the negative-of-negative:
**real config files like `package.json` and `tsconfig.json` ARE indexed**
while lockfiles, minified bundles, and source maps are NOT.

This is the canonical regression test for #24's blocklist. Without it, a
future change that loosens the blocklist (lockfile Settings explode) or
tightens it past the safe range (legitimate JSON config rejected) shows
up as a snapshot diff.

## Layout

- `package.json` — REAL config. Must produce Settings (name, version,
  scripts.*, dependencies.*).
- `tsconfig.json` — REAL config. Must produce Settings (compilerOptions.*).
- `src/index.ts` — REAL source. Must produce TypeScript symbols.
- `package-lock.json` — LOCKFILE. Must be blocked by `ast.ShouldSkip`.
  **Zero Settings.**
- `src/bundle.min.js` — MINIFIED. Must be blocked by `ast.ShouldSkip`.
  **Zero symbols.**
- `src/index.js.map` — SOURCE MAP. Must be blocked by `ast.ShouldSkip`.
  **Zero symbols.**

## Layered defenses — why the minified bundle lives in `src/`, not `dist/`

Pincher has three independent gates that filter noise files. They run in
order, and each catches a different class:

1. **`gocodewalker` directory exclusion** (first, walker-level). Skips
   well-known build/cache directories: `dist/`, `node_modules/`,
   `vendor/`, `target/`, `.next/`, etc. Files inside these directories
   never reach the indexer's per-file checks.
2. **`ast.ShouldSkip`** (second, file-level). Catches lockfiles by
   exact basename, minified bundles by suffix, source maps by suffix.
   Increments `files_blocked` so the user sees what got filtered.
3. **`ast.IsSourceFile`** (third, language-detection). Silently drops
   files with no registered extractor (Markdown pre-#38, etc.).

If the minified bundle lived under `dist/`, it would be dir-excluded by
gate (1) and never test gate (2)'s minified-suffix path. Putting it in
`src/` exercises the blocklist code that PR #24 added — the snapshot's
`files_blocked` count is the regression gate for that code.

## What the snapshot pins

- `files_blocked` count is exactly **3** (lockfile + minified + source map),
  proving all three blocklist patterns actually fire on files that aren't
  caught by directory exclusion first.
- Setting count from `package-lock.json` is 0 (verified via the snapshot
  not containing a path-derived Setting from that file).
- TypeScript Function/Class counts from `src/index.ts` match expected.
- `package.json` and `tsconfig.json` produce JSON Settings (PR #23
  confidence 1.0) — proves the blocklist isn't over-broadening to refuse
  real JSON config.
