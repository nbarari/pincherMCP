package supervisor

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// #407: After supervisor respawns the inner, push a
// `notifications/tools/list_changed` to the client so MCP clients with
// cached tool registries (Claude Code, Cursor, Codex) re-issue
// `tools/list` and pick up new tools live. Without this, a binary swap
// that ADDS tools is silently invisible until the user starts a fresh
// session — defeating the auto-restart-on-drift workflow for any
// release that adds tools.
func TestSupervisor_RespawnEmitsToolsListChangedNotification(t *testing.T) {
	clientStdinR, clientStdinW := io.Pipe()
	var clientStdout bytes.Buffer
	var stdoutMu sync.Mutex

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
		Stdout: &lockedWriter{w: &clientStdout, mu: &stdoutMu},
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

	// Drive the handshake so respawn has init params to replay.
	initLine := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	initdLine := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	clientStdinW.Write([]byte(initLine))
	clientStdinW.Write([]byte(initdLine))

	waitForReceived(t, getCurrent, []byte(initdLine), time.Second)

	// Trigger respawn by closing inner #1.
	fakes[0].Close()

	// Wait for inner #2 to be spawned.
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

	// Wait for the supervisor to write the notification to client stdout.
	want := []byte(`"method":"notifications/tools/list_changed"`)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stdoutMu.Lock()
		seen := bytes.Contains(clientStdout.Bytes(), want)
		stdoutMu.Unlock()
		if seen {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stdoutMu.Lock()
	got := clientStdout.Bytes()
	stdoutMu.Unlock()
	if !bytes.Contains(got, want) {
		t.Errorf("client stdout missing tools/list_changed notification after respawn; got: %q", got)
	}

	clientStdinW.Close()
	fakes[1].Close()
	cancel()
	<-runDone
}

// lockedWriter serialises writes to a shared bytes.Buffer so test goroutine
// reads can race-safely inspect contents.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
