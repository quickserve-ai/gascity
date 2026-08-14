package doctor

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

var heartbeatNow = time.Date(2026, 8, 13, 17, 33, 4, 0, time.UTC)

// newTestBeadsCacheReconcileCheck builds the check with every external seam
// stubbed: a frozen clock, an in-memory heartbeat table, and a pid-liveness
// probe the test controls. Nothing touches the filesystem or the process table.
func newTestBeadsCacheReconcileCheck(records map[string]beads.ReconcileHeartbeat, scopes ...string) *BeadsCacheReconcileCheck {
	c := NewBeadsCacheReconcileCheck("/city", scopes, true)
	c.now = func() time.Time { return heartbeatNow }
	c.processAlive = func(int) bool { return true }
	c.readHeartbeat = func(_, scope string) (beads.ReconcileHeartbeat, bool, error) {
		hb, ok := records[scope]
		if !ok {
			return beads.ReconcileHeartbeat{}, false, nil
		}
		return hb, true, nil
	}
	return c
}

// healthyHeartbeat is a city cache that reconciled 40 s ago on a 30 s cadence —
// one missed tick, which the reconciler routinely produces under load.
func healthyHeartbeat() beads.ReconcileHeartbeat {
	return beads.ReconcileHeartbeat{
		Scope:           "city",
		Prefix:          "ga",
		PID:             4242,
		ArmedAt:         heartbeatNow.Add(-3 * time.Hour),
		LastReconcileAt: heartbeatNow.Add(-40 * time.Second),
		IntervalMs:      (30 * time.Second).Milliseconds(),
		State:           "live",
		UpdatedAt:       heartbeatNow.Add(-40 * time.Second),
	}
}

// TestBeadsCacheReconcileCheckFiresOnStalledCache reproduces ga-yc0chj: the
// city ("ga") cache last reconciled 3h31m ago on a 30 s cadence while a sibling
// rig kept ticking. The check must name the stalled scope and must NOT be
// silenced by the healthy sibling.
func TestBeadsCacheReconcileCheckFiresOnStalledCache(t *testing.T) {
	stalled := healthyHeartbeat()
	stalled.LastReconcileAt = heartbeatNow.Add(-(3*time.Hour + 31*time.Minute))
	healthyRig := healthyHeartbeat()
	healthyRig.Scope = "qcore"
	healthyRig.Prefix = "qc"
	healthyRig.IntervalMs = (60 * time.Second).Milliseconds()

	c := newTestBeadsCacheReconcileCheck(map[string]beads.ReconcileHeartbeat{
		"city":  stalled,
		"qcore": healthyRig,
	}, "city", "qcore")

	got := c.Run(&CheckContext{})
	if got.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError (message: %s)", got.Status, got.Message)
	}
	if got.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory — the watch reports, it must not gate", got.Severity)
	}
	if !strings.Contains(got.Message, "city") || !strings.Contains(got.Message, "rig=ga") {
		t.Errorf("Message = %q, want it to name the stalled scope and its bead prefix", got.Message)
	}
	if strings.Contains(got.Message, "qcore") {
		t.Errorf("Message = %q, want the healthy sibling rig excluded from the finding", got.Message)
	}
	if got.FixHint == "" {
		t.Error("FixHint is empty; a stalled reconciler needs an operator next step")
	}
}

// TestBeadsCacheReconcileCheckFiresWhenArmedButNeverReconciled covers the
// reconciler that comes up and never completes a cycle: LastReconcileAt stays
// zero, so the staleness clock must run from ArmedAt instead of reading the
// zero timestamp as "unknown" and going quiet.
func TestBeadsCacheReconcileCheckFiresWhenArmedButNeverReconciled(t *testing.T) {
	hb := healthyHeartbeat()
	hb.LastReconcileAt = time.Time{}
	hb.ArmedAt = heartbeatNow.Add(-2 * time.Hour)

	c := newTestBeadsCacheReconcileCheck(map[string]beads.ReconcileHeartbeat{"city": hb}, "city")

	got := c.Run(&CheckContext{})
	if got.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError (message: %s)", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "NEVER completed a reconcile") {
		t.Errorf("Message = %q, want it to distinguish never-reconciled from went-stale", got.Message)
	}
}

// TestBeadsCacheReconcileCheckQuietOnHealthyCache is the accept case. A watch
// that warns on healthy fleets gets ignored, and being ignored is the failure
// this check exists to fix — so the healthy shapes are pinned explicitly.
func TestBeadsCacheReconcileCheckQuietOnHealthyCache(t *testing.T) {
	justArmed := healthyHeartbeat()
	justArmed.Scope = "qcore"
	justArmed.ArmedAt = heartbeatNow.Add(-3 * time.Second)
	justArmed.LastReconcileAt = time.Time{}

	slowCadence := healthyHeartbeat()
	slowCadence.Scope = "astro"
	// LARGE cadence (120 s): 9 minutes stale is inside 5 x 120 s, and would
	// have fired against a SMALL-cadence store's window.
	slowCadence.IntervalMs = (120 * time.Second).Milliseconds()
	slowCadence.LastReconcileAt = heartbeatNow.Add(-9 * time.Minute)

	// SMALL cadence (30 s): 5 x 30 s is 2.5 min, but the floor holds the
	// window at 5 min, so a single slow bd full scan does not alarm.
	underFloor := healthyHeartbeat()
	underFloor.LastReconcileAt = heartbeatNow.Add(-4 * time.Minute)

	c := newTestBeadsCacheReconcileCheck(map[string]beads.ReconcileHeartbeat{
		"city":  underFloor,
		"qcore": justArmed,
		"astro": slowCadence,
	}, "city", "qcore", "astro")

	got := c.Run(&CheckContext{})
	if got.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; message = %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "3 beads cache(s)") {
		t.Errorf("Message = %q, want it to report all 3 watched scopes as evaluated", got.Message)
	}
}

// TestBeadsCacheReconcileCheckQuietOnUnknownData pins the fail-safe direction.
// Every ambiguous input must be quiet: a scope with no record, an unreadable
// record, a malformed record (no arm stamp / no interval), a record left behind
// by a process that has since exited, and a record stamped in the future.
func TestBeadsCacheReconcileCheckQuietOnUnknownData(t *testing.T) {
	veryStale := func(mutate func(*beads.ReconcileHeartbeat)) beads.ReconcileHeartbeat {
		hb := healthyHeartbeat()
		hb.LastReconcileAt = heartbeatNow.Add(-5 * time.Hour)
		mutate(&hb)
		return hb
	}

	cases := []struct {
		name  string
		setup func(c *BeadsCacheReconcileCheck)
	}{
		{
			name: "no record on disk",
			setup: func(c *BeadsCacheReconcileCheck) {
				c.readHeartbeat = func(_, _ string) (beads.ReconcileHeartbeat, bool, error) {
					return beads.ReconcileHeartbeat{}, false, nil
				}
			},
		},
		{
			name: "record unreadable",
			setup: func(c *BeadsCacheReconcileCheck) {
				c.readHeartbeat = func(_, _ string) (beads.ReconcileHeartbeat, bool, error) {
					return beads.ReconcileHeartbeat{}, false, errors.New("permission denied")
				}
			},
		},
		{
			name: "record has no arm stamp",
			setup: func(c *BeadsCacheReconcileCheck) {
				hb := veryStale(func(h *beads.ReconcileHeartbeat) { h.ArmedAt = time.Time{} })
				c.readHeartbeat = func(_, _ string) (beads.ReconcileHeartbeat, bool, error) {
					return hb, true, nil
				}
			},
		},
		{
			name: "record has no reconcile interval",
			setup: func(c *BeadsCacheReconcileCheck) {
				hb := veryStale(func(h *beads.ReconcileHeartbeat) { h.IntervalMs = 0 })
				c.readHeartbeat = func(_, _ string) (beads.ReconcileHeartbeat, bool, error) {
					return hb, true, nil
				}
			},
		},
		{
			name: "writer process is gone",
			setup: func(c *BeadsCacheReconcileCheck) {
				hb := veryStale(func(*beads.ReconcileHeartbeat) {})
				c.readHeartbeat = func(_, _ string) (beads.ReconcileHeartbeat, bool, error) {
					return hb, true, nil
				}
				c.processAlive = func(int) bool { return false }
			},
		},
		{
			name: "heartbeat stamped in the future",
			setup: func(c *BeadsCacheReconcileCheck) {
				hb := veryStale(func(h *beads.ReconcileHeartbeat) {
					h.ArmedAt = heartbeatNow.Add(time.Hour)
					h.LastReconcileAt = heartbeatNow.Add(time.Hour)
				})
				c.readHeartbeat = func(_, _ string) (beads.ReconcileHeartbeat, bool, error) {
					return hb, true, nil
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestBeadsCacheReconcileCheck(nil, "city")
			tc.setup(c)
			got := c.Run(&CheckContext{})
			if got.Status != StatusOK {
				t.Fatalf("Status = %v, want StatusOK (fail quiet); message = %q", got.Status, got.Message)
			}
			if !strings.Contains(got.Message, "nothing to watch") {
				t.Errorf("Message = %q, want the not-evaluated message", got.Message)
			}
		})
	}
}

// TestBeadsCacheReconcileCheckSkipsWithoutController pins that a stopped
// controller is not a stalled cache: every record on disk is then a leftover.
func TestBeadsCacheReconcileCheckSkipsWithoutController(t *testing.T) {
	hb := healthyHeartbeat()
	hb.LastReconcileAt = heartbeatNow.Add(-5 * time.Hour)
	c := newTestBeadsCacheReconcileCheck(map[string]beads.ReconcileHeartbeat{"city": hb}, "city")
	c.controllerRunning = false

	got := c.Run(&CheckContext{})
	if got.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK; message = %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "controller not running") {
		t.Errorf("Message = %q, want the controller-not-running skip", got.Message)
	}
}

// TestBeadsCacheReconcileCheckEndToEndOnDisk exercises the REAL seams: a
// heartbeat written through beads.WriteReconcileHeartbeat, read back through
// beads.ReadReconcileHeartbeat, with the live-process probe pointed at this
// test's own pid. Only the clock is injected. It is the proof that the
// publisher and the watch agree on the on-disk contract, and it renders the
// exact line an operator sees.
func TestBeadsCacheReconcileCheckEndToEndOnDisk(t *testing.T) {
	city := t.TempDir()
	armed := heartbeatNow.Add(-4 * time.Hour)
	stalled := beads.ReconcileHeartbeat{
		Scope:           "city",
		Prefix:          "ga",
		PID:             os.Getpid(),
		ArmedAt:         armed,
		LastReconcileAt: heartbeatNow.Add(-(3*time.Hour + 31*time.Minute)),
		IntervalMs:      (30 * time.Second).Milliseconds(),
		State:           "live",
		UpdatedAt:       heartbeatNow.Add(-(3*time.Hour + 31*time.Minute)),
	}
	if err := beads.WriteReconcileHeartbeat(city, stalled); err != nil {
		t.Fatalf("WriteReconcileHeartbeat: %v", err)
	}
	healthy := stalled
	healthy.Scope = "qcore"
	healthy.Prefix = "qc"
	healthy.IntervalMs = (60 * time.Second).Milliseconds()
	healthy.LastReconcileAt = heartbeatNow.Add(-59 * time.Second)
	if err := beads.WriteReconcileHeartbeat(city, healthy); err != nil {
		t.Fatalf("WriteReconcileHeartbeat: %v", err)
	}

	c := NewBeadsCacheReconcileCheck(city, []string{"city", "qcore"}, true)
	c.now = func() time.Time { return heartbeatNow }

	got := c.Run(&CheckContext{})
	if got.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError; message = %q", got.Status, got.Message)
	}
	var buf strings.Builder
	printResult(&buf, got, true)
	t.Logf("rendered doctor output:\n%s", buf.String())
}
