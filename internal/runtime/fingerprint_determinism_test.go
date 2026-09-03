package runtime

import "testing"

// TestLiveFingerprintIsDeterministicOverIdenticalConfig pins the half of
// ga-wd5se7 that turned out NOT to be the defect.
//
// The reconciler decides live-config drift by comparing a stored
// started_live_hash against LiveFingerprint(agentCfg) recomputed each tick, and
// that comparison was firing ~195 times an hour across eight qcore seats whose
// live config nobody was editing. Two explanations were possible: the hash is
// unstable over the same input, or the CONFIG handed to it differs tick to tick.
// This test settles the first, so the search can go to the second without
// re-auditing the hash functions.
//
// It matters beyond this bead: every fingerprint in this file feeds a drift
// decision that can drain or restart a live session, so an order-dependent hash
// would be a restart loop waiting for a map to rehash.
func TestLiveFingerprintIsDeterministicOverIdenticalConfig(t *testing.T) {
	build := func() Config {
		return Config{
			SessionLive: []string{"tmux set -g mouse on", "tmux set -g status off"},
			Env: map[string]string{
				"ZED": "26", "ALPHA": "1", "MIKE": "13", "BRAVO": "2", "YANKEE": "25",
			},
		}
	}

	// Same value, hashed repeatedly in one process: catches an unsorted map or
	// slice range inside the hasher, which Go's randomized map iteration would
	// otherwise surface only intermittently — exactly the shape that produces a
	// drift alarm every 96 seconds and never in a unit test run once.
	cfg := build()
	want := LiveFingerprint(cfg)
	for i := 0; i < 500; i++ {
		if got := LiveFingerprint(cfg); got != want {
			t.Fatalf("LiveFingerprint is not stable over one value: iteration %d gave %q, want %q", i, got, want)
		}
	}

	// Two independently-built values with identical content must agree, so the
	// hash cannot depend on allocation or insertion order either.
	for i := 0; i < 500; i++ {
		if got := LiveFingerprint(build()); got != want {
			t.Fatalf("two identically-built Configs hash differently: iteration %d gave %q, want %q", i, got, want)
		}
	}

	// And it must actually distinguish a real change, or stability is vacuous.
	changed := build()
	changed.SessionLive = append([]string(nil), changed.SessionLive...)
	changed.SessionLive[0] = "tmux set -g mouse off"
	if got := LiveFingerprint(changed); got == want {
		t.Fatal("LiveFingerprint did not change when SessionLive changed; the drift check cannot see real edits")
	}
}
