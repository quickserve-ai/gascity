package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type capturedNudgeBounce struct {
	cityPath, to, subject, body string
}

// nudgeBounceRecorder counts mailer opens and closes and captures notices.
type nudgeBounceRecorder struct {
	opens, closes int
	sent          []capturedNudgeBounce
}

// recordNudgeDeadLetterBounces swaps the mailer seam for a recorder whose
// sends return err.
func recordNudgeDeadLetterBounces(t *testing.T, err error) *nudgeBounceRecorder {
	t.Helper()
	rec := &nudgeBounceRecorder{}
	prev := openNudgeDeadLetterMailer
	openNudgeDeadLetterMailer = func(cityPath string, _ *config.City) (nudgeDeadLetterSendFunc, func(), error) {
		rec.opens++
		send := func(to, subject, body string) error {
			rec.sent = append(rec.sent, capturedNudgeBounce{cityPath: cityPath, to: to, subject: subject, body: body})
			return err
		}
		return send, func() { rec.closes++ }, nil
	}
	t.Cleanup(func() { openNudgeDeadLetterMailer = prev })
	return rec
}

// captureNudgeDeadLetterBounces is recordNudgeDeadLetterBounces for tests
// that only read the captured notices.
func captureNudgeDeadLetterBounces(t *testing.T, err error) *[]capturedNudgeBounce {
	t.Helper()
	return &recordNudgeDeadLetterBounces(t, err).sent
}

// writeNudgeBounceCity writes a city whose configured named sessions are
// mayor and deacon (city-scoped) and demo/witness and demo/scout (rig-scoped,
// whose runtime session names differ from their identities).
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

[[agent]]
name = "witness"
dir = "demo"
provider = "missing-provider"

[[agent]]
name = "scout"
dir = "demo"
provider = "missing-provider"

[[named_session]]
template = "witness"
dir = "demo"

[[named_session]]
template = "scout"
dir = "demo"
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

// One operation both prunes an old Dead entry (rewriting Dead) and
// dead-letters a new item: exactly the new item bounces.
func TestNudgeDeadLetterBouncePruneAndNewDeathInOneOperation(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	got := captureNudgeDeadLetterBounces(t, nil)
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()
	afterWindow := now.Add(defaultQueuedNudgeDeadRetention + 10*time.Minute)

	old := newBounceTestNudge("n-old", "mayor", "deacon", "session", now)
	enqueueNearlyExhausted(t, dir, store, old)
	fresh := newBounceTestNudge("n-fresh", "mayor", "deacon", "session", now)
	enqueueNearlyExhausted(t, dir, store, fresh)
	if _, err := recordQueuedNudgeFailureDetailed(dir, store, []string{old.ID}, errors.New("old"), now); err != nil {
		t.Fatalf("dead-letter old: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("bounces after first death = %d, want 1", len(*got))
	}

	if _, err := recordQueuedNudgeFailureDetailed(dir, store, []string{fresh.ID}, errors.New("fresh"), afterWindow); err != nil {
		t.Fatalf("dead-letter fresh: %v", err)
	}
	if ids := deadQueueIDs(t, dir); len(ids) != 1 || ids[0] != "n-fresh" {
		t.Fatalf("dead = %v, want [n-fresh] (the same operation must have pruned n-old)", ids)
	}
	if len(*got) != 2 {
		t.Fatalf("bounces = %d, want 2 (n-old once, then n-fresh once)", len(*got))
	}
	if body := (*got)[1].body; !strings.Contains(body, "Nudge: n-fresh") {
		t.Fatalf("second bounce is not for n-fresh:\n%s", body)
	}
}

// The sender and target name the same rig-scoped seat in two forms
// (configured identity and runtime session name): no bounce. The same shapes naming two different
// seats do bounce, so the skip is the identity match, not the shape.
func TestNudgeDeadLetterBounceSkipsSenderNamingTargetByAnotherForm(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(dir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sessionNameOf := func(identity string) string {
		spec, ok, err := session.FindNamedSessionSpecForTarget(cfg, loadedCityName(cfg, dir), identity, "")
		if err != nil || !ok {
			t.Fatalf("spec(%s) = %v, %v", identity, ok, err)
		}
		if spec.SessionName == "" || spec.SessionName == identity {
			t.Fatalf("spec(%s).SessionName = %q, want a distinct runtime name", identity, spec.SessionName)
		}
		return spec.SessionName
	}
	cases := []struct {
		name, agent string
		want        int
	}{
		{"same seat by session name", sessionNameOf("demo/witness"), 0},
		{"other seat by session name", sessionNameOf("demo/scout"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeNudgeBounceCity(t)
			got := captureNudgeDeadLetterBounces(t, nil)
			store := beads.NudgesStore{Store: beads.NewMemStore()}
			now := time.Now()
			item := newBounceTestNudge("n-form", tc.agent, "demo/witness", "session", now)
			enqueueNearlyExhausted(t, dir, store, item)
			if _, err := recordQueuedNudgeFailureDetailed(dir, store, []string{item.ID}, errors.New("boom"), now); err != nil {
				t.Fatalf("recordQueuedNudgeFailureDetailed: %v", err)
			}
			if len(*got) != tc.want {
				t.Fatalf("bounces = %d, want %d (agent %q, sender demo/witness)", len(*got), tc.want, tc.agent)
			}
		})
	}
}

// A nudge superseded by a newer one for the same reference does not bounce.
func TestNudgeDeadLetterBounceSkipsSuperseded(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	got := captureNudgeDeadLetterBounces(t, nil)
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()
	ref := &nudgeReference{Kind: "bead", ID: "ga-ref"}

	first := newBounceTestNudge("n-first", "mayor", "deacon", "wait", now)
	first.Reference = ref
	if err := enqueueQueuedNudgeWithStore(dir, store, first); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	second := newBounceTestNudge("n-second", "mayor", "deacon", "wait", now)
	second.Reference = ref
	if err := enqueueQueuedNudgeWithStore(dir, store, second); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	if ids := deadQueueIDs(t, dir); len(ids) != 1 || ids[0] != "n-first" {
		t.Fatalf("dead = %v, want [n-first] superseded", ids)
	}
	if len(*got) != 0 {
		t.Fatalf("bounces = %+v, want none for a superseded nudge", *got)
	}
}

// A burst of 25 deaths in one operation opens the mailer once, sends 10, and
// names the other 15 in one warning.
func TestNudgeDeadLetterBounceBurstIsCappedAndBatched(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	rec := recordNudgeDeadLetterBounces(t, nil)
	var warnings bytes.Buffer
	prevWarn := nudgeWarningWriter
	nudgeWarningWriter = &warnings
	t.Cleanup(func() { nudgeWarningWriter = prevWarn })
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()

	ids := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		item := newBounceTestNudge(fmt.Sprintf("n-burst-%02d", i), "mayor", "deacon", "session", now)
		enqueueNearlyExhausted(t, dir, store, item)
		ids = append(ids, item.ID)
	}
	dead, err := recordQueuedNudgeFailureDetailed(dir, store, ids, errors.New("lid closed"), now)
	if err != nil || len(dead) != 25 {
		t.Fatalf("recordQueuedNudgeFailureDetailed = %d, %v; want 25 dead-lettered", len(dead), err)
	}
	if rec.opens != 1 || rec.closes != 1 {
		t.Fatalf("mailer opens/closes = %d/%d, want 1/1 per operation", rec.opens, rec.closes)
	}
	if len(rec.sent) != nudgeDeadLetterBouncesPerCall {
		t.Fatalf("sent = %d, want %d", len(rec.sent), nudgeDeadLetterBouncesPerCall)
	}
	w := warnings.String()
	if strings.Count(w, "\n") != 1 || !strings.Contains(w, "15 dead-lettered nudges not bounced") {
		t.Fatalf("warnings = %q, want one line naming 15 unbounced nudges", w)
	}
	seen := map[string]int{}
	for _, b := range rec.sent {
		for _, id := range ids {
			if strings.Contains(b.body, "Nudge: "+id+"\n") {
				seen[id]++
			}
		}
	}
	for _, id := range ids {
		if strings.Contains(w, id) {
			seen[id]++
		}
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("nudge %s is accounted for %d times across sends and the warning, want exactly 1", id, seen[id])
		}
	}
}

// A managed-wake rollback fails the enqueuing command with exit 1, so the
// death it records does not bounce.
func TestNudgeDeadLetterBounceSkipsManagedWakeRollback(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_AGENT", "deacon")
	dir := writeNudgeBounceCity(t)
	got := captureNudgeDeadLetterBounces(t, nil)
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := store.SetMetadata(info.ID, "state", "closing"); err != nil {
		t.Fatalf("SetMetadata(state): %v", err)
	}
	prevManaged, prevPoke, prevObserve := nudgeCityUsesManagedReconciler, nudgePokeController, nudgeObserveTarget
	nudgeCityUsesManagedReconciler = func(cityPath string) bool { return cityPath == dir }
	nudgePokeController = func(string) error { return nil }
	nudgeObserveTarget = func(nudgeTarget, beads.Store, runtime.Provider) (worker.LiveObservation, error) {
		return worker.LiveObservation{Running: false}, nil
	}
	t.Cleanup(func() {
		nudgeCityUsesManagedReconciler, nudgePokeController, nudgeObserveTarget = prevManaged, prevPoke, prevObserve
	})
	target := nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}},
		sessionID:   info.ID,
		sessionName: info.SessionName,
		identity:    "worker",
		agent:       config.Agent{Name: "worker", Provider: "claude"},
	}

	var stdout, stderr bytes.Buffer
	if code := deliverSessionNudgeWithWorker(target, store, fake, nudgeBounceSecret, nudgeDeliveryImmediate, false, &stdout, &stderr); code != 1 {
		t.Fatalf("deliverSessionNudgeWithWorker = %d, want 1 (the caller sees the failure)", code)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Dead) != 1 || state.Dead[0].Sender != "deacon" || !strings.HasPrefix(state.Dead[0].LastError, nudgeManagedWakeRollbackCause) {
		t.Fatalf("dead = %+v, want one rollback death sent by deacon", state.Dead)
	}
	if len(*got) != 0 {
		t.Fatalf("bounces = %+v, want none for a rollback the caller already saw", *got)
	}
}

// The cause is one line, free of control characters, and at most 160 runes
// in the body (80 in the subject).
func TestNudgeDeadLetterBounceCauseIsOneLineAndBounded(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeNudgeBounceCity(t)
	got := captureNudgeDeadLetterBounces(t, nil)
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()
	item := newBounceTestNudge("n-cause", "mayor", "deacon", "session", now)
	enqueueNearlyExhausted(t, dir, store, item)
	cause := "provider said:\n\x1b[31mecho\r\n" + strings.Repeat("x", 400) + " TAIL-MARKER"
	if _, err := recordQueuedNudgeFailureDetailed(dir, store, []string{item.ID}, errors.New(cause), now); err != nil {
		t.Fatalf("recordQueuedNudgeFailureDetailed: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("bounces = %d, want 1", len(*got))
	}
	b := (*got)[0]
	var causeLine string
	for _, line := range strings.Split(b.body, "\n") {
		if strings.HasPrefix(line, "Cause: ") {
			causeLine = strings.TrimPrefix(line, "Cause: ")
		}
	}
	if !strings.HasPrefix(causeLine, "provider said: [31mecho xxx") {
		t.Fatalf("cause line = %q, want newlines and control characters collapsed", causeLine)
	}
	if n := len([]rune(causeLine)); n != nudgeDeadLetterCauseMax {
		t.Fatalf("cause runes = %d, want %d", n, nudgeDeadLetterCauseMax)
	}
	if strings.Contains(b.body, "TAIL-MARKER") || strings.ContainsAny(b.subject, "\r\n\x1b") {
		t.Fatalf("cause not bounded or subject not one line: %q", b.subject)
	}
	if n := len([]rune(strings.TrimPrefix(b.subject, "[nudge dead-lettered] to mayor: "))); n != nudgeDeadLetterSubjectCauseMax {
		t.Fatalf("subject cause runes = %d, want %d", n, nudgeDeadLetterSubjectCauseMax)
	}
}
