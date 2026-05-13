// Command probe is a permanent diagnostic harness for the supervisor's
// auto-restart-on-binary-drift flow. It drives an out-of-band JSON-RPC
// session against either a bare `pincher` or `pincher supervised`
// process, completes the MCP initialize handshake (including
// roots/list reply — the inner blocks until that arrives), bumps the
// binary's mtime to simulate a rebuild, and reports for each tool call
// whether the response was received before EOF.
//
// History: this code originated as cmd/test-364 (the #364 escape hatch
// that surfaced silent process exits), evolved into cmd/test-371 and
// cmd/test-371b (the two-mode reproducer that pinned #371's lost-
// response root cause), and is consolidated here as a maintained
// debugging tool. Not a CLI subcommand of pincher itself — too niche;
// callers that just want a one-shot liveness check should use
// `pincher health-check` instead.
//
// Usage:
//
//	go run ./internal/supervisor/cmd/probe -binary ./pincher.exe
//	go run ./internal/supervisor/cmd/probe -binary ./pincher.exe -supervised
//	go run ./internal/supervisor/cmd/probe -binary ./pincher.exe -supervised -timeout 60s
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
	"path/filepath"
	"sync"
	"time"
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

// rpcTimeout returns the per-RPC round-trip wait used in probe expect()
// calls. Defaults to 15s; tunable via PINCHER_TEST_RPC_TIMEOUT (Go
// duration syntax, e.g. "30s", "1m"). Default bumped from 5s → 15s in
// v0.55 (#681 Bucket A) — the original 5s budget was borderline on
// the windows-latest CI runner under -p 2 parallelism, surfacing as
// `TestRun_EndToEnd_BareMode` flakes (`timeout 5s waiting for id=10`).
// 15s is still fast for a healthy run; dramatically reduces flake rate.
func rpcTimeout() time.Duration {
	if v := os.Getenv("PINCHER_TEST_RPC_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 15 * time.Second
}

func main() {
	binPath := flag.String("binary", "", "Path to pincher binary. Default: ./pincher.exe in cwd.")
	supervised := flag.Bool("supervised", false, "Drive `pincher supervised` instead of bare `pincher`.")
	timeout := flag.Duration("timeout", 30*time.Second, "Total wall-clock budget for the run.")
	flag.Parse()

	if *binPath == "" {
		wd, err := os.Getwd()
		if err != nil {
			die("cwd: %v", err)
		}
		*binPath = filepath.Join(wd, "pincher.exe")
	}
	if _, err := os.Stat(*binPath); err != nil {
		die("binary not found at %s: %v", *binPath, err)
	}
	if err := run(*binPath, *supervised, *timeout); err != nil {
		die("%v", err)
	}
}

func run(binPath string, supervised bool, totalTimeout time.Duration) error {
	mode := "bare"
	if supervised {
		mode = "supervised"
	}
	fmt.Fprintf(os.Stderr, "probe mode=%s binary=%s\n", mode, binPath)

	ctx, cancel := context.WithTimeout(context.Background(), totalTimeout)
	defer cancel()

	args := []string{}
	if supervised {
		args = append(args, "supervised")
	}
	cmd := exec.CommandContext(ctx, binPath, args...)
	// PINCHER_AUTO_RESTART_ON_DRIFT=1 is what makes the inner self-exit
	// after a tool call when its binary's mtime has advanced — the
	// trigger we're probing.
	cmd.Env = append(os.Environ(), "PINCHER_AUTO_RESTART_ON_DRIFT=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stderrBuf := &bytesCollector{}
	go func() { _, _ = io.Copy(stderrBuf, stderrPipe) }()
	defer func() {
		fmt.Fprintln(os.Stderr, "\n==== captured stderr ====")
		fmt.Fprintln(os.Stderr, stderrBuf.String())
		fmt.Fprintln(os.Stderr, "==== end captured stderr ====")
	}()
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	r := bufio.NewReader(stdout)
	send := func(payload string) error {
		fmt.Fprintln(os.Stderr, "→", trunc(payload, 100))
		_, err := io.WriteString(stdin, payload+"\n")
		return err
	}
	expect := func(id int, deadline time.Duration) (rpcMsg, error) {
		ch := make(chan struct {
			m   rpcMsg
			err error
		}, 1)
		go func() {
			for {
				line, rerr := r.ReadString('\n')
				if rerr != nil {
					ch <- struct {
						m   rpcMsg
						err error
					}{rpcMsg{}, rerr}
					return
				}
				var m rpcMsg
				if jerr := json.Unmarshal([]byte(line), &m); jerr != nil {
					continue
				}
				// Server→client request: id + method present. Reply
				// minimally so the inner doesn't block. The real MCP
				// server sends roots/list during init.
				if m.ID != nil && m.Method != "" {
					var resp string
					if m.Method == "roots/list" {
						resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"roots":[]}}`+"\n", string(m.ID))
					} else {
						resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"not implemented by probe"}}`+"\n", string(m.ID))
					}
					_, _ = io.WriteString(stdin, resp)
					continue
				}
				if m.ID == nil {
					continue
				}
				var got int
				if jerr := json.Unmarshal(m.ID, &got); jerr != nil || got != id {
					continue
				}
				fmt.Fprintln(os.Stderr, "←", trunc(line, 120))
				ch <- struct {
					m   rpcMsg
					err error
				}{m, nil}
				return
			}
		}()
		select {
		case res := <-ch:
			return res.m, res.err
		case <-time.After(deadline):
			return rpcMsg{}, fmt.Errorf("timeout %s waiting for id=%d", deadline, id)
		}
	}

	// 1. initialize
	fmt.Fprintln(os.Stderr, "\n=== initialize ===")
	if err := send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"supervisor-probe","version":"1"}}}`); err != nil {
		return err
	}
	if _, err := expect(1, rpcTimeout()); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)

	// 2. health (pre-bump)
	fmt.Fprintln(os.Stderr, "\n=== call 10: health (pre-bump) ===")
	if err := send(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"health","arguments":{}}}`); err != nil {
		return err
	}
	if _, err := expect(10, rpcTimeout()); err != nil {
		return fmt.Errorf("pre-bump health: %w", err)
	}
	fmt.Fprintln(os.Stderr, "✓ pre-bump health response received")

	// 3. bump mtime
	fmt.Fprintln(os.Stderr, "\n=== bump pincher.exe mtime ===")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(binPath, future, future); err != nil {
		return fmt.Errorf("chtimes: %w", err)
	}
	time.Sleep(200 * time.Millisecond)

	// 4. stats (post-bump) — should receive response, then inner exits
	fmt.Fprintln(os.Stderr, "\n=== call 20: stats (post-bump; inner should self-restart after responding) ===")
	if err := send(`{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"stats","arguments":{}}}`); err != nil {
		return err
	}
	post := "✗ NO RESPONSE for call 20 — confirms the response-loss bug"
	if _, err := expect(20, rpcTimeout()); err == nil {
		post = "✓ post-bump stats response received (response-loss fix working)"
	}
	fmt.Fprintln(os.Stderr, post)

	// 5. process state — bare mode: inner should be exiting; supervised: parent stays alive
	time.Sleep(500 * time.Millisecond)
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()
	select {
	case e := <-exitCh:
		if supervised {
			fmt.Fprintln(os.Stderr, "✗ supervisor process exited:", e, "(supervised mode should keep parent alive across respawn)")
		} else {
			fmt.Fprintln(os.Stderr, "ℹ inner process exited (expected in bare mode):", e)
		}
		return nil
	case <-time.After(800 * time.Millisecond):
		// fall through
	}

	// 6. supervised mode: try a follow-up call against the new inner
	if !supervised {
		fmt.Fprintln(os.Stderr, "ℹ inner still alive 800ms after call 20 — bare mode would normally have exited; possible flush race")
		return nil
	}
	fmt.Fprintln(os.Stderr, "\n=== call 30: health on (presumed) new inner ===")
	if err := send(`{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"health","arguments":{}}}`); err != nil {
		fmt.Fprintln(os.Stderr, "✗ send to supervisor failed (stdio closed):", err)
		return nil
	}
	if _, err := expect(30, 3*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "✗ NO RESPONSE for follow-up call 30:", err)
	} else {
		fmt.Fprintln(os.Stderr, "✓ follow-up health response received — supervised flow recovered")
	}
	return nil
}

type bytesCollector struct {
	mu  sync.Mutex
	buf []byte
}

func (b *bytesCollector) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *bytesCollector) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func trunc(s string, n int) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func die(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "FAIL:", fmt.Sprintf(format, args...))
	os.Exit(1)
}
