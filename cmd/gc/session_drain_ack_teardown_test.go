package main

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// drainAckTeardownFixture is the ga-x99xh0 replay shape: a max-2 pool whose two
// seats each hold one routed in_progress bead.
type drainAckTeardownFixture struct {
	now      time.Time
	cityDir  string
	cfg      *config.City
	store    beads.Store
	sp       *runtime.Fake
	rec      *events.Fake
	dops     *fakeDrainOps
	dt       *drainTracker
	clk      *clock.Fake
	stderr   bytes.Buffer
	sessions []beads.Bead
	work     []beads.Bead
}

const drainAckTeardownTemplate = "repo/worker"

func newDrainAckTeardownFixture(t *testing.T) *drainAckTeardownFixture {
	t.Helper()
	return newDrainAckTeardownFixtureSized(t, 2, 2)
}

// newDrainAckTeardownFixtureSized is the fixture with max_active_sessions =
// maxActive and `seats` seats, each holding one routed in_progress bead.
func newDrainAckTeardownFixtureSized(t *testing.T, maxActive, seats int) *drainAckTeardownFixture {
	t.Helper()
	now := time.Date(2026, 9, 25, 23, 19, 33, 0, time.UTC)
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "worker")
	f := &drainAckTeardownFixture{
		now:     now,
		cityDir: cityDir,
		cfg: &config.City{
			Workspace: config.Workspace{Name: "trace-town"},
			Session:   config.SessionConfig{Provider: "fake"},
			Agents: []config.Agent{{
				Name:              "worker",
				Dir:               "repo",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(maxActive),
			}},
		},
		store: beads.NewMemStore(),
		sp:    runtime.NewFake(),
		rec:   events.NewFake(),
		dops:  newFakeDrainOps(),
		dt:    newDrainTracker(),
		clk:   &clock.Fake{Time: now},
	}
	inProgress := "in_progress"
	for slot := 1; slot <= seats; slot++ {
		work := createRoutedReadyBeadForReplacement(t, f.store, "platform leg")
		seat := createCanonicalPoolSession(t, f.store, &f.cfg.Agents[0], now, slot)
		setPoolSessionActive(t, f.store, seat.ID)
		seat = mustGetBead(t, f.store, seat.ID)
		name := seat.Metadata["session_name"]
		if err := f.sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("start seat %s runtime: %v", name, err)
		}
		assignee := seat.ID
		if err := f.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			t.Fatalf("assign %s to %s: %v", work.ID, seat.ID, err)
		}
		f.sessions = append(f.sessions, seat)
		f.work = append(f.work, mustGetBead(t, f.store, work.ID))
	}
	return f
}

// tick runs one reconcile pass over the fixture's current session beads.
func (f *drainAckTeardownFixture) tick(t *testing.T, dops drainOps) {
	t.Helper()
	ds := buildDesiredState("trace-town", f.cityDir, f.clk.Now(), f.cfg, f.sp, f.store, io.Discard)
	current := make([]beads.Bead, 0, len(f.sessions))
	for _, s := range f.sessions {
		current = append(current, mustGetBead(t, f.store, s.ID))
	}
	reconcileSessionBeads(
		context.Background(), current, ds.State, map[string]bool{drainAckTeardownTemplate: true},
		f.cfg, f.sp, f.store, dops, nil, nil, f.dt, ds.PoolDesiredCounts, false, nil, "trace-town",
		nil, f.clk, f.rec, 0, 0, io.Discard, &f.stderr,
	)
}

// tickSummary is the template tick summary the trace would record for the pool
// template, built from the fixture's current open seats and the given pool demand.
func (f *drainAckTeardownFixture) tickSummary(t *testing.T, poolDesired map[string]int) (templateTickSummary, bool) {
	t.Helper()
	snap, err := loadSessionBeadSnapshot(f.store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	_, summaries := buildTemplateTickSummaries(f.cfg, snap.OpenInfos(), nil, poolDesired, nil, nil)
	sum, ok := summaries[drainAckTeardownTemplate]
	return sum, ok
}

// drainAckedEventCount counts SessionDrainAckedWithAssignedWork events recorded.
func drainAckedEventCount(rec *events.Fake) int {
	n := 0
	for _, ev := range rec.Events {
		if ev.Type == events.SessionDrainAckedWithAssignedWork {
			n++
		}
	}
	return n
}

// openSeatCount is the template's open_count as the tick-summary trace reads it:
// open session beads whose normalized template is the pool's.
func (f *drainAckTeardownFixture) openSeatCount(t *testing.T) int {
	t.Helper()
	snap, err := loadSessionBeadSnapshot(f.store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	n := 0
	for _, info := range snap.OpenInfos() {
		if normalizedSessionTemplateInfo(info, f.cfg) == drainAckTeardownTemplate {
			n++
		}
	}
	return n
}

// TestDrainAckWithAssignedWork_PoolSeatsTornDownAndReplaced replays the Friday
// 2026-09-25 11:19 PM sequence behind ga-x99xh0: a max-2 pool, both seats
// assigned an in_progress bead, both told to drain (an account move) and both
// acknowledging while still assigned. Before the fix each seat was put to sleep
// holding its bead, the assigned-work wake resumed it on the old account, and
// the pool read open_count 2 / desired 2 for 178 ticks while serving nothing.
//
// The contract now: the ack tears the seat down AND releases its bead in the
// same act — assignee cleared, status back to open, gc.routed_to kept — so the
// next tick reads open_count 0 and desires two fresh seats for the two beads.
// The drain_acked_with_assigned_work event still fires: it is the observation,
// recorded before the release acts on it.
func TestDrainAckWithAssignedWork_PoolSeatsTornDownAndReplaced(t *testing.T) {
	f := newDrainAckTeardownFixture(t)
	if got := f.openSeatCount(t); got != 2 {
		t.Fatalf("precondition: open_count = %d, want 2", got)
	}
	for _, s := range f.sessions {
		if err := f.dops.setDrainAck(s.Metadata["session_name"]); err != nil {
			t.Fatalf("setDrainAck(%s): %v", s.Metadata["session_name"], err)
		}
	}

	// Tick 1: live runtimes + agent drain-acks -> stop-pending, async stop queued.
	f.tick(t, f.dops)
	for _, s := range f.sessions {
		waitForProviderStopped(t, f.sp, s.Metadata["session_name"])
	}
	// Tick 2: runtimes gone -> finalize the acks.
	f.tick(t, f.dops)

	if acked := drainAckedEventCount(f.rec); acked != 2 {
		t.Fatalf("%s events = %d, want 2 (the observation must still be recorded)", events.SessionDrainAckedWithAssignedWork, acked)
	}
	logged := f.stderr.String()
	for _, s := range f.sessions {
		got := mustGetBead(t, f.store, s.ID)
		if got.Status != "closed" {
			t.Errorf("seat %s: status=%q state=%q sleep_reason=%q, want closed — a seat that acked a drain while assigned must be torn down, not retained",
				s.ID, got.Status, got.Metadata["state"], got.Metadata["sleep_reason"])
		}
		if want := sessionpkg.CanonicalCloseReason(drainAckTeardownCloseReason); got.Metadata["close_reason"] != want {
			t.Errorf("seat %s close_reason = %q, want %q", s.ID, got.Metadata["close_reason"], want)
		}
	}
	for _, w := range f.work {
		got := mustGetBead(t, f.store, w.ID)
		if got.Assignee != "" {
			t.Errorf("work %s assignee = %q, want cleared so the pool can re-draw it", w.ID, got.Assignee)
		}
		if got.Status != "open" {
			t.Errorf("work %s status = %q, want open", w.ID, got.Status)
		}
		if route := got.Metadata[beadmeta.RoutedToMetadataKey]; route != drainAckTeardownTemplate {
			t.Errorf("work %s gc.routed_to = %q, want %q kept", w.ID, route, drainAckTeardownTemplate)
		}
		if !drainAckTeardownAudited(logged, w.ID) {
			t.Errorf("no release audit line for %s with path=%s; stderr=%s", w.ID, drainAckTeardownReleasePath, logged)
		}
	}

	// The next tick: nothing is retained, and the two released beads are demand
	// for two fresh seats. The summary is read BEFORE the desired-state build,
	// because that build mints the replacement seats' beads.
	if got := f.openSeatCount(t); got != 0 {
		t.Fatalf("open_count after the drain acks = %d, want 0", got)
	}
	sum, ok := f.tickSummary(t, map[string]int{drainAckTeardownTemplate: 2})
	if !ok {
		t.Fatalf("no tick summary for %s", drainAckTeardownTemplate)
	}
	if sum.reason != TraceReasonNoMatchingSession || sum.fields["open_count"] != 0 {
		t.Errorf("tick summary after teardown = reason %s open_count %v, want %s / 0", sum.reason, sum.fields["open_count"], TraceReasonNoMatchingSession)
	}
	if held, present := sum.fields["retained_for"]; present {
		t.Errorf("tick summary after teardown carries retained_for=%v, want it omitted with no open seats", held)
	}
	ds := buildDesiredState("trace-town", f.cityDir, f.clk.Now(), f.cfg, f.sp, f.store, io.Discard)
	desired := 0
	for _, tp := range ds.State {
		if tp.TemplateName == drainAckTeardownTemplate {
			desired++
		}
	}
	if desired != 2 {
		t.Fatalf("desired %s seats after the drain acks = %d, want 2 (a replacement per released bead)", drainAckTeardownTemplate, desired)
	}
	// The replacements are NEW seats, not the torn-down ones reopened.
	snap, err := loadSessionBeadSnapshot(f.store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	for _, info := range snap.OpenInfos() {
		for _, old := range f.sessions {
			if info.ID == old.ID {
				t.Fatalf("torn-down seat %s is open again", old.ID)
			}
		}
	}
}

// drainAckTeardownAudited reports whether the release audit line for id names
// the drain-ack teardown as its path.
func drainAckTeardownAudited(logged, id string) bool {
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "RELEASED work "+id+":") && strings.Contains(line, "path="+drainAckTeardownReleasePath) {
			return true
		}
	}
	return false
}

// TestDrainAckTeardown_TickSummaryNamesRetainedSeats pins the trace wiring in the
// incident's retained shape: two open seats asleep/idle, each holding a bead, is
// what the tick summary used to call a bare "retained". It must now say what the
// seats are retained FOR.
func TestDrainAckTeardown_TickSummaryNamesRetainedSeats(t *testing.T) {
	f := newDrainAckTeardownFixture(t)
	for _, s := range f.sessions {
		for k, v := range map[string]string{"state": "asleep", "sleep_reason": "idle"} {
			if err := f.store.SetMetadata(s.ID, k, v); err != nil {
				t.Fatalf("SetMetadata(%s=%s): %v", k, v, err)
			}
		}
	}
	sum, ok := f.tickSummary(t, map[string]int{drainAckTeardownTemplate: 2})
	if !ok {
		t.Fatalf("no tick summary for %s", drainAckTeardownTemplate)
	}
	if sum.reason != TraceReasonRetained || sum.fields["open_count"] != 2 {
		t.Fatalf("tick summary = reason %s open_count %v, want retained / 2", sum.reason, sum.fields["open_count"])
	}
	if got, want := sum.fields["retained_for"], map[string]int{"asleep:idle": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("retained_for = %#v, want %#v", got, want)
	}
}

// TestDrainAckTeardown_RegressionGuardWorkingSeatStillCounted is a regression
// guard, not a test of the teardown: it never drains, so it never reaches the new
// code. It pins that a genuinely working seat (runtime alive, bead in_progress, no
// drain) is untouched by the ticks around it — open, assigned, and counted.
func TestDrainAckTeardown_RegressionGuardWorkingSeatStillCounted(t *testing.T) {
	f := newDrainAckTeardownFixture(t)
	f.tick(t, newFakeDrainOps())
	f.tick(t, newFakeDrainOps())

	for i, s := range f.sessions {
		got := mustGetBead(t, f.store, s.ID)
		if got.Status == "closed" {
			t.Errorf("working seat %s was closed; close_reason=%q", s.ID, got.Metadata["close_reason"])
		}
		w := mustGetBead(t, f.store, f.work[i].ID)
		if w.Assignee != s.ID || w.Status != "in_progress" {
			t.Errorf("working seat's bead %s: assignee=%q status=%q, want %q/in_progress", w.ID, w.Assignee, w.Status, s.ID)
		}
	}
	if got := f.openSeatCount(t); got != 2 {
		t.Fatalf("open_count with two working seats = %d, want 2", got)
	}
	for _, ev := range f.rec.Events {
		if ev.Type == events.SessionDrainAckedWithAssignedWork {
			t.Fatalf("unexpected %s for a seat that never drained", ev.Type)
		}
	}
}

// TestDrainAckTeardown_CertParkedWorkIsRetained pins the one assigned-work shape
// the teardown must not release: a bead parked on a certification wait
// (ga-mzovhi). Its owner is asleep by design and the cert-landing-patrol wakes
// it by the assignment, so the seat keeps the bead and stays open.
func TestDrainAckTeardown_CertParkedWorkIsRetained(t *testing.T) {
	f := newDrainAckTeardownFixture(t)
	f.sessions = f.sessions[:1]
	parked := f.work[0]
	if err := f.store.Update(parked.ID, beads.UpdateOpts{Labels: []string{beadmeta.CertWaitHoldLabel}}); err != nil {
		t.Fatalf("park %s: %v", parked.ID, err)
	}
	name := f.sessions[0].Metadata["session_name"]
	if err := f.dops.setDrainAck(name); err != nil {
		t.Fatalf("setDrainAck(%s): %v", name, err)
	}
	f.tick(t, f.dops)
	waitForProviderStopped(t, f.sp, name)
	f.tick(t, f.dops)

	got := mustGetBead(t, f.store, f.sessions[0].ID)
	if got.Status == "closed" {
		t.Fatalf("seat holding cert-parked work was torn down; close_reason=%q", got.Metadata["close_reason"])
	}
	w := mustGetBead(t, f.store, parked.ID)
	if w.Assignee != f.sessions[0].ID {
		t.Fatalf("cert-parked bead assignee = %q, want %q (the park's owner must be kept)", w.Assignee, f.sessions[0].ID)
	}
	if n := drainAckedEventCount(f.rec); n != 1 {
		t.Fatalf("%s events = %d, want exactly 1", events.SessionDrainAckedWithAssignedWork, n)
	}
	if want := "seat kept: cert_parked_work:" + parked.ID; !strings.Contains(f.stderr.String(), want) {
		t.Fatalf("stderr does not name the retention reason %q: %s", want, f.stderr.String())
	}
}

// staleLabelCacheStore serves a cached Get for staleID that predates its labels,
// while its LIVE handle reads the backing store — the CachingStore shape in which
// a cert hold exists only in the backing store.
type staleLabelCacheStore struct {
	beads.Store
	staleID string
}

func (s staleLabelCacheStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err == nil && id == s.staleID {
		b.Labels = nil
	}
	return b, err
}

func (s staleLabelCacheStore) Handles() beads.StoreHandles {
	backing := beads.HandlesFor(s.Store)
	return beads.StoreHandles{Cached: backing.Cached, Live: backing.Live, Writer: s}
}

// TestDrainAckTeardown_CertParkReadsLiveLabelsNotCache: a cached row without the
// hold label must not license the release when the live row carries it.
func TestDrainAckTeardown_CertParkReadsLiveLabelsNotCache(t *testing.T) {
	mem := beads.NewMemStore()
	seat, err := mem.Create(beads.Bead{
		Title:    "worker session",
		Type:     sessionBeadType,
		Status:   "open",
		Metadata: map[string]string{"session_name": "worker-1", "pool_managed": "true", "pool_slot": "1"},
	})
	if err != nil {
		t.Fatalf("create seat: %v", err)
	}
	work, err := mem.Create(beads.Bead{
		Title:    "parked PR carrier",
		Type:     "task",
		Assignee: seat.ID,
		Labels:   []string{beadmeta.CertWaitHoldLabel},
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress := "in_progress"
	if err := mem.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set work in_progress: %v", err)
	}
	store := staleLabelCacheStore{Store: mem, staleID: work.ID}
	if cached, _ := store.Get(work.ID); beadmeta.CertParkSuppressesAssignedWake(cached.Labels) {
		t.Fatal("precondition: the cached row must NOT show the hold, or the test proves nothing")
	}

	var stderr bytes.Buffer
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	outcome := tearDownDrainAckedPoolSeat("", nil, store, nil, seedSessionInfo(mustGetBead(t, mem, seat.ID)), now, &stderr)
	if outcome.closed || outcome.retainedFor != "cert_parked_work:"+work.ID {
		t.Fatalf("outcome = %+v, want retained for cert_parked_work:%s (the live row carries the hold); stderr=%s", outcome, work.ID, stderr.String())
	}
	if got := mustGetBead(t, mem, work.ID); got.Assignee != seat.ID || got.Status != "in_progress" {
		t.Fatalf("parked work assignee=%q status=%q, want %q/in_progress", got.Assignee, got.Status, seat.ID)
	}
	if got := mustGetBead(t, mem, seat.ID); got.Status == "closed" {
		t.Fatal("the park's owner was closed on a stale cached row")
	}
}

// TestDrainAckTeardown_NonPoolSeatsKeptWithReason is the control for seats the
// teardown may not mutate: a plain (non-pool) seat and a configured named seat
// that drain-ack holding work stay open and keep the work, the observation fires
// exactly once, and the log names why the seat was kept.
func TestDrainAckTeardown_NonPoolSeatsKeptWithReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
	}{
		{name: "plain seat", meta: nil},
		{name: "named seat", meta: map[string]string{namedSessionMetadataKey: "true", "pool_managed": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", true)
			fake := events.NewFake()
			env.rec = fake
			seat := env.createSessionBead("worker", "worker")
			env.markSessionActive(&seat)
			if tc.meta != nil {
				env.setSessionMetadata(&seat, tc.meta)
			}
			work, err := env.store.Create(beads.Bead{Title: "task", Type: "task", Status: "in_progress", Assignee: seat.ID})
			if err != nil {
				t.Fatalf("Create(work): %v", err)
			}
			dops := newFakeDrainOps()
			if err := dops.setDrainAck("worker"); err != nil {
				t.Fatalf("setDrainAck: %v", err)
			}
			cfgNames := map[string]bool{"worker": true}
			reconcileSessionBeads(
				context.Background(), []beads.Bead{seat}, env.desiredState, cfgNames, env.cfg, env.sp,
				env.store, dops, nil, nil, env.dt, nil, false, nil, "",
				nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
			)
			got := env.reconcileStopPendingToTerminal(t, env.sp, seat, dops, cfgNames)
			if got.Status == "closed" {
				t.Fatalf("%s was closed; a non-pool seat must be kept", tc.name)
			}
			if w := mustGetBead(t, env.store, work.ID); w.Assignee != seat.ID {
				t.Fatalf("work assignee = %q, want %q kept", w.Assignee, seat.ID)
			}
			if n := drainAckedEventCount(fake); n != 1 {
				t.Fatalf("%s events = %d, want exactly 1", events.SessionDrainAckedWithAssignedWork, n)
			}
			if !strings.Contains(env.stderr.String(), "seat kept: ") {
				t.Fatalf("stderr does not name why the seat was kept: %s", env.stderr.String())
			}
		})
	}
}

// TestDrainAckTeardown_SingletonPoolIdentityKept: an agent with
// max_active_sessions = 1 and no namepool runs a canonical singleton pool — its
// seat is pool-managed but keeps one stable configured identity, bead and
// conversation (config.Agent.UsesCanonicalSingletonPoolIdentity). A drain ack
// while it holds work must not tear it down: it stays open, keeps its bead, the
// observation fires exactly once, and the log names singleton_identity.
func TestDrainAckTeardown_SingletonPoolIdentityKept(t *testing.T) {
	f := newDrainAckTeardownFixtureSized(t, 1, 1)
	if !f.cfg.Agents[0].UsesCanonicalSingletonPoolIdentity() {
		t.Fatal("precondition: the fixture agent must use a canonical singleton identity")
	}
	seat := f.sessions[0]
	name := seat.Metadata["session_name"]
	if err := f.dops.setDrainAck(name); err != nil {
		t.Fatalf("setDrainAck(%s): %v", name, err)
	}
	f.tick(t, f.dops)
	waitForProviderStopped(t, f.sp, name)
	f.tick(t, f.dops)

	if got := mustGetBead(t, f.store, seat.ID); got.Status == "closed" {
		t.Fatalf("singleton seat was torn down; close_reason=%q", got.Metadata["close_reason"])
	}
	if w := mustGetBead(t, f.store, f.work[0].ID); w.Assignee != seat.ID {
		t.Fatalf("singleton seat's bead assignee = %q, want %q kept", w.Assignee, seat.ID)
	}
	if n := drainAckedEventCount(f.rec); n != 1 {
		t.Fatalf("%s events = %d, want exactly 1", events.SessionDrainAckedWithAssignedWork, n)
	}
	if want := "seat kept: " + drainAckRetainedSingletonIdentity; !strings.Contains(f.stderr.String(), want) {
		t.Fatalf("stderr does not name %q: %s", want, f.stderr.String())
	}
}
