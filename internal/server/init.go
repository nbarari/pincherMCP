package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pinit "github.com/kwad77/pincher/internal/init"
)

// handleInit is the MCP handler for the `init` tool (#253). It wraps
// internal/init.Plan with safety properties suited to an agent
// context: dry-run by default (write=true required to mutate),
// always-global targets rejected (continue), and a path-escape gate
// that refuses to write outside the resolved project root.
//
// Per-target output shape (mirrors the plan + adds diff_preview):
//
//	{
//	  "target": "claude",
//	  "path":   "/abs/path/CLAUDE.md",
//	  "action": "wrote" | "updated" | "appended" | "unchanged" | "would_*",
//	  "diff_preview": "...first 800 chars of the new content...",
//	  "bytes_in":  N,
//	  "bytes_out": M
//	}
//
// "unchanged" is a new action (not in CLI vocab) that fires when the
// existing file already matches what we'd write byte-for-byte. It
// lets the agent skip surfacing the call result entirely if it
// wants — pure information that doesn't need user attention.
func (s *Server) handleInit(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start, tool, args := beginCall(req)

	target, _ := args["target"].(string)
	if target == "" {
		target = "detect"
	}
	write, _ := args["write"].(bool)
	projectPath, _ := args["project_path"].(string)
	if projectPath == "" {
		projectPath = s.sessionRoot
	}
	if projectPath == "" {
		return s.errResultRich(
			"init: no project_path provided and no session root detected — agents must pass project_path explicitly when the MCP server has no roots configured",
			[]map[string]string{
				{"tool": "init", "args": `{"project_path":"/absolute/path/to/project","target":"detect"}`,
					"why": "pass project_path explicitly; target=detect auto-picks based on marker files / dirs"},
				{"tool": "list", "args": `{}`,
					"why": "see already-indexed project paths to copy from"},
			}), nil
	}

	// Resolve the project root once so escape checks compare against
	// the symlink-canonical form. Rejection on resolve failure is
	// safer than silently treating an invalid path as the cwd.
	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		return errResult(fmt.Sprintf("init: project_path resolve: %v", err)), nil
	}

	// Hard-reject continue: it's always-global, path lives in the
	// user's home directory, and an MCP-driven write would silently
	// escape project_path. The CLI keeps the broader semantic.
	if target == "continue" {
		return s.errResultRich(
			"init: target=continue is not available via MCP — its path is always global (~/.continue/config.json) and would escape project_path. Use the `pincher init --target=continue` CLI.",
			[]map[string]string{
				{"tool": "init", "args": `{"target":"detect","project_path":"` + absProjectPath + `"}`,
					"why": "let pincher auto-pick a per-project target instead of continue's always-global one"},
			}), nil
	}

	targets, err := pinit.ResolveTargets(target, absProjectPath)
	if err != nil {
		// pinit.ResolveTargets already names the valid targets in its
		// error message; wrap with rich envelope so the recovery hints
		// surface alongside the named values.
		//
		// #1018: ResolveTargets's "one of: ..." enumeration is the CLI's
		// valid-targets list and includes `continue`. The MCP handler
		// hard-rejects `continue` above (line 69), so listing it as
		// valid for an MCP caller misleads — telling them they CAN pass
		// it when in fact they can't. Strip it from the enumeration in
		// MCP context so the error stays truthful.
		msg := fmt.Sprintf("init: %v", err)
		msg = strings.ReplaceAll(msg, ", continue,", ",")
		return s.errResultRich(
			msg,
			[]map[string]string{
				{"tool": "init", "args": `{"target":"detect","project_path":"` + absProjectPath + `"}`,
					"why": "detect picks the best target based on .claude/, .cursor/, .vscode/, etc."},
				{"tool": "init", "args": `{"target":"all","project_path":"` + absProjectPath + `"}`,
					"why": "write to every supported per-project target at once"},
			}), nil
	}

	results := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		// Filter out AlwaysGlobal targets (continue, codex). Both
		// target=all and target=detect can include them; from MCP we
		// always skip them — their paths are outside project_path so
		// an MCP-driven write would silently escape scope.
		//
		// #1075: surface the skip as a result entry instead of silently
		// dropping. Pre-fix `target=codex` returned `results: []` with
		// no signal — the user got nothing, not a refusal, not an
		// explanation. Same silent-confidently-wrong family as
		// #1063/#1064/#1065. Now: a "skipped_always_global" entry per
		// dropped target with the CLI-fallback affordance.
		if t.AlwaysGlobal {
			results = append(results, map[string]any{
				"target": t.Name,
				"action": "skipped_always_global",
				"reason": fmt.Sprintf(
					"target=%q has an always-global path (outside project_path); MCP refuses to write outside project scope. Use `pincher init --target=%s` from the CLI to write the global config.",
					t.Name, t.Name),
			})
			continue
		}

		plan, err := pinit.Plan(t, absProjectPath, false)
		if err != nil {
			results = append(results, map[string]any{
				"target": t.Name,
				"action": "error",
				"error":  err.Error(),
			})
			continue
		}

		// Path-escape gate: filepath.Rel returns a "../..." path when
		// the target escapes projectPath. Refuse the write/dry-run for
		// such targets so a malicious / misconfigured PathFn can't be
		// coerced into writing outside the agent's project scope.
		rel, relErr := filepath.Rel(absProjectPath, plan.Path)
		if relErr != nil || strings.HasPrefix(rel, "..") {
			results = append(results, map[string]any{
				"target": t.Name,
				"path":   plan.Path,
				"action": "error",
				"error":  "target path escapes project_path; refusing to write",
			})
			continue
		}

		action := plan.Action
		if plan.Existing == plan.Updated {
			action = "unchanged"
		} else if !write {
			// #849: present-tense so the JSON reads "would_update", not
			// the ungrammatical "would_updated" — shares pinit's helper
			// with the CLI's dry-run text (fixed there in #803).
			action = "would_" + pinit.PresentTenseAction(action)
		}

		entry := map[string]any{
			"target":       t.Name,
			"path":         plan.Path,
			"action":       action,
			"diff_preview": diffPreview(plan.Updated),
			"bytes_in":     plan.BytesIn,
			"bytes_out":    plan.BytesOut,
		}

		if write && action != "unchanged" {
			if err := pinit.WriteFileEnsuringDir(plan.Path, plan.Updated); err != nil {
				entry["action"] = "error"
				entry["error"] = err.Error()
			}
		}
		results = append(results, entry)
	}

	data := map[string]any{
		"results": results,
		"dry_run": !write,
	}

	// Surface the dashboard URL when one is live so the agent can hand
	// it to the user — same affordance as `pincher init`'s post-write
	// banner. Best-effort; missing is silent.
	if base, _, ok := findLiveHTTPServerForServer(s); ok {
		data["dashboard_url"] = base
	}

	return s.jsonResultWithMeta(data, start, tool, args, 0), nil
}

// diffPreview returns the first 800 characters of the new content for
// the MCP response. The agent uses this to confirm what would land
// before re-running with write=true. Full content lives on disk after
// write; the preview is a "looks right" gate, not a complete record.
func diffPreview(updated string) string {
	const cap = 800
	if len(updated) <= cap {
		return updated
	}
	return updated[:cap] + "\n…[truncated]…"
}

// findLiveHTTPServerForServer mirrors cmd/pinch's findLiveHTTPServer
// without needing the *db.Store directly — used to surface the
// dashboard URL in the init response. Returns (baseURL, _, false) on
// any error or no-row outcome.
func findLiveHTTPServerForServer(s *Server) (string, int, bool) {
	row, err := s.store.GetLatestHTTPSession()
	if err != nil || row.HTTPURL == "" {
		return "", 0, false
	}
	return row.HTTPURL, row.HTTPPID, true
}
