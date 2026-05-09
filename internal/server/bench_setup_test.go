package server

import (
	"flag"
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences pincher's slog output during benchmarks so the
// captured `go test -bench` text isn't interleaved with `INFO pincher.indexed`
// lines emitted by the indexer goroutine the server harness creates.
// See internal/index/bench_setup_test.go for the same pattern (#50).
func TestMain(m *testing.M) {
	flag.Parse()
	if flag.Lookup("test.bench").Value.String() != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	os.Exit(m.Run())
}
