package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

type capturedNudgeBounce struct {
	cityPath, to, subject, body string
}

// captureNudgeDeadLetterBounces swaps the bouncer for a recorder that returns
// err for every notice.
func captureNudgeDeadLetterBounces(t *testing.T, err error) *[]capturedNudgeBounce {
	t.Helper()
	var got []capturedNudgeBounce
	prev := nudgeDeadLetterBouncer
	nudgeDeadLetterBouncer = func(cityPath, to, subject, body string) error {
		got = append(got, capturedNudgeBounce{cityPath: cityPath, to: to, subject: subject, body: body})
		return err
	}
	t.Cleanup(func() { nudgeDeadLetterBouncer = prev })
	return &got
}

// writeNudgeBounceCity writes a city whose configured named sessions are
// mayor and deacon.
func writeNudgeBounceCity(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "missing-provider"

[[agent]]
name = "deacon"
provider = "missing-provider"

[providers.missing-provider]
command = "missing-provider"

[[named_session]]
template = "mayor"

[[named_session]]
template = "deacon"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	return dir
}

const nudgeBounceSecret = "SECRET-NUDGE-TEXT-do-not-echo"

func newBounceTestNudge(id, agent, sender, source string, now time.Time) queuedNudge {
	item := newQueuedNudgeWithOptions(agent, nudgeBounceSecret, source, now, queuedNudgeOptions{ID: id})
	item.Sender = sender
	item.SenderSession = ""
	return item
}

// enqueueNearlyExhausted enqueues item one failure short of retry exhaustion.
func enqueueNearlyExhausted(t *testing.T, dir string, store beads.NudgesStore, item queuedNudge) {
	t.Helper()
	item.Attempts = defaultQueuedNudgeMaxAttempts - 1
	if err := enqueueQueuedNudgeWithStore(dir, store, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore(%s): %v", item.ID, err)
	}
}

func deadQueueIDs(t *testing.T, dir string) []string {
	t.Helper()
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return queuedNudgeIDs(state.Dead)
}

// (a) retry exhaustion through recordQueuedNudgeFailureDetailed.
func TestNudgeDeadLetterBounceRetryExhaustion(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	got := captureNudgeDeadLetterBounces(t, nil)
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()

	item := newBounceTestNudge("n-exhaust", "mayor", "deacon", "session", now)
	enqueueNearlyExhausted(t, dir, store, item)
	if len(*got) != 0 {
		t.Fatalf("bounces after enqueue = %d, want 0", len(*got))
	}

	dead, err := recordQueuedNudgeFailureDetailed(dir, store, []string{item.ID}, errors.New("provider refused the paste"), now)
	if err != nil {
		t.Fatalf("recordQueuedNudgeFailureDetailed: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("dead-lettered = %d, want 1", len(dead))
	}
	if len(*got) != 1 {
		t.Fatalf("bounces = %d, want exactly 1", len(*got))
	}
	b := (*got)[0]
	if b.cityPath != dir || b.to != "deacon" {
		t.Fatalf("bounce city/to = %q/%q, want %q/deacon", b.cityPath, b.to, dir)
	}
	if want := "[nudge dead-lettered] to mayor: provider refused the paste"; b.subject != want {
		t.Fatalf("subject = %q, want %q", b.subject, want)
	}
	if strings.Contains(b.body, nudgeBounceSecret) || strings.Contains(b.subject, nudgeBounceSecret) {
		t.Fatalf("bounce carries the nudge message text:\n%s", b.body)
	}
	for _, want := range []string{
		"Nudge: n-exhaust",
		"Target: mayor",
		"Cause: provider refused the paste",
		"Attempts: 5",
		"Created: " + now.UTC().Format(time.RFC3339),
		"Dead: " + now.UTC().Format(time.RFC3339),
		"Read it: .gc/nudges/state.json, the \"dead\" list, id n-exhaust",
		"The nudge was not delivered. If it mattered, re-send it or mail it.",
	} {
		if !strings.Contains(b.body, want) {
			t.Errorf("body missing %q:\n%s", want, b.body)
		}
	}
}

// (b) a TTL-expired item is swept by a later, unrelated queue operation.
func TestNudgeDeadLetterBounceTTLExpiryOnUnrelatedOperation(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	got := captureNudgeDeadLetterBounces(t, nil)
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()

	stale := newBounceTestNudge("n-stale", "mayor", "deacon", "session", now.Add(-2*time.Hour))
	stale.ExpiresAt = now.Add(-time.Minute).UTC()
	if err := enqueueQueuedNudgeWithStore(dir, store, stale); err != nil {
		t.Fatalf("enqueue stale: %v", err)
	}
	if len(*got) != 0 {
		t.Fatalf("bounces after enqueue = %d, want 0", len(*got))
	}

	other := newBounceTestNudge("n-other", "deacon", "", "session", now)
	if err := enqueueQueuedNudgeWithStore(dir, store, other); err != nil {
		t.Fatalf("enqueue unrelated: %v", err)
	}
	if ids := deadQueueIDs(t, dir); len(ids) != 1 || ids[0] != "n-stale" {
		t.Fatalf("dead = %v, want [n-stale]", ids)
	}
	if len(*got) != 1 {
		t.Fatalf("bounces = %d, want exactly 1", len(*got))
	}
	if b := (*got)[0]; b.to != "deacon" || b.subject != "[nudge dead-lettered] to mayor: expired" {
		t.Fatalf("bounce to/subject = %q/%q", b.to, b.subject)
	}
}

// (c) senders and sources that must never bounce.
func TestNudgeDeadLetterBounceSkipsIneligibleSenders(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cases := []struct {
		name, agent, sender, source string
	}{
		{"empty sender", "mayor", "", "session"},
		{"blank sender", "mayor", "   ", "session"},
		{"human", "mayor", "human", "session"},
		{"self", "mayor", "mayor", "session"},
		{"mail notify wake", "mayor", "deacon", "mail"},
		{"unconfigured sender", "mayor", "order:cert-patrol", "session"},
		{"unconfigured bare name", "mayor", "ghost", "session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeNudgeBounceCity(t)
			got := captureNudgeDeadLetterBounces(t, nil)
			store := beads.NudgesStore{Store: beads.NewMemStore()}
			now := time.Now()
			item := newBounceTestNudge("n-skip", tc.agent, tc.sender, tc.source, now)
			enqueueNearlyExhausted(t, dir, store, item)
			dead, err := recordQueuedNudgeFailureDetailed(dir, store, []string{item.ID}, errors.New("boom"), now)
			if err != nil || len(dead) != 1 {
				t.Fatalf("recordQueuedNudgeFailureDetailed = %d, %v; want 1 dead-lettered", len(dead), err)
			}
			if len(*got) != 0 {
				t.Fatalf("bounces = %+v, want none", *got)
			}
		})
	}
}

// (d) an item already dead never bounces again, including when retention
// pruning rewrites Dead and shifts its position.
func TestNudgeDeadLetterBounceAlreadyDeadDoesNotRebounce(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	got := captureNudgeDeadLetterBounces(t, nil)
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()
	// Both items are enqueued now; n-old dies at now, n-new half a
	// retention window later, so a pass after the window prunes only n-old.
	later := now.Add(defaultQueuedNudgeDeadRetention / 2)
	afterWindow := now.Add(defaultQueuedNudgeDeadRetention + 10*time.Minute)

	first := newBounceTestNudge("n-old", "mayor", "deacon", "session", now)
	enqueueNearlyExhausted(t, dir, store, first)
	second := newBounceTestNudge("n-new", "mayor", "deacon", "session", now)
	enqueueNearlyExhausted(t, dir, store, second)
	if _, err := recordQueuedNudgeFailureDetailed(dir, store, []string{first.ID}, errors.New("first"), now); err != nil {
		t.Fatalf("dead-letter first: %v", err)
	}
	if _, err := recordQueuedNudgeFailureDetailed(dir, store, []string{second.ID}, errors.New("second"), later); err != nil {
		t.Fatalf("dead-letter second: %v", err)
	}
	if len(*got) != 2 {
		t.Fatalf("bounces = %d, want 2 (one per item)", len(*got))
	}
	if ids := deadQueueIDs(t, dir); len(ids) != 2 || ids[0] != "n-old" {
		t.Fatalf("dead = %v, want [n-old n-new] before the prune", ids)
	}

	// An unrelated failure record past the window runs the retention prune:
	// n-old leaves Dead and n-new moves to index 0.
	if _, err := recordQueuedNudgeFailureDetailed(dir, store, []string{"n-absent"}, errors.New("unrelated"), afterWindow); err != nil {
		t.Fatalf("unrelated operation: %v", err)
	}
	if ids := deadQueueIDs(t, dir); len(ids) != 1 || ids[0] != "n-new" {
		t.Fatalf("dead = %v, want [n-new] (retention prune must have rewritten Dead)", ids)
	}
	if err := runNudgeQueueMaintenanceSweep(dir, afterWindow.Add(time.Minute)); err != nil {
		t.Fatalf("maintenance sweep: %v", err)
	}
	if len(*got) != 2 {
		t.Fatalf("bounces = %d, want still 2 after later operations", len(*got))
	}
}

// (e) a callback error means the state write did not commit: nothing bounces.
func TestNudgeDeadLetterBounceSkippedWhenCallbackFails(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	got := captureNudgeDeadLetterBounces(t, nil)
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()
	item := newBounceTestNudge("n-rollback", "mayor", "deacon", "session", now)
	if err := enqueueQueuedNudgeWithStore(dir, store, item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	wantErr := errors.New("abort")
	err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		moved := state.Pending[0]
		moved.DeadAt = now.UTC()
		moved.LastError = "would have died"
		state.Pending = nil
		state.Dead = append(state.Dead, moved)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("withNudgeQueueState err = %v, want %v", err, wantErr)
	}
	if len(*got) != 0 {
		t.Fatalf("bounces = %+v, want none when fn fails", *got)
	}
	if ids := deadQueueIDs(t, dir); len(ids) != 0 {
		t.Fatalf("dead = %v, want none (uncommitted)", ids)
	}
}

// (f) a failing bouncer warns once and never changes the operation's result.
func TestNudgeDeadLetterBounceFailureDoesNotAffectOperation(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	got := captureNudgeDeadLetterBounces(t, errors.New("mail store down"))
	var warnings bytes.Buffer
	prevWarn := nudgeWarningWriter
	nudgeWarningWriter = &warnings
	t.Cleanup(func() { nudgeWarningWriter = prevWarn })
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()

	item := newBounceTestNudge("n-warn", "mayor", "deacon", "session", now)
	enqueueNearlyExhausted(t, dir, store, item)
	dead, err := recordQueuedNudgeFailureDetailed(dir, store, []string{item.ID}, context.DeadlineExceeded, now)
	if err != nil {
		t.Fatalf("recordQueuedNudgeFailureDetailed err = %v, want nil despite bounce failure", err)
	}
	if len(dead) != 1 || dead[0].ID != item.ID {
		t.Fatalf("dead-lettered = %v, want [n-warn]", queuedNudgeIDs(dead))
	}
	if len(*got) != 1 {
		t.Fatalf("bounce attempts = %d, want 1 (no retry)", len(*got))
	}
	if n := strings.Count(warnings.String(), "n-warn"); n != 1 || !strings.Contains(warnings.String(), "mail store down") {
		t.Fatalf("warnings = %q, want one warning naming n-warn and the cause", warnings.String())
	}
	if ids := deadQueueIDs(t, dir); len(ids) != 1 || ids[0] != "n-warn" {
		t.Fatalf("dead = %v, want [n-warn]", ids)
	}
}

// The production bouncer writes one plain message bead addressed verbatim to
// the configured mailbox, and creates no session bead while doing it.
func TestNudgeDeadLetterBounceDefaultSendsPlainMailWithoutSessions(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	dir := writeNudgeBounceCity(t)
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()

	item := newBounceTestNudge("n-real", "mayor", "deacon", "session", now)
	enqueueNearlyExhausted(t, dir, store, item)
	if _, err := recordQueuedNudgeFailureDetailed(dir, store, []string{item.ID}, errors.New("unresolved target"), now); err != nil {
		t.Fatalf("recordQueuedNudgeFailureDetailed: %v", err)
	}

	cityStore, err := openStoreAtForCity(dir, dir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	defer closeBeadStoreHandle(cityStore) //nolint:errcheck
	all, err := cityStore.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var msgs []beads.Bead
	for _, b := range all {
		switch {
		case b.Type == "message":
			msgs = append(msgs, b)
		case b.Type == "session" || hasLabelForTest(b.Labels, "gc:session"):
			t.Fatalf("bounce created a session bead: %+v", b)
		}
	}
	if len(msgs) != 1 {
		t.Fatalf("message beads = %d (of %d beads), want 1", len(msgs), len(all))
	}
	m := msgs[0]
	if m.Assignee != "deacon" || m.From != nudgeDeadLetterBounceFrom {
		t.Fatalf("message to/from = %q/%q, want deacon/%s", m.Assignee, m.From, nudgeDeadLetterBounceFrom)
	}
	if !strings.HasPrefix(m.Title, "[nudge dead-lettered] to mayor: unresolved target") {
		t.Fatalf("title = %q", m.Title)
	}
	if strings.Contains(m.Description, nudgeBounceSecret) {
		t.Fatalf("message body carries the nudge text: %q", m.Description)
	}
}

func hasLabelForTest(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
