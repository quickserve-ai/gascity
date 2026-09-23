package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestRigRestartRecordsLivenessButAlwaysStops pins the two halves of the rig
// restart's termination handling (ga-ksac39, Codex #106 r3 and r4):
//
//   - THE PROBE GATES THE RECORD. A seat that was running is recorded as an
//     operator kill, which counts in the ratio's denominator. A seat that was not
//     running is recorded as observed-dead, which does not. Recording every
//     configured seat as an operator kill inflated the denominator.
//   - THE PROBE NEVER GATES THE STOP. IsRunning is narrower than what Stop cleans
//     up (k8s Stop deletes pods in every phase), so skipping Stop on a false
//     probe left runtime resources behind while the restart reported success.
func TestRigRestartRecordsLivenessButAlwaysStops(t *testing.T) {
	for _, tc := range []struct {
		name     string
		running  bool
		wantKind string
	}{
		{name: "running seat is an operator kill", running: true, wantKind: string(runtime.KindOperatorKill)},
		{name: "not-running seat is still stopped, as observed-dead", running: false, wantKind: string(runtime.KindObservedDead)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newFakeMutatorState(t)
			sessionName := agentSessionName(state.cityName, "myrig/worker", state.cfg.Workspace.SessionTemplate)
			if tc.running {
				if err := state.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
					t.Fatalf("start %s: %v", sessionName, err)
				}
			}
			h := newTestCityHandler(t, state)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rig/myrig/restart"), nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}

			if n := state.sp.CountCalls("Stop", sessionName); n != 1 {
				t.Fatalf("Stop(%s) called %d times, want 1 (the stop must not depend on the liveness probe)", sessionName, n)
			}

			fake, ok := state.eventProv.(*events.Fake)
			if !ok {
				t.Fatalf("event provider is %T, want *events.Fake", state.eventProv)
			}
			var kinds []string
			for _, ev := range fake.Events {
				if ev.Type != events.SessionTerminated || ev.Subject != sessionName {
					continue
				}
				var p events.SessionTerminatedPayload
				if err := json.Unmarshal(ev.Payload, &p); err != nil {
					t.Fatalf("decode session.terminated payload: %v", err)
				}
				kinds = append(kinds, p.Kind)
			}
			if len(kinds) != 1 || kinds[0] != tc.wantKind {
				t.Fatalf("session.terminated kinds for %s = %v, want exactly [%s]", sessionName, kinds, tc.wantKind)
			}
		})
	}
}
