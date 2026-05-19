package index

import (
	"runtime"
	"testing"
)

// #1572 v0.87: memory-pressure-aware watcher backoff. The watcher
// reads available memory before each tick and skips when it's below
// a configurable floor. These tests pin the parser + the env-var
// resolution; the watcher integration is exercised by the existing
// Watch tests (the backoff is a guarded `continue` that doesn't
// change the loop's structure).

func TestParseMemAvailableMiB_HappyPath_1572(t *testing.T) {
	t.Parallel()
	// Realistic /proc/meminfo excerpt.
	meminfo := `MemTotal:       16384000 kB
MemFree:         2048000 kB
MemAvailable:    8388608 kB
Buffers:          512000 kB
Cached:          4096000 kB
`
	mib, ok := parseMemAvailableMiB(meminfo)
	if !ok {
		t.Fatal("expected ok=true on a well-formed MemAvailable line")
	}
	// 8388608 kB / 1024 = 8192 MiB.
	if mib != 8192 {
		t.Errorf("parseMemAvailableMiB = %d MiB, want 8192", mib)
	}
}

func TestParseMemAvailableMiB_MissingLine_1572(t *testing.T) {
	t.Parallel()
	// Old kernels (pre-3.14) lack MemAvailable. Must report ok=false
	// so the watcher doesn't back off on a bad reading.
	meminfo := `MemTotal:       16384000 kB
MemFree:         2048000 kB
Cached:          4096000 kB
`
	if _, ok := parseMemAvailableMiB(meminfo); ok {
		t.Error("expected ok=false when MemAvailable line is absent")
	}
}

func TestParseMemAvailableMiB_MalformedValue_1572(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"MemAvailable:    notanumber kB\n",
		"MemAvailable:\n",
		"MemAvailable:    -5 kB\n",
	} {
		if _, ok := parseMemAvailableMiB(bad); ok {
			t.Errorf("expected ok=false on malformed line %q", bad)
		}
	}
}

func TestWatcherMemoryBackoffMiB_Default_1572(t *testing.T) {
	t.Setenv("PINCHER_WATCHER_MEMORY_BACKOFF_MIB", "")
	if got := watcherMemoryBackoffMiB(); got != defaultWatcherMemoryBackoffMiB {
		t.Errorf("watcherMemoryBackoffMiB = %d, want default %d", got, defaultWatcherMemoryBackoffMiB)
	}
}

func TestWatcherMemoryBackoffMiB_EnvOverride_1572(t *testing.T) {
	t.Setenv("PINCHER_WATCHER_MEMORY_BACKOFF_MIB", "1024")
	if got := watcherMemoryBackoffMiB(); got != 1024 {
		t.Errorf("watcherMemoryBackoffMiB = %d, want 1024 from env", got)
	}
}

func TestWatcherMemoryBackoffMiB_BadEnvFallsBackToDefault_1572(t *testing.T) {
	for _, bad := range []string{"notanumber", "-1", "  "} {
		t.Setenv("PINCHER_WATCHER_MEMORY_BACKOFF_MIB", bad)
		if got := watcherMemoryBackoffMiB(); got != defaultWatcherMemoryBackoffMiB {
			t.Errorf("bad env %q: watcherMemoryBackoffMiB = %d, want default %d",
				bad, got, defaultWatcherMemoryBackoffMiB)
		}
	}
}

// availableMemoryMiB contract: on Linux it should return a plausible
// reading; on every other platform it must report ok=false so the
// watcher never backs off on an unsupported host.
func TestAvailableMemoryMiB_PlatformContract_1572(t *testing.T) {
	t.Parallel()
	mib, ok := availableMemoryMiB()
	if runtime.GOOS == "linux" {
		if !ok {
			t.Skip("Linux but /proc/meminfo unreadable (containerised CI without /proc?) — not a failure of the code under test")
		}
		if mib <= 0 {
			t.Errorf("Linux availableMemoryMiB = %d, want a positive reading", mib)
		}
	} else {
		if ok {
			t.Errorf("non-Linux (%s) must report ok=false; got ok=true mib=%d", runtime.GOOS, mib)
		}
	}
}
