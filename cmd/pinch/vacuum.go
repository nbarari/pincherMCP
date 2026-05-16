package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
)

// runVacuumCLI implements `pincher vacuum` — runs SQLite VACUUM to
// reclaim pages freed by `pincher project rm` / `pincher project
// prune-stale` so the database file on disk actually shrinks (#732).
//
// VACUUM is deliberately a separate, explicit CLI step rather than a
// flag on an MCP tool: it rewrites the whole file and holds an
// exclusive lock for the duration, which on a multi-GB DB can take a
// while. Keeping it out of the hot MCP path means a long-running
// `pincher` server never blocks queries on a vacuum the user didn't ask
// for. Pair it with prune-stale: prune drops the rows, vacuum reclaims
// the space.
func runVacuumCLI(args []string) {
	log.SetOutput(io.Discard)

	fs := flag.NewFlagSet("vacuum", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Override data directory")
	asJSON := fs.Bool("json", false, "Emit a structured JSON receipt")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher vacuum [--json] [--data-dir DIR]")
		fmt.Fprintln(os.Stderr, "  Rewrites the database file, reclaiming space freed by project removal.")
		fmt.Fprintln(os.Stderr, "  Holds an exclusive lock for the duration — run when no agent is mid-query.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	store, _, err := openProjectStore(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pincher vacuum: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	before := dbFileSize(store.Path)
	vacRes, vacErr := store.Vacuum()
	if vacErr != nil {
		fmt.Fprintf(os.Stderr, "pincher vacuum: %v\n", vacErr)
		os.Exit(1)
	}
	after := dbFileSize(store.Path)
	reclaimed := before - after
	if reclaimed < 0 {
		reclaimed = 0
	}

	// #1149: when an open WAL reader pinned the freelist, VACUUM
	// silently reclaims 0 B and the user reads it as "vacuum is a
	// no-op." The probing checkpoint's busy result tells us this
	// happened — surface a targeted advisory so the user sees the
	// real cause and the recovery path (close the running MCP child
	// or retry post-/mcp-reconnect).
	walAdvisory := ""
	if vacRes.WalReaderBusy && reclaimed == 0 {
		walAdvisory = "another pincher process holds an open reader — WAL freelist pages are pinned and VACUUM cannot reclaim them. Restart the MCP server (or run after active sessions disconnect), then retry."
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		payload := map[string]any{
			"vacuumed":        true,
			"bytes_before":    before,
			"bytes_after":     after,
			"bytes_reclaimed": reclaimed,
			"path":            store.Path,
			"wal_reader_busy": vacRes.WalReaderBusy,
		}
		if walAdvisory != "" {
			payload["advisory"] = walAdvisory
		}
		// #1219 steps 3-4: post-VACUUM PRAGMA optimize + per-vtab
		// FTS5 'optimize' are advisory. Surface failures so users
		// know why subsequent query plans might still be stale
		// without treating the run as a failure (the load-bearing
		// VACUUM landed).
		if vacRes.OptimizeError != "" {
			payload["optimize_error"] = vacRes.OptimizeError
		}
		if vacRes.FTSOptimizeError != "" {
			payload["fts_optimize_error"] = vacRes.FTSOptimizeError
		}
		_ = enc.Encode(payload)
		return
	}
	fmt.Fprintf(os.Stdout, "Vacuumed %s\n  before:    %s\n  after:     %s\n  reclaimed: %s\n",
		store.Path, humanBytes(before), humanBytes(after), humanBytes(reclaimed))
	if walAdvisory != "" {
		fmt.Fprintf(os.Stdout, "\nNote: %s\n", walAdvisory)
	}
	if vacRes.OptimizeError != "" {
		fmt.Fprintf(os.Stdout, "\nWarning: PRAGMA optimize failed (advisory, not vacuum failure): %s\n", vacRes.OptimizeError)
	}
	if vacRes.FTSOptimizeError != "" {
		fmt.Fprintf(os.Stdout, "\nWarning: FTS5 'optimize' failed (advisory, not vacuum failure): %s\n", vacRes.FTSOptimizeError)
	}
}

// dbFileSize returns the size of the database file in bytes, or 0 if it
// can't be stat'd (a freshly-opened in-memory or missing file).
func dbFileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
