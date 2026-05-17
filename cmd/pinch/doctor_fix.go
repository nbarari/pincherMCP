package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/kwad77/pincher/internal/db"
)

// #1260 §3: `pincher doctor --fix` — auto-resolves the SAFE subset
// of advisories the doctor diagnoses. Mirrors `npm audit fix` /
// `cargo fix` user expectation: tells you what's wrong AND offers
// the obvious next action.
//
// Tight safe-action allowlist (only operations that can't damage
// user data or in-flight work):
//
//   1. VACUUM the DB to reclaim disk space if it has > vacuumThresholdMB
//      of bloat per the doctor diagnostic. Non-destructive — VACUUM
//      rewrites the file losslessly. Holds an exclusive lock for the
//      duration but is bounded by DB size.
//
// Explicitly NOT in the safe subset (require explicit user action):
//
//   - Project deletion (`pincher project rm`) — destructive, needs
//     confirm. Even with --force the user typed the project name.
//   - Force-reindex of stale projects — non-destructive but I/O heavy
//     (can be hours on a 10k+ file repo). User should choose when to
//     pay that cost. Doctor surfaces the stale list; user decides.
//   - prune-stale — destructive deletion path; also has its own --force.
//
// Returns a structured FixReport mirroring StatsReport / DoctorReport
// so --json works the same way across the CLI.

// FixReport is the structured form `pincher doctor --fix --json` emits.
type FixReport struct {
	DataDir string      `json:"data_dir"`
	Actions []FixAction `json:"actions"`
}

// FixAction is one auto-fix attempt. Status is one of:
//   - "applied" — fix ran and succeeded
//   - "skipped" — advisory present but doesn't qualify for auto-fix
//     (e.g. project deletion needs confirm); details says why
//   - "noop"    — no work needed (e.g. DB has no bloat)
//   - "error"   — fix attempted and failed; details carries the error
type FixAction struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Details string `json:"details,omitempty"`
}

// runDoctorFix performs the safe-action allowlist + emits the report.
// Called when `pincher doctor --fix` is invoked. Output mirrors the
// other CLI subcommands: text by default, JSON with --json.
func runDoctorFix(store *db.Store, dir string, asJSON bool, out io.Writer) {
	report := FixReport{DataDir: dir, Actions: []FixAction{}}

	report.Actions = append(report.Actions, fixVacuumIfBloated(store))

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}
	fmt.Fprint(out, formatFixText(&report))
}

// fixVacuumIfBloated runs VACUUM when the DB has > vacuumThresholdBytes
// of free pages worth reclaiming. Non-destructive — VACUUM rewrites
// the file losslessly. The pincher vacuum CLI already exists; this
// wraps it with a threshold gate so doctor --fix doesn't pay the cost
// when there's no win to chase.
//
// Threshold: 50 MB of reclaimable space. Below that the VACUUM cost
// (exclusive lock for the duration, scales with DB size) isn't worth
// the disk saved. Threshold pinned in tests so a future tightening
// is a deliberate decision, not silent drift.
const vacuumThresholdBytes = 50 * 1024 * 1024

func fixVacuumIfBloated(store *db.Store) FixAction {
	// Measure before + after via on-disk file size — matches the
	// vacuum CLI's approach (VacuumResult itself only exposes
	// diagnostic flags, not byte counts).
	before := dbFileSize(store.Path)
	res, err := store.Vacuum()
	if err != nil {
		return FixAction{
			Name:    "vacuum-db",
			Status:  "error",
			Details: fmt.Sprintf("VACUUM failed: %v", err),
		}
	}
	after := dbFileSize(store.Path)
	reclaimed := before - after
	if reclaimed < 0 {
		reclaimed = 0
	}
	// #1149: open WAL reader pinned the freelist — VACUUM ran but
	// reclaimed 0. Same advisory as `pincher vacuum` surfaces.
	if res.WalReaderBusy && reclaimed == 0 {
		return FixAction{
			Name:    "vacuum-db",
			Status:  "skipped",
			Details: "another pincher process holds an open reader — WAL freelist pinned. Restart the MCP server (or wait for active sessions to disconnect), then re-run.",
		}
	}
	if reclaimed < vacuumThresholdBytes {
		return FixAction{
			Name:    "vacuum-db",
			Status:  "noop",
			Details: fmt.Sprintf("reclaimed %s (< %s threshold) — DB is healthy", db.FormatSize(int(reclaimed)), db.FormatSize(int(vacuumThresholdBytes))),
		}
	}
	return FixAction{
		Name:    "vacuum-db",
		Status:  "applied",
		Details: fmt.Sprintf("reclaimed %s (%s → %s)", db.FormatSize(int(reclaimed)), db.FormatSize(int(before)), db.FormatSize(int(after))),
	}
}

// formatFixText renders the FixReport as a compact text block. One
// line per action with status-prefixed verb ("✓ applied", "·  noop",
// "!  error"); a trailing summary count.
func formatFixText(r *FixReport) string {
	var out []byte
	out = append(out, fmt.Sprintf("pincher doctor --fix\n  data_dir: %s\n", r.DataDir)...)
	applied, noop, errored, skipped := 0, 0, 0, 0
	for _, a := range r.Actions {
		var prefix string
		switch a.Status {
		case "applied":
			prefix = "  + applied"
			applied++
		case "noop":
			prefix = "  · noop   "
			noop++
		case "skipped":
			prefix = "  - skipped"
			skipped++
		case "error":
			prefix = "  ! error  "
			errored++
		default:
			prefix = "  ? unknown"
		}
		out = append(out, fmt.Sprintf("%s  %-14s  %s\n", prefix, a.Name, a.Details)...)
	}
	out = append(out, fmt.Sprintf("\nSummary: %d applied · %d noop · %d skipped · %d error\n", applied, noop, skipped, errored)...)
	if applied == 0 && errored == 0 {
		out = append(out, "\nNo safe fixes were needed. For destructive remediations (project deletion, force-reindex), run the targeted subcommand explicitly.\n"...)
	}
	return string(out)
}

// runDoctorFixCLI is the package-main entry point — called from
// runDoctorCLI when --fix is set. Stays alongside runDoctorCLI so the
// flag dispatch is one-liner-trivial.
func runDoctorFixCLI(dir string, asJSON bool) {
	store, err := db.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher: failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()
	runDoctorFix(store, dir, asJSON, os.Stdout)
}
