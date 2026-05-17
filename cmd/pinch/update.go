package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	updateGitHubOwner = "kwad77"
	updateGitHubRepo  = "pincher" // post-#198 repo rename. The old kwad77/pincherMCP redirects, but the canonical path is `pincher`.
	updateModulePath  = "github.com/kwad77/pincher"
	updateGoInstall   = "github.com/kwad77/pincher/cmd/pinch@latest"
)

// runUpdateCLI implements `pincher update [--check] [--source DIR] [--yes]`.
//
// Two modes, auto-detected:
//
//   - **In-repo**: when invoked from inside a clone of pincher-repo
//     (or --source points at one), runs `git pull --ff-only` and rebuilds
//     the binary in-place via `go build`.
//   - **Standalone**: queries GitHub releases for the latest tag, compares
//     against the running version, and (when release assets land) will
//     download a prebuilt binary. Until then, falls back to `go install`,
//     which currently requires the go.mod module path to match the GitHub
//     URL — see the docs banner.
//
// The check-only mode (`--check`) prints what *would* happen and exits 0
// regardless of whether an update is available; cron-style callers should
// pipe stdout into a comparison.
func runUpdateCLI(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "Only check for updates; do not apply")
	source := fs.String("source", "", "Path to a pincher git checkout to update (default: auto-detect)")
	yes := fs.Bool("yes", false, "Skip confirmation prompts")
	dryRun := fs.Bool("dry-run", false, "Print the commands that would run, but do not execute them")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: pincher update [--check] [--source DIR] [--yes] [--dry-run]")
		fmt.Fprintln(os.Stderr, "  Auto-updates pincher in place. If invoked from a clone of pincher-repo,")
		fmt.Fprintln(os.Stderr, "  runs `git pull --ff-only && go build`. Otherwise queries GitHub releases.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	out := os.Stdout

	repoRoot := detectUpdateSource(*source)
	if repoRoot != "" {
		if err := updateInRepo(out, repoRoot, *check, *yes, *dryRun); err != nil {
			fmt.Fprintf(os.Stderr, "pincher update: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := updateStandalone(out, *check, *yes, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "pincher update: %v\n", err)
		os.Exit(1)
	}
}

// detectUpdateSource returns the absolute path to a pincher checkout
// that should be updated, or "" if none is detected. Override wins over
// auto-detect; auto-detect tries cwd first, then the binary's directory.
func detectUpdateSource(override string) string {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return ""
		}
		if findRepoRoot(abs) != "" {
			return abs
		}
		return ""
	}

	if cwd, err := os.Getwd(); err == nil {
		if root := findRepoRoot(cwd); root != "" {
			return root
		}
	}

	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if root := findRepoRoot(filepath.Dir(exe)); root != "" {
			return root
		}
	}

	return ""
}

// findRepoRoot walks up from start looking for a go.mod whose `module`
// directive names the pincher module. Returns the directory containing
// the matching go.mod, or "" if none found within 16 ancestors.
func findRepoRoot(start string) string {
	dir := start
	for i := 0; i < 16; i++ {
		if isPincherModule(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

func isPincherModule(goModPath string) bool {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		return strings.Contains(line, updateModulePath)
	}
	return false
}

// updateInRepo runs `git pull --ff-only` then `go build` in repoRoot.
func updateInRepo(out io.Writer, repoRoot string, check, yes, dryRun bool) error {
	fmt.Fprintf(out, "pincher update: in-repo mode (%s)\n", repoRoot)

	current, err := gitDescribe(repoRoot)
	if err != nil {
		return fmt.Errorf("git describe: %w", err)
	}
	fmt.Fprintf(out, "  current: %s (binary v%s)\n", current, version)

	if dryRun || check {
		fmt.Fprintln(out, "  fetching origin to compare...")
	}
	if !dryRun {
		if err := runGit(repoRoot, "fetch", "--quiet", "origin"); err != nil {
			return fmt.Errorf("git fetch: %w", err)
		}
	}

	branch, err := gitCurrentBranch(repoRoot)
	if err != nil {
		return fmt.Errorf("git current branch: %w", err)
	}

	ahead, behind, err := gitAheadBehind(repoRoot, "HEAD", "origin/"+branch)
	if err != nil {
		// Fall back: maybe origin/<branch> doesn't exist locally yet.
		ahead, behind = 0, 0
	}
	fmt.Fprintf(out, "  branch:  %s (ahead %d, behind %d)\n", branch, ahead, behind)

	if behind == 0 {
		fmt.Fprintln(out, "  already up to date")
		if !check {
			return rebuildBinary(out, repoRoot, dryRun)
		}
		return nil
	}

	if check {
		fmt.Fprintf(out, "  update available: %d commit(s) on origin/%s\n", behind, branch)
		return nil
	}

	if !yes {
		fmt.Fprintf(out, "  pull %d commit(s) and rebuild? [y/N] ", behind)
		if !confirmYes() {
			return errors.New("aborted by user")
		}
	}

	if dryRun {
		fmt.Fprintf(out, "  would run: git pull --ff-only origin %s\n", branch)
	} else {
		if err := runGit(repoRoot, "pull", "--ff-only", "origin", branch); err != nil {
			return fmt.Errorf("git pull: %w", err)
		}
	}

	return rebuildBinary(out, repoRoot, dryRun)
}

func rebuildBinary(out io.Writer, repoRoot string, dryRun bool) error {
	binName := "pincher"
	if runtime.GOOS == "windows" {
		binName = "pincher.exe"
	}
	target := filepath.Join(repoRoot, binName)

	if dryRun {
		fmt.Fprintf(out, "  would run: go build -o %s ./cmd/pinch/\n", target)
		return nil
	}

	fmt.Fprintf(out, "  building -> %s\n", target)
	cmd := exec.Command("go", "build", "-o", target, "./cmd/pinch/")
	cmd.Dir = repoRoot
	cmd.Env = os.Environ()
	if data, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build:\n%s\n%w", strings.TrimSpace(string(data)), err)
	}
	fmt.Fprintln(out, "  build OK")
	return nil
}

// gitRelease describes one GitHub release as much as we need for update logic.
type gitRelease struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

// detectInstallMethod inspects the running binary's path to figure out
// how pincher was installed. Returns one of:
//
//   - "homebrew"  — installed via brew (path under Homebrew prefix)
//   - "binary"    — direct download / build (anything else)
//
// Used by updateStandalone (#1260 §5) to dispatch the right upgrade
// command. Homebrew users currently get told to run `go install` which
// they can't follow without a Go toolchain — directing them to
// `brew upgrade` instead closes the gap.
//
// Skips Scoop (not yet shipped, #1260 §1) and Docker (rare to update
// from inside a container — users update by pulling a new image).
//
// On any error resolving the exe path, returns "binary" so the existing
// GitHub-asset / go-install path runs unchanged. Defensive: a detection
// false-negative just routes through the slower path, never blocks.
func detectInstallMethod(exePath string) string {
	if exePath == "" {
		return "binary"
	}
	abs, err := filepath.Abs(exePath)
	if err != nil {
		return "binary"
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	abs = filepath.ToSlash(abs)
	// Apple-silicon default + Intel-Mac default + Linuxbrew default.
	brewPrefixes := []string{
		"/opt/homebrew/", // Apple silicon (M1+)
		"/usr/local/",    // Intel Mac (and the default brew --prefix)
		"/home/linuxbrew/.linuxbrew/",
	}
	for _, p := range brewPrefixes {
		if strings.HasPrefix(abs, p) {
			return "homebrew"
		}
	}
	return "binary"
}

// upgradeViaHomebrew renders the brew upgrade hint OR runs the command
// directly when --yes is passed. Stays advisory by default: brew is the
// kind of tool whose output the user generally wants to see live, and
// running it from inside an unrelated process can confuse a user who
// then re-runs brew themselves.
func upgradeViaHomebrew(out io.Writer, check, yes, dryRun bool) error {
	fmt.Fprintln(out, "  install method: homebrew")
	fmt.Fprintln(out, "  recommended:    brew update && brew upgrade pincher")
	if check {
		return nil
	}
	if dryRun {
		fmt.Fprintln(out, "  would run: brew update && brew upgrade pincher")
		return nil
	}
	if !yes {
		fmt.Fprintln(out, "  re-run with --yes to invoke brew automatically, or run the command above yourself.")
		return nil
	}
	if _, err := exec.LookPath("brew"); err != nil {
		return fmt.Errorf("brew not found on PATH despite homebrew install path; run manually")
	}
	fmt.Fprintln(out, "  running: brew update")
	if err := runBrew(out, "update"); err != nil {
		return fmt.Errorf("brew update: %w", err)
	}
	fmt.Fprintln(out, "  running: brew upgrade pincher")
	if err := runBrew(out, "upgrade", "pincher"); err != nil {
		return fmt.Errorf("brew upgrade pincher: %w", err)
	}
	return nil
}

// brewRunner is the indirection point so tests can verify dispatch
// shape without invoking actual brew on the test runner.
var brewRunner = func(out io.Writer, args ...string) error {
	cmd := exec.Command("brew", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = os.Environ()
	return cmd.Run()
}

func runBrew(out io.Writer, args ...string) error {
	return brewRunner(out, args...)
}

// updateStandalone implements the no-checkout path: query GitHub releases,
// pick a matching asset for this platform, fall back to `go install`.
//
// #1260 §5: when the running binary lives under a Homebrew prefix,
// short-circuit and direct the user to `brew upgrade pincher` instead.
// Pre-fix Mac users got `go install` instructions they couldn't follow.
func updateStandalone(out io.Writer, check, yes, dryRun bool) error {
	fmt.Fprintln(out, "pincher update: standalone mode")
	fmt.Fprintf(out, "  current: v%s\n", version)

	if exe, err := os.Executable(); err == nil {
		if method := detectInstallMethod(exe); method == "homebrew" {
			return upgradeViaHomebrew(out, check, yes, dryRun)
		}
	}

	rel, err := fetchLatestRelease()
	if err != nil {
		return fmt.Errorf("fetch latest release: %w", err)
	}
	fmt.Fprintf(out, "  latest:  %s\n", rel.TagName)

	if normaliseVersion(rel.TagName) == normaliseVersion(version) {
		fmt.Fprintln(out, "  already up to date")
		return nil
	}

	asset := pickAssetForPlatform(rel)
	if asset.BrowserDownloadURL == "" {
		fmt.Fprintf(out, "  no prebuilt binary published for %s/%s\n", runtime.GOOS, runtime.GOARCH)
		fmt.Fprintln(out, "  fallback: `go install` (requires Go on PATH)")
		fmt.Fprintln(out, "  note: go.mod module path is "+updateModulePath+",")
		fmt.Fprintln(out, "        which differs from the GitHub URL kwad77/pincher.")
		fmt.Fprintln(out, "        `go install` will fail until go.mod is renamed; clone + build manually.")
		if check {
			return nil
		}
		if dryRun {
			fmt.Fprintf(out, "  would run: go install %s\n", updateGoInstall)
			return nil
		}
		if !yes {
			fmt.Fprint(out, "  attempt `go install` anyway? [y/N] ")
			if !confirmYes() {
				return errors.New("aborted by user")
			}
		}
		return runGoInstall(out)
	}

	fmt.Fprintf(out, "  asset:   %s (%d bytes)\n", asset.Name, asset.Size)
	if check {
		fmt.Fprintln(out, "  (check-only; not downloading)")
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "  would download: %s\n", asset.BrowserDownloadURL)
		return nil
	}
	if !yes {
		fmt.Fprintf(out, "  download and replace running binary? [y/N] ")
		if !confirmYes() {
			return errors.New("aborted by user")
		}
	}
	return downloadAndSwap(out, asset.BrowserDownloadURL)
}

// updateReleasesURL is the GitHub releases-latest endpoint used by
// fetchLatestRelease. Overridable so tests can point at an httptest
// mirror without going to the real network.
var updateReleasesURL = fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", updateGitHubOwner, updateGitHubRepo)

func fetchLatestRelease() (gitRelease, error) {
	req, err := http.NewRequest("GET", updateReleasesURL, nil)
	if err != nil {
		return gitRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "pincher-update/"+version)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return gitRelease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return gitRelease{}, fmt.Errorf("github api returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel gitRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return gitRelease{}, fmt.Errorf("decode release json: %w", err)
	}
	return rel, nil
}

// pickAssetForPlatform returns the asset whose name best matches GOOS/GOARCH.
// Conventions accepted (case-insensitive):
//
//	pincher_<os>_<arch>[.exe]
//	pincher-<version>-<os>-<arch>[.exe]
//	pincher.<os>.<arch>[.exe]
//
// Empty BrowserDownloadURL means no asset matched.
func pickAssetForPlatform(rel gitRelease) struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
} {
	osTag := strings.ToLower(runtime.GOOS)
	archTag := strings.ToLower(runtime.GOARCH)
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		if !strings.Contains(name, osTag) || !strings.Contains(name, archTag) {
			continue
		}
		// archive formats deliberately not supported in this pass — the
		// publish workflow can ship raw binaries for now (#TBD: add tar.gz/zip
		// extraction once release artifacts settle).
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, ".zip") {
			continue
		}
		return a
	}
	return struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	}{}
}

// downloadAndSwap fetches url, writes it to a temp file next to the
// running binary, and atomically renames it over the running binary.
// On Windows the running .exe cannot be deleted; we move it aside first
// (mirrors the gh and rustup self-update strategy).
// downloadAndSwap is the production entry point: it locates the running
// binary's path via os.Executable() and delegates the actual download +
// atomic install to downloadAndInstallAt, which is exercised directly by
// tests against an httptest.Server + a temp exePath. The split keeps
// the test path from having to override os.Executable() and from
// risking the test binary being renamed mid-run.
func downloadAndSwap(out io.Writer, url string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	return downloadAndInstallAt(out, url, exePath)
}

// downloadAndInstallAt fetches `url` and atomically replaces the file at
// `exePath` with the response body. Inner half of downloadAndSwap; see
// that function's doc for why the split exists.
func downloadAndInstallAt(out io.Writer, url, exePath string) error {
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, "pincher-update-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	fmt.Fprintf(out, "  downloading...\n")
	resp, err := http.Get(url)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("write download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		// Non-fatal on Windows where Chmod is largely a no-op.
		fmt.Fprintf(out, "  warn: chmod %s: %v\n", tmpPath, err)
	}

	if runtime.GOOS == "windows" {
		// Windows can't replace a running .exe; move it aside.
		old := exePath + ".old"
		_ = os.Remove(old)
		if _, statErr := os.Stat(exePath); statErr == nil {
			if err := os.Rename(exePath, old); err != nil {
				return fmt.Errorf("move running binary aside: %w", err)
			}
		}
	}
	if err := os.Rename(tmpPath, exePath); err != nil {
		return fmt.Errorf("install new binary: %w", err)
	}

	fmt.Fprintf(out, "  installed -> %s\n", exePath)
	return nil
}

// goInstallRunner is the indirection point for runGoInstall. Tests
// override it to verify the LookPath-then-exec sequencing without
// actually invoking `go install` (which has network deps and would
// rebuild the test binary into the user's GOBIN).
var goInstallRunner = func(out io.Writer, target string) error {
	cmd := exec.Command("go", "install", target)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = os.Environ()
	return cmd.Run()
}

func runGoInstall(out io.Writer) error {
	if _, err := exec.LookPath("go"); err != nil {
		return errors.New("`go` not found on PATH; install Go or clone the repo and `go build`")
	}
	fmt.Fprintf(out, "  running: go install %s\n", updateGoInstall)
	return goInstallRunner(out, updateGoInstall)
}

// confirmYes reads a single line from stdin and returns true if it
// starts with 'y' or 'Y'.
func confirmYes() bool {
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return false
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return strings.HasPrefix(line, "y")
}

// gitDescribe returns the output of `git describe --tags --always` in repoRoot.
func gitDescribe(repoRoot string) (string, error) {
	cmd := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitCurrentBranch(repoRoot string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitAheadBehind(repoRoot, local, remote string) (ahead, behind int, err error) {
	cmd := exec.Command("git", "rev-list", "--left-right", "--count", local+"..."+remote)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output: %q", out)
	}
	fmt.Sscanf(parts[0], "%d", &ahead)
	fmt.Sscanf(parts[1], "%d", &behind)
	return ahead, behind, nil
}

func runGit(repoRoot string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// normaliseVersion strips a leading 'v' so semver-style tags compare
// equal to the build-time `version` constant.
func normaliseVersion(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "v")
}
