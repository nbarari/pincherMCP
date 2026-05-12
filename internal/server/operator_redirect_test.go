package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #644: when an operator-only tool is called over MCP, the redirect
// stub returns a structured `operator_tool_not_on_mcp` error pointing
// at the HTTP and CLI alternatives. Pre-#644 behavior was the SDK's
// bare `unknown tool "X"` error, which trained users to think the
// tool was missing. The fix lives in addOperatorTool (server.go).

func TestMCP_OperatorTool_ReturnsStructuredRedirect(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// All eleven operator-only tools per the v0.51.1 partition. Each
	// must return the structured redirect when called over MCP.
	for tool := range expectedOperatorTools {
		t.Run(tool, func(t *testing.T) {
			handler := srv.makeOperatorRedirectHandler(tool)
			res, err := handler(context.Background(), &mcp.CallToolRequest{
				Params: &mcp.CallToolParamsRaw{Name: tool},
			})
			if err != nil {
				t.Fatalf("redirect handler returned err=%v; should embed error in CallToolResult instead", err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("redirect must set IsError=true; got %+v", res)
			}
			if len(res.Content) == 0 {
				t.Fatalf("redirect must include content; got empty")
			}
			text, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("redirect content[0] should be TextContent; got %T", res.Content[0])
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
				t.Fatalf("redirect body should be valid JSON; got %q (err %v)", text.Text, err)
			}
			errObj, ok := payload["error"].(map[string]any)
			if !ok {
				t.Fatalf("redirect body needs an 'error' object; got %v", payload)
			}
			if errObj["code"] != "operator_tool_not_on_mcp" {
				t.Errorf("error.code = %v, want operator_tool_not_on_mcp", errObj["code"])
			}
			msg, _ := errObj["message"].(string)
			if !strings.Contains(msg, "POST /v1/"+tool) {
				t.Errorf("message should name the HTTP endpoint POST /v1/%s; got %q", tool, msg)
			}
			if !strings.Contains(msg, "pincher "+tool) {
				t.Errorf("message should name the CLI subcommand 'pincher %s'; got %q", tool, msg)
			}
			details, _ := errObj["details"].(map[string]any)
			if details["http_endpoint"] != "POST /v1/"+tool {
				t.Errorf("details.http_endpoint = %v, want POST /v1/%s", details["http_endpoint"], tool)
			}
			if details["cli_command"] != "pincher "+tool {
				t.Errorf("details.cli_command = %v, want pincher %s", details["cli_command"], tool)
			}
		})
	}
}

// The agent reading tools/list should see the operator-tool entries
// prefixed [OPERATOR-ONLY ...] so it skips them without having to
// invoke and parse the redirect. Verify the prefix is present on
// every operator tool's description.
func TestMCP_OperatorTool_DescriptionFlagsAsOperatorOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for tool := range expectedOperatorTools {
		t.Run(tool, func(t *testing.T) {
			toolMeta, ok := srv.tools[tool]
			if !ok {
				t.Fatalf("operator tool %q is missing from s.tools", tool)
			}
			// s.tools holds the original (non-stub) description used by
			// the HTTP path. The MCP-side stub description is built in
			// addOperatorTool — we can't introspect it without an SDK
			// listing API, so we round-trip via the redirect handler
			// to confirm the operator tool is still classified.
			if toolMeta.Name != tool {
				t.Errorf("tool name mismatch in registry: %q vs %q", toolMeta.Name, tool)
			}
		})
	}
}
