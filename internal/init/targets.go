package init

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Target describes one of the editor / agent rule files that
// `pincher init` knows how to seed. Each target writes the policy
// block in its expected location and format; the marker-block
// convention (`<!-- pincher:start --> ... <!-- pincher:end -->`) is
// shared across every target so re-runs replace in place rather than
// duplicating.
//
// Closes #191. Carved out of cmd/pinch in #253 so the MCP server can
// import the target machinery without dragging package main along.
type Target struct {
	// Name is the value the user passes to --target / the MCP
	// `target` arg (e.g. "cursor").
	Name string

	// Describe is the one-line summary shown in --help output and the
	// post-write banner.
	Describe string

	// SupportsGlobal reports whether `--global` is meaningful for
	// this target. Claude maps to ~/.claude/CLAUDE.md when global;
	// cursor / windsurf / aider have no equivalent global rules file
	// (the files live per-project), so passing --global with those
	// is an error. continue is global-only — the file always lives
	// at ~/.continue/config.json regardless of cwd.
	SupportsGlobal bool

	// AlwaysGlobal is true for targets where --global is implied
	// (the rules file is global by design — currently just continue).
	AlwaysGlobal bool

	// PathFn resolves the absolute file path. global is the user's
	// --global value; honored only when SupportsGlobal &&
	// !AlwaysGlobal. cwd is the project root the caller wants paths
	// resolved against — CLI passes os.Getwd(); MCP passes the
	// session project root. Targets ignoring cwd (the AlwaysGlobal
	// continue target) accept it as a no-op for signature uniformity.
	PathFn func(cwd string, global bool) (string, error)

	// DetectFn returns true when a marker file or directory for this
	// editor exists under cwd. Used by --target=detect.
	DetectFn func(cwd string) bool

	// WriteFn produces the new file content given the existing
	// content (may be empty if the file doesn't exist) and the raw
	// policy markdown embedded in the binary. Returns (newContent,
	// action) where action is "wrote" / "updated" / "appended" /
	// "error".
	WriteFn func(existing, policy string) (string, string)
}

// AllTargets is the registry of every editor / agent target the init
// path knows how to write to. Order is meaningful for --target=all
// and detection-priority output ordering.
var AllTargets = []Target{
	ClaudeTarget,
	CursorTarget,
	CursorLegacyTarget,
	WindsurfTarget,
	AiderTarget,
	ContinueTarget,
	CodexTarget,
	ZedTarget,
	GeminiTarget,
	WarpTarget,
	VSCodeTarget,
	VSCodeMCPTarget,
	JetBrainsTarget,
	AntigravityTarget,
}

// FindTarget looks up a target by its --target value.
func FindTarget(name string) (Target, bool) {
	for _, t := range AllTargets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// TargetNames returns the list of valid --target values for help
// text. Order matches AllTargets, with the detect/all aliases
// appended.
func TargetNames() []string {
	out := make([]string, 0, len(AllTargets)+2)
	for _, t := range AllTargets {
		out = append(out, t.Name)
	}
	out = append(out, "detect", "all")
	return out
}

// DetectTargets walks cwd and returns every target whose DetectFn
// returns true. If none match, returns just claude (the safe default).
func DetectTargets(cwd string) []Target {
	var hits []Target
	for _, t := range AllTargets {
		if t.DetectFn != nil && t.DetectFn(cwd) {
			hits = append(hits, t)
		}
	}
	if len(hits) == 0 {
		hits = append(hits, ClaudeTarget)
	}
	return hits
}

// ResolveTargets expands a target name (a single target name,
// "detect", or "all") into the concrete list of Targets to write.
// cwd is the project root used for the "detect" target's marker-file
// scan; pass os.Getwd() from the CLI or the session project root
// from the MCP handler.
func ResolveTargets(name, cwd string) ([]Target, error) {
	switch name {
	case "":
		return nil, fmt.Errorf("--target is required (one of: %s)", strings.Join(TargetNames(), ", "))
	case "detect":
		return DetectTargets(cwd), nil
	case "all":
		// MCP-context callers should filter out AlwaysGlobal targets
		// (continue) before write — `target=all` from an MCP request
		// must not silently escape the project_path. The CLI keeps
		// the historical broad semantic.
		return AllTargets, nil
	}
	t, ok := FindTarget(name)
	if !ok {
		return nil, fmt.Errorf("unknown --target %q (one of: %s)", name, strings.Join(TargetNames(), ", "))
	}
	return []Target{t}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// claude — the original target (./CLAUDE.md, ~/.claude/CLAUDE.md global)
// ─────────────────────────────────────────────────────────────────────────────

var ClaudeTarget = Target{
	Name:           "claude",
	Describe:       "Claude Code: ./CLAUDE.md (or ~/.claude/CLAUDE.md with --global)",
	SupportsGlobal: true,
	PathFn:         ResolveCLAUDEPath,
	DetectFn: func(cwd string) bool {
		_, err := os.Stat(filepath.Join(cwd, "CLAUDE.md"))
		return err == nil
	},
	WriteFn: MergePolicyBlock,
}

// ─────────────────────────────────────────────────────────────────────────────
// cursor (modern, .mdc with YAML frontmatter under .cursor/rules/)
// ─────────────────────────────────────────────────────────────────────────────

const cursorRuleFrontmatter = `---
description: pincher MCP code-intelligence usage policy
globs:
  - "**/*"
alwaysApply: true
---

`

// cursorHookCheckEquivalent ships the Cursor counterpart of Claude
// Code's PreToolUse hook (#627). Cursor doesn't have a runtime hook
// surface, so the redirect is convention-based: this static rule
// preamble tells the agent to prefer pincher tools over Read / Grep on
// indexed files, mirroring the hook's decision logic (#1336 v0.86).
//
// Lives OUTSIDE the marker block in pincher.mdc — `pincher init` only
// touches the marker block on subsequent writes, so this preamble
// survives policy refreshes and only gets re-rendered on a brand-new
// rule file. Users who don't want it can delete the preamble in their
// existing pincher.mdc and it won't grow back.
const cursorHookCheckEquivalent = `# When to use pincher tools (Cursor PreToolUse hook equivalent)

Cursor doesn't have Claude Code's runtime PreToolUse hook ([#627](https://github.com/kwad77/pincher/issues/627)), so the redirect is convention-based here. The contract below mirrors the hook's decision logic from [cmd/pinch/hook_check.go](https://github.com/kwad77/pincher/blob/master/cmd/pinch/hook_check.go).

**Before any Read on a file in this repo:**
- If the path is indexed and the file is >4 KB and has >5 symbols, call ` + "`mcp__pincher__context id=<symbol_id> lite=true`" + ` instead — same retrieval, ~80% smaller payload.
- Get the symbol_id from ` + "`mcp__pincher__search query=<name>`" + ` first.
- Skip the redirect when: the file is a test/spec source (` + "`*_test.go`, `*.spec.ts`, `*.test.jsx`, `tests/**`" + `, etc), under 4 KB, has <5 symbols, or you've already passed an offset/limit.

**Before any Grep on an identifier-shaped pattern:**
- If the pattern is a single camelCase / snake_case / dotted identifier, call ` + "`mcp__pincher__search query=<pattern>`" + ` — BM25 ranking + snippets beat unranked grep matches.
- Skip the redirect when: the pattern has regex metacharacters, is a phrase (contains a space), or no project is indexed yet.

Pincher's own hook redirects mid-flight on Claude Code installs ([#1635](https://github.com/kwad77/pincher/issues/1635) advisory when not installed); under Cursor the agent applies the same rules deliberately.

`

var CursorTarget = Target{
	Name:     "cursor",
	Describe: "Cursor (modern): ./.cursor/rules/pincher.mdc with YAML frontmatter + Read/Grep hook-check equivalent",
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			return "", fmt.Errorf("cursor target has no global variant; rules live per-project")
		}
		return filepath.Join(cwd, ".cursor", "rules", "pincher.mdc"), nil
	},
	DetectFn: func(cwd string) bool {
		_, err := os.Stat(filepath.Join(cwd, ".cursor"))
		return err == nil
	},
	WriteFn: cursorMDCWriter,
}

// cursorMDCWriter wraps the policy in MDX YAML frontmatter on first
// write. On subsequent writes (existing file present), it preserves
// any frontmatter the user has customised and only replaces the
// marker block in the body.
func cursorMDCWriter(existing, policy string) (string, string) {
	if existing == "" {
		body, _ := MergePolicyBlockBare("", policy)
		// #1336 v0.86: prepend the Cursor hook-check equivalent on
		// brand-new writes only. Subsequent `pincher init` runs only
		// touch the marker block, so the preamble survives policy
		// refreshes (and a user who deletes it doesn't get it back).
		return cursorRuleFrontmatter + cursorHookCheckEquivalent + body, "wrote"
	}
	frontmatter, body := SplitMDXFrontmatter(existing)
	mergedBody, action := MergePolicyBlockBare(body, policy)
	return frontmatter + mergedBody, action
}

// SplitMDXFrontmatter returns (frontmatterIncludingTrailingBlank, body).
// If no frontmatter delimiter is found, returns ("", content).
func SplitMDXFrontmatter(content string) (string, string) {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return "", content
	}
	rest := content[4:]
	if strings.HasPrefix(content, "---\r\n") {
		rest = content[5:]
	}
	closeIdx := strings.Index(rest, "\n---\n")
	if closeIdx < 0 {
		closeIdx = strings.Index(rest, "\n---\r\n")
		if closeIdx < 0 {
			return "", content
		}
	}
	end := len(content) - len(rest) + closeIdx + len("\n---\n")
	if end > len(content) {
		end = len(content)
	}
	if end < len(content) && content[end] == '\n' {
		end++
	}
	return content[:end], content[end:]
}

// ─────────────────────────────────────────────────────────────────────────────
// cursor-legacy (./.cursorrules, plain text)
// ─────────────────────────────────────────────────────────────────────────────

var CursorLegacyTarget = Target{
	Name:     "cursor-legacy",
	Describe: "Cursor (legacy): ./.cursorrules plain text",
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			return "", fmt.Errorf("cursor-legacy target has no global variant")
		}
		return filepath.Join(cwd, ".cursorrules"), nil
	},
	DetectFn: func(cwd string) bool {
		_, err := os.Stat(filepath.Join(cwd, ".cursorrules"))
		return err == nil
	},
	WriteFn: MergePolicyBlockBare,
}

// ─────────────────────────────────────────────────────────────────────────────
// windsurf (./.windsurfrules, plain text/markdown)
// ─────────────────────────────────────────────────────────────────────────────

var WindsurfTarget = Target{
	Name:     "windsurf",
	Describe: "Windsurf: ./.windsurfrules plain text/markdown",
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			return "", fmt.Errorf("windsurf target has no global variant")
		}
		return filepath.Join(cwd, ".windsurfrules"), nil
	},
	DetectFn: func(cwd string) bool {
		_, err := os.Stat(filepath.Join(cwd, ".windsurfrules"))
		return err == nil
	},
	WriteFn: MergePolicyBlockBare,
}

// ─────────────────────────────────────────────────────────────────────────────
// aider (./CONVENTIONS.md, the documented Aider convention)
// ─────────────────────────────────────────────────────────────────────────────

var AiderTarget = Target{
	Name:     "aider",
	Describe: "Aider: ./CONVENTIONS.md (Aider's documented convention)",
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			return "", fmt.Errorf("aider --global needs ~/.aider.conf.yml work — not yet implemented; use project CONVENTIONS.md")
		}
		return filepath.Join(cwd, "CONVENTIONS.md"), nil
	},
	DetectFn: func(cwd string) bool {
		if _, err := os.Stat(filepath.Join(cwd, "CONVENTIONS.md")); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(cwd, ".aider.conf.yml")); err == nil {
			return true
		}
		return false
	},
	WriteFn: MergePolicyBlockBare,
}

// ─────────────────────────────────────────────────────────────────────────────
// zed (./.rules, plain markdown — Zed's AI rules convention)
// ─────────────────────────────────────────────────────────────────────────────
//
// Zed AI rules live in `.rules` at the project root (or
// ~/.config/zed/.rules globally). Same plain-markdown shape as
// cursor-legacy and windsurf; the marker-block convention handles
// safe coexistence with other tools that may also write to `.rules`.
// See https://zed.dev/docs/ai/rules. #658 wave-1 init parity.

var ZedTarget = Target{
	Name:           "zed",
	Describe:       "Zed: ./.rules plain markdown (or ~/.config/zed/.rules with --global)",
	SupportsGlobal: true,
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("user home dir: %w", err)
			}
			return filepath.Join(home, ".config", "zed", ".rules"), nil
		}
		return filepath.Join(cwd, ".rules"), nil
	},
	DetectFn: func(cwd string) bool {
		// Project-level marker — `.zed/` directory or `.rules` file at
		// the project root. Either signals the user is on Zed.
		if _, err := os.Stat(filepath.Join(cwd, ".zed")); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(cwd, ".rules")); err == nil {
			return true
		}
		return false
	},
	WriteFn: MergePolicyBlockBare,
}

// ─────────────────────────────────────────────────────────────────────────────
// gemini (./GEMINI.md, ~/.gemini/GEMINI.md global — same shape as claude)
// ─────────────────────────────────────────────────────────────────────────────
//
// Gemini CLI loads project rules from GEMINI.md at the project root
// (or ~/.gemini/GEMINI.md globally), same convention as Claude Code's
// CLAUDE.md. #658 wave-1 init parity.

var GeminiTarget = Target{
	Name:           "gemini",
	Describe:       "Gemini CLI: ./GEMINI.md (or ~/.gemini/GEMINI.md with --global)",
	SupportsGlobal: true,
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("user home dir: %w", err)
			}
			return filepath.Join(home, ".gemini", "GEMINI.md"), nil
		}
		return filepath.Join(cwd, "GEMINI.md"), nil
	},
	DetectFn: func(cwd string) bool {
		if _, err := os.Stat(filepath.Join(cwd, "GEMINI.md")); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(cwd, ".gemini")); err == nil {
			return true
		}
		return false
	},
	WriteFn: MergePolicyBlock,
}

// warp (./WARP.md, ~/.warp/WARP.md global — same shape as claude)
// ─────────────────────────────────────────────────────────────────────────────
//
// Warp Agent loads project-scoped instructions from WARP.md at the
// project root (or ~/.warp/WARP.md globally). Same plain-markdown
// convention as Claude Code's CLAUDE.md and Gemini's GEMINI.md, so the
// marker-block writer is reused without modification. #658 wave-1 init
// parity, last leg.

var WarpTarget = Target{
	Name:           "warp",
	Describe:       "Warp Agent: ./WARP.md (or ~/.warp/WARP.md with --global)",
	SupportsGlobal: true,
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("user home dir: %w", err)
			}
			return filepath.Join(home, ".warp", "WARP.md"), nil
		}
		return filepath.Join(cwd, "WARP.md"), nil
	},
	DetectFn: func(cwd string) bool {
		if _, err := os.Stat(filepath.Join(cwd, "WARP.md")); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(cwd, ".warp")); err == nil {
			return true
		}
		return false
	},
	WriteFn: MergePolicyBlock,
}

// ─────────────────────────────────────────────────────────────────────────────
// vscode (./.github/copilot-instructions.md — GitHub Copilot's documented
// project-rules convention, picked up by VS Code Copilot Chat, Codespaces,
// and GitHub.com when reasoning over the repo)
// ─────────────────────────────────────────────────────────────────────────────
//
// Per GitHub's documented convention, Copilot reads
// `.github/copilot-instructions.md` at the repo root and includes it as
// system context for chat/completion. Plain-markdown shape, so the
// shared MergePolicyBlock writer handles marker-block idempotency
// without modification. The file lives under `.github/` rather than the
// project root, so the writer signature is unchanged but the PathFn
// joins the subdirectory before the filename.
//
// "vscode" is the target name users will reach for; the file is more
// accurately a GitHub Copilot artifact, but VS Code is the dominant
// surface that reads it (Copilot Chat in VS Code, JetBrains Copilot,
// Codespaces all consume the same file). #658 wave-1.5 — VS Code share
// makes this a first-class citizen, not a wave-2 deferral.

var VSCodeTarget = Target{
	Name:     "vscode",
	Describe: "VS Code / GitHub Copilot: ./.github/copilot-instructions.md (project-only, per GitHub's documented convention)",
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			return "", fmt.Errorf("vscode target has no global variant; copilot-instructions.md is per-repo by design")
		}
		return filepath.Join(cwd, ".github", "copilot-instructions.md"), nil
	},
	DetectFn: func(cwd string) bool {
		for _, marker := range []string{
			filepath.Join(".github", "copilot-instructions.md"),
			".vscode",
			filepath.Join(".github", "instructions"),
		} {
			if _, err := os.Stat(filepath.Join(cwd, marker)); err == nil {
				return true
			}
		}
		return false
	},
	WriteFn: MergePolicyBlock,
}

// ─────────────────────────────────────────────────────────────────────────────
// continue (~/.continue/config.json, JSON-string merge into systemMessage)
// ─────────────────────────────────────────────────────────────────────────────

var ContinueTarget = Target{
	Name:         "continue",
	Describe:     "Continue.dev: ~/.continue/config.json (merges into systemMessage)",
	AlwaysGlobal: true,
	PathFn: func(cwd string, global bool) (string, error) {
		_ = cwd
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("user home dir: %w", err)
		}
		return filepath.Join(home, ".continue", "config.json"), nil
	},
	DetectFn: func(cwd string) bool {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		_, err = os.Stat(filepath.Join(home, ".continue"))
		return err == nil
	},
	WriteFn: continueJSONWriter,
}

// continueJSONWriter merges the policy into the `systemMessage` field
// of a Continue config.json. Markers are line-prefixed with `// ` so
// the same scan-and-replace pattern works inside a JSON-escaped string.
func continueJSONWriter(existing, policy string) (string, string) {
	const (
		startMark = "// pincher:start"
		endMark   = "// pincher:end"
	)
	header := "// Managed by `pincher init --target=continue`. Edit `pincher init` to change this block.\n"
	block := startMark + "\n" + header + strings.TrimRight(policy, "\n") + "\n" + endMark

	mergeMessage := func(prev string) (string, string) {
		if prev == "" {
			return block, "wrote"
		}
		startIdx := strings.Index(prev, startMark)
		endIdx := strings.Index(prev, endMark)
		if startIdx >= 0 && endIdx > startIdx {
			afterIdx := endIdx + len(endMark)
			before := strings.TrimRight(prev[:startIdx], "\n")
			after := strings.TrimLeft(prev[afterIdx:], "\n")
			merged := block
			if before != "" {
				merged = before + "\n\n" + merged
			}
			if after != "" {
				merged = merged + "\n\n" + after
			}
			return merged, "updated"
		}
		return strings.TrimRight(prev, "\n") + "\n\n" + block, "appended"
	}

	if existing == "" {
		msg, action := mergeMessage("")
		raw, _ := json.MarshalIndent(map[string]any{"systemMessage": msg}, "", "  ")
		return string(raw) + "\n", action
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(existing), &cfg); err != nil {
		return existing, "error"
	}
	prev, _ := cfg["systemMessage"].(string)
	merged, action := mergeMessage(prev)
	cfg["systemMessage"] = merged

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return existing, "error"
	}
	return string(out) + "\n", action
}


// ─────────────────────────────────────────────────────────────────────────────
// jetbrains (.idea/.junie/guidelines.md — JetBrains AI Assistant project rules)
// ─────────────────────────────────────────────────────────────────────────────
//
// JetBrains AI Assistant (the AI feature shipped in 2024+ IDE builds —
// IntelliJ IDEA, PyCharm, GoLand, WebStorm, etc.) reads project-level
// instructions from `.idea/.junie/guidelines.md` (the "Junie" code-
// generation companion's per-project conventions file). #1335 v0.76
// parity wave 2.
//
// Detection: the `.idea/` directory is the canonical JetBrains project
// marker — every JetBrains IDE creates it on first open. The presence
// of `.idea/` alone is enough to identify the user as a JetBrains
// user; we don't require `.junie/` to pre-exist because part of this
// target's value is bootstrapping the directory for new adopters.
//
// Path: project-local only. JetBrains AI Assistant's global-rules
// path varies per IDE (preferences UI rather than a stable filesystem
// path), so SupportsGlobal stays false — pincher init --global
// against this target is an error, matching the cursor / windsurf /
// aider pattern. Users who want global JetBrains rules add them via
// the IDE's Preferences > Tools > AI Assistant settings.
//
// Writer: MergePolicyBlockBare (no front-matter header — the file is
// plain markdown that AI Assistant inlines into its system prompt
// when the file exists).

var JetBrainsTarget = Target{
	Name:     "jetbrains",
	Describe: "JetBrains AI Assistant: ./.idea/.junie/guidelines.md (IntelliJ IDEA, PyCharm, GoLand, WebStorm, etc.)",
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			return "", fmt.Errorf("jetbrains target is project-only — global rules are configured via the IDE's Preferences > Tools > AI Assistant UI, not a stable filesystem path")
		}
		return filepath.Join(cwd, ".idea", ".junie", "guidelines.md"), nil
	},
	DetectFn: func(cwd string) bool {
		// `.idea/` is the JetBrains project marker — every JetBrains
		// IDE creates it on first open of a project. Presence alone
		// confirms a JetBrains user; .junie/ subdir may or may not
		// exist yet (writing the target bootstraps it).
		_, err := os.Stat(filepath.Join(cwd, ".idea"))
		return err == nil
	},
	WriteFn: MergePolicyBlockBare,
}

// ─────────────────────────────────────────────────────────────────────────────
// antigravity (./.antigravity/rules.md — Google Antigravity IDE project rules)
// ─────────────────────────────────────────────────────────────────────────────
//
// Google Antigravity (Gemini-3 IDE, announced late 2025) follows the
// `.<tool>/rules.md` project-rules convention common across the agent-IDE
// ecosystem (mirrors `.cursor/rules` / `.windsurf/rules.md` shapes).
// Adding it surfaces pincher to a fresh user base without requiring
// hand-crafted MCP host configuration. #1368.
//
// Detection: the `.antigravity/` directory at the project root is the
// canonical marker. Presence alone confirms an Antigravity user;
// `rules.md` may or may not exist yet (writing the target bootstraps it).
//
// Path: project-local only. Antigravity's global-rules surface — if it
// exposes one — lives in the IDE's preferences UI rather than a stable
// filesystem path; SupportsGlobal stays false to match the
// cursor / windsurf / jetbrains pattern. The error message routes users
// to the IDE's settings explicitly so they don't silently get a no-op.
//
// Writer: MergePolicyBlockBare (no front-matter header — the file is
// plain markdown the agent inlines into its system prompt). Mirrors
// the cursor / windsurf / zed / jetbrains shape so re-runs replace the
// marker block in place (idempotent), and `--force` overrides the
// existing block wholesale.

var AntigravityTarget = Target{
	Name:     "antigravity",
	Describe: "Google Antigravity: ./.antigravity/rules.md (Gemini-3 agent IDE)",
	PathFn: func(cwd string, global bool) (string, error) {
		if global {
			return "", fmt.Errorf("antigravity target is project-only — global rules are configured via Antigravity's preferences UI, not a stable filesystem path")
		}
		return filepath.Join(cwd, ".antigravity", "rules.md"), nil
	},
	DetectFn: func(cwd string) bool {
		_, err := os.Stat(filepath.Join(cwd, ".antigravity"))
		return err == nil
	},
	WriteFn: MergePolicyBlockBare,
}
