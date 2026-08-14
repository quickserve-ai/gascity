package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// ---------------------------------------------------------------------------
// ga-2otk73 regression suite (iteration 2).
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
// a live ~17-agent fleet, and a wrongly closed session bead kills a working
// agent's continuity. Each REJECT case below is paired with the ACCEPT case
// that proves the corresponding guard is load-bearing.
// ---------------------------------------------------------------------------

const (
	squatTestIdentity = "myrig/refinery"
	squatTestTemplate = "myrig/refinery"

	// squatTickInterval is how far runSquatSync advances the per-test clock
	// between ticks. Three ticks therefore span namedNameReleaseConfirmWindow
	// exactly, and each gap stays inside namedNameReleaseProbeMaxGap — the
	// minimum sequence a release requires.
	squatTickInterval = time.Minute
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
// The collision path.
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

// hookedGetStore fires onGet before every Get, with the 1-based call index for
// that id. It exists to exercise the read-compare-write fence: a creator that
// commits between the confirming probe and the close must abort the release.
type hookedGetStore struct {
	beads.Store
	mu    sync.Mutex
	calls map[string]int
	onGet func(id string, n int)
}

func (s *hookedGetStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[id]++
	n := s.calls[id]
	hook := s.onGet
	s.mu.Unlock()
	if hook != nil {
		hook(id, n)
	}
	return s.Store.Get(id)
}

var (
	squatClockMu sync.Mutex
	squatClocks  = map[*testing.T]*clock.Fake{}
)

// squatTickClock returns the per-test fake clock runSquatSync drives. The
// multi-tick confirmation window is a WALL-CLOCK gate, so ticks must advance
// time; a fresh clock per call would make the window unsatisfiable and every
// test would read as "fail safe" for the wrong reason.
func squatTickClock(t *testing.T) *clock.Fake {
	t.Helper()
	squatClockMu.Lock()
	defer squatClockMu.Unlock()
	clk, ok := squatClocks[t]
	if !ok {
		clk = &clock.Fake{Time: time.Now().UTC()}
		squatClocks[t] = clk
		t.Cleanup(func() {
			squatClockMu.Lock()
			delete(squatClocks, t)
			squatClockMu.Unlock()
		})
	}
	return clk
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

// runSquatSync drives one syncSessionBeads tick against the split-view store,
// then advances the per-test clock by squatTickInterval.
func runSquatSync(t *testing.T, store beads.Store, cfg *config.City, sessionName string, sp runtime.Provider) string {
	t.Helper()
	ds := squatSyncDesiredState(sessionName)
	clk := squatTickClock(t)
	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)
	clk.Advance(squatTickInterval)
	return stderr.String()
}

// runSquatSyncTicks drives n ticks and returns the concatenated stderr.
func runSquatSyncTicks(t *testing.T, n int, store beads.Store, cfg *config.City, sessionName string, sp runtime.Provider) string {
	t.Helper()
	var out strings.Builder
	for i := 0; i < n; i++ {
		out.WriteString(runSquatSync(t, store, cfg, sessionName, sp))
	}
	return out.String()
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
// lease and no runtime must have the name taken back once the multi-tick
// confirmation window closes, and the very next tick must materialize the
// session — reusing the SAME bead, so the identity's conversation survives.
// RED before the fix: the holder stays open forever and every tick logs
// `session name already exists`.
func TestSyncSessionBeads_ReleasesNamedSessionNameFromAbandonedInvisibleHolder(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, map[string]string{
		"session_key": "sk-live-conversation",
	})
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

	out := runSquatSyncTicks(t, namedNameReleaseConfirmTicks, store, cfg, sessionName, runtime.NewFake())
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
	// R4: continuity. The recovery tick must REOPEN the same bead, not mint a
	// fresh one — a new bead would silently drop session_key and restart the
	// agent's conversation.
	if owners[0].ID != holder.ID {
		t.Fatalf("recovered owner = %s, want the original bead %s reopened (continuity dropped)", owners[0].ID, holder.ID)
	}
	if got := strings.TrimSpace(owners[0].Metadata["session_key"]); got != "sk-live-conversation" {
		t.Fatalf("reopened session_key = %q, want the original conversation handle", got)
	}
	// The wedge itself must not survive onto the reopened bead.
	if got := strings.TrimSpace(owners[0].Metadata["pending_create_started_at"]); got == "" {
		t.Fatalf("reopened bead has no pending_create_started_at; the reopen should restamp a FRESH claim clock")
	}
}

// R4 unit: the close reason must keep the bead reopen-eligible. A named bead
// closed as "gc_swept" / "failed-create" / "orphaned" is rejected by
// closedNamedSessionReopenEligible, so its identity loses session_key and comes
// back as a new conversation.
func TestReleasedNamedSessionHolderStaysReopenEligible(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, map[string]string{
		"session_key": "sk-live-conversation",
	})
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks, store, cfg, sessionName, runtime.NewFake())
	assertBeadClosed(t, mem, holder.ID, "released holder")

	got, err := mem.Get(holder.ID)
	if err != nil {
		t.Fatalf("Get(holder): %v", err)
	}
	if got.Metadata["state"] != namedSessionSquatCloseState {
		t.Fatalf("terminal state = %q, want %q", got.Metadata["state"], namedSessionSquatCloseState)
	}
	// The claim must be gone, or the reopened bead inherits the wedge.
	if v := strings.TrimSpace(got.Metadata["pending_create_claim"]); v != "" {
		t.Fatalf("pending_create_claim = %q on the closed bead, want cleared", v)
	}
	// session_name must SURVIVE: reopenClosedConfiguredNamedSessionBead
	// requires it to match, and the close alone already releases the name.
	if v := strings.TrimSpace(got.Metadata["session_name"]); v != sessionName {
		t.Fatalf("session_name = %q on the closed bead, want %q preserved for the reopen lookup", v, sessionName)
	}
	found, ok, err := session.FindClosedNamedSessionBeadForSessionName(mem, squatTestIdentity, sessionName)
	if err != nil || !ok {
		t.Fatalf("FindClosedNamedSessionBeadForSessionName = (%v, %v, %v), want the released bead — continuity was DROPPED", found.ID, ok, err)
	}
	if found.ID != holder.ID {
		t.Fatalf("reopen-eligible bead = %s, want %s", found.ID, holder.ID)
	}
	// And the name really is free again.
	if err := session.EnsureSessionNameAvailableWithConfigForOwner(mem, cfg, sessionName, "", squatTestIdentity); err != nil {
		t.Fatalf("session_name still held after release: %v", err)
	}
}

// ACCEPT: one confirming observation is NOT proof. runtime.Liveness has no
// error channel and tmux's StateCache reports every session not-running once
// its snapshot is >30s stale, so a single "stopped" answer from a degraded
// probe is indistinguishable from a real death. The name must stay held until
// the window closes.
// (mutation-checked: setting namedNameReleaseConfirmTicks=1 and the window to 0
// flips this to a release on tick 1.)
func TestSyncSessionBeads_HoldsNameUntilConfirmationWindowCloses(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	out := runSquatSyncTicks(t, namedNameReleaseConfirmTicks-1, store, cfg, sessionName, runtime.NewFake())
	if !strings.Contains(out, "awaiting confirmation") {
		t.Fatalf("stderr = %q, want the awaiting-confirmation diagnostic", out)
	}
	if strings.Contains(out, "released session_name") {
		t.Fatalf("released before the confirmation window closed: %q", out)
	}
	assertBeadOpen(t, mem, holder.ID, "holder inside the confirmation window")

	// The final confirming tick completes the window.
	out = runSquatSync(t, store, cfg, sessionName, runtime.NewFake())
	if !strings.Contains(out, "released session_name") {
		t.Fatalf("stderr = %q, want the release once the window closed", out)
	}
	assertBeadClosed(t, mem, holder.ID, "holder after the confirmation window")
}

// ACCEPT: the confirmations must be CONSECUTIVE. A probe that flickers — dead,
// alive, dead, dead — is exactly the transient-degradation signature the window
// exists to reject, so the sequence must restart, not accumulate.
// (mutation-checked: counting cumulative observations instead of consecutive
// ones flips this to a close.)
func TestSyncSessionBeads_ConfirmationSequenceRestartsWhenRuntimeReappears(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	dead := runtime.NewFake()
	live := runtime.NewFake()
	if err := live.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start(%s): %v", sessionName, err)
	}

	// dead, dead, ALIVE, dead, dead — five ticks, but never three in a row.
	for _, sp := range []runtime.Provider{dead, dead, live, dead, dead} {
		if out := runSquatSync(t, store, cfg, sessionName, sp); strings.Contains(out, "released session_name") {
			t.Fatalf("released on a flickering probe: %q", out)
		}
	}
	assertBeadOpen(t, mem, holder.ID, "flickering runtime probe")
}

// ACCEPT: the holder's lease is still LIVE (a genuinely slow start). Leave it
// alone — the create stays blocked, which is the correct behavior.
func TestSyncSessionBeads_KeepsInvisibleNamedSessionNameHolderWithLiveLease(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 2*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks+2, store, cfg, sessionName, runtime.NewFake())
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

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks+2, store, cfg, sessionName, sp)
	assertBeadOpen(t, mem, holder.ID, "live runtime holder")
}

// ACCEPT: the runtime probe is UNAVAILABLE (nil provider). "Cannot tell" must
// never read as "stopped". Proves namedSessionRuntimeConfirmedStopped fails
// CLOSED.
func TestSyncSessionBeads_KeepsInvisibleNamedSessionNameHolderWhenRuntimeUnobservable(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks+2, store, cfg, sessionName, nil)
	assertBeadOpen(t, mem, holder.ID, "unobservable runtime")
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

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks+2, store, cfg, sessionName, runtime.NewFake())
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

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks+2, store, cfg, sessionName, runtime.NewFake())
	assertBeadOpen(t, mem, holder.ID, "identity-less holder")
}

// ACCEPT: a MANUAL session is excluded unconditionally — a human-attached
// session has no pending-create lease semantics and no owner to recreate it.
func TestSyncSessionBeads_KeepsManualNamedSessionNameHolder(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, map[string]string{
		"manual_session": "true",
	})
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks+2, store, cfg, sessionName, runtime.NewFake())
	assertBeadOpen(t, mem, holder.ID, "manual holder")
}

// ACCEPT: a named session with NO pending-create claim (the healthy shape — an
// asleep on-demand session waiting to be woken) is never a candidate. This is
// the class that carries real continuity with no runtime to probe, so the
// runtime gate cannot protect it; the CLAIM gate is the one that must.
func TestSyncSessionBeads_KeepsAsleepNamedSessionNameHolderWithoutPendingCreate(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, map[string]string{
		"state":                     string(session.StateAsleep),
		"session_key":               "sk-live-conversation",
		"pending_create_claim":      "", // delete
		"pending_create_started_at": "", // delete
	})
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks+2, store, cfg, sessionName, runtime.NewFake())
	assertBeadOpen(t, mem, holder.ID, "asleep named holder, no claim")
}

// REJECT/R2: work assigned to the named IDENTITY must NOT block the release.
// An on_demand named session with no visible canonical bead enters the desired
// set ONLY because namedWorkReady[identity] is true, so identity-assigned work
// is the REASON this create is being attempted — gating the release on it makes
// the repair self-cancelling for the incident's own mode.
//
// The demand work must also SURVIVE the release: it is addressed to a durable
// configured identity, not to this dead bead, and clearing its assignee would
// destroy the signal that makes the successor materialize.
func TestSyncSessionBeads_ReleasesDespiteIdentityAssignedDemandWork(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	store := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	work, err := mem.Create(beads.Bead{Title: "demand", Type: "task", Assignee: squatTestIdentity})
	if err != nil {
		t.Fatalf("Create(demand work): %v", err)
	}

	runSquatSyncTicks(t, namedNameReleaseConfirmTicks, store, cfg, sessionName, runtime.NewFake())
	assertBeadClosed(t, mem, holder.ID, "holder with identity-assigned demand work")

	got, err := mem.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	if strings.TrimSpace(got.Assignee) != squatTestIdentity {
		t.Fatalf("demand work assignee = %q after release, want %q preserved — the on_demand wake signal was destroyed", got.Assignee, squatTestIdentity)
	}
}

// ACCEPT/TOCTOU: an async start that commits between the confirming probe and
// the close must abort the release. The fence re-reads the holder BY ID and
// compares the pending-create attempt fingerprint; closeBead's own re-check
// (status == "closed") would not catch this, because a session that just
// started is still OPEN.
func TestSyncSessionBeads_AbortsReleaseWhenCreateCommitsBeforeClose(t *testing.T) {
	cfg := squatTestCity()
	sessionName := squatTestSessionName(t, cfg)
	mem := beads.NewMemStore()
	holder := squattingNamedSessionBead(t, mem, sessionName, 15*time.Minute, nil)
	split := &cacheSplitStore{Store: mem, hiddenID: holder.ID}

	hooked := &hookedGetStore{Store: split}

	// The confirming ticks run with the hook disarmed.
	runSquatSyncTicks(t, namedNameReleaseConfirmTicks-1, hooked, cfg, sessionName, runtime.NewFake())

	// Arm for the releasing tick. Reads of the holder on that tick, in order:
	// (1) the raw read, (2) the typed read the gates run against, (3) the fence
	// re-read. Commit the create just before the fence read observes it.
	hooked.mu.Lock()
	hooked.calls = nil
	hooked.onGet = func(id string, n int) {
		if id != holder.ID || n != 3 {
			return
		}
		if err := mem.SetMetadataBatch(id, map[string]string{
			"state":                string(session.StateActive),
			"pending_create_claim": "",
			"last_woke_at":         time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Errorf("committing the create under the fence: %v", err)
		}
	}
	hooked.mu.Unlock()

	out := runSquatSync(t, hooked, cfg, sessionName, runtime.NewFake())
	if strings.Contains(out, "released session_name") {
		t.Fatalf("released a holder whose create committed under the fence: %q", out)
	}
	// The abort must come from the FENCE, not from an earlier gate — otherwise
	// this test would pass vacuously if the read ordering ever changed.
	if !strings.Contains(out, "changed under us") {
		t.Fatalf("stderr = %q, want the read-compare-write fence to reject the release", out)
	}
	assertBeadOpen(t, mem, holder.ID, "create committed between probe and close")
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

// The confirmation window must be wider than the tmux StateCache stale TTL, or
// N "stopped" answers can all be N reads of the same degraded snapshot. This
// pins the derivation so a future tuning pass cannot quietly re-open the hole.
func TestNamedNameReleaseConfirmWindowClearsRuntimeStaleTTL(t *testing.T) {
	const tmuxStaleTTL = 30 * time.Second // internal/runtime/tmux/state_cache.go
	if namedNameReleaseConfirmWindow <= tmuxStaleTTL {
		t.Fatalf("confirmation window %s must exceed the runtime stale TTL %s", namedNameReleaseConfirmWindow, tmuxStaleTTL)
	}
	if namedNameReleaseConfirmTicks < 2 {
		t.Fatalf("confirmation ticks = %d, want at least 2 consecutive observations", namedNameReleaseConfirmTicks)
	}
}

// LIVENESS, not safety: the confirmation chain must actually be able to close
// at the real tick cadence, or this fix silently never fires. A tick is driven
// by daemon.patrol_interval (default 30s) but can block for a whole
// session.startup_timeout (default 300s) during a start wave — which is exactly
// the fleet state that abandons a half-create. The chain gap must clear that,
// or every slow tick resets the sequence and the wedge is permanent again.
func TestNamedNameReleaseProbeGapToleratesRealTickCadence(t *testing.T) {
	var daemon config.DaemonConfig
	patrol := daemon.PatrolIntervalDuration()
	var sess config.SessionConfig
	startup := sess.StartupTimeoutDuration()

	if namedNameReleaseProbeMaxGap <= patrol {
		t.Fatalf("chain gap %s must exceed the default patrol interval %s", namedNameReleaseProbeMaxGap, patrol)
	}
	if namedNameReleaseProbeMaxGap <= startup {
		t.Fatalf("chain gap %s must exceed the default startup timeout %s — a start wave would reset the chain every time and the recovery would never converge", namedNameReleaseProbeMaxGap, startup)
	}
	// And the window must be reachable in a bounded number of ticks.
	if ticks := int(namedNameReleaseConfirmWindow/patrol) + 1; ticks > 20 {
		t.Fatalf("confirmation window %s needs %d patrol ticks at %s; recovery latency is unreasonable", namedNameReleaseConfirmWindow, ticks, patrol)
	}
}

// The close reason must not be one closedNamedSessionReopenEligible rejects.
func TestNamedSessionSquatCloseStateIsReopenEligible(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title: squatTestIdentity,
		Type:  sessionBeadType,
		Labels: []string{
			sessionBeadLabel,
		},
		Metadata: map[string]string{
			"session_name":               "sn",
			namedSessionMetadataKey:      boolMetadata(true),
			namedSessionIdentityMetadata: squatTestIdentity,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadataBatch(b.ID, session.ClosePatch(time.Now().UTC(), namedSessionSquatCloseState)); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok, err := session.FindClosedNamedSessionBead(store, squatTestIdentity); err != nil || !ok {
		t.Fatalf("close state %q is NOT reopen-eligible (ok=%v err=%v) — continuity would be dropped", namedSessionSquatCloseState, ok, err)
	}
}
