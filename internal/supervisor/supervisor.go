// Package supervisor wraps an inner pincher MCP server with auto-respawn
// + initialize-replay so the MCP client sees an unbroken stdio session
// even when the inner exits (e.g. on schema/binary drift via #352, on
// crash, or on OS-level kill).
//
// Architecture:
//
//	┌──────────────┐  stdio  ┌────────────────────┐  stdio  ┌──────────────┐
//	│  MCP client  ├────────►│      Supervisor    │◄────────┤ inner pincher│
//	│  (Claude/Codex)│       │ (long-lived)       │ exec.Cmd│   MCP server │
//	└──────────────┘         │  ▲ captures init   │         └──────────────┘
//	                         │  ▲ replays on      │
//	                         │    inner exit      │
//	                         └────────────────────┘
//
// Two pump goroutines run concurrently:
//   - clientToInner reads JSON-RPC lines from the client and writes them
//     to the current inner's stdin, optionally capturing initialize and
//     notifications/initialized for later replay.
//   - innerToClient io.Copy's the current inner's stdout back to the
//     client. On EOF (inner exit), it respawns the inner, replays the
//     captured init, and resumes copying.
//
// The current inner is held behind a sync.RWMutex. The inner→client pump
// takes the write lock during respawn so the client→inner pump's writes
// to the (now-broken) old stdin pipe don't race with the swap.
//
// Known limitation (MVP scope — S1 of #X):
//
// Requests in flight during the ~100ms respawn window may be lost — the
// write to the broken old stdin returns ErrBrokenPipe and we don't retry
// on the new inner. The client sees no response for that request and
// will time out. S2 (liveness probe + circuit breaker) reduces respawn
// frequency; a future enhancement could buffer + retry pending writes.
package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
)

// Supervisor wraps an inner pincher process with auto-respawn semantics.
// Configure via the public fields, then call Run.
type Supervisor struct {
	// BinaryPath is the absolute path to the pincher binary to spawn as
	// the inner MCP server. Typically the supervisor's own argv[0] when
	// invoked as `pincher supervised`.
	BinaryPath string

	// InnerArgs are passed verbatim to the inner pincher invocation.
	// Empty means "default MCP server mode" — pincher's main() with no
	// subcommand and no flags runs the stdio MCP loop.
	InnerArgs []string

	// Env is the environment for the inner. Pass os.Environ() in the
	// usual case; tests substitute a controlled set.
	Env []string

	// Stdin / Stdout / Stderr are the streams to/from the MCP client.
	// In production these are os.Stdin / os.Stdout / os.Stderr; tests
	// substitute pipes.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// spawnFn is the inner-process factory. Tests override this with a
	// fake to avoid spawning real binaries. Defaults to defaultSpawn,
	// which exec's BinaryPath with InnerArgs and Env.
	spawnFn func() (*innerProc, error)

	mu              sync.RWMutex
	inner           *innerProc
	initLine        []byte
	initializedLine []byte

	// Restarts is incremented every time the inner exits and is
	// successfully respawned. Read by tests + future health surface.
	Restarts atomic.Int32
}

// innerProc bundles a running inner process with the pipes we own.
type innerProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

// New returns a supervisor pre-configured with the default exec-based
// spawner. Callers must still set Stdin/Stdout/Stderr/Env before Run.
func New(binaryPath string) *Supervisor {
	s := &Supervisor{BinaryPath: binaryPath}
	s.spawnFn = s.defaultSpawn
	return s
}

// defaultSpawn execs s.BinaryPath with InnerArgs/Env and returns the
// inner with stdin/stdout pipes connected. The inner's stderr is
// forwarded to s.Stderr (so users see the inner's logs without the
// supervisor having to demux them).
func (s *Supervisor) defaultSpawn() (*innerProc, error) {
	cmd := exec.Command(s.BinaryPath, s.InnerArgs...)
	cmd.Env = s.Env
	cmd.Stderr = s.Stderr

	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	return &innerProc{cmd: cmd, stdin: in, stdout: out}, nil
}

// Run is the supervisor's main loop. It spawns the initial inner,
// runs both pump goroutines, and returns when the client disconnects
// (stdin EOF) or the context is cancelled. Returns nil on clean
// client-driven shutdown; non-nil for setup failures or unrecoverable
// respawn errors.
func (s *Supervisor) Run(ctx context.Context) error {
	// Sub-context lets us cancel the pumps once one of them signals
	// shutdown via its return — without this, the inner→client pump
	// would respawn infinitely as the just-closed inner returns EOF
	// on every Copy call.
	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	p, err := s.spawnFn()
	if err != nil {
		return fmt.Errorf("initial spawn: %w", err)
	}
	s.mu.Lock()
	s.inner = p
	s.mu.Unlock()

	clientDone := make(chan error, 1)
	innerDone := make(chan error, 1)

	go func() { clientDone <- s.pumpClientToInner(pumpCtx) }()
	go func() { innerDone <- s.pumpInnerToClient(pumpCtx) }()

	// Either pump terminating ends the supervisor:
	//   - clientToInner returning means the client closed stdin →
	//     cancel + shut down the inner.
	//   - innerToClient returning means an unrecoverable respawn
	//     error — propagate.
	//   - context cancellation → both pumps return; drain both.
	var (
		runErr           error
		clientPumpClosed bool
		innerPumpClosed  bool
	)
	select {
	case err := <-clientDone:
		runErr = err
		clientPumpClosed = true
	case err := <-innerDone:
		runErr = err
		innerPumpClosed = true
	case <-ctx.Done():
		runErr = ctx.Err()
	}

	cancel()           // signal pumps to stop respawning / reading
	s.shutdownInner()  // close pipes so any in-flight Copy / Read returns

	// Drain only the pump(s) we haven't already received from. The
	// select above consumed at most one channel; reading from an
	// already-drained buffered channel here would block on empty.
	if !clientPumpClosed {
		<-clientDone
	}
	if !innerPumpClosed {
		<-innerDone
	}
	return runErr
}

// pumpClientToInner reads JSON-RPC lines from the client stdin,
// captures initialize / notifications/initialized for replay, and
// forwards each line to the current inner's stdin.
//
// Returns nil on client EOF (clean disconnect) or a non-nil error on
// read failure. Write errors to the inner pipe are non-fatal — the
// inner-side pump owns the respawn flow.
func (s *Supervisor) pumpClientToInner(ctx context.Context) error {
	r := bufio.NewReader(s.Stdin)
	for {
		if ctx.Err() != nil {
			return nil
		}
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			s.maybeCaptureInit(line)
			if writeErr := s.writeToInner(line); writeErr != nil {
				slog.Debug("supervisor.client_to_inner.write_err",
					"err", writeErr.Error(), "loss_window", "respawn")
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				slog.Info("supervisor.client_eof")
				return nil
			}
			return fmt.Errorf("read client stdin: %w", err)
		}
	}
}

// pumpInnerToClient io.Copy's the current inner's stdout to the
// client. When the inner exits (Copy returns), it triggers a respawn
// and resumes copying from the new inner. Returns nil on context
// cancellation or non-nil if respawn fails.
func (s *Supervisor) pumpInnerToClient(ctx context.Context) error {
	for {
		// Check shutdown FIRST: Run() cancels pumpCtx + closes the
		// inner pipe to signal "we're done". Without this check we'd
		// re-enter the respawn loop on the just-closed inner.
		if ctx.Err() != nil {
			return nil
		}
		s.mu.RLock()
		in := s.inner
		s.mu.RUnlock()
		if in == nil {
			// Shutdown set inner to nil — exit cleanly.
			return nil
		}

		_, _ = io.Copy(s.Stdout, in.stdout)

		// Reap the inner so we have its exit code for logging. cmd
		// is nil in tests where the inner is a stdio pair without a
		// real OS process; skip Wait/ProcessState in that path.
		var waitErr error
		exitCode := -1
		if in.cmd != nil {
			waitErr = in.cmd.Wait()
			if in.cmd.ProcessState != nil {
				exitCode = in.cmd.ProcessState.ExitCode()
			}
		}

		if ctx.Err() != nil {
			return nil
		}

		slog.Info("supervisor.inner_exited",
			"exit_code", exitCode,
			"wait_err", fmt.Sprint(waitErr),
			"restarts_so_far", s.Restarts.Load())

		if err := s.respawn(); err != nil {
			return fmt.Errorf("respawn after inner exit: %w", err)
		}
		s.Restarts.Add(1)
	}
}

// maybeCaptureInit parses just enough of an inbound JSON-RPC line to
// detect an initialize request or notifications/initialized
// notification, and stashes the raw bytes for later replay. We re-capture
// each time so re-init from the client (rare, e.g. after roots change)
// updates the replay payload.
func (s *Supervisor) maybeCaptureInit(line []byte) {
	var head struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return
	}
	switch head.Method {
	case "initialize":
		s.mu.Lock()
		s.initLine = append(s.initLine[:0], line...)
		s.mu.Unlock()
	case "notifications/initialized", "initialized":
		s.mu.Lock()
		s.initializedLine = append(s.initializedLine[:0], line...)
		s.mu.Unlock()
	}
}

// writeToInner writes one client-originated line to the current inner's
// stdin. Returns the underlying write error so the caller can log
// (loss-during-respawn telemetry); does NOT trigger respawn — that's
// the inner-side pump's job.
func (s *Supervisor) writeToInner(line []byte) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.inner == nil {
		return errors.New("supervisor: inner is nil")
	}
	_, err := s.inner.stdin.Write(line)
	return err
}

// respawn spawns a fresh inner, replays the captured init handshake,
// and atomically swaps it in for the old. Holds the write lock for the
// duration so client→inner writes can't race with the swap.
func (s *Supervisor) respawn() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.spawnFn()
	if err != nil {
		return err
	}

	// Replay handshake. If the client never finished initialize we
	// have nothing to replay — the new inner will just wait for the
	// client's initialize like a fresh start.
	if len(s.initLine) > 0 {
		if _, err := p.stdin.Write(s.initLine); err != nil {
			_ = p.cmd.Process.Kill()
			return fmt.Errorf("replay initialize: %w", err)
		}
	}
	if len(s.initializedLine) > 0 {
		if _, err := p.stdin.Write(s.initializedLine); err != nil {
			_ = p.cmd.Process.Kill()
			return fmt.Errorf("replay initialized: %w", err)
		}
	}

	s.inner = p
	slog.Info("supervisor.respawn", "init_replayed", len(s.initLine) > 0)
	return nil
}

// shutdownInner kills and reaps the current inner. Idempotent. Tolerates
// cmd==nil for tests where the inner is a fake stdio pair.
func (s *Supervisor) shutdownInner() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inner == nil {
		return
	}
	if s.inner.cmd != nil && s.inner.cmd.Process != nil {
		_ = s.inner.cmd.Process.Kill()
		_ = s.inner.cmd.Wait()
	}
	// Close stdin to unblock any pending writes; closing stdout
	// helps the inner→client pump's io.Copy return.
	_ = s.inner.stdin.Close()
	if closer, ok := s.inner.stdout.(io.Closer); ok {
		_ = closer.Close()
	}
	s.inner = nil
}
