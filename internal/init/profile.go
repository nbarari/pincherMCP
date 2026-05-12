// Package init's profile.go (#631) walks a target directory cheaply at
// install time and classifies its files by extraction tier so the user
// sees realistic savings expectations BEFORE running their first
// session — not after they've already concluded "pincher doesn't work
// on my Ruby/Scala/Elixir codebase."
//
// The classification is intentionally a single fast pass: registered
// extractor confidence (#159 RegisteredConfidence) is the only signal,
// no per-file content sampling. Per-file sampling happens during the
// real `pincher index` pass later — the install-time profile is
// purely about setting expectations from the file extension census.
package init

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/boyter/gocodewalker"
	"github.com/kwad77/pincher/internal/ast"
)

// Tier names match the README #621 vocabulary so the user sees one
// consistent story across install, dashboard, and docs.
const (
	tierAST          = "AST"
	tierStableRegex  = "stable-regex"
	tierApproxRegex  = "approx-regex"
	tierStub         = "stub"
	tierUnsupported  = "unsupported"
	profileFileLimit = 5000
)

// LanguageStat is the per-language census row.
type LanguageStat struct {
	Language string
	Files    int
	Tier     string
	Savings  string // human-readable expected-savings range
}

// Profile is the install-time classification result for a directory.
type Profile struct {
	Languages       []LanguageStat
	TotalFiles      int
	HeadlineTier    string
	HeadlineMessage string
	Truncated       bool // hit profileFileLimit before walker finished
}

// tierFor maps Extractor.Confidence() to the README tier vocabulary.
// Confidence values are the extractor self-declared parser quality
// (#159 RegisteredConfidence), not per-symbol averages — they don't
// drift with corpus content.
//
// The cutoff for stable-vs-approx-regex is 0.80 — stable-regex
// extractors register at 0.85, approximate-regex at 0.70. Sitting the
// boundary at 0.80 keeps either side robust to a future tweak that
// nudges a confidence by 0.02.
func tierFor(confidence float64) (tier, savings string) {
	switch {
	case confidence < 0:
		return tierUnsupported, "—"
	case confidence >= 0.95:
		return tierAST, "95-99% on retrieval"
	case confidence >= 0.80:
		return tierStableRegex, "80-95% on retrieval"
	case confidence >= 0.50:
		return tierApproxRegex, "60-85% on retrieval; structural queries less reliable"
	default:
		return tierStub, "1-3% — pincher won't accelerate this workflow"
	}
}

// ProfileDir walks dir (respecting .gitignore via gocodewalker) and
// returns a per-language census + headline tier. Caps at
// profileFileLimit files so a node_modules-rich install path can't
// stall init for minutes — the cap firing is surfaced via Truncated
// so the report can disclose it.
func ProfileDir(dir string) (*Profile, error) {
	queue := make(chan *gocodewalker.File, 256)
	walker := gocodewalker.NewFileWalker(dir, queue)
	walker.ExcludeDirectory = profileSkippedDirs()

	walkErr := make(chan error, 1)
	go func() { walkErr <- walker.Start() }()

	counts := map[string]int{}
	total := 0
	truncated := false
	for f := range queue {
		total++
		if total > profileFileLimit {
			truncated = true
			walker.Terminate()
			// Drain the remaining items so the goroutine can exit.
			go func() {
				for range queue {
				}
			}()
			break
		}
		lang := ast.DetectLanguage(f.Filename)
		if lang == "" {
			continue
		}
		counts[lang]++
	}
	if err := <-walkErr; err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}

	out := &Profile{TotalFiles: total, Truncated: truncated}
	out.Languages = make([]LanguageStat, 0, len(counts))
	tierFiles := map[string]int{}
	for lang, n := range counts {
		conf := ast.RegisteredConfidence(lang)
		tier, savings := tierFor(conf)
		out.Languages = append(out.Languages, LanguageStat{
			Language: lang, Files: n, Tier: tier, Savings: savings,
		})
		tierFiles[tier] += n
	}
	// Headline tier: tier with the most indexed-language files.
	// Unsupported files (no extractor) don't count toward any tier.
	sort.Slice(out.Languages, func(i, j int) bool {
		if out.Languages[i].Files != out.Languages[j].Files {
			return out.Languages[i].Files > out.Languages[j].Files
		}
		return out.Languages[i].Language < out.Languages[j].Language
	})
	out.HeadlineTier, out.HeadlineMessage = headline(tierFiles)
	return out, nil
}

func headline(tierFiles map[string]int) (string, string) {
	var top string
	var topN int
	totalIndexed := 0
	for tier, n := range tierFiles {
		totalIndexed += n
		if n > topN {
			top, topN = tier, n
		}
	}
	if totalIndexed == 0 {
		return "", "No files in indexable languages — pincher won't have anything to accelerate here."
	}
	pct := float64(topN) / float64(totalIndexed) * 100
	switch top {
	case tierAST:
		return tierAST, fmt.Sprintf("AST-majority (%.0f%% of indexable files at AST confidence). Expect 50-90%% savings on a typical session.", pct)
	case tierStableRegex:
		return tierStableRegex, fmt.Sprintf("Stable-regex majority (%.0f%% of indexable files). Expect 30-70%% savings on a typical session; structural queries reliable.", pct)
	case tierApproxRegex:
		return tierApproxRegex, fmt.Sprintf("Approximate-regex majority (%.0f%% of indexable files). Expect 10-40%% savings; structural queries are best-effort.", pct)
	case tierStub:
		return tierStub, fmt.Sprintf("Stub-majority (%.0f%% of indexable files at stub-tier coverage). pincher will index but offer minimal acceleration on this codebase shape.", pct)
	}
	return top, fmt.Sprintf("%s-majority (%.0f%%).", top, pct)
}

// PrintProfile renders a Profile to out as a one-screen summary. Empty
// or all-unsupported profiles still print, with an honest "no
// indexable files" line — silence here would be worse than a clear
// expectations-set.
func PrintProfile(out io.Writer, p *Profile) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Project profile:")
	if len(p.Languages) == 0 {
		fmt.Fprintln(out, "  (no files in indexable languages found)")
	} else {
		// Column widths sized for the corpus headline (longest language
		// names, biggest realistic file counts). 14-char language
		// column fits "JavaScript" / "TypeScript" / "Markdown".
		for _, ls := range p.Languages {
			fmt.Fprintf(out, "  %-14s %6d files   %-14s expected savings %s\n",
				ls.Language, ls.Files, ls.Tier, ls.Savings)
		}
	}
	if p.Truncated {
		fmt.Fprintf(out, "\n(profile truncated at %d files — actual index will cover the full tree)\n", profileFileLimit)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Headline tier: %s\n", p.HeadlineMessage)
}

// profileSkippedDirs returns directory names the install-time profiler
// excludes — a strict subset of the indexer's blocklist focused on the
// directories most likely to dominate the file count without
// reflecting "what the user actually edits". Profile is a sample, not
// a complete index, so it intentionally errs toward fewer surprises.
func profileSkippedDirs() []string {
	return []string{
		".git", ".hg", ".svn",
		"node_modules", "vendor", "target", "build", "dist",
		"__pycache__", ".venv", "venv", ".tox",
		".idea", ".vscode",
		"testdata", // pincher's own convention, but harmless for non-pincher repos
	}
}

// FormatProfile is a string variant of PrintProfile for tests and
// other non-stream callers.
func FormatProfile(p *Profile) string {
	var b strings.Builder
	PrintProfile(&b, p)
	return b.String()
}
