package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

func cloudWakeCheckWith(t *testing.T, agents []config.Agent, sessions []beads.Bead) *doctor.CheckResult {
	t.Helper()
	store := beads.NewMemStore()
	for _, b := range sessions {
		if _, err := store.Create(b); err != nil {
			t.Fatal(err)
		}
	}
	check := &cloudWakeDoctorCheck{
		cfg:      &config.City{Agents: agents},
		cityPath: t.TempDir(),
		newStore: func(string) (beads.Store, error) { return store, nil },
	}
	return check.Run(nil)
}

func cloudWakeSessionBead(name string, meta map[string]string) beads.Bead {
	m := map[string]string{"session_name": name, "agent_name": name, "template": name}
	for k, v := range meta {
		m[k] = v
	}
	return beads.Bead{
		Title:     name,
		Type:      session.BeadType,
		Labels:    []string{session.LabelSession},
		Metadata:  m,
		CreatedAt: time.Now(),
	}
}

func TestCloudWakeDoctorNoCloudSeatsIsQuietOK(t *testing.T) {
	r := cloudWakeCheckWith(t, []config.Agent{{Name: "plain"}}, nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v", r.Status)
	}
}

func TestCloudWakeDoctorMissingBindingWarns(t *testing.T) {
	agents := []config.Agent{{Name: "cloudy", WakeTransport: config.WakeTransportClaudeCloud}}
	r := cloudWakeCheckWith(t, agents, []beads.Bead{cloudWakeSessionBead("cloudy", nil)})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s)", r.Status, r.Message)
	}
	if r.Severity != doctor.SeverityAdvisory {
		t.Fatal("cloud-wake findings must be advisory, not gate-blocking")
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "bind-cloud") {
		t.Errorf("finding must name the fix command:\n%s", joined)
	}
}

func TestCloudWakeDoctorSuspectBindingWarns(t *testing.T) {
	agents := []config.Agent{{Name: "cloudy", WakeTransport: config.WakeTransportClaudeCloud}}
	dir := t.TempDir()
	r := cloudWakeCheckWith(t, agents, []beads.Bead{cloudWakeSessionBead("cloudy", map[string]string{
		session.MetadataCloudWakeSessionID:        "session_01DOCTORsuspect0000000000",
		session.MetadataCloudWakeAccountDir:       dir,
		session.MetadataCloudWakeBindingSuspect:   "refused_not_found",
		session.MetadataCloudWakeBindingSuspectAt: "2026-09-06T00:00:00Z",
	})})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s)", r.Status, r.Message)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "SUSPECT") || !strings.Contains(joined, "refused_not_found") {
		t.Errorf("suspect finding must carry the outcome class:\n%s", joined)
	}
}

func TestCloudWakeDoctorHealthyBindingReportsBothFacts(t *testing.T) {
	agents := []config.Agent{{Name: "cloudy", WakeTransport: config.WakeTransportClaudeCloud}}
	dir := t.TempDir()
	r := cloudWakeCheckWith(t, agents, []beads.Bead{cloudWakeSessionBead("cloudy", map[string]string{
		session.MetadataCloudWakeSessionID:     "session_01DOCTORhealthy0000000000",
		session.MetadataCloudWakeAccountDir:    dir,
		session.MetadataCloudWakeLastOutcome:   "queued_remote",
		session.MetadataCloudWakeLastOutcomeAt: "2026-09-06T00:00:00Z",
	})})
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v (%s) details=%v", r.Status, r.Message, r.Details)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, "queued_remote") {
		t.Errorf("reachability fact (last outcome) missing:\n%s", joined)
	}
}
