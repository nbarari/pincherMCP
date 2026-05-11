package db

import (
	"testing"
)

func TestMaybeFireCelebration_FirstCrossingFires(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	threshold, fired, err := store.MaybeFireCelebration(150_000)
	if err != nil {
		t.Fatalf("MaybeFireCelebration: %v", err)
	}
	if !fired {
		t.Fatalf("expected first 100k crossing to fire; got fired=false")
	}
	if threshold != 100_000 {
		t.Fatalf("expected threshold=100000; got %d", threshold)
	}
}

func TestMaybeFireCelebration_AlreadyFired_DoesNotRefire(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if _, _, err := store.MaybeFireCelebration(150_000); err != nil {
		t.Fatalf("first fire: %v", err)
	}
	threshold, fired, err := store.MaybeFireCelebration(200_000)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if fired {
		t.Fatalf("threshold 100k should not refire; got threshold=%d", threshold)
	}
}

func TestMaybeFireCelebration_HighestOnly_VaultingMultiple(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// One huge call vaults from 0 → 2M, crossing 100k, 500k, 1M all at
	// once. Per spec we fire only the highest (1M). Subsequent calls
	// at 2M should NOT fire 100k or 500k retroactively.
	threshold, fired, err := store.MaybeFireCelebration(2_000_000)
	if err != nil {
		t.Fatalf("MaybeFireCelebration: %v", err)
	}
	if !fired || threshold != 1_000_000 {
		t.Fatalf("expected fired=true threshold=1000000; got fired=%v threshold=%d", fired, threshold)
	}
	// Second call at 2M: 1M already fired, no lower threshold should
	// fire either (spec: highest only, no backfill).
	_, fired2, err := store.MaybeFireCelebration(2_000_000)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if fired2 {
		t.Fatalf("backfill of skipped lower thresholds is not allowed")
	}
}

func TestMaybeFireCelebration_BelowFirstThreshold_Silent(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	threshold, fired, err := store.MaybeFireCelebration(50_000)
	if err != nil {
		t.Fatalf("MaybeFireCelebration: %v", err)
	}
	if fired || threshold != 0 {
		t.Fatalf("below 100k should be silent; got fired=%v threshold=%d", fired, threshold)
	}
}

func TestMaybeFireCelebration_GrowsThroughTiers(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// Realistic growth: each call advances cumulative by enough to
	// cross exactly one threshold.
	tiers := []struct {
		cumulative int64
		want       int64
	}{
		{150_000, 100_000},
		{600_000, 500_000},
		{1_500_000, 1_000_000},
		{6_000_000, 5_000_000},
		{12_000_000, 10_000_000},
	}
	for _, tc := range tiers {
		threshold, fired, err := store.MaybeFireCelebration(tc.cumulative)
		if err != nil {
			t.Fatalf("at %d: %v", tc.cumulative, err)
		}
		if !fired || threshold != tc.want {
			t.Errorf("at cumulative=%d: want threshold=%d fired=true; got threshold=%d fired=%v",
				tc.cumulative, tc.want, threshold, fired)
		}
	}
}
