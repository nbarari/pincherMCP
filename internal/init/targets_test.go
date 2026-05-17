package init

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const samplePolicy = "## Pincher Usage Policy\n\nPrefer pincher tools over Read/Grep/Glob.\n"

// withCWD switches into dir for the duration of the test, restoring on
// cleanup. Required for target PathFn calls that resolve from cwd.
func withCWD(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// withHome overrides HOME / USERPROFILE so UserHomeDir() returns dir.
func withHome(t *testing.T, dir string) {
	t.Helper()
	if v, ok := os.LookupEnv("HOME"); ok {
		t.Setenv("HOME", v)
	}
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestInitTargets_RegistryShape(t *testing.T) {
	want := []string{"claude", "cursor", "cursor-legacy", "windsurf", "aider", "continue", "codex", "zed", "gemini", "warp", "vscode", "vscode-mcp", "jetbrains"}
	if len(AllTargets) != len(want) {
		t.Fatalf("registry has %d targets, want %d", len(AllTargets), len(want))
	}
	for i, t0 := range AllTargets {
		if t0.Name != want[i] {
			t.Errorf("registry[%d].Name = %q, want %q", i, t0.Name, want[i])
		}
		if t0.PathFn == nil || t0.WriteFn == nil {
			t.Errorf("target %q missing PathFn or WriteFn", t0.Name)
		}
	}
	names := TargetNames()
	if !strings.Contains(strings.Join(names, ","), "detect") {
		t.Error("TargetNames should include 'detect'")
	}
	if !strings.Contains(strings.Join(names, ","), "all") {
		t.Error("TargetNames should include 'all'")
	}
}

func TestFindTarget(t *testing.T) {
	if _, ok := FindTarget("cursor"); !ok {
		t.Error("expected to find 'cursor'")
	}
	if _, ok := FindTarget("nonexistent"); ok {
		t.Error("unexpected hit for 'nonexistent'")
	}
}

// ─── cursor (modern .mdc) ────────────────────────────────────────────────────

func TestCursor_FreshWriteEmitsFrontmatter(t *testing.T) {
	out, action := cursorMDCWriter("", samplePolicy)
	if action != "wrote" {
		t.Errorf("action=%q, want wrote", action)
	}
	if !strings.HasPrefix(out, "---\n") {
		t.Error("cursor .mdc must start with YAML frontmatter delimiter")
	}
	if !strings.Contains(out, "alwaysApply: true") {
		t.Error("expected alwaysApply: true in frontmatter")
	}
	if !strings.Contains(out, MarkerStart) {
		t.Error("expected pincher start marker in body")
	}
}

func TestCursor_PreservesUserEditedFrontmatter(t *testing.T) {
	custom := "---\n" +
		"description: my custom rules\n" +
		"globs:\n  - \"src/**\"\n" +
		"alwaysApply: false\n" +
		"---\n\n" +
		"# Some prose I wrote\n\n" +
		MarkerStart + "\n" +
		"OLD POLICY\n" +
		MarkerEnd + "\n"
	out, action := cursorMDCWriter(custom, samplePolicy)
	if action != "updated" {
		t.Errorf("action=%q, want updated", action)
	}
	if !strings.Contains(out, "my custom rules") {
		t.Error("custom frontmatter description should be preserved")
	}
	if !strings.Contains(out, `globs:`+"\n  - \"src/**\"") {
		t.Error("custom globs should be preserved")
	}
	if !strings.Contains(out, "alwaysApply: false") {
		t.Error("custom alwaysApply should be preserved")
	}
	if !strings.Contains(out, "# Some prose I wrote") {
		t.Error("user prose should be preserved")
	}
	if strings.Contains(out, "OLD POLICY") {
		t.Error("old policy body should have been replaced")
	}
	if !strings.Contains(out, "Pincher Usage Policy") {
		t.Error("new policy body should be present")
	}
}

func TestCursor_IdempotentRewrite(t *testing.T) {
	first, _ := cursorMDCWriter("", samplePolicy)
	second, action := cursorMDCWriter(first, samplePolicy)
	if action != "updated" {
		t.Errorf("second write action=%q, want updated", action)
	}
	if first != second {
		t.Errorf("non-idempotent rewrite:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestSplitMDXFrontmatter(t *testing.T) {
	cases := []struct {
		name           string
		input          string
		wantFM, wantBd string
	}{
		{
			name:   "no frontmatter",
			input:  "just a body\nwith two lines\n",
			wantFM: "",
			wantBd: "just a body\nwith two lines\n",
		},
		{
			name:   "simple frontmatter",
			input:  "---\nfoo: bar\n---\nbody\n",
			wantFM: "---\nfoo: bar\n---\n",
			wantBd: "body\n",
		},
		{
			name:   "frontmatter with blank line before body",
			input:  "---\nfoo: bar\n---\n\nbody\n",
			wantFM: "---\nfoo: bar\n---\n\n",
			wantBd: "body\n",
		},
		{
			name:   "malformed (no close)",
			input:  "---\nfoo: bar\nbody\n",
			wantFM: "",
			wantBd: "---\nfoo: bar\nbody\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm, bd := SplitMDXFrontmatter(c.input)
			if fm != c.wantFM {
				t.Errorf("frontmatter=%q, want %q", fm, c.wantFM)
			}
			if bd != c.wantBd {
				t.Errorf("body=%q, want %q", bd, c.wantBd)
			}
		})
	}
}

func TestCursorLegacy_FreshAndIdempotent(t *testing.T) {
	first, action := CursorLegacyTarget.WriteFn("", samplePolicy)
	if action != "wrote" {
		t.Errorf("first action=%q, want wrote", action)
	}
	if !strings.Contains(first, "Pincher Usage Policy") {
		t.Error("expected policy body")
	}
	second, action := CursorLegacyTarget.WriteFn(first, samplePolicy)
	if action != "updated" {
		t.Errorf("second action=%q, want updated", action)
	}
	if first != second {
		t.Error("cursor-legacy rewrite was not idempotent")
	}
}

func TestWindsurf_PreservesExistingContent(t *testing.T) {
	existing := "# My Windsurf Rules\n\nUse strict types.\n"
	out, action := WindsurfTarget.WriteFn(existing, samplePolicy)
	if action != "appended" {
		t.Errorf("action=%q, want appended", action)
	}
	if !strings.Contains(out, "Use strict types.") {
		t.Error("existing windsurf content should be preserved")
	}
	if !strings.Contains(out, MarkerStart) {
		t.Error("expected pincher block to be appended")
	}
}

func TestAider_GlobalIsRejected(t *testing.T) {
	if _, err := AiderTarget.PathFn(t.TempDir(), true); err == nil {
		t.Error("aider --global should error (not yet implemented)")
	}
}

func TestContinue_FreshWriteProducesValidJSON(t *testing.T) {
	out, action := continueJSONWriter("", samplePolicy)
	if action != "wrote" {
		t.Errorf("action=%q, want wrote", action)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v\n--- output ---\n%s", err, out)
	}
	msg, ok := cfg["systemMessage"].(string)
	if !ok {
		t.Fatal("expected systemMessage to be a string")
	}
	if !strings.Contains(msg, "// pincher:start") || !strings.Contains(msg, "// pincher:end") {
		t.Error("expected line-prefixed pincher markers in systemMessage")
	}
	if !strings.Contains(msg, "Pincher Usage Policy") {
		t.Error("expected policy body in systemMessage")
	}
}

func TestContinue_PreservesUnknownKeysAndExistingMessage(t *testing.T) {
	prior, _ := json.Marshal(map[string]any{
		"models":        []map[string]string{{"title": "claude", "provider": "anthropic"}},
		"systemMessage": "Be concise.",
	})
	out, action := continueJSONWriter(string(prior), samplePolicy)
	if action != "appended" {
		t.Errorf("action=%q, want appended", action)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if _, ok := cfg["models"]; !ok {
		t.Error("models key should be preserved")
	}
	msg, _ := cfg["systemMessage"].(string)
	if !strings.Contains(msg, "Be concise.") {
		t.Error("prior systemMessage should be preserved")
	}
	if !strings.Contains(msg, "// pincher:start") {
		t.Error("expected pincher markers")
	}
}

func TestContinue_IdempotentReplace(t *testing.T) {
	first, _ := continueJSONWriter("", samplePolicy)
	second, action := continueJSONWriter(first, samplePolicy)
	if action != "updated" {
		t.Errorf("action=%q, want updated", action)
	}
	if first != second {
		t.Errorf("continue rewrite not idempotent\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestContinue_MalformedJSONReturnsError(t *testing.T) {
	out, action := continueJSONWriter("{this is not valid", samplePolicy)
	if action != "error" {
		t.Errorf("action=%q, want error", action)
	}
	if out != "{this is not valid" {
		t.Error("malformed JSON should be returned unchanged")
	}
}

func TestCursorPathFn_RelativeToCwd(t *testing.T) {
	tmp := t.TempDir()
	withCWD(t, tmp)
	got, err := CursorTarget.PathFn(tmp, false)
	if err != nil {
		t.Fatalf("PathFn: %v", err)
	}
	gotAbs, _ := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(filepath.Dir(got))))
	wantAbs, _ := filepath.EvalSymlinks(tmp)
	if gotAbs != wantAbs {
		t.Errorf("cursor path = %q (resolved dir %q), want under %q", got, gotAbs, wantAbs)
	}
	if !strings.HasSuffix(got, filepath.Join(".cursor", "rules", "pincher.mdc")) {
		t.Errorf("cursor path %q lacks expected suffix", got)
	}
}

func TestDetectTargets_FindsCursorAndWindsurf(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".windsurfrules"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits := DetectTargets(tmp)
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "cursor") {
		t.Errorf("expected cursor in detection (.cursor/ exists), got %q", joined)
	}
	if !strings.Contains(joined, "windsurf") {
		t.Errorf("expected windsurf in detection (.windsurfrules exists), got %q", joined)
	}
}

func TestDetectTargets_EmptyDirFallsBackToClaude(t *testing.T) {
	tmp := t.TempDir()
	withHome(t, tmp)
	hits := DetectTargets(tmp)
	if len(hits) != 1 || hits[0].Name != "claude" {
		names := []string{}
		for _, h := range hits {
			names = append(names, h.Name)
		}
		t.Errorf("empty dir detection = %v, want [claude]", names)
	}
}

func TestResolveTargets(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantLen  int
		wantErr  bool
		wantName string
	}{
		{name: "single", input: "windsurf", wantLen: 1, wantName: "windsurf"},
		{name: "all", input: "all", wantLen: len(AllTargets)},
		{name: "unknown", input: "vim", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveTargets(c.input, t.TempDir())
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != c.wantLen {
				t.Errorf("got %d targets, want %d", len(got), c.wantLen)
			}
			if c.wantName != "" && got[0].Name != c.wantName {
				t.Errorf("got name %q, want %q", got[0].Name, c.wantName)
			}
		})
	}
}

// Plan exercises the pure planning function: produces the would-be
// content + action without writing anything to disk.
func TestPlan_ProducesPlanWithoutWriting(t *testing.T) {
	tmp := t.TempDir()
	plan, err := Plan(WindsurfTarget, tmp, false)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Action != "wrote" {
		t.Errorf("action=%q, want wrote (empty target file)", plan.Action)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".windsurfrules")); !os.IsNotExist(err) {
		t.Errorf("Plan must not touch disk; stat err=%v", err)
	}
	if !strings.Contains(plan.Updated, MarkerStart) {
		t.Error("plan output missing marker block")
	}
}

func TestPolicyMarkdown_NotEmpty(t *testing.T) {
	if len(strings.TrimSpace(PolicyMarkdown)) == 0 {
		t.Fatal("embedded PolicyMarkdown is empty; init() should have panicked at package load")
	}
}

// ─── jetbrains (.idea/.junie/guidelines.md) ──────────────────────────────────
// #1335 v0.76 parity wave 2.

// Positive: PathFn produces the expected project-relative path.
func TestJetBrainsTarget_PathFn_ProjectLocal(t *testing.T) {
	cwd := "/proj/myrepo"
	got, err := JetBrainsTarget.PathFn(cwd, false)
	if err != nil {
		t.Fatalf("PathFn: %v", err)
	}
	want := filepath.Join(cwd, ".idea", ".junie", "guidelines.md")
	if got != want {
		t.Errorf("PathFn = %q, want %q", got, want)
	}
}

// Negative: --global is rejected loudly. Pre-fix a silent fallback
// to ./.idea/... would mask a misconfigured invocation; an error
// surfaces the constraint.
func TestJetBrainsTarget_PathFn_GlobalRejected(t *testing.T) {
	_, err := JetBrainsTarget.PathFn("/proj/x", true)
	if err == nil {
		t.Fatal("expected error for --global, got nil")
	}
	if !strings.Contains(err.Error(), "project-only") {
		t.Errorf("error should explain the constraint; got: %v", err)
	}
}

// Positive: detection fires on a directory containing .idea/, which
// is the universal JetBrains project marker.
func TestJetBrainsTarget_DetectFn_FiresOnIdeaDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".idea"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !JetBrainsTarget.DetectFn(dir) {
		t.Error("DetectFn returned false despite .idea/ being present")
	}
}

// Negative: no .idea/ means not a JetBrains project.
func TestJetBrainsTarget_DetectFn_FalseOnNonJetBrainsDir(t *testing.T) {
	dir := t.TempDir()
	if JetBrainsTarget.DetectFn(dir) {
		t.Error("DetectFn returned true on a directory without .idea/")
	}
}

// Cross-check: writer is MergePolicyBlockBare (same as zed/gemini —
// plain markdown, no front-matter). Two writes don't duplicate.
func TestJetBrainsTarget_WriteIdempotent(t *testing.T) {
	out1, action1 := JetBrainsTarget.WriteFn("", "test policy body")
	if action1 != "wrote" {
		t.Errorf("first action = %q, want wrote", action1)
	}
	if !strings.Contains(out1, "test policy body") {
		t.Error("first write missing policy body")
	}
	out2, action2 := JetBrainsTarget.WriteFn(out1, "test policy body")
	if action2 == "wrote" {
		t.Errorf("second action = %q, expected update/replace not wrote", action2)
	}
	if strings.Count(out2, MarkerStart) != 1 {
		t.Errorf("second write created %d marker blocks, want exactly 1 (idempotent replace)", strings.Count(out2, MarkerStart))
	}
}
