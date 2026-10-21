package session

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// ga-knhu61: a session wait bead is machinery — it must carry a non-human
// await_type AT CREATE, or the beads create seam defaults it to "human" and
// the on-creation notifier pages per wait.
func TestCreateWaitSetsMachineryAwaitTypeAtCreate(t *testing.T) {
	sess := sessionBeadFixture("gc-session", "open", map[string]string{
		"__title":            "worker",
		"session_name":       "worker-1",
		"continuation_epoch": "5",
	})
	s, rec := recordingWaitStore(t, sess)

	if _, err := s.CreateWait(WaitSpec{
		SessionID: "gc-session",
		Kind:      "deps",
		DepIDs:    []string{"x-1"},
		Now:       time.Date(2026, 3, 2, 4, 5, 6, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateWait: %v", err)
	}
	creates := rec.CallsForOp("Create")
	if len(creates) != 1 {
		t.Fatalf("want 1 Create, got %d", len(creates))
	}
	if got := creates[0].Bead.AwaitType; got != beads.AwaitBead {
		t.Fatalf("Create carried AwaitType %q, want %q — it must be on the CREATE itself, not a follow-up write", got, beads.AwaitBead)
	}
}

// A retried wait is re-derived from its kind, not copied: a wait minted before
// await_type existed carries "" (or "human" from the create seam's default),
// and copying that forward would page a human for machinery on every retry.
func TestRetryClosedWaitRederivesMachineryAwaitType(t *testing.T) {
	for _, tc := range []struct {
		kind, stored, want string
	}{
		{kind: "deps", stored: "", want: beads.AwaitBead},
		{kind: "deps", stored: "human", want: beads.AwaitBead},
		{kind: "timer", stored: "", want: beads.AwaitTimer},
	} {
		sess := sessionBeadFixture("gc-session", "open", map[string]string{
			"session_name":       "worker",
			"continuation_epoch": "2",
		})
		wait := waitBeadFixture("w-1", "closed", "gc-session", map[string]string{
			"__title":          "wait:worker",
			"session_name":     "worker",
			"kind":             tc.kind,
			"state":            "failed",
			"delivery_attempt": "1",
		})
		wait.AwaitType = tc.stored
		s, rec := recordingWaitStore(t, sess, wait)

		if _, err := s.RetryClosedWait("w-1", "2", time.Date(2026, 3, 2, 4, 5, 6, 0, time.UTC)); err != nil {
			t.Fatalf("kind=%s stored=%q: RetryClosedWait: %v", tc.kind, tc.stored, err)
		}
		creates := rec.CallsForOp("Create")
		if len(creates) != 1 {
			t.Fatalf("kind=%s stored=%q: want 1 Create, got %d", tc.kind, tc.stored, len(creates))
		}
		if got := creates[0].Bead.AwaitType; got != tc.want {
			t.Fatalf("kind=%s stored=%q: replacement carried AwaitType %q, want %q", tc.kind, tc.stored, got, tc.want)
		}
	}
}

func TestWaitAwaitTypeMapsTimerKind(t *testing.T) {
	if got := waitAwaitType("timer"); got != beads.AwaitTimer {
		t.Fatalf("waitAwaitType(timer) = %q, want %q", got, beads.AwaitTimer)
	}
	for _, kind := range []string{"deps", "probe", ""} {
		if got := waitAwaitType(kind); got != beads.AwaitBead {
			t.Fatalf("waitAwaitType(%q) = %q, want %q — never human", kind, got, beads.AwaitBead)
		}
	}
}
