package ast

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// pythonRunner manages a persistent CPython subprocess invoked with
// `python3 -c <script> --daemon`. The daemon loop in
// python_extract.py reads one newline-delimited JSON request per
// line from stdin and writes one response per line to stdout.
//
// All public methods serialise via mu so concurrent per-file
// goroutines can safely share a single runner (the stdin/stdout
// pipes themselves are not multiplexed at the byte level — request
// and response must round-trip atomically). The serialisation cost
// is acceptable because the runner exists specifically to amortise
// the Windows ~80ms process-spawn + Python interpreter-init across
// many extract calls; serialisation overhead is microseconds.
//
// Lifecycle: lazy-started on first extract call. Restarted on EOF /
// broken-pipe / framing error. Never explicitly stopped — the OS
// reaps the subprocess when the parent pincher process exits. A
// future PR (#1626 follow-up) can add explicit Shutdown if
// per-Indexer lifecycle becomes important.
//
// Failure semantics: extract returns (nil, false) on any subprocess
// or framing error so callers can fall back to the per-file
// spawn path (preserving the existing "fall back to regex on
// failure" chain). The runner self-heals: after a failure it
// resets started=false so the next call relaunches.
type pythonRunner struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	started bool
	nextID  atomic.Int64
}

// defaultPythonRunner is the process-singleton runner used by
// extractPythonAST when PINCHER_PYTHON_AST_DAEMON=1.
var defaultPythonRunner = &pythonRunner{}

// daemonRequestTimeout caps an in-flight extract call. Mirrors the
// pyASTTimeout cap on the one-shot path — guards against pathological
// inputs that hang the parser. Set higher (15s) than the one-shot
// (10s) so an exceptional request doesn't trip the timeout while a
// queued goroutine is waiting on the mutex.
const daemonRequestTimeout = 15 * time.Second

// extract sends one request through the persistent subprocess and
// returns the response. Lazily starts the subprocess on first call.
// Restarts on any framing error so a single bad request doesn't
// poison the runner for subsequent good ones.
func (r *pythonRunner) extract(relpath string, source []byte) (*pythonResponse, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.started {
		if err := r.startLocked(); err != nil {
			slog.Warn("pincher.python_runner.start.err", "err", err)
			return nil, false
		}
	}

	req := struct {
		ID         int64  `json:"id"`
		Path       string `json:"path"`
		ContentB64 string `json:"content_b64"`
	}{
		ID:         r.nextID.Add(1),
		Path:       relpath,
		ContentB64: base64.StdEncoding.EncodeToString(source),
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, false
	}
	reqBytes = append(reqBytes, '\n')

	// Send + receive with a timeout. We can't use ctx-based
	// cancellation cleanly because io.Writer/io.Reader against an
	// os.Pipe don't support context. Use a deadline goroutine that
	// kills the subprocess on timeout — that breaks the pipe read
	// and unblocks us.
	done := make(chan struct{})
	var deadlineFired atomic.Bool
	go func() {
		select {
		case <-done:
			return
		case <-time.After(daemonRequestTimeout):
			deadlineFired.Store(true)
			r.killLocked() // safe to call without mu — kill is its own sync
		}
	}()
	defer close(done)

	if _, err := r.stdin.Write(reqBytes); err != nil {
		r.resetLocked()
		if deadlineFired.Load() {
			return nil, false
		}
		return nil, false
	}
	respLine, err := r.stdout.ReadString('\n')
	if err != nil {
		r.resetLocked()
		return nil, false
	}
	respLine = strings.TrimSpace(respLine)
	if respLine == "" {
		r.resetLocked()
		return nil, false
	}

	var resp pythonResponse
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		r.resetLocked()
		return nil, false
	}
	if resp.Error != "" {
		// Parser error or bad request — the daemon stays alive,
		// just this request failed. Don't reset the runner;
		// caller falls back to regex per the existing chain.
		return nil, false
	}
	return &resp, true
}

// startLocked launches the Python subprocess in daemon mode. Caller
// must hold mu.
func (r *pythonRunner) startLocked() error {
	pyCmd := pythonCommand()
	if pyCmd == nil {
		return errors.New("no working CPython 3 interpreter on PATH")
	}
	args := append(append([]string{}, pyCmd[1:]...), "-c", pythonExtractScript, "--daemon")
	r.cmd = exec.CommandContext(context.Background(), pyCmd[0], args...)
	stdin, err := r.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := r.cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := r.cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start: %w", err)
	}
	r.stdin = stdin
	r.stdout = bufio.NewReader(stdout)
	r.started = true
	return nil
}

// resetLocked tears down the current subprocess so the next extract
// call relaunches. Caller must hold mu.
func (r *pythonRunner) resetLocked() {
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		_, _ = r.cmd.Process.Wait()
	}
	r.cmd = nil
	r.stdin = nil
	r.stdout = nil
	r.started = false
}

// killLocked is the timeout path's process killer. Lock-free because
// it's invoked from the deadline goroutine while the main extract
// path holds mu — exec.Process.Kill is concurrent-safe.
func (r *pythonRunner) killLocked() {
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
}
