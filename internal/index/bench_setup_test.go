package index

import (
	"flag"
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences pincher's slog output during benchmarks so the
// captured `go test -bench` text isn't interleaved with `INFO pincher.indexed`
// lines. Without this, baseline `.bench.txt` files become unparseable —
// every benchmark line gets a log entry tail-spliced onto it (#50).
//
// We only suppress when -bench is set so unit tests retain their normal
// log surface for debugging.
func TestMain(m *testing.M) {
	flag.Parse()
	if flag.Lookup("test.bench").Value.String() != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	os.Exit(m.Run())
}
