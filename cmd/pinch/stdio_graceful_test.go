package main

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// #1568 v0.83 — gate the isGracefulStdioShutdown classifier. The
// pre-fix log.Fatalf treated every non-nil Run() return as fatal,
// including the clean stdin-EOF case the FILE-M host-conformance
// harness exercises. The classifier is the seam: if it returns true,
// main() falls out of the stdio loop normally; if false, log.Fatalf
// fires. Both directions matter — false negatives drop responses,
// false positives silence real errors.

func TestIsGracefulStdioShutdown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Graceful: nil (the no-error case).
		{"nil_is_graceful", nil, true},
		// Graceful: SDK formats EOF in the message text.
		{"sdk_server_is_closing_eof", fmt.Errorf("server is closing: EOF"), true},
		// Graceful: bare io.EOF (covered via message).
		{"bare_io_eof", io.EOF, true},
		// Graceful: wrapped io.EOF.
		{"wrapped_io_eof", fmt.Errorf("transport: %w", io.EOF), true},
		// Graceful: pipe closed on the other end during shutdown.
		{"closed_pipe", errors.New("io: read/write on closed pipe"), true},
		// Fatal: malformed JSON-RPC from the host.
		{"protocol_error_is_fatal", errors.New("invalid jsonrpc version"), false},
		// Fatal: connection reset.
		{"reset_is_fatal", errors.New("connection reset by peer"), false},
		// Fatal: write error before EOF.
		{"write_error_is_fatal", errors.New("write tcp: broken pipe"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isGracefulStdioShutdown(tc.err)
			if got != tc.want {
				t.Errorf("isGracefulStdioShutdown(%v) = %t; want %t", tc.err, got, tc.want)
			}
		})
	}
}
