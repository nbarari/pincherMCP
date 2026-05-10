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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// probeIDPrefix tags the JSON-RPC `id` field on supervisor-internal
	// liveness probes so the inner→client pump can recognize the
	// matching response and swallow it (don't leak probe responses to
	// the client).
	probeIDPrefix = "__pincher_supervisor_probe_"

	// initIDPrefix tags the JSON-RPC `id` field on supervisor-issued
	// initialize-replay requests. The matching response from the new
	// inner is intercepted in forwardInnerStdoutWithProbeFilter and
	// NEVER reaches the client — the client already received an
	// initialize response from the original inner and a duplicate
	// (with the original ID OR a different one with the same shape)
	// would violate JSON-RPC framing. S1.5 fix for #371.
	initIDPrefix = "__pincher_supervisor_init_"

	// SupervisorStatusToolName is the MCP tool name the supervisor
	// answers directly (without forwarding to the inner). Agents call
	// it to check supervisor health: restart count, probe stats,
	// uptime. Out-of-band knowledge for now — the tool is NOT injected
	// into tools/list responses (that's a future enhancement). The
	// dotted notation distinguishes it from inner pincher tools, which
	// are unprefixed (search, query, symbol, etc.).
	SupervisorStatusToolName = "pincher.supervisor.status"

	// Defaults for liveness/circuit-breaker tunables. Tests override.
	defaultProbeInterval = 30 * time.Second
	defaultProbeTimeout  = 5 * time.Second
	defaultMaxRestarts   = 5
	defaultRestartWindow = 60 * time.Second

	// defaultRespawnQuietWindow is the post-respawn window during which
	// server-initiated notifications from the new inner (e.g.
	// `notifications/tools/list_changed`, `notifications/initialized`
	// echoes) are dropped instead of forwarded. The client already
	// processed equivalent notifications from the original inner;
	// re-firing them mid-session would surface as duplicate state
	// changes the client may reject. 500ms is enough for the new
	// inner to finish its init-side notification burst without delaying
	// real responses much. Tests override with a smaller window.
	defaultRespawnQuietWindow = 500 * time.Millisecond
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

	// ProbeInterval / ProbeTimeout / MaxRestarts / RestartWindow tune
	// the S2 liveness + circuit-breaker behavior. Zero values mean
	// "use the default constant". Tests override with short values
	// (~50ms intervals) to keep runtime low.
	ProbeInterval time.Duration
	ProbeTimeout  time.Duration
	MaxRestarts   int
	RestartWindow time.Duration

	// RespawnQuietWindow tunes the post-respawn drop window for
	// server-initiated notifications. Zero uses the default.
	RespawnQuietWindow time.Duration

	mu              sync.RWMutex
	inner           *innerProc
	// initParams holds the most-recently-captured `params` payload
	// from a client `initialize` request. We store params (not the
	// whole line) because on respawn we synthesize a fresh request
	// with a supervisor-sentinel ID — matching the client's original
	// ID would let the new inner's response leak to the client and
	// look like a duplicate (the bug fixed in S1.5 / #371).
	initParams        []byte
	initializedLine   []byte
	initReplayCounter atomic.Int64

	// respawnQuietUntil is the wall-clock instant after which
	// server-initiated notifications start passing through to the
	// client again. Set in respawn() to now + RespawnQuietWindow;
	// checked in forwardInnerStdoutWithProbeFilter on every line.
	respawnQuietUntil atomic.Pointer[time.Time]

	// pendingProbe captures the timestamp at which the most recent
	// liveness probe was sent to the inner. Cleared (atomic.Pointer
	// to nil) when a matching response arrives. The probe goroutine
	// schedules a timeout-kill that fires only if the same probe is
	// still pending — recording the sentAt on the timer closure
	// detects "this probe got stuck" vs "a later probe replaced it."
	pendingProbe atomic.Pointer[probeState]

	probeIDCounter atomic.Int64

	// Restart history for circuit-breaker. Bounded ring of timestamps
	// of the last few restarts; entries older than RestartWindow are
	// trimmed at each recordRestart() call.
	restartHistoryMu sync.Mutex
	restartHistory   []time.Time

	// Restarts is incremented every time the inner exits and is
	// successfully respawned. Read by tests + future health surface.
	Restarts atomic.Int32

	// ProbesSent counts liveness probes dispatched. Useful for tests
	// asserting the goroutine is running.
	ProbesSent atomic.Int64

	// ProbesAnswered counts probes whose response was intercepted on
	// inner→client. Should track ProbesSent in steady state.
	ProbesAnswered atomic.Int64

	// ProbesTimedOut counts probes that fired the timeout-kill.
	// Non-zero in steady state suggests a flapping inner.
	ProbesTimedOut atomic.Int64

	// startedAt is set in Run before any pump goroutines launch.
	// Surfaced via the status tool as uptime.
	startedAt time.Time

	// lastRestartReason records why the most recent respawn happened.
	// Updated under restartHistoryMu. Surfaced via the status tool.
	lastRestartReason string

	// nextRestartReason is a one-shot override the probe-timeout path
	// uses to inject "probe timeout (inner unresponsive)" before
	// killInner; pumpInnerToClient picks it up on the next respawn
	// and clears it. Without this, every respawn would be labelled
	// "inner exited (code=X)" — unhelpful when the cause was our own
	// probe deciding the inner was hung.
	nextRestartReasonMu sync.Mutex
	nextRestartReason   string
}

// SupervisorStatus is the response payload of the
// `pincher.supervisor.status` MCP tool. Stable JSON shape (renaming or
// removing fields is a breaking change for any agent that parses it).
type SupervisorStatus struct {
	Alive             bool   `json:"alive"`
	UptimeSec         int64  `json:"uptime_sec"`
	Restarts          int32  `json:"restarts"`
	ProbesSent        int64  `json:"probes_sent"`
	ProbesAnswered    int64  `json:"probes_answered"`
	ProbesTimedOut    int64  `json:"probes_timed_out"`
	LastRestartReason string `json:"last_restart_reason,omitempty"`
	SupervisorVersion string `json:"supervisor_version,omitempty"`
}

// probeState is the pendingProbe payload — the time the probe went
// out, and the sentinel ID we expect on the response. Stored as an
// atomic.Pointer so the timeout closure can compare against the
// in-flight probe identity (rather than just an opaque flag).
type probeState struct {
	id     string
	sentAt time.Time
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

	s.startedAt = time.Now()

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
	go s.probeLoop(pumpCtx) // S2: liveness probe; exits on ctx cancel

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
	s.shutdownInner()  // close inner pipes so any in-flight Copy / Read returns

	// pumpClientToInner is blocked on Read(client.Stdin) — context
	// cancellation alone won't unblock that. If the caller's Stdin
	// is a Closer (os.Stdin, *io.PipeReader, *os.File all are), close
	// it so the pump's Read returns EOF and the goroutine exits. We
	// don't track ownership; closing an already-closed reader is a
	// safe no-op for these types.
	if c, ok := s.Stdin.(io.Closer); ok {
		_ = c.Close()
	}

	// Drain only the pump(s) we haven't already received from. Bounded
	// timeout protects against a client.Stdin that doesn't honor Close
	// (a non-stdlib io.Reader where Close is no-op or absent) — a leak
	// of one goroutine is better than a hung supervisor.
	drainTimeout := time.After(2 * time.Second)
	if !clientPumpClosed {
		select {
		case <-clientDone:
		case <-drainTimeout:
			slog.Warn("supervisor.client_pump_drain_timeout",
				"hint", "stdin reader not honoring Close()")
		}
	}
	if !innerPumpClosed {
		select {
		case <-innerDone:
		case <-drainTimeout:
			slog.Warn("supervisor.inner_pump_drain_timeout")
		}
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
			// S3: intercept calls to the supervisor's own status tool;
			// don't forward them to the inner. Direct response to the
			// client matches the tool's MCP shape.
			if s.handleStatusToolCall(line) {
				continue
			}
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

		// Read inner stdout line-by-line so we can intercept liveness
		// probe responses (S2). Probe responses carry an ID matching
		// probeIDPrefix and must NOT reach the client. All other lines
		// pass through verbatim.
		s.forwardInnerStdoutWithProbeFilter(in.stdout)

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

		// Record reason for the status tool. The probe-timeout path
		// pre-stages a one-shot override; pump consumes + clears it
		// here. Without override, fall back to the exit-code form.
		s.nextRestartReasonMu.Lock()
		override := s.nextRestartReason
		s.nextRestartReason = ""
		s.nextRestartReasonMu.Unlock()

		reason := override
		if reason == "" {
			reason = fmt.Sprintf("inner exited (code=%d)", exitCode)
		}
		s.restartHistoryMu.Lock()
		s.lastRestartReason = reason
		s.restartHistoryMu.Unlock()

		if err := s.respawn(); err != nil {
			return fmt.Errorf("respawn after inner exit: %w", err)
		}
		s.Restarts.Add(1)
		// S2 circuit breaker: if we've respawned too many times in a
		// short window, stop trying. The inner is in a bad state we
		// can't recover from by restarting (corrupt DB, missing
		// dependency, persistent crash). Surfacing as a Run() error
		// is more useful than a hot loop.
		if err := s.recordRestart(); err != nil {
			return err
		}
	}
}

// forwardInnerStdoutWithProbeFilter reads inner stdout line-by-line.
// Three categories of line get filtered:
//   - Liveness probe responses (id matches probeIDPrefix) — clear
//     the pending probe state.
//   - Init-replay responses (id matches initIDPrefix) — the client
//     already received an initialize response from the original
//     inner; surfacing a duplicate would close stdio. S1.5 / #371.
//   - Server-initiated notifications during the post-respawn quiet
//     window — these are usually `tools/list_changed` etc. that the
//     new inner fires on startup; mid-session re-firing confuses
//     clients.
//
// Everything else passes through to the client verbatim. Returns when
// the inner closes its stdout — the caller's loop handles respawn.
func (s *Supervisor) forwardInnerStdoutWithProbeFilter(stdout io.Reader) {
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			switch s.classifyInnerLine(line) {
			case innerLineProbeResponse:
				s.ProbesAnswered.Add(1)
			case innerLineInitReplayResponse:
				// drop silently
			case innerLineQuietWindowNotification:
				// drop silently
			default:
				_, _ = s.Stdout.Write(line)
			}
		}
		if err != nil {
			return
		}
	}
}

// innerLineKind buckets each parsed inner→client line into a forward
// or drop decision. classifyInnerLine implements the bucketing.
type innerLineKind int

const (
	innerLineForward innerLineKind = iota
	innerLineProbeResponse
	innerLineInitReplayResponse
	innerLineQuietWindowNotification
)

// classifyInnerLine inspects the line's JSON-RPC shape (best-effort
// — malformed lines forward verbatim) and returns the bucket. Side
// effect: clears pendingProbe when a probe response is recognized.
func (s *Supervisor) classifyInnerLine(line []byte) innerLineKind {
	var head struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return innerLineForward
	}

	// Notification: no id, has method. Drop during quiet window;
	// otherwise forward.
	if len(head.ID) == 0 && head.Method != "" {
		if t := s.respawnQuietUntil.Load(); t != nil && time.Now().Before(*t) {
			return innerLineQuietWindowNotification
		}
		return innerLineForward
	}

	// Response or request: has id. Check sentinel prefixes.
	if len(head.ID) == 0 {
		return innerLineForward
	}
	var idStr string
	if err := json.Unmarshal(head.ID, &idStr); err != nil {
		// Numeric id — not one of our string sentinels. Forward.
		return innerLineForward
	}
	switch {
	case strings.HasPrefix(idStr, probeIDPrefix):
		s.pendingProbe.Store(nil)
		return innerLineProbeResponse
	case strings.HasPrefix(idStr, initIDPrefix):
		return innerLineInitReplayResponse
	default:
		return innerLineForward
	}
}

// probeLoop sends a periodic JSON-RPC `tools/list` to the inner with a
// supervisor-internal ID prefix. The matching response is intercepted
// in forwardInnerStdoutWithProbeFilter (so the client never sees it).
// If a probe is still pending after probeTimeout, kill the inner — the
// existing inner-exit path then triggers respawn.
func (s *Supervisor) probeLoop(ctx context.Context) {
	interval := s.ProbeInterval
	if interval == 0 {
		interval = defaultProbeInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sendProbe()
		}
	}
}

// sendProbe writes a sentinel-id JSON-RPC request to the inner. Records
// the probe state for the timeout watcher; the response (when it
// arrives) clears it.
//
// Don't probe if a probe is already pending — that would obscure the
// timeout signal (we'd have multiple in-flight probes and lose the
// "is THIS one stuck?" semantic). The previous probe's timeout will
// fire on its own.
func (s *Supervisor) sendProbe() {
	if s.pendingProbe.Load() != nil {
		// A probe is already in flight; skip this tick.
		return
	}

	id := fmt.Sprintf("%s%d", probeIDPrefix, s.probeIDCounter.Add(1))
	now := time.Now()
	state := &probeState{id: id, sentAt: now}
	s.pendingProbe.Store(state)

	// JSON-RPC `tools/list` is a no-op-ish request — pincher answers
	// with the registered tool set, which is cheap and reliable.
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/list","params":{}}`+"\n", id)
	if err := s.writeToInner([]byte(payload)); err != nil {
		// Inner pipe broken; the inner-side pump will respawn. Clear
		// the pending state so we don't trip the timeout incorrectly.
		s.pendingProbe.CompareAndSwap(state, nil)
		return
	}
	s.ProbesSent.Add(1)

	// Schedule the timeout-kill. The closure compares pendingProbe
	// against the captured state pointer, so a probe answered before
	// timeout (which clears pendingProbe) AND a later probe replacing
	// this one both correctly bypass the kill.
	timeout := s.ProbeTimeout
	if timeout == 0 {
		timeout = defaultProbeTimeout
	}
	time.AfterFunc(timeout, func() {
		if s.pendingProbe.CompareAndSwap(state, nil) {
			// We were the in-flight probe; nothing answered us in
			// the timeout window. Treat the inner as hung. Stage a
			// one-shot reason so the next respawn surfaces "probe
			// timeout" rather than "inner exited (code=X)" via the
			// status tool.
			s.ProbesTimedOut.Add(1)
			s.nextRestartReasonMu.Lock()
			s.nextRestartReason = "probe timeout (inner unresponsive)"
			s.nextRestartReasonMu.Unlock()
			slog.Warn("supervisor.probe_timeout",
				"id", id,
				"sent_at", now,
				"timeout", timeout,
				"action", "killing_inner")
			s.killInner()
		}
	})
}

// killInner sends SIGKILL (or Windows equivalent) to the current inner
// process. The pump's Read on stdout will then return EOF, and the
// existing respawn path takes over.
func (s *Supervisor) killInner() {
	s.mu.RLock()
	in := s.inner
	s.mu.RUnlock()
	if in == nil || in.cmd == nil || in.cmd.Process == nil {
		return
	}
	_ = in.cmd.Process.Kill()
}

// recordRestart appends now to the restart history, trims entries
// older than RestartWindow, and returns a circuit-breaker error if
// the count exceeds MaxRestarts.
//
// The history slice is small (≤ MaxRestarts+1 elements at any time),
// so allocating per-call is cheap.
func (s *Supervisor) recordRestart() error {
	max := s.MaxRestarts
	if max == 0 {
		max = defaultMaxRestarts
	}
	window := s.RestartWindow
	if window == 0 {
		window = defaultRestartWindow
	}

	now := time.Now()
	cutoff := now.Add(-window)

	s.restartHistoryMu.Lock()
	defer s.restartHistoryMu.Unlock()

	// Trim aged entries before appending so the slice doesn't grow
	// unbounded in long-running sessions with sparse restarts.
	keep := s.restartHistory[:0]
	for _, t := range s.restartHistory {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now)
	s.restartHistory = keep

	if len(keep) > max {
		return fmt.Errorf("supervisor: %d restarts within %s — circuit breaker tripped, refusing to respawn further", len(keep), window)
	}
	return nil
}

// maybeCaptureInit parses just enough of an inbound JSON-RPC line to
// detect an initialize request or notifications/initialized
// notification, and stashes the params (for initialize) or the whole
// line (for the notification). We re-capture each time so re-init
// from the client (rare, e.g. after roots change) updates the replay
// payload.
//
// S1.5 (#371): only initialize PARAMS are stored. The replay
// synthesizes a fresh JSON-RPC request with a supervisor-sentinel ID
// so the new inner's response can be intercepted server-side and
// never reach the client (which would otherwise see a duplicate
// initialize response and close stdio).
func (s *Supervisor) maybeCaptureInit(line []byte) {
	var head struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return
	}
	switch head.Method {
	case "initialize":
		s.mu.Lock()
		// Default to "{}" if params absent, so the synthesized
		// replay is still well-formed JSON-RPC.
		if len(head.Params) == 0 {
			s.initParams = []byte("{}")
		} else {
			s.initParams = append(s.initParams[:0], head.Params...)
		}
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

// respawn spawns a fresh inner, replays the captured init handshake
// with a supervisor-sentinel ID (so the response can be intercepted),
// and atomically swaps it in for the old. Holds the write lock for the
// duration so client→inner writes can't race with the swap.
//
// Sets respawnQuietUntil to now + RespawnQuietWindow so the
// inner→client pump drops server-initiated notifications during the
// window. Without this drop, the new inner's
// `notifications/tools/list_changed` (and similar startup
// notifications) reach the client mid-session and look like
// unexpected state changes. S1.5 / #371.
func (s *Supervisor) respawn() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.spawnFn()
	if err != nil {
		return err
	}

	// Set quiet window BEFORE writing init replay so any
	// notifications the new inner emits before processing initialize
	// are also covered.
	quietWindow := s.RespawnQuietWindow
	if quietWindow == 0 {
		quietWindow = defaultRespawnQuietWindow
	}
	until := time.Now().Add(quietWindow)
	s.respawnQuietUntil.Store(&until)

	// Replay handshake with a supervisor-sentinel ID. The matching
	// response from the new inner is intercepted in
	// forwardInnerStdoutWithProbeFilter and dropped — the client
	// already received an initialize response from the original
	// inner.
	initReplayed := false
	if len(s.initParams) > 0 {
		sentinelID := fmt.Sprintf("%s%d", initIDPrefix, s.initReplayCounter.Add(1))
		// JSON-RPC line format. params is already a json.RawMessage
		// shape (object, null, or array — but spec mandates object
		// for initialize).
		payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"initialize","params":%s}`+"\n",
			sentinelID, string(s.initParams))
		if _, err := p.stdin.Write([]byte(payload)); err != nil {
			if p.cmd != nil && p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			return fmt.Errorf("replay initialize: %w", err)
		}
		initReplayed = true
	}
	if len(s.initializedLine) > 0 {
		if _, err := p.stdin.Write(s.initializedLine); err != nil {
			if p.cmd != nil && p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			return fmt.Errorf("replay initialized: %w", err)
		}
	}

	s.inner = p
	slog.Info("supervisor.respawn",
		"init_replayed", initReplayed,
		"quiet_window", quietWindow)
	return nil
}

// Status returns a snapshot of the supervisor's runtime stats.
// Atomic-loaded counters mean this is safe to call concurrently with
// the pumps. Used both by the MCP status tool and by tests.
func (s *Supervisor) Status() SupervisorStatus {
	s.restartHistoryMu.Lock()
	reason := s.lastRestartReason
	s.restartHistoryMu.Unlock()

	uptime := int64(0)
	if !s.startedAt.IsZero() {
		uptime = int64(time.Since(s.startedAt).Seconds())
	}

	s.mu.RLock()
	alive := s.inner != nil
	s.mu.RUnlock()

	return SupervisorStatus{
		Alive:             alive,
		UptimeSec:         uptime,
		Restarts:          s.Restarts.Load(),
		ProbesSent:        s.ProbesSent.Load(),
		ProbesAnswered:    s.ProbesAnswered.Load(),
		ProbesTimedOut:    s.ProbesTimedOut.Load(),
		LastRestartReason: reason,
	}
}

// handleStatusToolCall intercepts a JSON-RPC tools/call for the
// supervisor's status tool, writes a synthesized response back to the
// client, and returns true to signal "do NOT forward to inner". Returns
// false for any line that isn't a status-tool call (parse error, wrong
// method, wrong tool name) — caller forwards as usual.
func (s *Supervisor) handleStatusToolCall(line []byte) bool {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return false
	}
	if msg.Method != "tools/call" {
		return false
	}
	if msg.Params.Name != SupervisorStatusToolName {
		return false
	}

	status := s.Status()
	statusJSON, _ := json.MarshalIndent(status, "", "  ")

	// MCP CallToolResult shape: {"content": [{"type":"text","text":"..."}]}
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      msg.ID,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(statusJSON)},
			},
		},
	}
	out, _ := json.Marshal(resp)
	out = append(out, '\n')

	if _, err := s.Stdout.Write(out); err != nil {
		slog.Warn("supervisor.status_write_err", "err", err.Error())
	}
	return true
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
