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
	hbErr   map[string]error // bead id -> scripted refusal
	beats   [][2]string      // {id, actor} in invocation order
}

func (f *fakeHeartbeatStore) ListByAssignee(assignee, status string, _ int) ([]beads.Bead, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if status != "in_progress" {
		return nil, nil
	}
	return f.rows[assignee], nil
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
