package supervisor

import (
	"bytes"
	"context"
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
