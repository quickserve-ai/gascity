package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// ---------------------------------------------------------------------------
// ga-2otk73 regression suite.
//
// Incident: qcore/gastown.refinery was down ~15 minutes and could not restart.
// A half-created session bead (state=start-pending, pending_create_claim=true,
// wake_attempts=0) held the fixed session name "qcore--gastown__refinery" with
// NO tmux session and NO agent process. Every reconciler cycle failed with
// `session name already exists: "qcore--gastown__refinery" already belongs to
// ga-wisp-r4jw5ee` — 62 consecutive times — and neither the reconciler's
// pending-create rollback nor the pool sweep ever touched the holder. Service
// was restored only by a human closing the bead by hand.
//
// The suite is deliberately weighted toward ACCEPT cases: this loop supervises
// a live ~20-agent fleet, and a wrongly closed session bead kills a working
// agent's continuity. Each REJECT case below is paired with the ACCEPT case
// that proves the corresponding guard is load-bearing.
// ---------------------------------------------------------------------------

const (
	squatTestIdentity = "myrig/refinery"
	squatTestTemplate = "myrig/refinery"
)

func squatTestCity() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "myrig"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "refinery", Dir: "myrig"},
		},
	}
}

func squatTestSessionName(t *testing.T, cfg *config.City) string {
	t.Helper()
	name := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, squatTestIdentity)
	if strings.TrimSpace(name) == "" {
		t.Fatalf("NamedSessionRuntimeName(%q) = empty", squatTestIdentity)
	}
	return name
}

// squattingNamedSessionBead reproduces ga-wisp-r4jw5ee's exact persisted shape:
// created by session.Manager.createBeadOnly (the worker/nudge/wake lane), so it
// carries labels {gc:session, template:<identity>} and NOT the controller's
// agent:<identity> label, and it is parked in state=start-pending holding an
// unfinished pending-create claim.
func squattingNamedSessionBead(t *testing.T, store beads.Store, sessionName string, claimAge time.Duration, extra map[string]string) beads.Bead {
	t.Helper()
	meta := map[string]string{
		"session_name":               sessionName,
		"session_name_explicit":      "true",
		"template":                   squatTestTemplate,
		"agent_name":                 squatTestIdentity,
		"state":                      string(session.StateStartPending),
		"pending_create_claim":       "true",
		"pending_create_started_at":  time.Now().UTC().Add(-claimAge).Format(time.RFC3339),
		"wake_attempts":              "0",
		namedSessionMetadataKey:      boolMetadata(true),
		namedSessionIdentityMetadata: squatTestIdentity,
		namedSessionModeMetadata:     "on_demand",
		"continuation_epoch":         "1",
		"generation":                 "1",
	}
	for k, v := range extra {
		if v == "" {
			delete(meta, k)
			continue
		}
		meta[k] = v
	}
	b, err := store.Create(beads.Bead{
		Title:    squatTestIdentity,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "template:" + squatTestTemplate},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Create(squatting session bead): %v", err)
	}
	return b
}

func assertBeadClosed(t *testing.T, store beads.Store, id, what string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Status != "closed" {
		t.Fatalf("%s: bead %s status = %q, want closed (metadata=%v)", what, id, got.Status, got.Metadata)
	}
}

func assertBeadOpen(t *testing.T, store beads.Store, id, what string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Status == "closed" {
		t.Fatalf("%s: bead %s was CLOSED (close_reason=%q) — a live named session must not be touched", what, id, got.Metadata["close_reason"])
	}
}

// ===========================================================================
// GAP 1 — the pool sweep's stale-create-lease escape must be REACHABLE for a
// named session that has fallen out of the desired set.
//
// Before the fix, sweepUndesiredPoolSessionBeads bailed on
// `isManualSessionInfo(info) || isNamedSessionInfo(info)` BEFORE the
// pending-create lease check, and again on `!isEphemeralSessionInfo(info)`
// further down, so a named session could never reach the recovery at all.
// ===========================================================================

// REJECT: expired never-started lease + no runtime => sweepable.
// RED before the fix (closed = 0: the named guard bailed first).
func TestSweepUndesiredPoolSessionBeads_SweepsNamedSessionWithExpiredPendingCreateLease(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	// 15 minutes: past pendingCreateNeverStartedTimeout (10m), matching the
	// incident's claim age at the time the refinery was still wedged.
	bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, nil)

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		nil, // not desired
		cfg,
		runtime.NewFake(), // no runtime for sessionName
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — a named session with an expired pending-create lease and no runtime must be recoverable", closed)
	}
	assertBeadClosed(t, store, bead.ID, "expired named pending-create claim")
}

// ACCEPT: the lease is still LIVE. The bead must not be touched.
// Proves the lease gate is load-bearing (mutation-checked: shortening the
// claim age past pendingCreateNeverStartedTimeout flips this to closed=1).
func TestSweepUndesiredPoolSessionBeads_KeepsNamedSessionWithLivePendingCreateLease(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	// 2 minutes: well inside pendingCreateNeverStartedTimeout (10m). A slow
	// provider start looks exactly like this.
	bead := squattingNamedSessionBead(t, store, sessionName, 2*time.Minute, nil)

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		nil,
		cfg,
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — a live pending-create lease must be protected", closed)
	}
	assertBeadOpen(t, store, bead.ID, "live pending-create lease")
}

// ACCEPT: lease expired but a runtime IS live. The bead must not be touched —
// closing it would kill a working agent's continuity.
// Proves the runtime gate is load-bearing (mutation-checked: dropping the
// runtime probe flips this to closed=1).
func TestSweepUndesiredPoolSessionBeads_KeepsNamedSessionWithLiveRuntime(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, nil)

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start(%s): %v", sessionName, err)
	}

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		nil,
		cfg,
		sp,
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — a live runtime must be protected even with an expired lease", closed)
	}
	assertBeadOpen(t, store, bead.ID, "live runtime, expired lease")
}

// ACCEPT: the runtime probe is UNAVAILABLE (nil provider). "Cannot tell" must
// never read as "stopped". Proves namedSessionRuntimeConfirmedStopped fails
// CLOSED — the pool path below it does not (it sweeps on probe error), so this
// asymmetry has to be tested explicitly or the named path inherits a fail-open.
func TestSweepUndesiredPoolSessionBeads_KeepsNamedSessionWhenRuntimeUnobservable(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, nil)

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		nil,
		cfg,
		nil, // provider unavailable: liveness cannot be observed
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — an unobservable runtime must fail closed", closed)
	}
	assertBeadOpen(t, store, bead.ID, "unobservable runtime")
}

// ACCEPT: the store query was partial/unreliable this tick. Nothing is swept.
// Proves the partial-query gate still covers the new named path.
// (mutation-checked: passing storeQueryPartial=false flips this to closed=1.)
func TestSweepUndesiredPoolSessionBeads_SkipsNamedStaleCreateOnPartialStoreQuery(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, nil)

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		nil,
		cfg,
		runtime.NewFake(),
		true, // storeQueryPartial
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — a partial store query must not sweep", closed)
	}
	assertBeadOpen(t, store, bead.ID, "partial store query")
}

// ACCEPT: a named session that is in the DESIRED set is never a sweep
// candidate, expired claim or not. The sweep is a scale-down GC; the desired
// path belongs to the reconciler.
func TestSweepUndesiredPoolSessionBeads_KeepsDesiredNamedSessionWithExpiredLease(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, nil)

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		map[string]TemplateParams{sessionName: {
			TemplateName:            squatTestTemplate,
			InstanceName:            squatTestIdentity,
			ConfiguredNamedIdentity: squatTestIdentity,
		}},
		cfg,
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — a desired named session is not a sweep candidate", closed)
	}
	assertBeadOpen(t, store, bead.ID, "desired named session")
}

// ACCEPT: a named session with NO pending-create claim (the healthy shape — an
// asleep on-demand session waiting to be woken) is never a sweep candidate.
// This is the class the old blanket named-guard existed to protect, and it
// must stay protected.
func TestSweepUndesiredPoolSessionBeads_KeepsAsleepNamedSessionWithoutPendingCreate(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, map[string]string{
		"state":                     string(session.StateAsleep),
		"pending_create_claim":      "", // delete
		"pending_create_started_at": "", // delete
	})

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		nil,
		cfg,
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — an asleep named session with no pending-create claim must be protected", closed)
	}
	assertBeadOpen(t, store, bead.ID, "asleep named session, no claim")
}

// ACCEPT: a MANUAL session is excluded unconditionally — a human-attached
// session has no pending-create lease semantics and no owner to recreate it.
func TestSweepUndesiredPoolSessionBeads_KeepsManualSessionWithExpiredPendingCreate(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, map[string]string{
		"manual_session": "true",
	})

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		nil,
		cfg,
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — manual sessions are excluded unconditionally", closed)
	}
	assertBeadOpen(t, store, bead.ID, "manual session")
}

// ACCEPT: assigned work blocks the close. GCSweepSessionBeads routes through
// the live cross-store work guard; a squatter that somehow owns in-flight work
// must not be closed out from under it.
func TestSweepUndesiredPoolSessionBeads_KeepsNamedStaleCreateWithAssignedWork(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	bead := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, nil)

	work, err := store.Create(beads.Bead{Title: "in-flight work", Type: "task", Assignee: bead.ID})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	closed := sweepUndesiredPoolSessionBeads(
		beads.SessionStore{Store: store},
		nil,
		newSessionBeadSnapshot([]beads.Bead{bead}),
		nil,
		cfg,
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — assigned work must block the close", closed)
	}
	assertBeadOpen(t, store, bead.ID, "assigned work")
}

// ===========================================================================
// GAP 2 — the collision path itself.
//
// The reconciler never rolled the incident bead back because its per-tick
// session feed never contained it: every identity-discovery read the
// controller makes is IncludeClosed=false, which a CachingStore answers purely
// from its in-memory map, while the name-uniqueness check is IncludeClosed=true
// and takes a live backing read. The controller could see the NAME was taken
// and could not see BY WHAT.
//
// cacheSplitStore reproduces exactly that read-tier split. The repair does not
// depend on why the holder was invisible: the collision error already names
// the holder, and a by-ID read resolves it regardless of list tier.
// ===========================================================================

type cacheSplitStore struct {
	beads.Store
	hiddenID string
}

// List hides hiddenID from every IncludeClosed=false query (the cache-served
// tier) and reveals it on IncludeClosed=true (the live backing read).
func (s *cacheSplitStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	out, err := s.Store.List(q)
	if q.IncludeClosed || s.hiddenID == "" {
		return out, err
	}
	kept := make([]beads.Bead, 0, len(out))
	for _, b := range out {
		if b.ID == s.hiddenID {
			continue
		}
		kept = append(kept, b)
	}
	return kept, err
}

func squatSyncDesiredState(sessionName string) map[string]TemplateParams {
	return map[string]TemplateParams{
		sessionName: {
			TemplateName:            squatTestTemplate,
			InstanceName:            squatTestIdentity,
			Alias:                   squatTestIdentity,
			Command:                 "claude",
			ConfiguredNamedIdentity: squatTestIdentity,
			ConfiguredNamedMode:     "on_demand",
		},
	}
}

// runSquatSync drives one syncSessionBeads tick against the split-view store.
func runSquatSync(t *testing.T, store beads.Store, cfg *config.City, sessionName string, sp runtime.Provider) string {
	t.Helper()
	ds := squatSyncDesiredState(sessionName)
	clk := &clock.Fake{Time: time.Now().UTC()}
	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)
	return stderr.String()
}

func openSessionBeadsOwningName(t *testing.T, store beads.Store, sessionName string) []beads.Bead {
	t.Helper()
	all, err := store.List(beads.ListQuery{Label: sessionBeadLabel, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List(session beads): %v", err)
	}
	var owners []beads.Bead
	for _, b := range all {
		if b.Status == "closed" {
			continue
		}
		if strings.TrimSpace(b.Metadata["session_name"]) == sessionName {
			owners = append(owners, b)
		}
	}
	return owners
}

// REJECT: the incident. An invisible holder with an expired never-started
// lease and no runtime must have the name taken back, and the very next tick
// must materialize the session.
// RED before the fix: the holder stays open forever and every tick logs
// `session name already exists`.
func TestSyncSessionBeads_ReleasesNamedSessionNameFromAbandonedInvisibleHolder(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	// Precondition: the holder really is invisible to the controller's
	// identity-discovery tier and really is visible to the name-uniqueness
	// tier. Without this the test would pass vacuously on a plain store.
	if _, found, err := findOpenSessionBeadBySessionName(store, sessionName); err != nil || found {
		t.Fatalf("precondition: holder must be invisible to the cached list (found=%v err=%v)", found, err)
	}
	if err := session.EnsureSessionNameAvailableWithConfigForOwner(store, cfg, sessionName, "", squatTestIdentity); err == nil {
		t.Fatal("precondition: the name-uniqueness check must see the holder")
	}

	out := runSquatSync(t, store, cfg, sessionName, runtime.NewFake())
	if !strings.Contains(out, "released session_name") {
		t.Fatalf("stderr = %q, want the name-release diagnostic", out)
	}
	assertBeadClosed(t, mem, holder.ID, "abandoned invisible holder")

	// End-to-end: the next tick must actually bring the session back. That is
	// the whole point — the incident was a PERMANENT outage, not a slow one.
	if owners := openSessionBeadsOwningName(t, mem, sessionName); len(owners) != 0 {
		t.Fatalf("after release: %d open beads still own %q, want 0", len(owners), sessionName)
	}
	runSquatSync(t, store, cfg, sessionName, runtime.NewFake())
	owners := openSessionBeadsOwningName(t, mem, sessionName)
	if len(owners) != 1 {
		t.Fatalf("after recovery tick: %d open beads own %q, want exactly 1", len(owners), sessionName)
	}
}

// ACCEPT: the holder's lease is still LIVE (a genuinely slow start). Leave it
// alone — the create stays blocked, which is the correct behavior.
func TestSyncSessionBeads_KeepsInvisibleNamedSessionNameHolderWithLiveLease(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 2*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	runSquatSync(t, store, cfg, sessionName, runtime.NewFake())
	assertBeadOpen(t, mem, holder.ID, "live lease holder")
}

// ACCEPT: the holder's lease expired but its runtime is LIVE. This is a
// session that started and whose claim simply was not cleared; closing it
// would kill a working agent.
func TestSyncSessionBeads_KeepsInvisibleNamedSessionNameHolderWithLiveRuntime(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start(%s): %v", sessionName, err)
	}

	runSquatSync(t, store, cfg, sessionName, sp)
	assertBeadOpen(t, mem, holder.ID, "live runtime holder")
}

// ACCEPT: the holder belongs to a DIFFERENT configured named identity. Never
// close another identity's bead off a name collision — that is the
// ga-841/kg4uh4 phantom class. On ambiguity, do nothing.
func TestSyncSessionBeads_KeepsInvisibleNamedSessionNameHolderOfAnotherIdentity(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, map[string]string{
		namedSessionIdentityMetadata: "myrig/somebody-else",
	})
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	runSquatSync(t, store, cfg, sessionName, runtime.NewFake())
	assertBeadOpen(t, mem, holder.ID, "foreign-identity holder")
}

// ACCEPT: the holder carries no configured_named_identity at all (a legacy or
// hand-repaired bead). Same rule: do nothing.
func TestSyncSessionBeads_KeepsInvisibleNamedSessionNameHolderWithoutIdentity(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, map[string]string{
		namedSessionIdentityMetadata: "",
	})
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	runSquatSync(t, store, cfg, sessionName, runtime.NewFake())
	assertBeadOpen(t, mem, holder.ID, "identity-less holder")
}

// ACCEPT: the holder owns in-flight work. The live cross-store work guard must
// block the release.
func TestSyncSessionBeads_KeepsInvisibleNamedSessionNameHolderWithAssignedWork(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	work, err := mem.Create(beads.Bead{Title: "in-flight work", Type: "task", Assignee: holder.ID})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	inProgress := "in_progress"
	if err := mem.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	runSquatSync(t, store, cfg, sessionName, runtime.NewFake())
	assertBeadOpen(t, mem, holder.ID, "holder with assigned work")
}

// The typed conflict error must carry the holder ID and stay byte-identical to
// the fmt.Errorf it replaced, so every existing errors.Is caller and every log
// line is unchanged.
func TestSessionNameConflictErrorCarriesHolderAndPreservesText(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, nil)

	err := session.EnsureSessionNameAvailableWithConfigForOwner(store, cfg, sessionName, "", squatTestIdentity)
	if err == nil {
		t.Fatal("expected a session-name conflict")
	}
	if got := session.SessionNameConflictHolderID(err); got != holder.ID {
		t.Fatalf("SessionNameConflictHolderID = %q, want %q", got, holder.ID)
	}
	want := "session name already exists: \"" + sessionName + "\" already belongs to " + holder.ID
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
	// A non-conflict error must yield no holder.
	if got := session.SessionNameConflictHolderID(session.ErrSessionNameExists); got != "" {
		t.Fatalf("SessionNameConflictHolderID(bare sentinel) = %q, want empty", got)
	}
}

// poolSweepWouldDrain gates the managed-Dolt squatter probe to ticks where the
// sweep would actually destroy something. It must therefore admit every shape
// the sweep can now close, or a named-squatter close rides an unverified store.
// (It stays deliberately over-inclusive in the other direction: it omits the
// runtime probe, so an extra @@datadir probe is the worst case.)
func TestPoolSweepWouldDrain_CoversNamedStaleCreate(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	store := beads.NewMemStore()

	stale := squattingNamedSessionBead(t, store, sessionName, 15*time.Minute, nil)
	if !poolSweepWouldDrain(newSessionBeadSnapshot([]beads.Bead{stale}), map[string]TemplateParams{}, cfg) {
		t.Fatal("want drainPending=true: the sweep can close this named bead, so the store-identity probe must run")
	}
	if poolSweepWouldDrain(newSessionBeadSnapshot([]beads.Bead{stale}), map[string]TemplateParams{sessionName: {}}, cfg) {
		t.Fatal("want drainPending=false: a desired session is never a sweep candidate")
	}

	// A named session the sweep still refuses to touch must not trip the probe.
	healthy := squattingNamedSessionBead(t, store, sessionName+"-healthy", 15*time.Minute, map[string]string{
		"session_name":              sessionName + "-healthy",
		"state":                     string(session.StateAsleep),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	})
	if poolSweepWouldDrain(newSessionBeadSnapshot([]beads.Bead{healthy}), map[string]TemplateParams{}, cfg) {
		t.Fatal("want drainPending=false: an asleep named session with no pending-create claim is not a sweep candidate")
	}
}
