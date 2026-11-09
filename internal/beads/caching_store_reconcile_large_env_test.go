package beads

import (
	"testing"
	"time"
)

// The large-store cadence override (ga-6qht7x, ga-tyf38i install): unset keeps
// today's 120 s; a value in range is honoured; anything else is ignored and
// never yields a faster cadence than today's.
func TestCacheReconcileLargeIntervalOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset keeps 120 s", "", cacheReconcileIntervalLarge},
		{"300 s honoured", "300000", 300 * time.Second},
		{"the floor itself", "120000", cacheReconcileIntervalLarge},
		{"the ceiling itself", "900000", 15 * time.Minute},
		{"faster than today is ignored", "60000", cacheReconcileIntervalLarge},
		{"past the ceiling is ignored", "900001", cacheReconcileIntervalLarge},
		{"not a number is ignored", "5m", cacheReconcileIntervalLarge},
		{"negative is ignored", "-300000", cacheReconcileIntervalLarge},
		// 2^64 ns is 18446744073709.551616 ms, so this value times 1e6 wraps
		// to about 299.999 s, inside the bounds.
		{"a value that wraps into range is ignored", "18446744373709", cacheReconcileIntervalLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(CacheReconcileLargeIntervalEnv, tc.env)
			if got := beadCountCadence(7870); got != tc.want {
				t.Fatalf("beadCountCadence(7870) with %s=%q = %v, want %v", CacheReconcileLargeIntervalEnv, tc.env, got, tc.want)
			}
		})
	}
	// Smaller tiers never read the override.
	t.Setenv(CacheReconcileLargeIntervalEnv, "300000")
	if got := beadCountCadence(4999); got != cacheReconcileIntervalMedium {
		t.Fatalf("beadCountCadence(4999) = %v, want the medium tier %v", got, cacheReconcileIntervalMedium)
	}
	// The latency driver still composes as the slower of the two.
	if got := effectiveCadence(7870, true); got != 300*time.Second {
		t.Fatalf("effectiveCadence(7870, latency) = %v, want 300s", got)
	}
}
