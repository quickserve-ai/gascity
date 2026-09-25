package main

import (
	"bytes"
	"io"
	"strings"
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

// runOrderWispWatchdogForTest runs one pass over store for the named orders at
// the default cutoff, with store as both the wisp store and the session store.
func runOrderWispWatchdogForTest(t *testing.T, cfg *config.City, store beads.Store, now time.Time, orderNames ...string) orderWispWatchdogResult {
	t.Helper()
	return runOrderWispWatchdogWithSessionsForTest(t, cfg, store, store, now, orderNames...)
}

func runOrderWispWatchdogWithSessionsForTest(t *testing.T, cfg *config.City, store, sessions beads.Store, now time.Time, orderNames ...string) orderWispWatchdogResult {
	t.Helper()
	watch := make([]orderWispWatchdogOrder, 0, len(orderNames))
	for _, name := range orderNames {
		watch = append(watch, orderWispWatchdogOrder{order: orders.Order{Name: name}, scoped: name, staleAfter: orders.DefaultRunStaleAfter})
	}
	result, err := sweepStaleOrderWisps(
		[]orderWispWatchdogLeg{{store: store, orders: watch}},
		now,
		newOrderWispOwnerResolver(cfg, t.TempDir(), sessions),
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderWisps: %v", err)
	}
	return result
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

// TestOrderWispWatchdogSparesRootOnlyWispHeldByLiveSession is the liveness
// mutant (ga-puy7n0 test a). Every case is a stale root-only wisp whose claim
// names this city's own identity, so the locality check passes it: only the
// liveness predicate keeps it open. Delete the `if live` return in
// refVerdict and every case closes.
func TestOrderWispWatchdogSparesRootOnlyWispHeldByLiveSession(t *testing.T) {
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

			result := runOrderWispWatchdogForTest(t, orderWispWatchdogTestCity(t), store, wisp.CreatedAt.Add(7*time.Hour), "digest")

			requireBeadStatus(t, store, wisp.ID, "in_progress")
			if result.closed != 0 {
				t.Fatalf("closed = %d, want 0: a live session holds this wisp", result.closed)
			}
			if len(result.left) != 0 {
				t.Fatalf("left = %+v, want none: a held wisp is not an anomaly to report", result.left)
			}
		})
	}
}

// TestOrderWispWatchdogLeavesUnobservableClaimOpen is the locality mutant
// (woodhouse amendment A1). No case has a live session, so the liveness
// predicate lets every one through: only the locality check keeps another
// city's claim open. Make localityVerdict return abandoned unconditionally and
// every case closes.
func TestOrderWispWatchdogLeavesUnobservableClaimOpen(t *testing.T) {
	cases := []struct {
		name       string
		assignee   string
		meta       map[string]string
		wantReason string
	}{
		{name: "identity absent from the roster", assignee: "repo/dalinar", wantReason: "absent_from_roster"},
		{name: "binding this city does not mint", assignee: "repo/review.omp-1", wantReason: "foreign_binding"},
		{name: "session name no session bead carries", assignee: "their__dog-1-pool", wantReason: "names no session"},
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

			result := runOrderWispWatchdogForTest(t, orderWispWatchdogTestCity(t), store, wisp.CreatedAt.Add(7*time.Hour), "digest")

			requireBeadStatus(t, store, wisp.ID, "in_progress")
			if result.closed != 0 {
				t.Fatalf("closed = %d, want 0: this city cannot prove the claim is its own", result.closed)
			}
			summary := result.leftSummary()
			if !strings.Contains(summary, wisp.ID) || !strings.Contains(summary, tc.wantReason) {
				t.Fatalf("left summary = %q, want it to name %s and %q", summary, wisp.ID, tc.wantReason)
			}
		})
	}
}

// TestOrderWispWatchdogClosesAbandonedLocalClaimAndReleasesTheGate is
// ga-puy7n0 test b: a stale root-only wisp whose claim names this city's own
// session or agent, none of it live, is closed with an audit trail naming the
// order and the cutoff, and the order's gate opens on the next evaluation.
func TestOrderWispWatchdogClosesAbandonedLocalClaimAndReleasesTheGate(t *testing.T) {
	cases := []struct {
		name     string
		assignee string
		session  map[string]string
		wispMeta func(sessionID string) map[string]string
	}{
		{name: "configured pool instance with no session", assignee: "repo/worker-1"},
		{
			name:     "runtime session name of a closed session",
			assignee: "worker-sess-dead",
			session:  map[string]string{"session_name": "worker-sess-dead"},
		},
		{
			name:     "gc.session_id of a closed session",
			session:  map[string]string{"session_name": "worker-sess-gone"},
			wispMeta: func(id string) map[string]string { return map[string]string{"gc.session_id": id} },
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
			gate := &memoryOrderDispatcher{}
			if gated, err := gate.hasOpenWorkStrict(store, "digest"); err != nil || !gated {
				t.Fatalf("hasOpenWorkStrict before the sweep = %v, %v; want true: the wisp must be gating the order", gated, err)
			}

			result := runOrderWispWatchdogForTest(t, orderWispWatchdogTestCity(t), store, wisp.CreatedAt.Add(7*time.Hour), "digest")

			got := requireBeadStatus(t, store, wisp.ID, "closed")
			if result.closed != 1 || len(result.closedRoots) != 1 || result.closedRoots[0] != wisp.ID {
				t.Fatalf("result = %+v, want exactly %s closed", result, wisp.ID)
			}
			reason := got.Metadata["close_reason"]
			if !strings.Contains(reason, "digest") || !strings.Contains(reason, "run_stale_after=6h") {
				t.Fatalf("close_reason = %q, want it to name the order and the cutoff", reason)
			}
			if got.Metadata["order_wisp_sweep_order"] != "digest" || got.Metadata["order_wisp_sweep_stale_after"] != "6h0m0s" {
				t.Fatalf("audit metadata = %v, want order_wisp_sweep_order=digest and order_wisp_sweep_stale_after=6h0m0s", got.Metadata)
			}
			if got.Metadata["order_tracking_sweep_by"] != orderWispWatchdogMetadataInitiator {
				t.Fatalf("order_tracking_sweep_by = %q, want %q", got.Metadata["order_tracking_sweep_by"], orderWispWatchdogMetadataInitiator)
			}
			if gated, err := gate.hasOpenWorkStrict(store, "digest"); err != nil || gated {
				t.Fatalf("hasOpenWorkStrict after the sweep = %v, %v; want false: the order must fire on its next evaluation", gated, err)
			}
		})
	}
}

// TestOrderWispWatchdogLeavesYoungWispsAloneWhateverTheirHolder is ga-puy7n0
// test c: under the cutoff, neither a dead holder nor a live one moves the
// watchdog, and neither is reported.
func TestOrderWispWatchdogLeavesYoungWispsAloneWhateverTheirHolder(t *testing.T) {
	store := beads.NewMemStore()
	seedOrderWispSession(t, store, true, map[string]string{"session_name": "worker-sess-live"})
	abandoned := seedOrderWisp(t, store, "digest", "in_progress", "repo/worker-1", nil)
	held := seedOrderWisp(t, store, "digest", "in_progress", "worker-sess-live", nil)

	result := runOrderWispWatchdogForTest(t, orderWispWatchdogTestCity(t), store, abandoned.CreatedAt.Add(time.Hour), "digest")

	requireBeadStatus(t, store, abandoned.ID, "in_progress")
	requireBeadStatus(t, store, held.ID, "in_progress")
	if result.closed != 0 || len(result.left) != 0 {
		t.Fatalf("result = %+v, want nothing closed or reported under the cutoff", result)
	}
}

// TestOrderWispWatchdogHonorsPerOrderStaleAfter is ga-puy7n0 test d: an order's
// run_stale_after sets its own cutoff, and an order that sets none gets the
// default.
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
	result, err := sweepStaleOrderWisps(
		[]orderWispWatchdogLeg{{store: store, orders: watch}},
		fast.CreatedAt.Add(3*time.Hour),
		newOrderWispOwnerResolver(orderWispWatchdogTestCity(t), t.TempDir(), store),
	)
	if err != nil {
		t.Fatalf("sweepStaleOrderWisps: %v", err)
	}

	got := requireBeadStatus(t, store, fast.ID, "closed")
	if !strings.Contains(got.Metadata["close_reason"], "run_stale_after=2h") {
		t.Fatalf("close_reason = %q, want the order's own 2h cutoff", got.Metadata["close_reason"])
	}
	requireBeadStatus(t, store, slow.ID, "in_progress")
	if result.closed != 1 {
		t.Fatalf("closed = %d, want 1", result.closed)
	}
}

// TestOrderWispWatchdogLeavesUnclaimedWispOpenAndReportsIt pins the queued-
// demand rule: an unclaimed stale wisp is not closed, because a slow pool is
// not a dead one, but it is named every pass because it still gates its order.
// A session back-reference left behind by a released claim does not make it
// claimed.
func TestOrderWispWatchdogLeavesUnclaimedWispOpenAndReportsIt(t *testing.T) {
	store := beads.NewMemStore()
	dead := seedOrderWispSession(t, store, false, map[string]string{"session_name": "worker-sess-released"})
	unclaimed := seedOrderWisp(t, store, "digest", "", "", nil)
	released := seedOrderWisp(t, store, "digest", "", "", map[string]string{"gc.session_id": dead.ID})

	result := runOrderWispWatchdogForTest(t, orderWispWatchdogTestCity(t), store, unclaimed.CreatedAt.Add(7*time.Hour), "digest")

	requireBeadStatus(t, store, unclaimed.ID, "open")
	requireBeadStatus(t, store, released.ID, "open")
	if result.closed != 0 {
		t.Fatalf("closed = %d, want 0: unclaimed work is queued demand", result.closed)
	}
	summary := result.leftSummary()
	for _, id := range []string{unclaimed.ID, released.ID} {
		if !strings.Contains(summary, id) {
			t.Fatalf("left summary = %q, want it to name %s", summary, id)
		}
	}
	if !strings.Contains(summary, `"unclaimed"`) {
		t.Fatalf("left summary = %q, want the holder reported as unclaimed", summary)
	}
}

// TestOrderWispWatchdogResolvesOwnersWithoutMaterializingSessions is woodhouse
// amendment A2: judging owners that have no session bead must not create one.
// A resolver that materializes the session it is checking would read every
// dead owner as alive, so the session store must end the pass with exactly the
// beads it started with. The named seat is the shape a materializing resolver
// creates a bead for; the pool instance and the bare name are the shapes a
// lookup-by-identity reads.
func TestOrderWispWatchdogResolvesOwnersWithoutMaterializingSessions(t *testing.T) {
	wisps := beads.NewMemStore()
	sessions := beads.NewMemStore()
	seedOrderWispSession(t, sessions, false, map[string]string{"session_name": "unrelated-sess"})
	named := seedOrderWisp(t, wisps, "digest", "in_progress", "repo/lead", nil)
	pool := seedOrderWisp(t, wisps, "digest", "in_progress", "repo/worker-2", nil)
	bare := seedOrderWisp(t, wisps, "digest", "in_progress", "ghost-sess", nil)
	before := countAllBeadsForTest(t, sessions)

	result := runOrderWispWatchdogWithSessionsForTest(t, orderWispWatchdogTestCity(t), wisps, sessions, named.CreatedAt.Add(7*time.Hour), "digest")

	if after := countAllBeadsForTest(t, sessions); after != before {
		t.Fatalf("session store holds %d beads after the sweep, want %d: owner resolution must never create a session", after, before)
	}
	requireBeadStatus(t, wisps, named.ID, "in_progress")
	requireBeadStatus(t, wisps, pool.ID, "closed")
	requireBeadStatus(t, wisps, bare.ID, "in_progress")
	if result.closed != 1 {
		t.Fatalf("closed = %d, want 1 (only the configured pool instance with no session)", result.closed)
	}
}

func countAllBeadsForTest(t *testing.T, store beads.Store) int {
	t.Helper()
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	return len(all)
}

// TestOrderWispWatchdogSparesMoleculeWithLiveClaimedStep carries amendment A1
// into the subtree case: a stale molecule whose open step a live session holds
// stays open, while a stale molecule whose open steps nobody claims keeps the
// stale-subtree close the operator sweep always performed.
func TestOrderWispWatchdogSparesMoleculeWithLiveClaimedStep(t *testing.T) {
	store := beads.NewMemStore()
	seedOrderWispSession(t, store, true, map[string]string{"session_name": "worker-sess-live"})
	seedMolecule := func(stepAssignee, stepStatus string) (beads.Bead, beads.Bead) {
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
	liveRoot, liveStep := seedMolecule("worker-sess-live", "in_progress")
	idleRoot, idleStep := seedMolecule("", "")

	result := runOrderWispWatchdogForTest(t, orderWispWatchdogTestCity(t), store, liveRoot.CreatedAt.Add(7*time.Hour), "digest")

	requireBeadStatus(t, store, liveRoot.ID, "open")
	requireBeadStatus(t, store, liveStep.ID, "in_progress")
	requireBeadStatus(t, store, idleRoot.ID, "closed")
	requireBeadStatus(t, store, idleStep.ID, "closed")
	if result.closed != 2 {
		t.Fatalf("closed = %d, want 2 (the unclaimed molecule's root and step)", result.closed)
	}
}

// TestOrderWispWatchdogRuntimeWaitsOneIntervalThenSweeps pins the controller
// wiring: the first pass is spent arming the clock, never on the boot path,
// and a pass one interval later closes an abandoned wisp and says so. The city
// has no rigs, so the pass reads only the city store it is handed.
func TestOrderWispWatchdogRuntimeWaitsOneIntervalThenSweeps(t *testing.T) {
	store := beads.NewMemStore()
	seedOrderWispSession(t, store, false, map[string]string{"session_name": "worker-sess-dead"})
	wisp := seedOrderWisp(t, store, "digest", "in_progress", "worker-sess-dead", nil)
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
	requireBeadStatus(t, store, wisp.ID, "in_progress")

	cr.runOrderWispWatchdog(first.Add(orderWispWatchdogInterval - time.Second))
	requireBeadStatus(t, store, wisp.ID, "in_progress")

	cr.runOrderWispWatchdog(first.Add(orderWispWatchdogInterval))
	requireBeadStatus(t, store, wisp.ID, "closed")
	if !strings.Contains(stderr.String(), "order wisp watchdog closed 1 bead(s)") || !strings.Contains(stderr.String(), wisp.ID) {
		t.Fatalf("stderr = %q, want the close line naming %s", stderr.String(), wisp.ID)
	}
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

	result := runOrderWispWatchdogForTest(t, cfg, store, wisp.CreatedAt.Add(7*time.Hour), "digest")

	requireBeadStatus(t, backing, wisp.ID, "closed")
	if result.closed != 1 {
		t.Fatalf("closed = %d, want 1: the watchdog read a cached snapshot that never held this wisp", result.closed)
	}
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
