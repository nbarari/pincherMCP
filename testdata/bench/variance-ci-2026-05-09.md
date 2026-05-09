# Bench variance - 10 iterations at -benchtime=2s

> **Captured on CI hardware** via the manual `bench-variance.yml`
> workflow added in #166 (run 25601102287, 2026-05-09). Counterpart
> to `variance-2026-05-09.md` (dev-machine numbers). Both files
> together explain why the bench-regression CI gate is calibrated
> at 0.30 ns / 0.45 allocs against the CI-captured baselines (#158)
> but stays advisory pending a second multi-run confirmation —
> see #160/#162 for the failed promotion attempt.

Coefficient of variation (stddev/mean x 100) per benchmark.
Lower is better. Thresholds:

- **<10%** - safe to promote bench gate from advisory to required at +20% ns / +30% allocs.
- **10-20%** - workable but tight; widen thresholds to ~+30% / +45% before promoting.
- **>20%** - too noisy to gate; investigate test setup or drop from required gate.

| Benchmark | ns/op CV | allocs/op CV | ns/op mean | ns/op stddev | runs |
|-----------|---------:|-------------:|-----------:|-------------:|-----:|
| `BenchmarkIndex_Incremental_NoChange_GoProject` | 21.52% | - | 1,473,451 ns | 317,106 ns | 10 |
| `BenchmarkIndex_Incremental_NoChange_NodeMonorepo` | 4.54% | - | 1,391,357 ns | 63,146 ns | 10 |
| `BenchmarkIndex_Cold_NodeMonorepo` | 4.15% | 0.02% | 10,441,538 ns | 433,179 ns | 10 |
| `BenchmarkHandleSymbols_Batch20_GoProject` | 3.05% | - | 330,332 ns | 10,086 ns | 10 |
| `BenchmarkHandleSearch_BM25_K8sOps` | 2.69% | - | 1,620,280 ns | 43,563 ns | 10 |
| `BenchmarkIndex_Cold_GoProject` | 2.57% | 0.02% | 9,212,689 ns | 236,398 ns | 10 |
| `BenchmarkHandleSymbol_GoProject` | 2.46% | - | 299,930 ns | 7,377 ns | 10 |
| `BenchmarkHandleSearch_BM25_GoProject` | 2.14% | 0.01% | 862,357 ns | 18,477 ns | 10 |
| `BenchmarkHandleSearch_Parallel_GoProject` | 1.76% | 0.01% | 424,840 ns | 7,483 ns | 10 |
| `BenchmarkIndex_Incremental_NoChange_K8sOps` | 1.42% | - | 1,393,721 ns | 19,723 ns | 10 |
| `BenchmarkIndex_Cold_K8sOps` | 1.25% | 0.01% | 16,537,900 ns | 205,972 ns | 10 |
| `BenchmarkHandleQuery_SingleHopJoin_GoProject` | 1.12% | - | 381,506 ns | 4,274 ns | 10 |
| `BenchmarkIndex_Force_GoProject` | 1.01% | 0.01% | 7,471,582 ns | 75,527 ns | 10 |
| `BenchmarkAuth_TimingProfile/wrong_long_prefix_match` | 0.96% | - | 5,474 ns | 53 ns | 10 |
| `BenchmarkHandleQuery_NodeScan_GoProject` | 0.77% | - | 271,000 ns | 2,088 ns | 10 |
| `BenchmarkAuth_TimingProfile/correct` | 0.74% | - | 108,382 ns | 804 ns | 10 |
| `BenchmarkAuth_TimingProfile/wrong_last_byte` | 0.63% | - | 4,308 ns | 27 ns | 10 |
| `BenchmarkAuth_TimingProfile/wrong_first_byte` | 0.49% | - | 4,296 ns | 21 ns | 10 |
| `BenchmarkHandleArchitecture_GoProject` | 0.47% | - | 959,278 ns | 4,476 ns | 10 |
| `BenchmarkAuth_TimingProfile/wrong_short` | 0.41% | - | 4,300 ns | 18 ns | 10 |
| `BenchmarkAuth_TimingProfile/wrong_same_length` | 0.27% | - | 4,291 ns | 12 ns | 10 |

## Summary

- **Safe (<10% CV)**: 20 of 21 benchmarks
- **Tight (10-20% CV)**: 0 of 21 benchmarks
- **Noisy (>=20% CV)**: 1 of 21 benchmarks

### Noisy benchmarks (gate would flap)
- `BenchmarkIndex_Incremental_NoChange_GoProject` - ns/op CV 21.5%

