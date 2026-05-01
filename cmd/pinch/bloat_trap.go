package main

import (
	"os"
	"path/filepath"
)

// projectMarkers are filenames or directories whose presence at the top
// of a directory indicates a likely project root. The hook-mode bloat
// trap guard requires at least one to be present before indexing.
var projectMarkers = []string{
	".git", // any git repo (including worktrees and submodules)
	".hg",
	".svn",
	"go.mod",
	"package.json",
	"pyproject.toml",
	"Cargo.toml",
	"Gemfile",
	"pom.xml",
	"build.gradle",
	"build.gradle.kts",
	"Makefile",
	"CMakeLists.txt",
}

// isBloatTrap reports whether path looks like a bad target for indexing.
// Returns (true, reason) when refusal is warranted.
//
// Two failure modes from prior incidents drive the guard:
//
//  1. Indexing $HOME directly: gocodewalker has no .gitignore protection
//     outside a git repo, so it descends into ~/Library/Caches, the Go
//     module cache, npm caches, etc. and produces millions of low-signal
//     "symbols" that bloat the DB to 10s of GB.
//
//  2. Claude Code SessionStart hook firing in a directory that isn't a
//     real project root (the cwd happens to lack project markers). The
//     hook's `pincher index --hook` falls back to indexing cwd, which
//     could be anywhere — including $HOME if the user opened Claude
//     Code from there.
//
// The strictness depends on context: a manual `pincher index PATH` is
// an explicit user action and only trips on the catastrophic cases ($HOME,
// /). The SessionStart hook can target any directory and needs the
// broader project-marker check.
func isBloatTrap(path string, hookMode bool) (bool, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, ""
	}
	// Resolve symlinks so $HOME and a symlink path that resolves to it
	// both compare equal.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	// Catastrophic cases: refused regardless of mode.
	if abs == "/" {
		return true, "filesystem root"
	}
	if home := userHomeDir(); home != "" {
		if abs == home {
			return true, "user home directory"
		}
	}

	if !hookMode {
		return false, ""
	}

	// Hook mode: require at least one project marker. Without it, the
	// hook is firing in a directory that wasn't meant to be indexed.
	for _, m := range projectMarkers {
		if _, err := os.Stat(filepath.Join(abs, m)); err == nil {
			return false, ""
		}
	}
	return true, "no project marker found"
}

// userHomeDir resolves $HOME (or %USERPROFILE% on Windows) and follows
// symlinks. Returns "" when the env var is unset.
func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		return resolved
	}
	return home
}
