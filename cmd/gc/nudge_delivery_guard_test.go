package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// startGuardTestSession creates and starts a claude session on a Fake
// provider and returns it with a poller target that reads idle.
func startGuardTestSession(t *testing.T, dir string, fake *runtime.Fake) (session.Info, nudgeTarget, worker.LiveObservation) {
	t.Helper()
	store := openNudgeBeadStore(dir)
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{info.SessionName: idleSince}
	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		sessionID:   info.ID,
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: info.SessionName,
	}
	return info, target, worker.LiveObservation{Running: true, LastActivity: &idleSince}
}

// TestTryDeliverQueuedNudgesByPollerKeepsADeferredNudgeQueued is the queue half
// of ga-ubfc7j. When the runtime refuses to type because the pane holds a
// question dialog (ErrNudgeDeferredHumanPrompt), nothing reached the seat, so
// the item must stay pending, due now, WITHOUT spending one of its
// defaultQueuedNudgeMaxAttempts: a dialog left open for an hour would
// otherwise dead-letter every reminder queued behind it. Both passes -- the
// deferral and the later delivery -- must leave a record an answer can be
// matched against.
func TestTryDeliverQueuedNudgesByPollerKeepsADeferredNudgeQueued(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	item := newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	fake := runtime.NewFake()
	info, target, obs := startGuardTestSession(t, dir, fake)
	store := openNudgeBeadStore(dir).Store
	fake.NudgeErrors = map[string]error{info.SessionName: fmt.Errorf("worker nudge: %w", &tmux.NudgeDeferredError{
		Session: info.SessionName,
		Reason:  tmux.NudgeDeferReasonQuestionDialog,
		Stage:   "before_type",
	})}

	for pass := 1; pass <= defaultQueuedNudgeMaxAttempts+1; pass++ {
		delivered, err := tryDeliverQueuedNudgesByPoller(target, store, store, fake, 3*time.Second, obs)
		if err != nil {
			t.Fatalf("pass %d: tryDeliverQueuedNudgesByPoller: %v", pass, err)
		}
		if delivered {
			t.Fatalf("pass %d: delivered = true for a deferred nudge", pass)
		}
	}
	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0: a deferral must keep the item queued", len(pending), len(inFlight), len(dead))
	}
	if pending[0].Attempts != 0 {
		t.Fatalf("attempts = %d after %d deferrals, want 0: a deferral sent nothing and must not burn an attempt", pending[0].Attempts, defaultQueuedNudgeMaxAttempts+1)
	}

	// The dialog is answered; the next pass delivers.
	fake.NudgeErrors = nil
	delivered, err := tryDeliverQueuedNudgesByPoller(target, store, store, fake, 3*time.Second, obs)
	if err != nil || !delivered {
		t.Fatalf("after the dialog closed: delivered=%v err=%v, want true/nil", delivered, err)
	}

	recs, err := readNudgeDeliveryRecords(dir)
	if err != nil {
		t.Fatalf("readNudgeDeliveryRecords: %v", err)
	}
	var deferred, deliveredRecs []nudgeDeliveryRecord
	for _, r := range recs {
		switch r.Outcome {
		case "deferred":
			deferred = append(deferred, r)
		case "delivered":
			deliveredRecs = append(deliveredRecs, r)
		}
	}
	if len(deferred) == 0 {
		t.Fatalf("no deferred record in %s; records: %+v", nudgeDeliveryLogPath(dir), recs)
	}
	if len(deliveredRecs) != 1 {
		t.Fatalf("delivered records = %d, want 1; records: %+v", len(deliveredRecs), recs)
	}
	for _, r := range append(deferred[:1], deliveredRecs...) {
		if r.Session != info.SessionName || r.SessionID != info.ID || r.Agent != "worker" {
			t.Errorf("record %+v: want session %q id %q agent worker", r, info.SessionName, info.ID)
		}
		if len(r.ItemIDs) != 1 || r.ItemIDs[0] != item.ID {
			t.Errorf("record %+v: item ids = %v, want [%s]", r, r.ItemIDs, item.ID)
		}
		if len(r.MessageSHA256) != 64 {
			t.Errorf("record %+v: message hash %q is not a sha256 hex digest", r, r.MessageSHA256)
		}
		if r.Time.IsZero() {
			t.Errorf("record %+v: no timestamp", r)
		}
	}
	if deferred[0].Reason != tmux.NudgeDeferReasonQuestionDialog {
		t.Errorf("deferred reason = %q, want %q", deferred[0].Reason, tmux.NudgeDeferReasonQuestionDialog)
	}
	if deferred[0].MessageSHA256 != deliveredRecs[0].MessageSHA256 {
		t.Errorf("deferred and delivered hashes differ for the same item: %q vs %q", deferred[0].MessageSHA256, deliveredRecs[0].MessageSHA256)
	}
}

// TestTryDeliverQueuedNudgesByPollerRecordsAFailedDelivery: a genuine failure
// still spends an attempt, as before, and is recorded too.
func TestTryDeliverQueuedNudgesByPollerRecordsAFailedDelivery(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	fake := runtime.NewFake()
	info, target, obs := startGuardTestSession(t, dir, fake)
	store := openNudgeBeadStore(dir).Store
	fake.NudgeErrors = map[string]error{info.SessionName: tmux.ErrNudgeSubmitUnconfirmed}

	if delivered, err := tryDeliverQueuedNudgesByPoller(target, store, store, fake, 3*time.Second, obs); err != nil || delivered {
		t.Fatalf("delivered=%v err=%v, want false/nil", delivered, err)
	}
	pending, _, _, err := listQueuedNudges(dir, "worker", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || pending[0].Attempts != 1 {
		t.Fatalf("pending = %+v, want one item with 1 attempt (an unconfirmed submit still counts)", pending)
	}
	recs, err := readNudgeDeliveryRecords(dir)
	if err != nil {
		t.Fatalf("readNudgeDeliveryRecords: %v", err)
	}
	if len(recs) != 1 || recs[0].Outcome != "failed" || !strings.Contains(recs[0].Reason, "not confirmed") {
		t.Fatalf("records = %+v, want one failed record carrying the cause", recs)
	}
}

// TestDeliverSessionNudgeQueuesWhenThePaneHoldsAHumanPrompt: a direct
// `gc session nudge` that meets a question dialog must not fail and must not
// be lost: it is queued, and the dispatcher delivers it once the dialog is
// answered.
func TestDeliverSessionNudgeQueuesWhenThePaneHoldsAHumanPrompt(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	info, target, _ := startGuardTestSession(t, dir, fake)
	store := openNudgeBeadStore(dir).Store
	fake.NudgeErrors = map[string]error{info.SessionName: &tmux.NudgeDeferredError{
		Session: info.SessionName,
		Reason:  tmux.NudgeDeferReasonApprovalPrompt,
		Stage:   "before_type",
	}}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithWorker(target, store, fake, "check deploy status", nudgeDeliveryImmediate, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithWorker = %d, want 0 (queued); stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Queued nudge") {
		t.Fatalf("stdout = %q, want a queued result", stdout.String())
	}
	if !strings.Contains(stderr.String(), tmux.NudgeDeferReasonApprovalPrompt) {
		t.Fatalf("stderr = %q, want the deferral reason named", stderr.String())
	}
	pending, _, _, err := listQueuedNudges(dir, target.agentKey(), time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || pending[0].Message != "check deploy status" {
		t.Fatalf("pending = %+v, want the nudge queued", pending)
	}
}

// TestTryDeliverQueuedNudgesByPollerRecordsAWithheldSubmit: a refusal at the
// submit comes after the text was typed. The log must say so rather than
// record a deferral that sent nothing.
func TestTryDeliverQueuedNudgesByPollerRecordsAWithheldSubmit(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	item := newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-time.Minute))
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	fake := runtime.NewFake()
	info, target, obs := startGuardTestSession(t, dir, fake)
	store := openNudgeBeadStore(dir).Store
	fake.NudgeErrors = map[string]error{info.SessionName: &tmux.NudgeDeferredError{
		Session: info.SessionName,
		Reason:  tmux.NudgeDeferReasonQuestionDialog,
		Stage:   "before_submit",
	}}
	if _, err := tryDeliverQueuedNudgesByPoller(target, store, store, fake, 3*time.Second, obs); err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
	}
	recs, err := readNudgeDeliveryRecords(dir)
	if err != nil {
		t.Fatalf("readNudgeDeliveryRecords: %v", err)
	}
	if len(recs) != 1 || recs[0].Outcome != nudgeDeliveryOutcomeSubmitWithheld || recs[0].Reason != tmux.NudgeDeferReasonQuestionDialog {
		t.Fatalf("records = %+v, want one submit_withheld/question_dialog", recs)
	}
}
