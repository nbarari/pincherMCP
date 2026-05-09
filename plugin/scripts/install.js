#!/usr/bin/env node
// Cross-platform dispatcher for the pincher plugin's binary installer.
// Invoked from hooks/hooks.json on SessionStart. Forwards to the platform-
// specific script (install.sh on POSIX, install.ps1 on Windows) so each
// script can speak its native shell idioms without a polyglot header.
//
// Node is chosen as the dispatcher because every Claude Code install already
// has Node (Claude Code itself is a Node CLI). No third-party modules, no
// install step — just child_process + path.
'use strict';

const { spawnSync } = require('node:child_process');
const path = require('node:path');

const root = process.env.CLAUDE_PLUGIN_ROOT;
if (!root) {
  console.error('pincher-plugin: CLAUDE_PLUGIN_ROOT is unset — aborting');
  process.exit(1);
}

const isWindows = process.platform === 'win32';
const scriptPath = isWindows
  ? path.join(root, 'scripts', 'install.ps1')
  : path.join(root, 'scripts', 'install.sh');

const { cmd, args } = isWindows
  ? { cmd: 'powershell', args: ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', scriptPath] }
  : { cmd: 'sh',          args: [scriptPath] };

// Install: stdout is silenced (install scripts log to stderr only) so it
// doesn't pollute the SessionStart hook's stdout, which is reserved for
// the additionalContext JSON envelope produced by `pincher index --hook`
// below. Stderr is inherited so install progress / errors still surface
// in Claude Code's session-start log line.
const result = spawnSync(cmd, args, {
  stdio: ['ignore', 'ignore', 'inherit'],
  env: process.env,
});

if ((result.status ?? 1) !== 0) {
  // Install failed — don't try to prime context against a possibly-missing
  // binary; preserve the install script's exit code so the user sees the
  // right status in the hook log.
  process.exit(result.status ?? 1);
}

// Adoption priming (#138): after a successful install, run
// `pincher index --hook` against the user's project so the SessionStart
// hook injects a "Pincher ready" additionalContext envelope. Without this,
// agents see pincher tools available but are not strongly primed to use
// them — the default Read/Grep wins and pincher tools sit idle.
//
// The binary lives at <plugin-root>/bin/pincher[.exe]. Fall back to
// `pincher` on PATH when the install fast-path symlinked from there.
const fs = require('node:fs');
const binCandidate = isWindows
  ? path.join(root, 'bin', 'pincher.exe')
  : path.join(root, 'bin', 'pincher');
const binPath = fs.existsSync(binCandidate) ? binCandidate : 'pincher';

spawnSync(binPath, ['index', '--hook'], {
  // Run from the user's cwd at hook time — that's the project Claude Code
  // is operating on. `pincher index --hook` derives the project from cwd
  // when no PATH arg is given.
  cwd: process.cwd(),
  // stdout: inherit so the JSON envelope reaches the hook system.
  // stderr: inherit so log lines surface in the hook log.
  stdio: ['ignore', 'inherit', 'inherit'],
  env: process.env,
});

// Non-zero from `index --hook` means the bloat-trap refused the path
// (e.g. SessionStart fired from /tmp or $HOME) or some other error. Exit
// 0 anyway so the SessionStart hook isn't reported as failed — install
// itself succeeded; the missing context is acceptable degradation.
process.exit(0);
