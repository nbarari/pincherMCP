package server

import (
	"encoding/json"
	"strings"
	"testing"

	initpkg "github.com/kwad77/pincher/internal/init"
)

// TestInitTool_DescriptionMentionsEveryTarget (#1414) pins the
// `init` MCP tool description against the `internal/init` target
// registry. Pre-fix the jetbrains (#1335) and antigravity (#1385)
// targets shipped + worked but the tool description still listed
// only the v0.69-era target set; an agent reading the description
// wouldn't think to ask for them.
//
// Mechanical guard so adding a target without updating the
// description fails fast in CI — same description-vs-runtime
// parity shape pinned by TestToolContract_GoldenFile.
func TestInitTool_DescriptionMentionsEveryTarget(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)

	initTool, ok := srv.tools["init"]
	if !ok {
		t.Fatal("init tool not registered")
	}

	// Outer description must name every registered target.
	for _, target := range initpkg.AllTargets {
		if !strings.Contains(initTool.Description, target.Name) {
			t.Errorf("init tool description missing target %q — registry has it but description doesn't list it. Either add it to the description or remove it from internal/init AllTargets.", target.Name)
		}
	}

	// `target` param's enum-style description must also list every
	// registered target. InputSchema is `any` upstream; round-trip
	// through JSON to walk it, same pattern as TestToolContract_GoldenFile.
	raw, err := json.Marshal(initTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal init InputSchema: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal init InputSchema: %v", err)
	}
	targetDesc := schema.Properties["target"].Description
	// `continue` is intentionally absent from the enum-style description
	// because it's always-rejected (the outer description carries the
	// rejection note instead). Every OTHER registered target must appear
	// in the enum so agents reading the schema can call them.
	for _, target := range initpkg.AllTargets {
		if target.Name == "continue" {
			continue
		}
		if !strings.Contains(targetDesc, target.Name) {
			t.Errorf("init target param description missing %q — registry has it but enum-style description omits it", target.Name)
		}
	}
}
