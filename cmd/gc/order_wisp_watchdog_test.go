package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderWispWatchdogTestCity is foreignIdentityTestCity plus a configured named
// session, so one fixture carries every owner shape the watchdog judges: a pool
// agent with numbered and namepool instances, a binding this city mints, and a
// named seat whose claims outlive its sessions.
func orderWispWatchdogTestCity(t *testing.T) *config.City {
	t.Helper()
	cfg := foreignIdentityTestCity(t)
	cfg.Agents = append(cfg.Agents, config.Agent{Name: "lead", Dir: "repo"})
	cfg.NamedSessions = []config.NamedSession{{Template: "lead", Dir: "repo", Mode: "on_demand"}}
	return cfg
}

// writeForbiddingStore fails every write. The order wisp watchdog is report
// only, so a pass over this store must attempt none: each attempt is recorded,
// fails the test, and returns an error so the caller cannot carry on as if it
// had landed. It embeds only the beads.Store interface, so no optional write
// interface (CreateWithStorage, ApplyGraphPlanWithStorage) is promoted through
// it: a caller that asserts one falls back to a Store method, and every Store
// method that writes is overridden below.
type writeForbiddingStore struct {
	beads.Store
	t *testing.T

	mu     sync.Mutex
	writes []string
}

func newWriteForbiddingStore(t *testing.T, store beads.Store) *writeForbiddingStore {
	return &writeForbiddingStore{Store: store, t: t}
}

func (s *writeForbiddingStore) forbid(op string) error {
	s.mu.Lock()
	s.writes = append(s.writes, op)
	s.mu.Unlock()
	s.t.Errorf("store write attempted: %s", op)
	return fmt.Errorf("write forbidden in this test: %s", op)
}

func (s *writeForbiddingStore) attempted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

func (s *writeForbiddingStore) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, s.forbid("Create")
}

func (s *writeForbiddingStore) Update(id string, _ beads.UpdateOpts) error {
	return s.forbid("Update " + id)
}

func (s *writeForbiddingStore) Close(id string) error { return s.forbid("Close " + id) }

func (s *writeForbiddingStore) Reopen(id string) error { return s.forbid("Reopen " + id) }

func (s *writeForbiddingStore) CloseAll(ids []string, _ map[string]string) (int, error) {
	return 0, s.forbid("CloseAll " + strings.Join(ids, ","))
}

func (s *writeForbiddingStore) SetMetadata(id, key, _ string) error {
	return s.forbid("SetMetadata " + id + " " + key)
}

func (s *writeForbiddingStore) SetMetadataBatch(id string, _ map[string]string) error {
	return s.forbid("SetMetadataBatch " + id)
}

func (s *writeForbiddingStore) SetLocalString(id, key, _ string) error {
	return s.forbid("SetLocalString " + id + " " + key)
}

func (s *writeForbiddingStore) Tx(commitMsg string, _ func(beads.Tx) error) error {
	return s.forbid("Tx " + commitMsg)
}

func (s *writeForbiddingStore) Delete(id string) error { return s.forbid("Delete " + id) }

func (s *writeForbiddingStore) DepAdd(issueID, dependsOnID, _ string) error {
	return s.forbid("DepAdd " + issueID + " " + dependsOnID)
}

func (s *writeForbiddingStore) DepRemove(issueID, dependsOnID string) error {
	return s.forbid("DepRemove " + issueID + " " + dependsOnID)
}

// seedOrderWisp creates a root-only order-run wisp for order and applies the
// claim shape the test needs. status "" leaves the bead open.
func seedOrderWisp(t *testing.T, store beads.Store, order, status, assignee string, metadata map[string]string) beads.Bead {
	t.Helper()
	meta := map[string]string{"gc.kind": "wisp"}
	for k, v := range metadata {
		meta[k] = v
	}
	wisp, err := store.Create(beads.Bead{
		Title:    "order wisp " + order,
		Type:     "task",
		Labels:   []string{"order-run:" + order},
		Assignee: assignee,
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Create(order wisp): %v", err)
	}
	if status != "" {
		if err := store.Update(wisp.ID, beads.UpdateOpts{Status: stringPtr(status)}); err != nil {
			t.Fatalf("Update(order wisp status): %v", err)
		}
	}
	got, err := store.Get(wisp.ID)
	if err != nil {
		t.Fatalf("Get(order wisp): %v", err)
	}
	return got
}

// seedOrderWispSession creates a session bead carrying metadata, closed when
// open is false.
func seedOrderWispSession(t *testing.T, store beads.Store, open bool, metadata map[string]string) beads.Bead {
	t.Helper()
	sb, err := store.Create(beads.Bead{
		Title:    "session",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}
	if !open {
		if err := store.Close(sb.ID); err != nil {
			t.Fatalf("Close(session bead): %v", err)
		}
	}
	return sb
}

// seedOrderMolecule creates a molecule root for the digest order with one step
// carrying the given claim. status "" leaves the step open.
func seedOrderMolecule(t *testing.T, store beads.Store, stepAssignee, stepStatus string) (beads.Bead, beads.Bead) {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: "mol-digest", Type: "molecule", Labels: []string{"order-run:digest"}})
	if err != nil {
		t.Fatalf("Create(molecule root): %v", err)
	}
	step, err := store.Create(beads.Bead{Title: "step", ParentID: root.ID, Assignee: stepAssignee})
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}
	if stepStatus != "" {
		if err := store.Update(step.ID, beads.UpdateOpts{Status: stringPtr(stepStatus)}); err != nil {
			t.Fatalf("Update(step status): %v", err)
		}
	}
	return root, step
}

// reportOrderWispsForTest runs one pass over runs for the named orders at the
// default cutoff, judging holders against sessions.
func reportOrderWispsForTest(t *testing.T, cfg *config.City, runs, sessions beads.Store, now time.Time, orderNames ...string) orderWispWatchdogResult {
	t.Helper()
	watch := make([]orderWispWatchdogOrder, 0, len(orderNames))
	for _, name := range orderNames {
		watch = append(watch, orderWispWatchdogOrder{order: orders.Order{Name: name}, scoped: name, staleAfter: orders.DefaultRunStaleAfter})
	}
	result, err := reportStaleOrderWisps(
		[]orderWispWatchdogLeg{{store: runs, orders: watch}},
		now,
		newOrderWispOwnerResolver(cfg, t.TempDir(), sessions),
	)
	if err != nil {
		t.Fatalf("reportStaleOrderWisps: %v", err)
	}
	return result
}

// staleRunByID returns the reported run with rootID, failing when the pass did
// not report it.
func staleRunByID(t *testing.T, result orderWispWatchdogResult, rootID string) orderWispStaleRun {
	t.Helper()
	for _, run := range result.stale {
		if run.rootID == rootID {
			return run
		}
	}
	t.Fatalf("stale runs = %+v, want one rooted at %s", result.stale, rootID)
	return orderWispStaleRun{}
}

func requireBeadStatus(t *testing.T, store beads.Store, id, want string) beads.Bead {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Status != want {
		t.Fatalf("%s status = %q, want %q", id, got.Status, want)
	}
	return got
}

func requireVerdict(t *testing.T, run orderWispStaleRun, state orderWispOwnerState, reason string) {
	t.Helper()
	if run.verdict.State != state || !strings.Contains(run.verdict.Reason, reason) {
		t.Fatalf("run %s verdict = %s, want %s with reason containing %q", run.rootID, run.verdict, state, reason)
	}
}

// TestOrderWispWatchdogWritesNothing is condition (a) of the report-only
// scope: no code path from the watchdog writes to any store. Every holder
// shape the watchdog distinguishes is stale here, the session store and the
// run store both fail on write, and the pass must still report every run.
// Put any close, reopen or metadata write back on the path and this fails.
func TestOrderWispWatchdogWritesNothing(t *testing.T) {
	runBacking := beads.NewMemStore()
	runBacking.IDPrefix = "rg"
	sessionBacking := beads.NewMemStore()
	live := seedOrderWispSession(t, sessionBacking, true, map[string]string{"session_name": "worker-sess-live"})
	dead := seedOrderWispSession(t, sessionBacking, false, map[string]string{"session_name": "worker-sess-dead"})
	held := seedOrderWisp(t, runBacking, "digest", "in_progress", "", map[string]string{"gc.session_id": live.ID})
	gone := seedOrderWisp(t, runBacking, "digest", "in_progress", "", map[string]string{"gc.session_id": dead.ID})
	foreign := seedOrderWisp(t, runBacking, "digest", "in_progress", "repo/dalinar", nil)
	queued := seedOrderWisp(t, runBacking, "digest", "", "", nil)
	molRoot, molStep := seedOrderMolecule(t, runBacking, "", "")
	runs := newWriteForbiddingStore(t, runBacking)
	sessions := newWriteForbiddingStore(t, sessionBacking)

	result := reportOrderWispsForTest(t, orderWispWatchdogTestCity(t), runs, sessions, held.CreatedAt.Add(7*time.Hour), "digest")

	if got := append(runs.attempted(), sessions.attempted()...); len(got) != 0 {
		t.Fatalf("the watchdog attempted store writes %v; it must write nothing", got)
	}
	requireVerdict(t, staleRunByID(t, result, held.ID), orderWispHeld, "its session is live")
	requireVerdict(t, staleRunByID(t, result, gone.ID), orderWispHolderGone, "closed")
	requireVerdict(t, staleRunByID(t, result, foreign.ID), orderWispUnobservable, "absent_from_roster")
	requireVerdict(t, staleRunByID(t, result, queued.ID), orderWispUnclaimed, "no open member")
	requireVerdict(t, staleRunByID(t, result, molRoot.ID), orderWispUnclaimed, "no open member")
	requireBeadStatus(t, runBacking, gone.ID, "in_progress")
	requireBeadStatus(t, runBacking, molStep.ID, "open")
}

// TestOrderWispWatchdogReportsEveryStaleRunWithItsVerdict is condition (b):
// every stale run is reported, held ones included, each with its holder, its
// verdict and the reason. Eight unclaimed runs are more than any sample would
// show, so a report that samples fails here. (Runs under the cutoff are not
// stale: TestOrderWispWatchdogLeavesYoungRunsUnreported.)
func TestOrderWispWatchdogReportsEveryStaleRunWithItsVerdict(t *testing.T) {
	store := beads.NewMemStore()
	sb := seedOrderWispSession(t, store, true, map[string]string{"session_name": "worker-sess-live"})
	held := seedOrderWisp(t, store, "digest", "in_progress", "", map[string]string{"gc.session_id": sb.ID})
	gone := seedOrderWisp(t, store, "digest", "in_progress", "repo/worker-1", nil)
	var queued []beads.Bead
	for i := 0; i < 8; i++ {
		queued = append(queued, seedOrderWisp(t, store, "digest", "", "", nil))
	}
	now := held.CreatedAt.Add(7 * time.Hour)

	result := reportOrderWispsForTest(t, orderWispWatchdogTestCity(t), store, store, now, "digest")

	if want := 2 + len(queued); len(result.stale) != want {
		t.Fatalf("stale runs = %d, want %d: every stale run, held ones included", len(result.stale), want)
	}
	lines := result.reportLines(now)
	report := strings.Join(lines, "\n")
	if len(lines) != 1+len(result.stale) {
		t.Fatalf("report = %q, want a header and one line per stale run", report)
	}
	if !strings.Contains(lines[0], "10 stale order run(s)") || !strings.Contains(lines[0], "1 held, 0 unobservable, 1 holder-gone, 8 unclaimed") || !strings.Contains(lines[0], "nothing is closed") {
		t.Fatalf("header = %q, want per-verdict counts and the report-only statement", lines[0])
	}
	for _, want := range []string{
		held.ID, `holder "` + sb.ID + `"`, "held: its session is live",
		gone.ID, `holder "repo/worker-1"`, "holder-gone: agent of this city",
		"run_stale_after=6h",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report = %q, want it to contain %q", report, want)
		}
	}
	for _, q := range queued {
		if !strings.Contains(report, q.ID) {
			t.Fatalf("report = %q, want it to name unclaimed run %s", report, q.ID)
		}
	}
	if !strings.Contains(report, `holder "unassigned", unclaimed: no open member of the run is claimed`) {
		t.Fatalf("report = %q, want unclaimed runs reported with their verdict", report)
	}
}

// TestOrderWispWatchdogReportsLiveHolderAsHeld is the liveness mutant
// (ga-puy7n0 test a). Every case is a stale root-only wisp whose claim names
// this city's own identity, so the locality step would call it holder-gone:
// only the liveness step reports it held. Delete the `if live` return in
// refVerdict and every case reports holder-gone.
func TestOrderWispWatchdogReportsLiveHolderAsHeld(t *testing.T) {
	cases := []struct {
		name     string
		assignee string
		session  map[string]string
		wispMeta func(sessionID string) map[string]string
	}{
		{
			name:     "exact gc.session_id",
			session:  map[string]string{"session_name": "worker-sess-7"},
			wispMeta: func(id string) map[string]string { return map[string]string{"gc.session_id": id} },
		},
		{
			name:     "runtime session name",
			assignee: "worker-sess-7",
			session:  map[string]string{"session_name": "worker-sess-7"},
		},
		{
			name:     "pool instance identity",
			assignee: "repo/worker-1",
			session:  map[string]string{"session_name": "repo--worker-1", "alias": "repo/worker-1"},
		},
		{
			name:     "namepool alias",
			assignee: "repo/furiosa",
			session:  map[string]string{"session_name": "repo--furiosa", "alias": "repo/furiosa"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sb := seedOrderWispSession(t, store, true, tc.session)
			var meta map[string]string
			if tc.wispMeta != nil {
				meta = tc.wispMeta(sb.ID)
			}
			wisp := seedOrderWisp(t, store, "digest", "in_progress", tc.assignee, meta)

			result := reportOrderWispsForTest(t, orderWispWatchdogTestCity(t), store, store, wisp.CreatedAt.Add(7*time.Hour), "digest")

			requireVerdict(t, staleRunByID(t, result, wisp.ID), orderWispHeld, "its session is live")
		})
	}
}

// TestOrderWispWatchdogReportsForeignClaimUnobservable is the locality mutant
// (woodhouse amendment A1). No case has a live session, so the liveness step
// lets every one through: only the locality step keeps another city's claim
// from being reported as this city's gone holder. Make localityVerdict return
// holder-gone unconditionally and every case fails.
func TestOrderWispWatchdogReportsForeignClaimUnobservable(t *testing.T) {
	cases := []struct {
		name       string
		assignee   string
		meta       map[string]string
		wantReason string
	}{
		{name: "identity absent from the roster", assignee: "repo/dalinar", wantReason: "absent_from_roster"},
		{name: "binding this city does not mint", assignee: "repo/review.omp-1", wantReason: "foreign_binding"},
		{name: "session name no session bead carries", assignee: "their__dog-1-pool", wantReason: "names no session"},
		{name: "session bead ID from a store this city does not own", assignee: "we-wisp-126vyfx", wantReason: "foreign_store_prefix"},
		{name: "session bead ID this city does not hold", meta: map[string]string{"gc.session_id": "hq-wisp-elsewhere"}, wantReason: "no session bead with that ID"},
		{
			// A local-looking assignee beside a foreign session back-reference
			// is the shared-pack collision shape: the identity is byte-identical
			// across cities, and only the session bead tells them apart.
			name:       "local-looking assignee with a foreign session ID",
			assignee:   "repo/worker-1",
			meta:       map[string]string{"gc.session_id": "hq-wisp-elsewhere"},
			wantReason: "no session bead with that ID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			wisp := seedOrderWisp(t, store, "digest", "in_progress", tc.assignee, tc.meta)

			result := reportOrderWispsForTest(t, orderWispWatchdogTestCity(t), store, store, wisp.CreatedAt.Add(7*time.Hour), "digest")

			requireVerdict(t, staleRunByID(t, result, wisp.ID), orderWispUnobservable, tc.wantReason)
		})
	}
}

// TestOrderWispWatchdogRunStoreSessionBeadIsNotLocality is condition (c): a
// session bead found in the swept rig store is not evidence that a claim is
// this city's. On a rig store shared with another city, that city's session
// beads sit there too, so a closed one must read unobservable, never
// holder-gone. An open one still proves the run is held. The control, the same
// closed session bead in this city's session store, is holder-gone. Let the
// locality step read the run store and the first two cases fail.
func TestOrderWispWatchdogRunStoreSessionBeadIsNotLocality(t *testing.T) {
	cases := []struct {
		name        string
		inRunStore  bool
		sessionOpen bool
		byName      bool
		wantState   orderWispOwnerState
		wantReason  string
	}{
		{name: "closed session bead in the rig store, by ID", inRunStore: true, wantState: orderWispUnobservable, wantReason: "no session bead with that ID in this city's session store"},
		{name: "closed session bead in the rig store, by name", inRunStore: true, byName: true, wantState: orderWispUnobservable, wantReason: "names no session in this city's session store"},
		{name: "open session bead in the rig store", inRunStore: true, sessionOpen: true, wantState: orderWispHeld, wantReason: "its session is live"},
		{name: "closed session bead in this city's session store", wantState: orderWispHolderGone, wantReason: "its session bead in this city's session store is closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := beads.NewMemStore()
			rig.IDPrefix = "rg"
			sessions := beads.NewMemStore()
			sessions.IDPrefix = "ct"
			home := sessions
			if tc.inRunStore {
				home = rig
			}
			sb := seedOrderWispSession(t, home, tc.sessionOpen, map[string]string{"session_name": "their__dog-1-pool"})
			assignee, meta := "", map[string]string{"gc.session_id": sb.ID}
			if tc.byName {
				assignee, meta = "their__dog-1-pool", nil
			}
			wisp := seedOrderWisp(t, rig, "digest", "in_progress", assignee, meta)

			result := reportOrderWispsForTest(t, orderWispWatchdogTestCity(t), rig, sessions, wisp.CreatedAt.Add(7*time.Hour), "digest")

			requireVerdict(t, staleRunByID(t, result, wisp.ID), tc.wantState, tc.wantReason)
		})
	}
}

// TestOrderWispWatchdogReportsGoneLocalHolderAndLeavesTheGateHeld is
// ga-puy7n0 test b under the report-only scope: a stale root-only wisp whose
// claim names this city's own session or agent, none of it live, is reported
// holder-gone and left exactly as it was, so it still gates its order.
func TestOrderWispWatchdogReportsGoneLocalHolderAndLeavesTheGateHeld(t *testing.T) {
	cases := []struct {
		name       string
		assignee   string
		session    map[string]string
		wispMeta   func(sessionID string) map[string]string
		wantReason string
	}{
		{name: "configured pool instance with no session", assignee: "repo/worker-1", wantReason: "agent of this city"},
		{
			name:       "runtime session name of a closed session",
			assignee:   "worker-sess-dead",
			session:    map[string]string{"session_name": "worker-sess-dead"},
			wantReason: "its session in this city has ended",
		},
		{
			name:       "gc.session_id of a closed session",
			session:    map[string]string{"session_name": "worker-sess-gone"},
			wispMeta:   func(id string) map[string]string { return map[string]string{"gc.session_id": id} },
			wantReason: "is closed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			var meta map[string]string
			if tc.session != nil {
				sb := seedOrderWispSession(t, store, false, tc.session)
				if tc.wispMeta != nil {
					meta = tc.wispMeta(sb.ID)
				}
			}
			wisp := seedOrderWisp(t, store, "digest", "in_progress", tc.assignee, meta)

			result := reportOrderWispsForTest(t, orderWispWatchdogTestCity(t), store, store, wisp.CreatedAt.Add(7*time.Hour), "digest")

			requireVerdict(t, staleRunByID(t, result, wisp.ID), orderWispHolderGone, tc.wantReason)
			got := requireBeadStatus(t, store, wisp.ID, "in_progress")
			if got.Assignee != wisp.Assignee || len(got.Metadata) != len(wisp.Metadata) {
				t.Fatalf("wisp after the pass = %+v, want it untouched (%+v)", got, wisp)
			}
			gate := &memoryOrderDispatcher{}
			if gated, err := gate.hasOpenWorkStrict(store, "digest"); err != nil || !gated {
				t.Fatalf("hasOpenWorkStrict after the pass = %v, %v; want true: a report-only pass leaves the gate held", gated, err)
			}
		})
	}
}

// TestOrderWispWatchdogLeavesYoungRunsUnreported is ga-puy7n0 test c: under
// the cutoff, neither a gone holder nor a live one is stale, and neither is
// reported.
func TestOrderWispWatchdogLeavesYoungRunsUnreported(t *testing.T) {
	store := beads.NewMemStore()
	seedOrderWispSession(t, store, true, map[string]string{"session_name": "worker-sess-live"})
	gone := seedOrderWisp(t, store, "digest", "in_progress", "repo/worker-1", nil)
	seedOrderWisp(t, store, "digest", "in_progress", "worker-sess-live", nil)

	result := reportOrderWispsForTest(t, orderWispWatchdogTestCity(t), store, store, gone.CreatedAt.Add(time.Hour), "digest")

	if len(result.stale) != 0 || result.reportLines(gone.CreatedAt.Add(time.Hour)) != nil {
		t.Fatalf("result = %+v, want nothing reported under the cutoff", result)
	}
}

// TestOrderWispWatchdogHonorsPerOrderStaleAfter is ga-puy7n0 test d: an order's
// run_stale_after sets its own report threshold, and an order that sets none
// gets the default.
func TestOrderWispWatchdogHonorsPerOrderStaleAfter(t *testing.T) {
	store := beads.NewMemStore()
	fast := seedOrderWisp(t, store, "fast", "in_progress", "repo/worker-1", nil)
	slow := seedOrderWisp(t, store, "slow", "in_progress", "repo/worker-2", nil)

	watch := orderWispWatchdogOrdersFor("", orderWispWatchdogTestCity(t), []orders.Order{
		{Name: "fast", Formula: "mol-fast", Trigger: "cooldown", Interval: "1h", RunStaleAfter: "2h"},
		{Name: "slow", Formula: "mol-slow", Trigger: "cooldown", Interval: "1h"},
		{Name: "probe", Exec: "true", Trigger: "cooldown", Interval: "1h"},
	})
	if len(watch) != 2 {
		t.Fatalf("watched orders = %+v, want fast and slow only (an exec order makes no wisp)", watch)
	}
	for _, o := range watch {
		want := orders.DefaultRunStaleAfter
		if o.scoped == "fast" {
			want = 2 * time.Hour
		}
		if o.staleAfter != want {
			t.Fatalf("%s staleAfter = %s, want %s", o.scoped, o.staleAfter, want)
		}
	}
	now := fast.CreatedAt.Add(3 * time.Hour)
	result, err := reportStaleOrderWisps(
		[]orderWispWatchdogLeg{{store: store, orders: watch}},
		now,
		newOrderWispOwnerResolver(orderWispWatchdogTestCity(t), t.TempDir(), store),
	)
	if err != nil {
		t.Fatalf("reportStaleOrderWisps: %v", err)
	}

	if len(result.stale) != 1 || result.stale[0].rootID != fast.ID {
		t.Fatalf("stale runs = %+v, want only %s (slow %s is under the 6h default)", result.stale, fast.ID, slow.ID)
	}
	if report := strings.Join(result.reportLines(now), "\n"); !strings.Contains(report, "run_stale_after=2h") {
		t.Fatalf("report = %q, want the order's own 2h threshold", report)
	}
}

// TestOrderWispWatchdogResolvesOwnersWithoutMaterializingSessions is woodhouse
// amendment A2: judging owners that have no session bead must not create one.
// A resolver that materializes the session it is checking would read every
// gone owner as alive. The session store fails on any write, so a
// materializing lookup fails the test outright. The named seat is the shape a
// materializing resolver creates a bead for; the pool instance and the bare
// name are the shapes a lookup-by-identity reads.
func TestOrderWispWatchdogResolvesOwnersWithoutMaterializingSessions(t *testing.T) {
	wisps := beads.NewMemStore()
	wisps.IDPrefix = "rg"
	sessionBacking := beads.NewMemStore()
	seedOrderWispSession(t, sessionBacking, false, map[string]string{"session_name": "unrelated-sess"})
	sessions := newWriteForbiddingStore(t, sessionBacking)
	named := seedOrderWisp(t, wisps, "digest", "in_progress", "repo/lead", nil)
	pool := seedOrderWisp(t, wisps, "digest", "in_progress", "repo/worker-2", nil)
	bare := seedOrderWisp(t, wisps, "digest", "in_progress", "ghost-sess", nil)

	result := reportOrderWispsForTest(t, orderWispWatchdogTestCity(t), wisps, sessions, named.CreatedAt.Add(7*time.Hour), "digest")

	if got := sessions.attempted(); len(got) != 0 {
		t.Fatalf("owner resolution attempted session-store writes %v; it must never create a session", got)
	}
	requireVerdict(t, staleRunByID(t, result, named.ID), orderWispHeld, "configured named session")
	requireVerdict(t, staleRunByID(t, result, pool.ID), orderWispHolderGone, "agent of this city")
	requireVerdict(t, staleRunByID(t, result, bare.ID), orderWispUnobservable, "names no session")
}

// TestOrderWispWatchdogJudgesTheWholeMolecule pins the subtree verdict, which
// is what woodhouse's block was about: a stale molecule whose open members
// nobody claims is reported unclaimed and left open (it is queued work, not a
// dead run), and a stale molecule with an open step a live session holds is
// reported held, naming that step.
func TestOrderWispWatchdogJudgesTheWholeMolecule(t *testing.T) {
	store := beads.NewMemStore()
	seedOrderWispSession(t, store, true, map[string]string{"session_name": "worker-sess-live"})
	liveRoot, liveStep := seedOrderMolecule(t, store, "worker-sess-live", "in_progress")
	idleRoot, idleStep := seedOrderMolecule(t, store, "", "")

	result := reportOrderWispsForTest(t, orderWispWatchdogTestCity(t), store, store, liveRoot.CreatedAt.Add(7*time.Hour), "digest")

	requireVerdict(t, staleRunByID(t, result, liveRoot.ID), orderWispHeld, "open member "+liveStep.ID+": its session is live")
	requireVerdict(t, staleRunByID(t, result, idleRoot.ID), orderWispUnclaimed, "no open member of the run is claimed")
	requireBeadStatus(t, store, idleRoot.ID, "open")
	requireBeadStatus(t, store, idleStep.ID, "open")
}

// TestOrderWispDoctorNoteAgreesWithTheWatchdog is condition (d): gc doctor's
// note on the bead gating an order judges the whole run, as the watchdog's
// report does, so the two print the same verdict. The molecule's root is
// unclaimed and only its step is held: a doctor that judged the root alone
// would call the run unclaimed while the watchdog calls it held. The note goes
// through doctor's own judgeRun, with only its session-store open stubbed.
func TestOrderWispDoctorNoteAgreesWithTheWatchdog(t *testing.T) {
	store := beads.NewMemStore()
	cfg := orderWispWatchdogTestCity(t)
	seedOrderWispSession(t, store, true, map[string]string{"session_name": "worker-sess-live"})
	heldRoot, _ := seedOrderMolecule(t, store, "worker-sess-live", "in_progress")
	idleRoot, _ := seedOrderMolecule(t, store, "", "")
	goneWisp := seedOrderWisp(t, store, "digest", "in_progress", "repo/worker-1", nil)
	now := heldRoot.CreatedAt.Add(7 * time.Hour)

	result := reportOrderWispsForTest(t, cfg, store, store, now, "digest")
	sessions := &doctorOrderWispSessions{cityPath: t.TempDir(), cfg: cfg}
	sessions.once.Do(func() { sessions.store = store })

	for _, root := range []beads.Bead{heldRoot, idleRoot, goneWisp} {
		run := staleRunByID(t, result, root.ID)
		work := describeOrderFiringOpenWork(root, func(root beads.Bead) (orderWispOwnerVerdict, error) {
			return sessions.judgeRun(store, root)
		})
		if work.Note != run.verdict.String() || work.Holder != run.verdict.Owner {
			t.Fatalf("doctor on %s = (holder %q, note %q), watchdog = (holder %q, %q); want them to agree",
				root.ID, work.Holder, work.Note, run.verdict.Owner, run.verdict)
		}
	}
}

// TestOrderWispWatchdogRuntimeWaitsOneIntervalThenReports pins the controller
// wiring: the first pass is spent arming the clock, never on the boot path,
// and a pass one interval later reports the stale run on stderr and writes
// nothing. The city has no rigs, so the pass reads only the city store it is
// handed, which fails on write.
func TestOrderWispWatchdogRuntimeWaitsOneIntervalThenReports(t *testing.T) {
	backing := beads.NewMemStore()
	seedOrderWispSession(t, backing, false, map[string]string{"session_name": "worker-sess-dead"})
	wisp := seedOrderWisp(t, backing, "digest", "in_progress", "worker-sess-dead", nil)
	store := newWriteForbiddingStore(t, backing)
	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cr := &CityRuntime{
		cityName:            "test-city",
		cfg:                 cfg,
		standaloneCityStore: store,
		orderSet:            []orders.Order{{Name: "digest", Formula: "mol-digest", Trigger: "cooldown", Interval: "24h"}},
		stdout:              io.Discard,
		stderr:              &stderr,
		logPrefix:           "gc test",
	}
	first := wisp.CreatedAt.Add(7 * time.Hour)

	cr.runOrderWispWatchdog(first)
	cr.runOrderWispWatchdog(first.Add(orderWispWatchdogInterval - time.Second))
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q before the first interval elapsed, want nothing", stderr.String())
	}

	cr.runOrderWispWatchdog(first.Add(orderWispWatchdogInterval))
	out := stderr.String()
	if !strings.Contains(out, "1 stale order run(s)") || !strings.Contains(out, wisp.ID) || !strings.Contains(out, "holder-gone: its session in this city has ended") {
		t.Fatalf("stderr = %q, want the report naming %s and its verdict", out, wisp.ID)
	}
	if got := store.attempted(); len(got) != 0 {
		t.Fatalf("the runtime pass attempted store writes %v; it must write nothing", got)
	}
	requireBeadStatus(t, backing, wisp.ID, "in_progress")
}

// TestOrderWispWatchdogSeesWispsWrittenBehindTheCache is the ga-v5vnyp lesson
// applied to wisps: the dispatcher writes wisp roots through its own uncached
// handle, so a candidate read against the controller's cached store would
// return nothing forever. The fixture primes the cache while the store is
// empty and then writes the wisp straight to the backing store.
func TestOrderWispWatchdogSeesWispsWrittenBehindTheCache(t *testing.T) {
	backing := beads.NewMemStore()
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive(): %v", err)
	}
	cfg := orderWispWatchdogTestCity(t)
	store := wrapStoreWithBeadPolicies(cached, cfg)
	wisp := seedOrderWisp(t, backing, "digest", "in_progress", "repo/worker-1", nil)

	result := reportOrderWispsForTest(t, cfg, store, store, wisp.CreatedAt.Add(7*time.Hour), "digest")

	requireVerdict(t, staleRunByID(t, result, wisp.ID), orderWispHolderGone, "agent of this city")
}

func TestOrderWispClaimRefs(t *testing.T) {
	cases := []struct {
		name string
		bead beads.Bead
		want []orderWispClaimRef
	}{
		{name: "open and unassigned is unclaimed", bead: beads.Bead{Status: "open", Metadata: map[string]string{"gc.session_id": "s-1"}}},
		{
			name: "assigned open bead is claimed",
			bead: beads.Bead{Status: "open", Assignee: "repo/worker-1"},
			want: []orderWispClaimRef{{value: "repo/worker-1"}},
		},
		{
			name: "in_progress without assignee claims by session back-reference",
			bead: beads.Bead{Status: "in_progress", Metadata: map[string]string{"gc.session_id": "s-1", "gc.session_name": "worker-sess"}},
			want: []orderWispClaimRef{{value: "s-1", sessionID: true}, {value: "worker-sess"}},
		},
		{
			name: "duplicate references collapse",
			bead: beads.Bead{Status: "in_progress", Assignee: "worker-sess", Metadata: map[string]string{"gc.session_name": "worker-sess", "gc.sessionName": "worker-sess"}},
			want: []orderWispClaimRef{{value: "worker-sess"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orderWispClaimRefs(tc.bead)
			if len(got) != len(tc.want) {
				t.Fatalf("refs = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("refs = %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}
