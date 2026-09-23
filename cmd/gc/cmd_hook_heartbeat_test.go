package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// fakeHeartbeatStore scripts the row listing and records every heartbeat with
// the actor it was invoked under, standing in for the BdStore below.
type fakeHeartbeatStore struct {
	rows    map[string][]beads.Bead // assignee -> in_progress rows
	listErr error
	partial error            // when set, List returns rows AND a PartialResultError wrapping it
	hbErr   map[string]error // bead id -> scripted refusal
	beats   [][2]string      // {id, actor} in invocation order
}

func (f *fakeHeartbeatStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Pin the codex P1 fix: the durable-issue default tier filters out
	// ephemeral rows, so the tick must ask for both tiers.
	if q.TierMode != beads.TierBoth {
		return nil, errors.New("tick listed without TierBoth; ephemeral in_progress rows would never be heartbeated")
	}
	if q.Status != "in_progress" {
		return nil, nil
	}
	if f.partial != nil {
		return f.rows[q.Assignee], &beads.PartialResultError{Op: "bd list", Err: f.partial}
	}
	return f.rows[q.Assignee], nil
}

// TestCmdHookHeartbeatBeatsRowsReturnedBesideAPartialListError pins the
// codex round-3 P1: mergeListTierResults (and a single malformed entry)
// return usable rows WITH a PartialResultError. Those rows are healthy claims
// and must still be refreshed; the partial failure is counted, not swallowed,
// and not allowed to discard the rows it came with.
func TestCmdHookHeartbeatBeatsRowsReturnedBesideAPartialListError(t *testing.T) {
	store := &fakeHeartbeatStore{
		rows:    map[string][]beads.Bead{"katya": {{ID: "ga-1", Assignee: "katya"}, {ID: "ga-2", Assignee: "katya"}}},
		partial: errors.New("ephemeral tier: parse row 7"),
		hbErr:   map[string]error{},
	}
	withHeartbeatStore(t, store, nil)
	withHeartbeatIdentities(t, []string{"katya"}, nil)
	t.Setenv("GC_SESSION_ID", "ga-sess")

	var stdout, stderr bytes.Buffer
	if code := cmdHookHeartbeat("", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookHeartbeat = %d, want 0 (lenient); stderr=%s", code, stderr.String())
	}
	if len(store.beats) != 2 {
		t.Fatalf("beats = %v, want both rows refreshed beside the partial error", store.beats)
	}
	if !strings.Contains(stdout.String(), "2 refreshed, 1 refused") {
		t.Fatalf("stdout = %q, want the partial failure counted as 1 refused", stdout.String())
	}
	if !strings.Contains(stderr.String(), "parse row 7") {
		t.Fatalf("stderr = %q, want the partial error surfaced", stderr.String())
	}
	// A total list failure still yields no beats — the partial path is the
	// only one that carries rows.
	store.partial, store.listErr, store.beats = nil, errors.New("bd list: connection refused"), nil
	stdout.Reset()
	stderr.Reset()
	if code := cmdHookHeartbeat("", false, &stdout, &stderr); code != 0 || len(store.beats) != 0 {
		t.Fatalf("total failure: code=%d beats=%v, want 0 and none", code, store.beats)
	}
}

func (f *fakeHeartbeatStore) Heartbeat(id, actor string) error {
	f.beats = append(f.beats, [2]string{id, actor})
	return f.hbErr[id]
}

// withHeartbeatStore installs a fake store opener for one test.
func withHeartbeatStore(t *testing.T, store hookHeartbeatBeadStore, openErr error) {
	t.Helper()
	restore := hookHeartbeatStore
	t.Cleanup(func() { hookHeartbeatStore = restore })
	hookHeartbeatStore = func(context.Context) (hookHeartbeatBeadStore, error) {
		if openErr != nil {
			return nil, openErr
		}
		return store, nil
	}
}

// withHeartbeatIdentities installs a fixed identity set for one test, and
// pins the start offset to 0 so row order in assertions is deterministic
// (production rotates it by wall clock; TestCmdHookHeartbeatRotatesTheStartRow
// covers that seam explicitly).
func withHeartbeatIdentities(t *testing.T, identities []string, err error) {
	t.Helper()
	restore := hookHeartbeatIdentities
	t.Cleanup(func() { hookHeartbeatIdentities = restore })
	hookHeartbeatIdentities = func(context.Context, string, string, io.Writer) ([]string, error) {
		return identities, err
	}
	withHeartbeatStartOffset(t, 0)
}

// withHeartbeatStartOffset pins the rotation seam for one test.
func withHeartbeatStartOffset(t *testing.T, off int) {
	t.Helper()
	restore := hookHeartbeatStartOffset
	t.Cleanup(func() { hookHeartbeatStartOffset = restore })
	hookHeartbeatStartOffset = func(int) int { return off }
}

// TestHookHeartbeatEligibleIdentitiesFencesStaleIncarnations pins the codex
// round-6 P1: the write-authorizing identity set is empty (an error) for a
// closed session bead and for an instance token that is missing or does not
// match the bead — the same fence the claim path applies — and non-empty for
// the live incarnation.
func TestHookHeartbeatEligibleIdentitiesFencesStaleIncarnations(t *testing.T) {
	live := session.Info{ID: "ga-sess", SessionName: "katya", InstanceToken: "tok-live", MetadataState: "active"}
	ids, err := hookHeartbeatEligibleIdentities(live, "tok-live")
	if err != nil || len(ids) == 0 {
		t.Fatalf("live incarnation: ids=%v err=%v, want identities and no error", ids, err)
	}
	// Codex round-8 P1: a DRAINING session is finishing the work it holds and
	// must keep refreshing it, unlike the claim path which refuses new claims.
	draining := live
	draining.MetadataState = string(session.StateDraining)
	if ids, err := hookHeartbeatEligibleIdentities(draining, "tok-live"); err != nil || len(ids) == 0 {
		t.Fatalf("draining incarnation: ids=%v err=%v, want identities and no error", ids, err)
	}
	stopped := live
	stopped.MetadataState = "stopped"
	if ids, err := hookHeartbeatEligibleIdentities(stopped, "tok-live"); err == nil || len(ids) != 0 {
		t.Fatalf("stopped state: ids=%v err=%v, want refusal", ids, err)
	}
	cases := map[string]struct {
		info  session.Info
		token string
	}{
		"closed bead":      {session.Info{ID: "ga-sess", SessionName: "katya", InstanceToken: "tok-live", MetadataState: "active", Closed: true}, "tok-live"},
		"superseded token": {live, "tok-old"},
		"empty token":      {live, ""},
		"bead token empty": {session.Info{ID: "ga-sess", SessionName: "katya", MetadataState: "active"}, "tok-live"},
	}
	for name, tc := range cases {
		ids, err := hookHeartbeatEligibleIdentities(tc.info, tc.token)
		if err == nil || len(ids) != 0 {
			t.Fatalf("%s: ids=%v err=%v, want no identities and an error", name, ids, err)
		}
		if !strings.Contains(err.Error(), "not heartbeat-eligible") {
			t.Fatalf("%s: err = %q, want the fence diagnostic", name, err)
		}
	}
}

// TestHookHeartbeatUnreusedPriorAliasesVouchesOnlyThroughUnclaimedHistory pins
// the codex round-8 P1-b writer set: a prior alias joins the identity set when
// nobody live answers to it (or it resolves back to this session), and is
// excluded — with a diagnostic — when a different live session has taken it,
// when it is ambiguous, or when the lookup fails. Entries already in the
// current set are not duplicated.
func TestHookHeartbeatUnreusedPriorAliasesVouchesOnlyThroughUnclaimedHistory(t *testing.T) {
	info := session.Info{ID: "ga-sess", Alias: "rictus", AliasHistory: []string{"nux", "toast", "capable", "rictus", "  ", "slit", "furiosa"}}
	current := []string{"ga-sess", "rictus"}
	resolve := func(alias string) (string, error) {
		switch alias {
		case "nux":
			return "", session.ErrSessionNotFound // retired name: keep
		case "toast":
			return "ga-other", nil // reused by a live successor: exclude
		case "capable":
			return "ga-sess", nil // resolves to this session: keep
		case "slit":
			return "", session.ErrAmbiguous // two claimants: exclude
		case "furiosa":
			return "", errors.New("store: connection refused") // lookup failed: exclude
		}
		t.Fatalf("unexpected lookup of %q (already-current entries must not be resolved)", alias)
		return "", nil
	}
	var diag bytes.Buffer
	got := hookHeartbeatUnreusedPriorAliases(info, current, resolve, &diag)
	if want := []string{"nux", "capable"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kept prior aliases = %v, want %v", got, want)
	}
	for _, excluded := range []string{`prior alias "toast" is now answered to by live session ga-other`, `prior alias "slit": ambiguous session identifier`, `prior alias "furiosa": store: connection refused`} {
		if !strings.Contains(diag.String(), excluded) {
			t.Errorf("diag = %q, want %q", diag.String(), excluded)
		}
	}
	if strings.Contains(diag.String(), `"nux"`) || strings.Contains(diag.String(), `"capable"`) {
		t.Errorf("diag = %q, must not report a kept alias", diag.String())
	}
}

// TestHookHeartbeatIdentitiesUnionsCurrentAndUnreusedHistory drives the
// production resolver through the front-door seam: a rebranded session whose
// old alias nobody live holds vouches for both names, and the front door is
// opened with the run's own context (codex round-8 P2), not a background one.
func TestHookHeartbeatIdentitiesUnionsCurrentAndUnreusedHistory(t *testing.T) {
	type ctxKey struct{}
	restore := hookHeartbeatSessionFrontDoor
	t.Cleanup(func() { hookHeartbeatSessionFrontDoor = restore })
	var sawCtx context.Context
	hookHeartbeatSessionFrontDoor = func(ctx context.Context) (*session.Store, error) {
		sawCtx = ctx
		store := beads.NewMemStore()
		store.HonorExplicitIDs = true // the seam is keyed on the session bead id the env carries
		if _, err := store.Create(beads.Bead{
			ID: "ga-sess", Title: "polecat", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{"session_name": "polecat-gc-1", "alias": "rictus", "alias_history": "nux", "instance_token": "tok-live", "state": "active"},
		}); err != nil {
			t.Fatalf("Create session bead: %v", err)
		}
		return sessionFrontDoor(store), nil
	}
	ctx := context.WithValue(context.Background(), ctxKey{}, "run")
	var diag bytes.Buffer
	got, err := hookHeartbeatIdentities(ctx, "ga-sess", "tok-live", &diag)
	if err != nil {
		t.Fatalf("hookHeartbeatIdentities: %v (diag=%s)", err, diag.String())
	}
	if want := []string{"ga-sess", "polecat-gc-1", "rictus", "nux"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("identities = %v, want %v", got, want)
	}
	if sawCtx == nil || sawCtx.Value(ctxKey{}) != "run" {
		t.Fatalf("front door opened with %v, want the run's context", sawCtx)
	}
	if diag.Len() != 0 {
		t.Fatalf("diag = %q, want nothing for an unreused alias", diag.String())
	}
}

// TestSessionCurrentClaimFrontDoorContextHonorsCancellation pins the round-8
// P2 shape at the opener: a context already cancelled opens nothing.
func TestSessionCurrentClaimFrontDoorContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	front, err := sessionCurrentClaimFrontDoorContext(ctx)
	if front != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("sessionCurrentClaimFrontDoorContext(cancelled) = %v, %v; want nil and context.Canceled", front, err)
	}
}

// TestCmdHookHeartbeatRotatesTheStartRow pins the codex round-6 P2: the
// deduplicated row list is rotated by the start offset, so a run that times
// out mid-list does not starve the same tail every tick.
func TestCmdHookHeartbeatRotatesTheStartRow(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "ga-sess")
	withHeartbeatIdentities(t, []string{"katya"}, nil)
	withHeartbeatStartOffset(t, 1)
	store := &fakeHeartbeatStore{
		rows:  map[string][]beads.Bead{"katya": {{ID: "ga-1", Assignee: "katya"}, {ID: "ga-2", Assignee: "katya"}, {ID: "ga-3", Assignee: "katya"}}},
		hbErr: map[string]error{},
	}
	withHeartbeatStore(t, store, nil)
	var stdout, stderr bytes.Buffer
	if code := cmdHookHeartbeat("", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookHeartbeat = %d, want 0; stderr=%s", code, stderr.String())
	}
	got := []string{store.beats[0][0], store.beats[1][0], store.beats[2][0]}
	want := []string{"ga-2", "ga-3", "ga-1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("beat order = %v, want rotated %v", got, want)
		}
	}
}

// TestCmdHookHeartbeatBeatsEachRowUnderItsOwnAssignee is the primary path AND
// the cross-spelling contract in one: rows found under different identity
// spellings are each heartbeated with THEIR OWN assignee as actor — bd's owner
// check is exact string equality, so any other actor would be refused.
func TestCmdHookHeartbeatBeatsEachRowUnderItsOwnAssignee(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "mc-sess1")
	withHeartbeatIdentities(t, []string{"katya", "gastown.katya"}, nil)
	store := &fakeHeartbeatStore{rows: map[string][]beads.Bead{
		"katya":         {{ID: "ga-1", Assignee: "katya"}},
		"gastown.katya": {{ID: "ga-2", Assignee: "gastown.katya"}},
	}}
	withHeartbeatStore(t, store, nil)

	var stdout, stderr bytes.Buffer
	if code := cmdHookHeartbeat("", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookHeartbeat = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := [][2]string{{"ga-1", "katya"}, {"ga-2", "gastown.katya"}}
	if len(store.beats) != 2 || store.beats[0] != want[0] || store.beats[1] != want[1] {
		t.Fatalf("beats = %v, want each row under its own assignee: %v", store.beats, want)
	}
	if !strings.Contains(stdout.String(), "2 refreshed, 0 refused") {
		t.Errorf("stdout = %q, want the refreshed/refused summary", stdout.String())
	}
}

// TestCmdHookHeartbeatUnionsRowsAcrossIdentities pins the dedup: a bead listed
// under two identities is heartbeated once.
func TestCmdHookHeartbeatUnionsRowsAcrossIdentities(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "mc-sess1")
	withHeartbeatIdentities(t, []string{"katya", "mc-sess1"}, nil)
	shared := beads.Bead{ID: "ga-1", Assignee: "katya"}
	store := &fakeHeartbeatStore{rows: map[string][]beads.Bead{
		"katya":    {shared},
		"mc-sess1": {shared},
	}}
	withHeartbeatStore(t, store, nil)

	var stdout, stderr bytes.Buffer
	if code := cmdHookHeartbeat("", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookHeartbeat = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(store.beats) != 1 {
		t.Fatalf("beats = %v, want the shared row heartbeated exactly once", store.beats)
	}
}

// TestCmdHookHeartbeatCountsRefusalsInsteadOfSwallowingThem pins consult gap 4:
// a refused heartbeat (spelling, reclaim, closed bead) must be visible in the
// summary and per-row diagnostics — countable — while the remaining rows still
// get their beat, and the lenient default still exits 0.
func TestCmdHookHeartbeatCountsRefusalsInsteadOfSwallowingThem(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "mc-sess1")
	withHeartbeatIdentities(t, []string{"katya"}, nil)
	store := &fakeHeartbeatStore{
		rows: map[string][]beads.Bead{
			"katya": {{ID: "ga-1", Assignee: "katya"}, {ID: "ga-2", Assignee: "katya"}},
		},
		hbErr: map[string]error{"ga-1": errors.New(`issue already claimed by "gastown.katya"`)},
	}
	withHeartbeatStore(t, store, nil)

	var stdout, stderr bytes.Buffer
	if code := cmdHookHeartbeat("", false, &stdout, &stderr); code != 0 {
		t.Fatalf("lenient refusal = exit %d, want 0 (a hook leg must not fail the turn)", code)
	}
	if len(store.beats) != 2 {
		t.Fatalf("beats = %v, want the refusal not to stop the remaining rows", store.beats)
	}
	if !strings.Contains(stdout.String(), "1 refreshed, 1 refused") {
		t.Errorf("stdout = %q, want the refusal counted in the summary", stdout.String())
	}
	if !strings.Contains(stderr.String(), "already claimed by") {
		t.Errorf("stderr = %q, want the per-row refusal surfaced", stderr.String())
	}
}

// TestCmdHookHeartbeatStrictFailsOnAnyRefusal is the other half of the
// contract: under --strict the same refusal exits 1, so a canary or the
// stage-2 liveness proof can assert every beat happened.
func TestCmdHookHeartbeatStrictFailsOnAnyRefusal(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "mc-sess1")
	withHeartbeatIdentities(t, []string{"katya"}, nil)
	store := &fakeHeartbeatStore{
		rows:  map[string][]beads.Bead{"katya": {{ID: "ga-1", Assignee: "katya"}}},
		hbErr: map[string]error{"ga-1": errors.New("issue closed")},
	}
	withHeartbeatStore(t, store, nil)

	var stdout, stderr bytes.Buffer
	if code := cmdHookHeartbeat("", true, &stdout, &stderr); code != 1 {
		t.Fatalf("strict refusal = exit %d, want 1", code)
	}
}

// TestCmdHookHeartbeatStrictTreatsZeroRefreshAsAMiss pins the codex round-4
// P1: every list succeeds but no row matches, so refused stays 0 — under
// --strict that is a miss (the mode exists to prove a heartbeat happened),
// while the lenient default still exits 0 and prints the same diagnostic.
func TestCmdHookHeartbeatStrictTreatsZeroRefreshAsAMiss(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "ga-sess")
	withHeartbeatIdentities(t, []string{"katya", "qcore/cherub-law.katya"}, nil)
	store := &fakeHeartbeatStore{rows: map[string][]beads.Bead{}, hbErr: map[string]error{}}
	withHeartbeatStore(t, store, nil)

	var stdout, stderr bytes.Buffer
	if code := cmdHookHeartbeat("", true, &stdout, &stderr); code != 1 {
		t.Fatalf("strict zero-refresh = exit %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "refreshed nothing") {
		t.Fatalf("stderr = %q, want the zero-refresh diagnostic", stderr.String())
	}
	if len(store.beats) != 0 {
		t.Fatalf("beats = %v, want none", store.beats)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdHookHeartbeat("", false, &stdout, &stderr); code != 0 {
		t.Fatalf("lenient zero-refresh = exit %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "0 refreshed, 0 refused") || !strings.Contains(stderr.String(), "refreshed nothing") {
		t.Fatalf("lenient run must still report: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestCmdHookHeartbeatIDOverrideSkipsSessionResolution pins the canary arm:
// --id heartbeats the named bead under the ambient actor without touching
// session identity or listing anything.
func TestCmdHookHeartbeatIDOverrideSkipsSessionResolution(t *testing.T) {
	t.Setenv("GC_SESSION_ID", "")
	withHeartbeatIdentities(t, nil, errors.New("must not be reached"))
	store := &fakeHeartbeatStore{}
	withHeartbeatStore(t, store, nil)

	var stdout, stderr bytes.Buffer
	if code := cmdHookHeartbeat("ga-7", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookHeartbeat(--id) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(store.beats) != 1 || store.beats[0] != [2]string{"ga-7", ""} {
		t.Fatalf("beats = %v, want ga-7 under the ambient actor", store.beats)
	}
}

// TestCmdHookHeartbeatMissesExitZeroByDefault pins the hook-leg contract for
// the resolution arms: no session identity, unreadable identities, and a store
// that will not open each print a diagnostic and exit 0.
func TestCmdHookHeartbeatMissesExitZeroByDefault(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T)
		wantMsg string
	}{
		{
			name: "no session identity",
			setup: func(t *testing.T) {
				t.Setenv("GC_SESSION_ID", "")
			},
			wantMsg: "no session identity",
		},
		{
			name: "identities unreadable",
			setup: func(t *testing.T) {
				t.Setenv("GC_SESSION_ID", "mc-sess1")
				withHeartbeatIdentities(t, nil, errors.New("session store is down"))
			},
			wantMsg: "session store is down",
		},
		{
			name: "store does not open",
			setup: func(t *testing.T) {
				t.Setenv("GC_SESSION_ID", "mc-sess1")
				withHeartbeatIdentities(t, []string{"katya"}, nil)
				withHeartbeatStore(t, nil, errors.New("store is down"))
			},
			wantMsg: "store is down",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			var stdout, stderr bytes.Buffer
			if code := cmdHookHeartbeat("", false, &stdout, &stderr); code != 0 {
				t.Fatalf("lenient miss = exit %d, want 0 (a hook leg must not fail the turn)", code)
			}
			if !strings.Contains(stderr.String(), tc.wantMsg) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantMsg)
			}
		})
	}
}
