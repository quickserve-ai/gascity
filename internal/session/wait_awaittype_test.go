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
