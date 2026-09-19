package doctor

import (
	"errors"
	"strings"
	"testing"
)

// The contract these tests defend is narrow and load-bearing: this check may
// return StatusOK ONLY when it actually compared two real build identities.
// Every other outcome is StatusSkipped (not assessed) or StatusWarning (a real
// difference). ga-7qkj was filed on a false green, so a test suite that let a
// not-assessed path render OK would be defending the bug.

func TestSupervisorBuildDrift(t *testing.T) {
	probeFailed := errors.New("connection refused")

	tests := []struct {
		name            string
		running         bool
		local           string
		serving         string
		probeErr        error
		drifted         bool
		wantStatus      CheckStatus
		wantMsgContains string
	}{
		{
			name:            "matching builds are the only OK",
			running:         true,
			local:           "f1f5cd76c",
			serving:         "f1f5cd76c",
			wantStatus:      StatusOK,
			wantMsgContains: "f1f5cd76c",
		},
		{
			name:            "a drifted supervisor warns and names both builds",
			running:         true,
			local:           "abc1234ff",
			serving:         "0d312c706",
			drifted:         true,
			wantStatus:      StatusWarning,
			wantMsgContains: "0d312c706",
		},
		{
			name:       "supervisor not running is not assessed",
			running:    false,
			local:      "f1f5cd76c",
			serving:    "f1f5cd76c",
			wantStatus: StatusSkipped,
		},
		{
			name:            "an unreachable /health is not assessed, not healthy",
			running:         true,
			local:           "f1f5cd76c",
			probeErr:        probeFailed,
			wantStatus:      StatusSkipped,
			wantMsgContains: "connection refused",
		},
		{
			name:       "a supervisor that reports no build identity is not assessed",
			running:    true,
			local:      "f1f5cd76c",
			serving:    "",
			wantStatus: StatusSkipped,
		},
		{
			// THE TRAP. `commit` defaults to the literal string "unknown", so a
			// comparison that only guards against "" reads it as a real identity
			// and reports drift against every supervisor that knows its own.
			name:       "the literal placeholder unknown is treated as absent, not as a build",
			running:    true,
			local:      unknownBuildID,
			serving:    "f1f5cd76c",
			drifted:    true,
			wantStatus: StatusSkipped,
		},
		{
			name:       "a supervisor reporting the placeholder is also not assessed",
			running:    true,
			local:      "f1f5cd76c",
			serving:    unknownBuildID,
			drifted:    true,
			wantStatus: StatusSkipped,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serving, probeErr := tc.serving, tc.probeErr
			fetch := func() (string, error) { return serving, probeErr }
			drifted := func(_, _ string) bool { return tc.drifted }
			c := NewSupervisorBuildDriftCheck(tc.running, tc.local, fetch, drifted)
			got := c.Run(nil)

			if got.Status != tc.wantStatus {
				t.Errorf("status = %v, want %v (message: %q)", got.Status, tc.wantStatus, got.Message)
			}
			if tc.wantMsgContains != "" && !strings.Contains(got.Message, tc.wantMsgContains) {
				t.Errorf("message %q does not contain %q", got.Message, tc.wantMsgContains)
			}
			if got.Message == "" {
				t.Error("every outcome must explain itself; message was empty")
			}
		})
	}
}

// TestSupervisorBuildDriftNeverVacuouslyOK is the regression guard for the
// defect itself, stated as an invariant rather than as a list of cases: if the
// two identities were not BOTH usable and actually compared, the result must
// not be OK. A future edit that adds a branch returning OK on a not-assessed
// path fails here even if nobody remembers to add it to the table above.
func TestSupervisorBuildDriftNeverVacuouslyOK(t *testing.T) {
	ids := []string{"", unknownBuildID, "f1f5cd76c", "0d312c706"}
	probes := []error{nil, errors.New("timeout")}

	for _, running := range []bool{true, false} {
		for _, local := range ids {
			for _, serving := range ids {
				for _, probeErr := range probes {
					for _, drifted := range []bool{true, false} {
						serving, probeErr, drifted := serving, probeErr, drifted
						fetch := func() (string, error) { return serving, probeErr }
						cmp := func(_, _ string) bool { return drifted }
						c := NewSupervisorBuildDriftCheck(running, local, fetch, cmp)
						got := c.Run(nil)

						comparable := running && probeErr == nil &&
							usableBuildID(local) && usableBuildID(serving)

						if got.Status == StatusOK && !comparable {
							t.Errorf("vacuous OK: running=%v local=%q serving=%q probeErr=%v -> %q",
								running, local, serving, probeErr, got.Message)
						}
						if got.Status == StatusOK && drifted {
							t.Errorf("reported OK while the comparison said drifted: local=%q serving=%q", local, serving)
						}
						if !got.Status.Assessed() && got.Status.IsFailure() {
							t.Error("a not-assessed status must not also be a failure")
						}
					}
				}
			}
		}
	}
}

// TestSupervisorBuildDriftDoesNotProbeWhenItCannotDecide pins the laziness the
// fetcher exists for: if the supervisor is down, or this binary has no identity
// of its own, the answer is already fixed and the round-trip is waste. A future
// refactor that gathers eagerly would slow every `gc start` warm-up scan that
// then filters this check out.
func TestSupervisorBuildDriftDoesNotProbeWhenItCannotDecide(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		local   string
	}{
		{"supervisor down", false, "f1f5cd76c"},
		{"no local identity", true, ""},
		{"placeholder local identity", true, unknownBuildID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probed := false
			fetch := func() (string, error) {
				probed = true
				return "f1f5cd76c", nil
			}
			c := NewSupervisorBuildDriftCheck(tc.running, tc.local, fetch, func(_, _ string) bool { return false })

			if got := c.Run(nil); got.Status != StatusSkipped {
				t.Errorf("status = %v, want StatusSkipped", got.Status)
			}
			if probed {
				t.Error("probed the supervisor although the outcome could not depend on it")
			}
		})
	}
}

// TestSupervisorBuildDriftSatisfiesCheck fails to compile if the check ever
// stops implementing the doctor Check interface — the omission that would let
// it be written, tested and never registered.
func TestSupervisorBuildDriftSatisfiesCheck(_ *testing.T) {
	var _ Check = NewSupervisorBuildDriftCheck(false, "", nil, nil)
}
