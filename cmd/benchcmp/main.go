// benchcmp compares a fresh `go test -bench` run against a committed
// baseline and exits non-zero on regression. Wired into CI via the
// `corpus-bench` Makefile target (#50).
//
// Regression policy (mirrors the issue's acceptance criteria):
//   - ns/op increase > 20% → fail (wall-clock noise floor on shared CI)
//   - allocs/op increase > 30% → fail (alloc count is more stable than time)
//   - benchmark in baseline missing from actual → fail (something disabled it)
//   - benchmark in actual missing from baseline → fail (forces baseline update)
//
// Usage:
//
//	go run ./cmd/benchcmp <baseline.txt> <actual.txt>
//
// The two files are the raw stdout of `go test -bench=. -benchmem` for the
// same package. Headers (goos/goarch/pkg/cpu) are advisory; only the
// `BenchmarkX-N    <iters>    <ns/op> ns/op   <B/op> B/op   <allocs/op> allocs/op`
// rows are compared. The trailing `-N` (GOMAXPROCS) is stripped before
// matching so cross-runner-CPU baselines still align.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	nsRegressionThreshold     = 0.20
	allocsRegressionThreshold = 0.30
)

type benchResult struct {
	NsPerOp     float64
	AllocsPerOp float64
	Raw         string
}

// benchLineRE matches a single Go benchmark output line.
//   BenchmarkName-N    <iters>    <ns/op> ns/op   <B/op> B/op   <allocs/op> allocs/op
//
// Capture groups: 1=name (with -N suffix), 2=ns/op, 3=allocs/op.
// We deliberately don't capture B/op — alloc count is the primary memory
// gate (more deterministic) and ns/op covers wall-clock.
var benchLineRE = regexp.MustCompile(
	`^(Benchmark\S+)\s+\d+\s+([0-9.]+)\s+ns/op\s+\d+\s+B/op\s+(\d+)\s+allocs/op`)

func parseFile(path string) (map[string]benchResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]benchResult)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		m := benchLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Strip the -N (GOMAXPROCS) suffix so a baseline captured on
		// a 4-core runner aligns with an actual captured on an 8-core
		// runner. The number itself doesn't change the per-op cost.
		name := stripGomaxprocs(m[1])
		ns, _ := strconv.ParseFloat(m[2], 64)
		allocs, _ := strconv.ParseFloat(m[3], 64)
		out[name] = benchResult{
			NsPerOp:     ns,
			AllocsPerOp: allocs,
			Raw:         line,
		}
	}
	return out, scanner.Err()
}

// stripGomaxprocs removes the trailing "-N" that Go test appends to each
// benchmark name (where N is GOMAXPROCS).
func stripGomaxprocs(name string) string {
	idx := strings.LastIndex(name, "-")
	if idx < 0 {
		return name
	}
	suffix := name[idx+1:]
	if _, err := strconv.Atoi(suffix); err != nil {
		return name
	}
	return name[:idx]
}

func percentDelta(baseline, actual float64) float64 {
	if baseline == 0 {
		return 0
	}
	return (actual - baseline) / baseline
}

// compare runs the diff/gate logic against two pre-parsed result sets.
// Returns the count of (regressions, missing-in-actual, new-without-baseline)
// so callers can decide their own exit code. Output is written to w in the
// same human-readable table format the CLI emits.
//
// Extracted from main() so the orchestration is testable directly without
// shelling out — covers the iteration, regression-flag formatting, and
// missing-benchmark paths that flag.Parse + os.Exit hide.
func compare(baseline, actual map[string]benchResult, w io.Writer) (regressions, missingActual, missingBaseline int) {
	fmt.Fprintf(w, "%-60s  %12s  %12s  %8s  %8s\n",
		"benchmark", "baseline ns", "actual ns", "Δns", "Δallocs")
	fmt.Fprintln(w, strings.Repeat("-", 110))

	for name, base := range baseline {
		act, ok := actual[name]
		if !ok {
			fmt.Fprintf(w, "MISSING IN ACTUAL: %s\n", name)
			missingActual++
			continue
		}
		nsDelta := percentDelta(base.NsPerOp, act.NsPerOp)
		allocsDelta := percentDelta(base.AllocsPerOp, act.AllocsPerOp)

		flagStr := ""
		if nsDelta > nsRegressionThreshold {
			flagStr += " [NS-REGRESSION]"
			regressions++
		}
		if allocsDelta > allocsRegressionThreshold {
			flagStr += " [ALLOCS-REGRESSION]"
			regressions++
		}
		fmt.Fprintf(w, "%-60s  %12.0f  %12.0f  %+7.1f%%  %+7.1f%%%s\n",
			name, base.NsPerOp, act.NsPerOp,
			nsDelta*100, allocsDelta*100, flagStr)
	}

	for name := range actual {
		if _, ok := baseline[name]; !ok {
			fmt.Fprintf(w, "NEW IN ACTUAL (no baseline): %s\n", name)
			missingBaseline++
		}
	}
	return regressions, missingActual, missingBaseline
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: benchcmp <baseline.txt> <actual.txt>")
		fmt.Fprintln(os.Stderr, "Compares two `go test -bench` outputs and exits 1 on regression.")
		fmt.Fprintln(os.Stderr, "Thresholds: ns/op +20%, allocs/op +30%, missing benchmarks fail.")
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}

	baseline, err := parseFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchcmp: read baseline: %v\n", err)
		os.Exit(2)
	}
	actual, err := parseFile(flag.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchcmp: read actual: %v\n", err)
		os.Exit(2)
	}

	if len(baseline) == 0 {
		fmt.Fprintln(os.Stderr, "benchcmp: baseline has zero benchmark rows — "+
			"is the file in `go test -bench=. -benchmem` format?")
		os.Exit(2)
	}

	regressions, missingActual, missingBaseline := compare(baseline, actual, os.Stdout)

	fmt.Println()
	if regressions == 0 && missingActual == 0 && missingBaseline == 0 {
		fmt.Printf("benchcmp: OK — %d benchmarks, no regressions.\n", len(baseline))
		return
	}

	fmt.Printf("benchcmp: FAIL — %d regression(s), %d missing in actual, %d new without baseline\n",
		regressions, missingActual, missingBaseline)
	if missingBaseline > 0 {
		fmt.Println("  → run `make corpus-bench-update` to add baselines for new benchmarks.")
	}
	os.Exit(1)
}
