package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeInner emulates a JSON-RPC stdio server for supervisor tests
// without spawning real pincher binaries. Lines arriving on its stdin
// are appended to Received; lines pre-loaded into Outbound are written
// to its stdout in order.
//
// closeStdoutAfterN: number of lines forwarded before the fake closes
// its stdout (simulating inner exit). 0 = never close.
type fakeInner struct {
	stdin  *io.PipeReader  // exposed to supervisor as inner.stdin's READ end (we read what supervisor writes)
	stdout *io.PipeWriter  // exposed to supervisor as inner.stdout's WRITE end (we write what supervisor reads)

	stdinW  *io.PipeWriter // we never write here; pipes are paired with the supervisor's view
	stdoutR *io.PipeReader

	mu         sync.Mutex
	received   [][]byte
	closedOnce sync.Once
	closed     chan struct{}

	// Bookkeeping
	id int
}

func newFakeInner(id int) *fakeInner {
	stdinR, stdinW := io.Pipe()   // supervisor writes to stdinW; fake reads from stdinR
	stdoutR, stdoutW := io.Pipe() // supervisor reads from stdoutR; fake writes to stdoutW
	f := &fakeInner{
		stdin:   stdinR,
		stdout:  stdoutW,
		stdinW:  stdinW,
		stdoutR: stdoutR,
		closed:  make(chan struct{}),
		id:      id,
	}
	// Read loop on stdinR — record everything the supervisor writes.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdinR.Read(buf)
			if n > 0 {
				f.mu.Lock()
				f.received = append(f.received, append([]byte(nil), buf[:n]...))
				f.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return f
}

// Receive returns all bytes the supervisor has written to this fake's
// stdin so far, joined into one slice for assertion convenience.
func (f *fakeInner) Receive() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []byte
	for _, chunk := range f.received {
		out = append(out, chunk...)
	}
	return out
}

// Send writes bytes to the fake's stdout — supervisor will forward
// them to its client.
func (f *fakeInner) Send(line string) error {
	_, err := f.stdout.Write([]byte(line))
	return err
}

// Close simulates the inner exiting: closes stdout (so supervisor's
// io.Copy returns EOF) and stdin (so any pending writes from the
// supervisor fail).
func (f *fakeInner) Close() {
	f.closedOnce.Do(func() {
		_ = f.stdout.Close()
		_ = f.stdinW.Close()
		close(f.closed)
	})
}

// fakeCmd is the bare-minimum *exec.Cmd-shaped object the supervisor
// reaps via cmd.Wait() and inspects via cmd.Process. We use a real
// (no-op) exec.Cmd to satisfy the type but never actually run it.
//
// The trick: use exec.Command with a no-op program ("true" on Unix,
// "cmd /c rem" on Windows isn't available without spawning). Simpler
// route: skip exec.Cmd entirely and use a custom innerProc shape for
// tests. But that means changing the production type.
//
// Pragmatic alternative for S1's tests: bypass the cmd field by
// constructing innerProc directly with cmd=nil and skipping the
// Wait()/ProcessState code paths (the supervisor handles cmd==nil
// gracefully — see pumpInnerToClient). For now, let's keep the
// supervisor robust to nil-cmd in tests and document.

// makeProc wires the fake inner into an innerProc the supervisor can
// adopt. Because we don't have a real *exec.Cmd, we set cmd=nil and
// rely on the supervisor's code path tolerating that — see comments
// inline in supervisor.go's pumpInnerToClient.
func (f *fakeInner) makeProc() *innerProc {
	return &innerProc{
		cmd:    nil, // supervisor must tolerate nil for tests
		stdin:  &writerCloser{w: f.stdinW},
		stdout: f.stdoutR,
	}
}

type writerCloser struct{ w *io.PipeWriter }

func (wc *writerCloser) Write(p []byte) (int, error) { return wc.w.Write(p) }
func (wc *writerCloser) Close() error                 { return wc.w.Close() }

// TestSupervisor_ForwardsClientToInner: a single line from the client
// arrives at the inner's stdin verbatim.
func TestSupervisor_ForwardsClientToInner(t *testing.T) {
	clientStdinR, clientStdinW := io.Pipe()
	var clientStdout bytes.Buffer

	fake := newFakeInner(1)

	sup := &Supervisor{
		Stdin:   clientStdinR,
		Stdout:  &clientStdout,
		Stderr:  io.Discard,
		spawnFn: func() (*innerProc, error) { return fake.makeProc(), nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	// Client sends a line.
	line := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}` + "\n"
	if _, err := clientStdinW.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}

	// Wait briefly for the line to traverse.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(fake.Receive(), []byte(line)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := fake.Receive(); !bytes.Contains(got, []byte(line)) {
		t.Errorf("inner did not receive line; got: %q", got)
	}

	// Client closes stdin → supervisor returns.
	clientStdinW.Close()
	fake.Close() // also close fake so innerToClient returns
	cancel()
	<-runDone
}

// TestSupervisor_CapturesAndReplaysInit: an initialize message
// captured during normal operation is replayed to the new inner after
// respawn.
func TestSupervisor_CapturesAndReplaysInit(t *testing.T) {
	clientStdinR, clientStdinW := io.Pipe()
	var clientStdout bytes.Buffer

	var fakeMu sync.Mutex
	var fakes []*fakeInner
	getCurrent := func() *fakeInner {
		fakeMu.Lock()
		defer fakeMu.Unlock()
		if len(fakes) == 0 {
			return nil
		}
		return fakes[len(fakes)-1]
	}

	sup := &Supervisor{
		Stdin:  clientStdinR,
		Stdout: &clientStdout,
		Stderr: io.Discard,
		spawnFn: func() (*innerProc, error) {
			fakeMu.Lock()
			id := len(fakes) + 1
			f := newFakeInner(id)
			fakes = append(fakes, f)
			fakeMu.Unlock()
			return f.makeProc(), nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	// 1. Send initialize then notifications/initialized then a tools/call.
	initLine := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	initdLine := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	callLine := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x"}}` + "\n"
	clientStdinW.Write([]byte(initLine))
	clientStdinW.Write([]byte(initdLine))
	clientStdinW.Write([]byte(callLine))

	// Wait for first inner to receive all three lines.
	waitForReceived(t, getCurrent, []byte(callLine), time.Second)

	// 2. Close first inner stdout → triggers respawn.
	fakes[0].Close()

	// 3. Wait for second inner to exist + receive replayed init.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fakeMu.Lock()
		n := len(fakes)
		fakeMu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(fakes); got < 2 {
		t.Fatalf("expected 2 inners after respawn, got %d", got)
	}

	// 4. Second inner should have received the init + initialized
	// replay, but NOT the tools/call (that was sent before respawn).
	waitForReceived(t, func() *fakeInner { return fakes[1] }, []byte(initLine), time.Second)
	got := fakes[1].Receive()
	if !bytes.Contains(got, []byte(initLine)) {
		t.Errorf("inner #2 missing initialize replay; got: %q", got)
	}
	if !bytes.Contains(got, []byte(initdLine)) {
		t.Errorf("inner #2 missing initialized replay; got: %q", got)
	}
	if bytes.Contains(got, []byte(callLine)) {
		t.Error("inner #2 received the tools/call from before respawn — unexpected (would imply double-replay)")
	}

	if r := sup.Restarts.Load(); r != 1 {
		t.Errorf("Restarts = %d, want 1", r)
	}

	clientStdinW.Close()
	fakes[1].Close()
	cancel()
	<-runDone
}

// TestSupervisor_ClientStdinEOFReturns: when the client closes stdin
// (clean disconnect), Run returns nil promptly without leaving the
// inner running.
func TestSupervisor_ClientStdinEOFReturns(t *testing.T) {
	clientStdinR, clientStdinW := io.Pipe()
	var clientStdout bytes.Buffer

	fake := newFakeInner(1)

	sup := &Supervisor{
		Stdin:   clientStdinR,
		Stdout:  &clientStdout,
		Stderr:  io.Discard,
		spawnFn: func() (*innerProc, error) { return fake.makeProc(), nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	// Immediately close client stdin.
	clientStdinW.Close()
	fake.Close()

	select {
	case err := <-runDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run() returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after client stdin EOF")
	}
}

// TestSupervisor_SpawnFailureReturnsError: when the initial spawn
// fails, Run returns the underlying error wrapped.
func TestSupervisor_SpawnFailureReturnsError(t *testing.T) {
	sup := &Supervisor{
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
		spawnFn: func() (*innerProc, error) {
			return nil, errors.New("synthetic spawn failure")
		},
	}

	err := sup.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from initial spawn failure")
	}
	if !strings.Contains(err.Error(), "synthetic spawn failure") {
		t.Errorf("error didn't wrap underlying cause: %v", err)
	}
}

// TestNormalizeInitMessageNotMatched: lines that aren't initialize or
// initialized are ignored by the capture path. (Indirect: send a
// different method, respawn, assert the new inner doesn't receive it
// as a replay.)
func TestSupervisor_NonInitLinesNotReplayed(t *testing.T) {
	clientStdinR, clientStdinW := io.Pipe()
	var clientStdout bytes.Buffer

	var fakes []*fakeInner
	var fakeMu sync.Mutex
	sup := &Supervisor{
		Stdin:  clientStdinR,
		Stdout: &clientStdout,
		Stderr: io.Discard,
		spawnFn: func() (*innerProc, error) {
			fakeMu.Lock()
			id := len(fakes) + 1
			f := newFakeInner(id)
			fakes = append(fakes, f)
			fakeMu.Unlock()
			return f.makeProc(), nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	// Send a non-init line.
	otherLine := `{"jsonrpc":"2.0","id":99,"method":"tools/list","params":{}}` + "\n"
	clientStdinW.Write([]byte(otherLine))

	waitForReceived(t, func() *fakeInner { return fakes[0] }, []byte(otherLine), time.Second)

	// Close inner #1 to trigger respawn.
	fakes[0].Close()

	// Wait for inner #2.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fakeMu.Lock()
		n := len(fakes)
		fakeMu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Inner #2 should NOT have the otherLine replayed.
	time.Sleep(100 * time.Millisecond) // give a moment for any erroneous replay
	got := fakes[1].Receive()
	if bytes.Contains(got, []byte(otherLine)) {
		t.Errorf("inner #2 received non-init line as replay; got: %q", got)
	}

	clientStdinW.Close()
	fakes[1].Close()
	cancel()
	<-runDone
}

// TestSupervisor_ProbeIsSentAndAnswered: with an aggressively short
// probe interval, the supervisor sends a probe to the inner; if the
// fake inner immediately replies with the matching ID, the response
// is intercepted (NOT forwarded to client) and ProbesAnswered ticks.
func TestSupervisor_ProbeIsSentAndAnswered(t *testing.T) {
	clientStdinR, clientStdinW := io.Pipe()
	var clientStdout bytes.Buffer

	fake := newFakeInner(1)

	sup := &Supervisor{
		Stdin:         clientStdinR,
		Stdout:        &clientStdout,
		Stderr:        io.Discard,
		ProbeInterval: 50 * time.Millisecond,
		ProbeTimeout:  500 * time.Millisecond,
		spawnFn:       func() (*innerProc, error) { return fake.makeProc(), nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	// Reply-bot: anytime the fake's stdin sees a probe-prefixed id,
	// echo a JSON-RPC response with that id back via Send().
	go func() {
		seen := []byte{}
		for {
			latest := fake.Receive()
			if !bytes.Equal(latest, seen) {
				delta := latest[len(seen):]
				lines := bytes.Split(delta, []byte("\n"))
				for _, line := range lines {
					if !bytes.Contains(line, []byte(probeIDPrefix)) {
						continue
					}
					var head struct {
						ID string `json:"id"`
					}
					if err := json.Unmarshal(line, &head); err != nil {
						continue
					}
					_ = fake.Send(`{"jsonrpc":"2.0","id":"` + head.ID + `","result":{"tools":[]}}` + "\n")
				}
				seen = latest
			}
			time.Sleep(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	// Wait for at least one probe-answer cycle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sup.ProbesAnswered.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := sup.ProbesAnswered.Load(); got < 1 {
		t.Errorf("ProbesAnswered = %d, want >= 1 after 2s with 50ms probe interval", got)
	}

	// Probe response must NOT have leaked to clientStdout.
	if bytes.Contains(clientStdout.Bytes(), []byte(probeIDPrefix)) {
		t.Errorf("probe response leaked to client stdout: %q", clientStdout.String())
	}

	clientStdinW.Close()
	fake.Close()
	cancel()
	<-runDone
}

// TestSupervisor_ProbeTimeoutKillsInner: when the fake inner doesn't
// answer probes, the timeout fires, kills the inner (via the existing
// killInner path), and ProbesTimedOut ticks. With cmd=nil in tests
// the kill is a no-op but the counter still increments.
func TestSupervisor_ProbeTimeoutKillsInner(t *testing.T) {
	clientStdinR, clientStdinW := io.Pipe()
	var clientStdout bytes.Buffer

	fake := newFakeInner(1)

	sup := &Supervisor{
		Stdin:         clientStdinR,
		Stdout:        &clientStdout,
		Stderr:        io.Discard,
		ProbeInterval: 100 * time.Millisecond,
		ProbeTimeout:  50 * time.Millisecond, // short to keep test snappy
		spawnFn:       func() (*innerProc, error) { return fake.makeProc(), nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	// Wait for the timeout to fire at least once.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if sup.ProbesTimedOut.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := sup.ProbesTimedOut.Load(); got < 1 {
		t.Errorf("ProbesTimedOut = %d, want >= 1", got)
	}

	clientStdinW.Close()
	fake.Close()
	cancel()
	<-runDone
}

// TestSupervisor_CircuitBreakerTrips: when respawns happen faster than
// MaxRestarts within RestartWindow, Run returns the breaker error.
func TestSupervisor_CircuitBreakerTrips(t *testing.T) {
	clientStdinR, _ := io.Pipe()
	var clientStdout bytes.Buffer

	var (
		spawnCount int
		fakes      []*fakeInner
		spawnMu    sync.Mutex
	)

	sup := &Supervisor{
		Stdin:  clientStdinR,
		Stdout: &clientStdout,
		Stderr: io.Discard,
		// Disable probes so they don't interfere with the breaker test.
		ProbeInterval: 24 * time.Hour,
		MaxRestarts:   2,
		RestartWindow: 10 * time.Second,
		spawnFn: func() (*innerProc, error) {
			spawnMu.Lock()
			defer spawnMu.Unlock()
			spawnCount++
			f := newFakeInner(spawnCount)
			fakes = append(fakes, f)
			// Each spawn returns a fake whose stdout we'll close
			// immediately to force respawn.
			go func(f *fakeInner) {
				time.Sleep(20 * time.Millisecond)
				f.Close()
			}(f)
			return f.makeProc(), nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	// Wait for Run to error out via breaker.
	select {
	case err := <-runDone:
		if err == nil {
			t.Fatal("expected breaker error, got nil")
		}
		if !strings.Contains(err.Error(), "circuit breaker") {
			t.Errorf("error doesn't mention breaker: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return within 3s — breaker may not have tripped")
	}
}

// TestSupervisor_StatusToolReturnsResponse: a tools/call for the
// supervisor's status tool is intercepted (NOT forwarded to inner)
// and a synthesized JSON-RPC response lands at the client.
func TestSupervisor_StatusToolReturnsResponse(t *testing.T) {
	clientStdinR, clientStdinW := io.Pipe()
	var clientStdout bytes.Buffer

	fake := newFakeInner(1)

	sup := &Supervisor{
		Stdin:         clientStdinR,
		Stdout:        &clientStdout,
		Stderr:        io.Discard,
		ProbeInterval: 24 * time.Hour, // disable probes for clarity
		spawnFn:       func() (*innerProc, error) { return fake.makeProc(), nil },
	}
	sup.Restarts.Store(2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	// Send a status tool call.
	call := `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"` + SupervisorStatusToolName + `","arguments":{}}}` + "\n"
	clientStdinW.Write([]byte(call))

	// Wait for response on client stdout.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(clientStdout.Bytes(), []byte(`"id":42`)) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Contains(clientStdout.Bytes(), []byte(`"id":42`)) {
		t.Fatalf("status tool response not seen on client stdout; got: %q", clientStdout.String())
	}

	// Inner should NOT have received the call.
	if bytes.Contains(fake.Receive(), []byte(SupervisorStatusToolName)) {
		t.Errorf("status tool call leaked to inner: %q", fake.Receive())
	}

	// Response payload should contain the SupervisorStatus fields.
	out := clientStdout.String()
	for _, want := range []string{"alive", "uptime_sec", "restarts", "probes_sent"} {
		if !strings.Contains(out, want) {
			t.Errorf("status response missing %q field: %q", want, out)
		}
	}

	clientStdinW.Close()
	fake.Close()
	cancel()
	<-runDone
}

// TestSupervisor_NonStatusToolPassesThrough: a tools/call for a
// regular tool name (search, etc.) is NOT intercepted — it forwards
// to the inner as normal.
func TestSupervisor_NonStatusToolPassesThrough(t *testing.T) {
	clientStdinR, clientStdinW := io.Pipe()
	var clientStdout bytes.Buffer

	fake := newFakeInner(1)

	sup := &Supervisor{
		Stdin:         clientStdinR,
		Stdout:        &clientStdout,
		Stderr:        io.Discard,
		ProbeInterval: 24 * time.Hour,
		spawnFn:       func() (*innerProc, error) { return fake.makeProc(), nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()

	call := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"search","arguments":{"query":"x"}}}` + "\n"
	clientStdinW.Write([]byte(call))

	waitForReceived(t, func() *fakeInner { return fake }, []byte(`"name":"search"`), time.Second)

	clientStdinW.Close()
	fake.Close()
	cancel()
	<-runDone
}

// TestSupervisor_StatusReflectsRestartReason: probe-timeout-triggered
// restarts surface as "probe timeout (inner unresponsive)" in the
// status payload, distinguishing from natural inner exits.
func TestSupervisor_StatusReflectsRestartReason(t *testing.T) {
	sup := &Supervisor{}
	sup.startedAt = time.Now()

	// No restart yet.
	if got := sup.Status().LastRestartReason; got != "" {
		t.Errorf("initial LastRestartReason = %q, want empty", got)
	}

	// Simulate a probe-timeout pre-stage + pump consume cycle.
	sup.nextRestartReason = "probe timeout (inner unresponsive)"
	override := sup.nextRestartReason
	sup.nextRestartReason = ""
	sup.lastRestartReason = override

	if got := sup.Status().LastRestartReason; got != "probe timeout (inner unresponsive)" {
		t.Errorf("LastRestartReason = %q, want probe timeout reason", got)
	}
}

// TestRecordRestart_TrimsOldEntries: entries older than RestartWindow
// don't count toward the breaker threshold.
func TestRecordRestart_TrimsOldEntries(t *testing.T) {
	sup := &Supervisor{
		MaxRestarts:   2,
		RestartWindow: 50 * time.Millisecond,
	}

	if err := sup.recordRestart(); err != nil {
		t.Fatalf("first restart should not trip: %v", err)
	}
	if err := sup.recordRestart(); err != nil {
		t.Fatalf("second restart should not trip: %v", err)
	}
	// Wait past window so the first two age out.
	time.Sleep(100 * time.Millisecond)

	if err := sup.recordRestart(); err != nil {
		t.Fatalf("third restart after window should not trip (first two aged out): %v", err)
	}
}

// waitForReceived polls fake.Receive() until needle appears or
// timeout. Helper for the async pump tests.
func waitForReceived(t *testing.T, getter func() *fakeInner, needle []byte, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f := getter()
		if f != nil && bytes.Contains(f.Receive(), needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f := getter()
	got := []byte("<nil fake>")
	if f != nil {
		got = f.Receive()
	}
	t.Fatalf("inner never received %q within %s; got: %q", needle, timeout, got)
}
