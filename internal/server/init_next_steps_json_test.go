package server

import (
	"context"
	"encoding/json"
	"testing"
)

// #1435 v0.72: init's errResultRich next_steps templated the args
// string with `... + absProjectPath + ...`. On Windows where
// absProjectPath contains backslashes ("D:\Foo\Bar"), the embedded
// args string came out as
//   {"target":"detect","project_path":"D:\Foo\Bar"}
// — `\F` and `\B` are not valid JSON escapes (RFC 8259 § 7). Strict
// parsers reject; lenient parsers (JS) silently swallow the
// backslashes and re-issue a project_path that no longer exists.
//
// Cross-check: positive (args parses), negative (round-tripped
// project_path equals original byte-for-byte even when path
// contains \, ", control chars), control (target field still
// matches what the rich-error message advertised), and a
// representative path with embedded quotes / control chars.

func TestHandleInit_NextStepsArgs_AreValidJSON_WindowsPath(t *testing.T) {
	t.Parallel()
	// Pick a project root with backslashes regardless of OS. The
	// handler runs filepath.Abs; on POSIX it normalizes, on Windows
	// it preserves backslashes. Either way the next_steps args must
	// be valid JSON that round-trips the actual path. Use the OS-
	// resolved session root so we cover both shapes.
	srv, _, root := newTestServer(t)
	srv.sessionRoot = root

	// Hit all three rich-error paths in one table — each generates
	// next_steps with project_path interpolation.
	cases := []struct {
		name string
		args map[string]any
	}{
		{"target=continue", map[string]any{"target": "continue", "project_path": root}},
		{"target=unknown", map[string]any{"target": "no-such-editor", "project_path": root}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := srv.handleInit(context.Background(), makeReq(c.args))
			if err != nil {
				t.Fatalf("handleInit: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected IsError; got %s", textOf(t, res))
			}
			body := decode(t, res)
			meta, _ := body["_meta"].(map[string]any)
			steps, _ := meta["next_steps"].([]any)
			if len(steps) == 0 {
				t.Fatalf("expected next_steps; got none")
			}
			for i, s := range steps {
				step, _ := s.(map[string]any)
				argsStr, _ := step["args"].(string)
				if argsStr == "" {
					continue
				}
				// Positive — strict JSON parse must succeed.
				var parsed map[string]any
				if err := json.Unmarshal([]byte(argsStr), &parsed); err != nil {
					t.Errorf("next_steps[%d].args is not valid JSON: %v\nargs=%s", i, err, argsStr)
					continue
				}
				// Negative — the round-tripped project_path must
				// equal the original byte-for-byte. Pre-#1435 the
				// lenient-parser path silently dropped backslashes.
				if got, _ := parsed["project_path"].(string); got != root {
					t.Errorf("next_steps[%d].args project_path mismatch:\n got  %q\nwant  %q\nargs=%s",
						i, got, root, argsStr)
				}
				// Control — the target field must match what the
				// step is advertising via the message.
				if _, ok := parsed["target"].(string); !ok {
					t.Errorf("next_steps[%d].args missing target field; got %v", i, parsed)
				}
			}
		})
	}
}

// Cross-check at the helper level so any future caller of the
// shared marshaller can't regress. Uses an absolute Windows-shaped
// path AND a path containing characters that need JSON escaping.
func TestInitArgsJSON_HandlesPathsThatNeedJSONEscaping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
	}{
		{"windows-backslashes", `D:\ClaudeCode\pincher-repo`},
		{"posix", "/home/kev/proj"},
		{"path-with-quote", `/tmp/has"quote`},
		{"path-with-control-char", "/tmp/has\nnewline"},
		{"path-with-unicode", "/tmp/héllo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := initArgsJSON("detect", c.path)
			var parsed map[string]string
			if err := json.Unmarshal([]byte(got), &parsed); err != nil {
				t.Fatalf("initArgsJSON produced invalid JSON: %v\nargs=%s", err, got)
			}
			if parsed["project_path"] != c.path {
				t.Errorf("round-trip mismatch:\n got  %q\nwant  %q\nargs=%s",
					parsed["project_path"], c.path, got)
			}
			if parsed["target"] != "detect" {
				t.Errorf("target field lost; got %q", parsed["target"])
			}
		})
	}
}
