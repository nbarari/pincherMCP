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
CORPORA := go-project k8s-ops node-monorepo
SNAPSHOT_DIR := testdata/corpus
SNAPSHOT_DATA := $(SNAPSHOT_DIR)/.snapshot-data

# Fields that are inherently noisy across runs / platforms — stripped from
# the canonical snapshot. Update when adding new noisy fields.
SNAPSHOT_NOISE_FIELDS := db_size_kb,duration_ms

# `jq` filter that removes noisy fields and produces a stable canonical form.
# Equivalent to: del(.db_size_kb, .duration_ms)
JQ_STRIP := 'del(.$(shell echo "$(SNAPSHOT_NOISE_FIELDS)" | sed "s/,/, ./g"))'

.PHONY: build test corpus-test corpus-snapshot-update bench bench-index bench-server

build:
	$(GO) build -o $(PINCHER_BIN) ./cmd/pinch/

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
# be correlated to known-good symbol counts. These do NOT yet have CI gating
# or committed baselines — that's a follow-up PR. For now they're a
# developer-side substrate.
#
# Quick-run (1s per benchmark, fast feedback):
#   make bench
# Full-run (3s per benchmark, more stable numbers):
#   make bench BENCHTIME=3s
BENCHTIME ?= 1s

bench: bench-index bench-server

bench-index:
	@echo "==> internal/index"
	$(GO) test ./internal/index/ -run=^$$ -bench=. -benchtime=$(BENCHTIME) -benchmem

bench-server:
	@echo "==> internal/server"
	$(GO) test ./internal/server/ -run=^$$ -bench=. -benchtime=$(BENCHTIME) -benchmem
