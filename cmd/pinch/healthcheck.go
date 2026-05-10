package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runHealthCheckCLI is a non-interactive probe useful for external
// watchdogs (cron, launchd, k8s liveness probes, monitoring agents).
// It spawns a pincher MCP server, completes the initialize handshake,
// requests `tools/list`, and exits 0 if the response arrives within
// the deadline. Any failure (spawn error, malformed response, timeout)
// produces a non-zero exit and a one-line diagnostic on stderr.
//
// Crucially, this targets ANY pincher binary — it doesn't require the
// supervisor to be running. The intended pairings:
//
//	pincher health-check                       # probe self via os.Executable
//	pincher health-check --binary /path/to/pincher
//	pincher health-check --binary /path/to/pincher --timeout 30s
//	pincher health-check --supervised          # spawn `pincher supervised` instead
//
// Output: prints `OK <stamped-version>` on stderr for success, or a
// failure summary. Stdout is reserved for future structured output
// (--json) and currently unused.
func runHealthCheckCLI(args []string) {
	fs := flag.NewFlagSet("health-check", flag.ExitOnError)
	binPath := fs.String("binary", "", "Path to the pincher binary to probe. Defaults to the running binary (os.Executable).")
	supervised := fs.Bool("supervised", false, "Probe via `pincher supervised` instead of a bare `pincher`.")
	timeout := fs.Duration("timeout", 10*time.Second, "Maximum total time for spawn + handshake + tools/list.")
	verbose := fs.Bool("verbose", false, "Print JSON-RPC traffic to stderr for debugging.")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "Usage: pincher health-check [--binary PATH] [--supervised] [--timeout DUR] [--verbose]")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Spawns a short-lived MCP client to confirm pincher answers the handshake +")
		fmt.Fprintln(out, "tools/list within --timeout. Exits 0 on success, non-zero on any failure.")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	bin := *binPath
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pincher health-check: cannot resolve own binary: %v\n", err)
			os.Exit(2)
		}
		bin = exe
	}

	cmdArgs := []string{}
	if *supervised {
		cmdArgs = append(cmdArgs, "supervised")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := healthCheckProbe(ctx, bin, cmdArgs, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

// replyToServerRequest produces a minimal JSON-RPC response to a
// server-initiated request. roots/list gets an empty roots array;
// anything else gets a "method not implemented" error so the server
// doesn't hang waiting on us. Best-effort write — failures are not
// fatal because we'll surface the real problem on the next read.
func replyToServerRequest(w io.Writer, id json.RawMessage, method string, verbose bool) {
	var resp map[string]any
	switch method {
	case "roots/list":
		resp = map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  map[string]any{"roots": []any{}},
		}
	default:
		resp = map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("method %q not implemented by health-check probe", method),
			},
		}
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return
	}
	out = append(out, '\n')
	if verbose {
		fmt.Fprintf(os.Stderr, "→ (reply to %s) %s", method, string(out))
	}
	_, _ = w.Write(out)
}

// healthCheckProbe spawns the binary, sends initialize +
// notifications/initialized + tools/list, and returns nil if all three
// succeed before the context deadline. Returns a wrapping error on any
// failure — the message is what the operator will see in cron logs.
func healthCheckProbe(ctx context.Context, bin string, args []string, verbose bool) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = os.Stderr // surface inner logs directly
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", bin, err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	r := bufio.NewReader(stdout)

	send := func(payload string) error {
		if verbose {
			fmt.Fprintf(os.Stderr, "→ %s", strings.TrimSpace(payload)+"\n")
		}
		_, err := io.WriteString(stdin, payload+"\n")
		return err
	}

	// expectResponseFor reads incoming lines until a response with the
	// given id arrives. It also handles SERVER→CLIENT requests
	// (roots/list, sampling, etc.) by replying with empty/permissive
	// payloads — without these replies the server blocks waiting for
	// us, which the original test surfaced as a tools/list deadlock.
	expectResponseFor := func(id int) (json.RawMessage, error) {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return nil, fmt.Errorf("read response (id=%d): %w", id, err)
			}
			if verbose {
				fmt.Fprintf(os.Stderr, "← %s", line)
			}
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if jsonErr := json.Unmarshal([]byte(line), &msg); jsonErr != nil {
				continue
			}

			// SERVER → CLIENT REQUEST: id present + method present
			// means the server is asking us something. Reply minimally
			// so the server's progress isn't blocked. roots/list is
			// the common one; for anything else we return an empty
			// JSON-RPC error to avoid hanging.
			if msg.ID != nil && msg.Method != "" {
				replyToServerRequest(stdin, msg.ID, msg.Method, verbose)
				continue
			}

			if msg.ID == nil {
				continue // server-initiated notification (no reply needed)
			}
			var got int
			if jsonErr := json.Unmarshal(msg.ID, &got); jsonErr != nil || got != id {
				continue
			}
			if len(msg.Error) > 0 {
				return nil, fmt.Errorf("server error on id=%d: %s", id, string(msg.Error))
			}
			return msg.Result, nil
		}
	}

	// 1. initialize
	if err := send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"pincher-health-check","version":"1"}}}`); err != nil {
		return fmt.Errorf("send initialize: %w", err)
	}
	initResult, err := expectResponseFor(1)
	if err != nil {
		return err
	}

	// 2. notifications/initialized — fire-and-forget
	if err := send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		return fmt.Errorf("send initialized: %w", err)
	}

	// 3. tools/list — actual liveness signal
	if err := send(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`); err != nil {
		return fmt.Errorf("send tools/list: %w", err)
	}
	toolsResult, err := expectResponseFor(2)
	if err != nil {
		return err
	}

	// Pretty-print the version + tool count from the responses for the
	// operator. Best-effort — failure to parse doesn't fail the probe.
	var initInfo struct {
		ServerInfo struct {
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	_ = json.Unmarshal(initResult, &initInfo)
	var toolsInfo struct {
		Tools []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(toolsResult, &toolsInfo)

	fmt.Fprintf(os.Stderr, "OK pincher %s (%d tools)\n", initInfo.ServerInfo.Version, len(toolsInfo.Tools))
	return nil
}
