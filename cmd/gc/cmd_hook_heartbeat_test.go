package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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

// withHeartbeatIdentities installs a fixed identity set for one test.
func withHeartbeatIdentities(t *testing.T, identities []string, err error) {
	t.Helper()
	restore := hookHeartbeatIdentities
	t.Cleanup(func() { hookHeartbeatIdentities = restore })
	hookHeartbeatIdentities = func(string) ([]string, error) {
		return identities, err
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
