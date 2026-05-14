# pincherMCP — developer Makefile
#
# Targets:
#   make build                     Build the pincher binary
#   make test                      Run the full test suite
#   make corpus-test               Verify pinned-corpus snapshots match (read-only)
#   make corpus-snapshot-update    Regenerate pinned-corpus snapshots (writes)
#
# Snapshot policy (#33):
#   - Counts (symbols, edges, files, kinds, confidences) are exact-match.
#   - Noisy fields (db_size_kb, duration_ms) are stripped before comparison;
#     they need tolerance bands and live in a future CI integration.
#   - `negative_assertions` (when added) require explicit code-review approval
#     to change — `corpus-snapshot-update` MUST NOT auto-regenerate them.

GO ?= go
PINCHER_BIN ?= ./pincher
CORPORA := go-project k8s-ops node-monorepo docs-site terraform-stack
SNAPSHOT_DIR := testdata/corpus
SNAPSHOT_DATA := $(SNAPSHOT_DIR)/.snapshot-data

# Fields that are inherently noisy across runs / platforms — stripped from
# the canonical snapshot. Update when adding new noisy fields.
SNAPSHOT_NOISE_FIELDS := db_size_kb,duration_ms

# `jq` filter that removes noisy fields and produces a stable canonical form.
# Equivalent to: del(.db_size_kb, .duration_ms)
JQ_STRIP := 'del(.$(shell echo "$(SNAPSHOT_NOISE_FIELDS)" | sed "s/,/, ./g"))'

.PHONY: build test install corpus-test corpus-snapshot-update bench bench-index bench-server corpus-bench corpus-bench-update

# Version stamped at build time via -ldflags. `git describe` produces e.g.
# `v0.10.0` on a clean tag, `v0.10.0-3-gabcdef` ahead of one, `-dirty`
# suffix when the tree has uncommitted changes. The leading `v` is stripped
# for parity with the release workflow's `${GITHUB_REF_NAME#v}` format.
# Falls back to `dev` if git is unavailable (e.g., source tarball builds).
PINCHER_VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS         := -s -w -X main.version=$(PINCHER_VERSION)

build:
	$(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(PINCHER_BIN) ./cmd/pinch/

# install: build + swap the on-PATH binary in place. The rename-out trick
# in scripts/swap-active-binary.sh is required on Windows because the OS
# locks running .exe files (#705). The supervisor's auto-restart-on-drift
# picks up the swap on the next MCP tool call — the dogfood loop needs
# zero manual user intervention (no /mcp, no kill, no copy).
install: build
	@bash scripts/swap-active-binary.sh --source=$(PINCHER_BIN)

test:
	$(GO) test ./...

# corpus-test: verify each pinned corpus produces the committed snapshot.
# Fails on ANY diff. Used by CI to gate regressions in extraction behaviour.
corpus-test: build
	@set -e; \
	mkdir -p $(SNAPSHOT_DATA); \
	for corpus in $(CORPORA); do \
		echo "==> $$corpus"; \
		rm -rf $(SNAPSHOT_DATA)/$$corpus; \
		mkdir -p $(SNAPSHOT_DATA)/$$corpus; \
		$(PINCHER_BIN) index --data-dir $(SNAPSHOT_DATA)/$$corpus --json-summary $(SNAPSHOT_DIR)/$$corpus \
			| jq -S $(JQ_STRIP) > $(SNAPSHOT_DATA)/$$corpus.actual.json; \
		if ! diff -u $(SNAPSHOT_DIR)/$$corpus.snapshot.json $(SNAPSHOT_DATA)/$$corpus.actual.json; then \
			echo ""; \
			echo "Snapshot mismatch for $$corpus."; \
			echo "If this change is intentional, run: make corpus-snapshot-update"; \
			echo "and review the diff in your PR."; \
			exit 1; \
		fi; \
	done
	@echo ""
	@echo "All corpus snapshots match."

# corpus-snapshot-update: regenerate committed snapshots. ONLY run when an
# intentional change to extraction behaviour requires it; the resulting diff
# in your PR IS the rationale.
#
# IMPORTANT: never re-runs negative assertions when those land — they require
# explicit code review to change.
corpus-snapshot-update: build
	@set -e; \
	mkdir -p $(SNAPSHOT_DATA); \
	for corpus in $(CORPORA); do \
		echo "==> regenerating $$corpus"; \
		rm -rf $(SNAPSHOT_DATA)/$$corpus; \
		mkdir -p $(SNAPSHOT_DATA)/$$corpus; \
		$(PINCHER_BIN) index --data-dir $(SNAPSHOT_DATA)/$$corpus --json-summary $(SNAPSHOT_DIR)/$$corpus \
			| jq -S $(JQ_STRIP) > $(SNAPSHOT_DIR)/$$corpus.snapshot.json; \
	done
	@echo ""
	@echo "Snapshots regenerated. Review the git diff before committing."

# Benchmarks (#50). Run against the pinned corpora so latency numbers can
# be correlated to known-good symbol counts.
#
# Two flows:
#   make bench                  Quick local feedback (no comparison gate)
#   make corpus-bench           Run benchmarks AND fail on regression vs baseline
#   make corpus-bench-update    Regenerate the committed baseline (intentional)
#
# CORPUS_BENCHTIME is separate from BENCHTIME so the regression gate can run
# at a longer/more-stable benchtime than ad-hoc `make bench` invocations.
BENCHTIME ?= 1s
CORPUS_BENCHTIME ?= 2s
BENCH_DIR := testdata/bench

bench: bench-index bench-server

bench-index:
	@echo "==> internal/index"
	$(GO) test ./internal/index/ -run=^$$ -bench=. -benchtime=$(BENCHTIME) -benchmem

bench-server:
	@echo "==> internal/server"
	$(GO) test ./internal/server/ -run=^$$ -bench=. -benchtime=$(BENCHTIME) -benchmem

# corpus-bench: run benchmarks and gate on regression vs the committed
# baseline under testdata/bench/. Mirrors corpus-test's diff-or-fail shape.
#
# Thresholds (cmd/benchcmp/main.go) — overridable via env so CI can widen
# them to absorb dev-vs-CI hardware mismatch. The committed baselines are
# dev-machine numbers; GitHub-hosted runners differ from the dev machine
# by more than the local-dev noise floor on some benchmarks (Cold_K8sOps
# +21% on CI is a typical example, well within hardware-mismatch range).
# Defaults are tight (local-dev-friendly); CI passes wider values.
#
# Defaults:
#   - ns/op increase > 20%   → fail
#   - allocs/op increase > 30% → fail
#   - new benchmarks without baseline → fail (forces baseline update)
BENCH_NS_THRESHOLD     ?= 0.20
BENCH_ALLOCS_THRESHOLD ?= 0.30
# Comma-separated benchmarks to skip from regression flagging. Use for
# benchmarks with documented high CV that would flap the gate. Per
# testdata/bench/variance-ci-2026-05-09.md, only Index_Incremental_NoChange_GoProject
# (21.5% CV on CI, I/O-bound) qualifies; everyone else is <10% CV and
# safe to gate normally.
BENCH_EXCLUDE          ?=
corpus-bench:
	@set -e; \
	tmpdir=$$(mktemp -d); \
	echo "==> internal/index (baseline gate, ns=$(BENCH_NS_THRESHOLD) allocs=$(BENCH_ALLOCS_THRESHOLD) exclude=$(BENCH_EXCLUDE))"; \
	$(GO) test ./internal/index/ -run=^$$ -bench=. -benchtime=$(CORPUS_BENCHTIME) -benchmem > $$tmpdir/index.txt; \
	$(GO) run ./cmd/benchcmp -ns-threshold=$(BENCH_NS_THRESHOLD) -allocs-threshold=$(BENCH_ALLOCS_THRESHOLD) -exclude="$(BENCH_EXCLUDE)" $(BENCH_DIR)/index.bench.txt $$tmpdir/index.txt; \
	echo ""; \
	echo "==> internal/server (baseline gate, ns=$(BENCH_NS_THRESHOLD) allocs=$(BENCH_ALLOCS_THRESHOLD) exclude=$(BENCH_EXCLUDE))"; \
	$(GO) test ./internal/server/ -run=^$$ -bench=. -benchtime=$(CORPUS_BENCHTIME) -benchmem > $$tmpdir/server.txt; \
	$(GO) run ./cmd/benchcmp -ns-threshold=$(BENCH_NS_THRESHOLD) -allocs-threshold=$(BENCH_ALLOCS_THRESHOLD) -exclude="$(BENCH_EXCLUDE)" $(BENCH_DIR)/server.bench.txt $$tmpdir/server.txt; \
	rm -rf $$tmpdir; \
	echo ""; \
	echo "Bench regression gate passed."

# corpus-bench-update: regenerate committed baselines under testdata/bench/.
# ONLY run when an intentional perf-affecting change requires it; the diff
# in your PR IS the rationale.
corpus-bench-update:
	@set -e; \
	mkdir -p $(BENCH_DIR); \
	echo "==> regenerating internal/index baseline"; \
	$(GO) test ./internal/index/ -run=^$$ -bench=. -benchtime=$(CORPUS_BENCHTIME) -benchmem > $(BENCH_DIR)/index.bench.txt; \
	echo "==> regenerating internal/server baseline"; \
	$(GO) test ./internal/server/ -run=^$$ -bench=. -benchtime=$(CORPUS_BENCHTIME) -benchmem > $(BENCH_DIR)/server.bench.txt; \
	echo ""; \
	echo "Baselines regenerated. Review the git diff before committing."
