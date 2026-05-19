package index

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// #1572 v0.87: memory-pressure-aware watcher backoff. On a host
// under memory pressure the file-watcher's 5s tick keeps poking the
// indexer on the same cadence; a large project on a small machine
// can OOM with no signal beyond "the process got killed". The
// watcher now reads available system memory before each tick and
// skips the tick (with a structured log line) when it's below a
// configurable floor.
//
// This first slice (#1572) implements the Linux reader only —
// `/proc/meminfo` is pure-file, no syscalls, no cgo. macOS
// (`vm_stat` / `sysctl`) and Windows (`GlobalMemoryStatusEx`) are
// follow-up slices. On every non-Linux platform availableMemoryMiB
// returns ok=false, so the watcher keeps its existing behaviour
// (never backs off) — a safe no-op rather than a wrong guess.

// defaultWatcherMemoryBackoffMiB is the floor below which the
// watcher skips a tick. 512 MiB available is conservative: enough
// headroom that a mid-size project's extraction pass won't OOM,
// low enough that the backoff doesn't fire on a healthy host.
const defaultWatcherMemoryBackoffMiB = 512

// watcherMemoryBackoffMiB resolves the backoff floor, honouring the
// PINCHER_WATCHER_MEMORY_BACKOFF_MIB env override. A non-positive or
// unparseable value falls back to the default. Setting it to 0
// effectively disables the backoff (availableMemoryMiB will always
// be >= 0 >= 0... actually a 0 floor means "only back off at
// literally zero free memory" — the practical off-switch).
func watcherMemoryBackoffMiB() int64 {
	raw := os.Getenv("PINCHER_WATCHER_MEMORY_BACKOFF_MIB")
	if raw == "" {
		return defaultWatcherMemoryBackoffMiB
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v < 0 {
		return defaultWatcherMemoryBackoffMiB
	}
	return v
}

// availableMemoryMiB returns the host's available memory in MiB.
// The bool is false when the platform can't be queried — callers
// MUST treat ok=false as "don't back off" rather than "no memory",
// so an unsupported platform keeps the pre-#1572 behaviour.
//
// Linux: parses MemAvailable from /proc/meminfo (the kernel's own
// estimate of memory available for new allocations without
// swapping — strictly better than MemFree, which omits reclaimable
// page cache). Other platforms: ok=false until their readers land.
func availableMemoryMiB() (int64, bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return parseMemAvailableMiB(string(data))
}

// parseMemAvailableMiB extracts the MemAvailable line from
// /proc/meminfo content and converts the kB value to MiB. Split out
// from availableMemoryMiB so it's unit-testable without a real
// /proc/meminfo. Returns ok=false when the line is absent or
// malformed.
//
// The /proc/meminfo line shape is:
//
//	MemAvailable:    8123456 kB
func parseMemAvailableMiB(meminfo string) (int64, bool) {
	for _, line := range strings.Split(meminfo, "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		// Expect: ["MemAvailable:", "<number>", "kB"]
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb < 0 {
			return 0, false
		}
		return kb / 1024, true
	}
	return 0, false
}
