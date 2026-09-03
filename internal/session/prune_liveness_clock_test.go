package session

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/liveness"
)

// Once session-liveness telemetry stopped touching the issues row,
// Bead.UpdatedAt on a session bead froze at its last genuine versioned write.
// The drained-asleep prune fallback reads that column, so without a replacement
// clock a session that is still beating would age past the cutoff and be pruned.
// EffectiveUpdatedAt is that replacement; these tests pin both halves.

func TestEffectiveUpdatedAtPrefersTheLaterOfRowAndLivenessClock(t *testing.T) {
	rowAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	liveAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	t.Run("liveness clock is newer", func(t *testing.T) {
		b := beads.Bead{
			UpdatedAt: rowAt,
			Metadata:  beads.StringMap{liveness.WrittenAtKey: liveAt.Format(time.RFC3339Nano)},
		}
		if got := EffectiveUpdatedAt(b); !got.Equal(liveAt) {
			t.Fatalf("EffectiveUpdatedAt = %v, want the liveness clock %v", got, liveAt)
		}
	})

	t.Run("row is newer", func(t *testing.T) {
		b := beads.Bead{
			UpdatedAt: liveAt,
			Metadata:  beads.StringMap{liveness.WrittenAtKey: rowAt.Format(time.RFC3339Nano)},
		}
		if got := EffectiveUpdatedAt(b); !got.Equal(liveAt) {
			t.Fatalf("EffectiveUpdatedAt = %v, want the row clock %v", got, liveAt)
		}
	})

	t.Run("no liveness rows is identity", func(t *testing.T) {
		b := beads.Bead{UpdatedAt: rowAt}
		if got := EffectiveUpdatedAt(b); !got.Equal(rowAt) {
			t.Fatalf("EffectiveUpdatedAt = %v, want %v unchanged", got, rowAt)
		}
	})

	t.Run("an unparseable clock degrades to the row", func(t *testing.T) {
		b := beads.Bead{
			UpdatedAt: rowAt,
			Metadata:  beads.StringMap{liveness.WrittenAtKey: "not-a-timestamp"},
		}
		if got := EffectiveUpdatedAt(b); !got.Equal(rowAt) {
			t.Fatalf("EffectiveUpdatedAt = %v, want %v unchanged", got, rowAt)
		}
	})
}

func TestPruneStateTimestampUsesTheLivenessClockForDrainedAsleep(t *testing.T) {
	rowAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	liveAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	base := beads.Bead{
		UpdatedAt: rowAt,
		CreatedAt: rowAt.Add(-24 * time.Hour),
		Metadata: beads.StringMap{
			"state":        string(StateAsleep),
			"sleep_reason": "drained",
		},
	}

	got, ok := pruneStateTimestamp(base, StateAsleep)
	if !ok || !got.Equal(rowAt) {
		t.Fatalf("without liveness rows: (%v,%v), want (%v,true)", got, ok, rowAt)
	}

	beating := base
	beating.Metadata = beads.StringMap{
		"state":               string(StateAsleep),
		"sleep_reason":        "drained",
		liveness.WrittenAtKey: liveAt.Format(time.RFC3339Nano),
	}
	got, ok = pruneStateTimestamp(beating, StateAsleep)
	if !ok {
		t.Fatalf("pruneStateTimestamp ok = false, want true")
	}
	if !got.Equal(liveAt) {
		t.Fatalf("pruneStateTimestamp = %v, want the liveness clock %v — a still-beating session must not age off the frozen row column", got, liveAt)
	}
}
