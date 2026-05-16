package server

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// requestIDHeader is the HTTP header pincher reads an inbound correlation
// ID from and echoes the resolved ID back on (#657).
const requestIDHeader = "X-Request-ID"

// maxRequestIDLen bounds a caller-supplied X-Request-ID. The value is
// echoed in a response header and embedded in JSON _meta — an unbounded
// or non-printable value is a header-injection / log-poisoning vector,
// so anything longer is treated as junk and replaced with a fresh ID.
const maxRequestIDLen = 200

type requestIDCtxKey struct{}

// newRequestID mints a UUID v7 — time-ordered, so a router sorting by
// request_id gets chronological order for free. Falls back to a v4 if
// the v7 generator errors (clock issues); never returns empty.
func newRequestID() string {
	if v7, err := uuid.NewV7(); err == nil {
		return v7.String()
	}
	return uuid.NewString()
}

// sanitizeRequestID accepts a caller-supplied ID only when it is
// non-empty, within the length bound, and printable ASCII (no control
// chars, no CR/LF). Otherwise it mints a fresh ID.
func sanitizeRequestID(raw string) string {
	if raw == "" || len(raw) > maxRequestIDLen {
		return newRequestID()
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return newRequestID()
		}
	}
	return raw
}

// withRequestIDContext stashes the resolved request ID on the context so
// the tool-handler wrapper can read it without re-threading every handler
// signature.
func withRequestIDContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDCtxKey{}, id)
}

// requestIDFromContext returns the request ID stashed by ServeHTTP, or ""
// for MCP-stdio calls (which carry no HTTP headers).
func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDCtxKey{}).(string)
	return id
}

// withRequestID wraps a tool handler so every response carries a
// correlation ID in _meta.request_id (#657). The ID comes from the
// X-Request-ID header (stashed in ctx by ServeHTTP) when the call
// arrived over HTTP; MCP-stdio calls and header-less HTTP calls get a
// freshly minted UUID v7. The resolved ID is logged so a router can
// trace one request end-to-end through pincher.
func (s *Server) withRequestID(handler mcp.ToolHandler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rid := requestIDFromContext(ctx)
		if rid == "" {
			rid = newRequestID()
			ctx = withRequestIDContext(ctx, rid)
		}
		res, err := handler(ctx, req)
		tool := ""
		if req != nil && req.Params != nil {
			tool = req.Params.Name
		}
		slog.Debug("pincher.tool.call", "tool", tool, "request_id", rid)
		if err == nil && res != nil {
			injectRequestID(res, rid)
		}
		return res, err
	}
}

// injectRequestID sets _meta.request_id on a tool result. Handler
// responses are a single TextContent carrying a JSON object built by
// jsonResultWithMeta (success) or errResultRich (error) — both have a
// _meta object. Plain errResult text isn't JSON; injection is skipped
// and the HTTP X-Request-ID response header still carries the ID.
func injectRequestID(res *mcp.CallToolResult, id string) {
	if len(res.Content) == 0 {
		return
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &data); err != nil {
		return
	}
	meta, ok := data["_meta"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		data["_meta"] = meta
	}
	meta["request_id"] = id
	// #1089: route through marshalMetaJSON so PINCHER_DEBUG_META=1 stays
	// honored across success bodies, error bodies, and this middleware
	// re-encode. Pre-fix the middleware always reset responses to
	// compact, defeating the env flag for any HTTP-served request.
	tc.Text = string(marshalMetaJSON(data))
}
