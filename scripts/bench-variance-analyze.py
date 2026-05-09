#!/usr/bin/env python3
"""ASCII-only output. Run with `python scripts/... > out.md`; on Windows,
prefix `chcp 65001 &&` or use --out flag to bypass the codepage.

Parse the per-iteration bench output captured by bench-variance.sh and
emit a summary table of coefficient of variation (stddev / mean) per
benchmark per metric (ns/op, allocs/op).

The CV thresholds gate whether we can promote the bench-regression CI gate
from advisory to required:
  - <10%  → safe to gate at the current +20% / +30% thresholds.
  - 10-20% → workable but headroom-tight; widen thresholds before required.
  - >20%  → too noisy; either fix the test setup or drop from gate.

Usage:
  python3 scripts/bench-variance-analyze.py
"""

import re
import statistics
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "scripts" / ".bench-variance"

# Match: BenchmarkName-N    iters    ns/op ns/op    B/op B/op    allocs/op allocs/op
BENCH_LINE = re.compile(
    r"^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+([\d.]+)\s+ns/op(?:\s+([\d.]+)\s+B/op)?(?:\s+([\d.]+)\s+allocs/op)?"
)


def parse_run(path: Path):
    """Return {bench_name: {"ns": float, "allocs": float|None}}."""
    out = {}
    for line in path.read_text(errors="replace").splitlines():
        m = BENCH_LINE.match(line.strip())
        if not m:
            continue
        name = m.group(1)
        ns = float(m.group(2))
        allocs = float(m.group(4)) if m.group(4) else None
        # Some benchmark names include sub-test labels with slashes - keep as-is
        out[name] = {"ns": ns, "allocs": allocs}
    return out


def cv(values):
    """Coefficient of variation (stddev/mean), returned as a percentage."""
    if not values or len(values) < 2:
        return None
    mean = statistics.mean(values)
    if mean == 0:
        return None
    return statistics.stdev(values) / mean * 100.0


def main():
    runs = sorted(DATA.glob("run-*.txt"))
    if not runs:
        print(f"no runs found at {DATA}")
        return 1

    # Aggregate: {bench: {"ns": [v1...vN], "allocs": [v1...vN]}}
    agg = {}
    for run in runs:
        parsed = parse_run(run)
        for name, vals in parsed.items():
            agg.setdefault(name, {"ns": [], "allocs": []})
            agg[name]["ns"].append(vals["ns"])
            if vals["allocs"] is not None:
                agg[name]["allocs"].append(vals["allocs"])

    # Sort by ns CV desc so the noisy ones show up first.
    rows = []
    for name, vals in agg.items():
        ns_cv = cv(vals["ns"])
        allocs_cv = cv(vals["allocs"]) if vals["allocs"] else None
        rows.append((name, ns_cv or 0, allocs_cv, vals["ns"], vals["allocs"]))
    rows.sort(key=lambda r: r[1], reverse=True)

    # Print markdown-shaped table.
    print(f"# Bench variance - {len(runs)} iterations at -benchtime=2s")
    print()
    print("Coefficient of variation (stddev/mean x 100) per benchmark.")
    print("Lower is better. Thresholds:")
    print()
    print("- **<10%** - safe to promote bench gate from advisory to required at +20% ns / +30% allocs.")
    print("- **10-20%** - workable but tight; widen thresholds to ~+30% / +45% before promoting.")
    print("- **>20%** - too noisy to gate; investigate test setup or drop from required gate.")
    print()
    print("| Benchmark | ns/op CV | allocs/op CV | ns/op mean | ns/op stddev | runs |")
    print("|-----------|---------:|-------------:|-----------:|-------------:|-----:|")
    for name, ns_cv, allocs_cv, ns_vals, allocs_vals in rows:
        ns_mean = statistics.mean(ns_vals) if ns_vals else 0
        ns_std = statistics.stdev(ns_vals) if len(ns_vals) > 1 else 0
        ns_cv_str = f"{ns_cv:.2f}%" if ns_cv else "-"
        allocs_cv_str = f"{allocs_cv:.2f}%" if allocs_cv else "-"
        print(f"| `{name}` | {ns_cv_str} | {allocs_cv_str} | {ns_mean:,.0f} ns | {ns_std:,.0f} ns | {len(ns_vals)} |")

    # Group summary
    safe = [r for r in rows if r[1] < 10]
    tight = [r for r in rows if 10 <= r[1] < 20]
    noisy = [r for r in rows if r[1] >= 20]

    print()
    print("## Summary")
    print()
    print(f"- **Safe (<10% CV)**: {len(safe)} of {len(rows)} benchmarks")
    print(f"- **Tight (10-20% CV)**: {len(tight)} of {len(rows)} benchmarks")
    print(f"- **Noisy (>=20% CV)**: {len(noisy)} of {len(rows)} benchmarks")
    print()

    if noisy:
        print("### Noisy benchmarks (gate would flap)")
        for name, ns_cv, _, _, _ in noisy:
            print(f"- `{name}` - ns/op CV {ns_cv:.1f}%")
        print()

    if tight:
        print("### Tight benchmarks (need wider thresholds)")
        for name, ns_cv, _, _, _ in tight:
            print(f"- `{name}` - ns/op CV {ns_cv:.1f}%")
        print()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
