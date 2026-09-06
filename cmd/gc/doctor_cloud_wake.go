package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/notify"
	"github.com/gastownhall/gascity/internal/notify/claudecloud"
	"github.com/gastownhall/gascity/internal/session"
)

// cloudWakeDoctorCheck reports, for every seat selecting
// wake_transport=claude-cloud, the two binding facts the design keeps
// separate (claudemsg-bridge-design.md §5.2/§5.5): VALIDITY (a binding
// exists, its account lineage names a real directory) and REACHABILITY
// (the last send outcome, and whether a refusal has marked the binding
// suspect). There is deliberately no boolean "is running" — a cloud seat
// has no runtime gc can observe.
type cloudWakeDoctorCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func (c *cloudWakeDoctorCheck) Name() string { return "cloud-wake-bindings" }

func (c *cloudWakeDoctorCheck) CanFix() bool { return false }

func (c *cloudWakeDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *cloudWakeDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "no cloud-wake seats configured"}
	if c == nil || c.cfg == nil || c.newStore == nil {
		return r
	}
	var cloudAgents []config.Agent
	for i := range c.cfg.Agents {
		if strings.TrimSpace(c.cfg.Agents[i].WakeTransport) == config.WakeTransportClaudeCloud {
			cloudAgents = append(cloudAgents, c.cfg.Agents[i])
		}
	}
	if len(cloudAgents) == 0 {
		return r
	}

	store, err := c.newStore(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("cloud-wake diagnostics skipped: %v", err)
		return r
	}
	// Session-class routing: with a [beads.classes.sessions] relocation the
	// generic city store would hold no session beads and every seat would
	// falsely read as unbound.
	store = cliSessionStore(store, c.cfg, c.cityPath)
	sessions, err := loadCloudWakeSessionBeads(store)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("cloud-wake diagnostics skipped: %v", err)
		return r
	}

	// A bare agent name is a valid match key only when exactly one
	// configured agent carries it — otherwise rig-a/worker and rig-b/worker
	// would both claim the same "worker" bead.
	bareNameCount := map[string]int{}
	for i := range c.cfg.Agents {
		bareNameCount[c.cfg.Agents[i].Name]++
	}

	var findings, details []string
	for i := range cloudAgents {
		agent := cloudAgents[i]
		name := agent.QualifiedName()
		b, found := newestSessionBeadForAgent(sessions, agent, bareNameCount[agent.Name] == 1)
		if !found {
			findings = append(findings, fmt.Sprintf("%s: no open session bead — nothing carries a cloud binding", name))
			continue
		}
		id := strings.TrimSpace(b.Metadata[session.MetadataCloudWakeSessionID])
		dir := strings.TrimSpace(b.Metadata[session.MetadataCloudWakeAccountDir])
		suspect := strings.TrimSpace(b.Metadata[session.MetadataCloudWakeBindingSuspect])
		suspectAt := strings.TrimSpace(b.Metadata[session.MetadataCloudWakeBindingSuspectAt])
		lastOutcome := strings.TrimSpace(b.Metadata[session.MetadataCloudWakeLastOutcome])
		lastOutcomeAt := strings.TrimSpace(b.Metadata[session.MetadataCloudWakeLastOutcomeAt])

		// Validity facts.
		switch {
		case id == "":
			findings = append(findings, fmt.Sprintf("%s: session bead %s has no cloud binding — stamp one with `gc session bind-cloud %s --cloud-id ... --account-dir ...`", name, b.ID, b.ID))
			continue
		case !claudecloud.ValidSessionID(id):
			findings = append(findings, fmt.Sprintf("%s: bound cloud session ID %q is not syntactically valid — sends are refused; rebind with `gc session bind-cloud`", name, id))
			continue
		case dir == "":
			findings = append(findings, fmt.Sprintf("%s: binding %s has no account lineage — sends are refused (ambient auth is banned); rebind with --account-dir", name, id))
		default:
			if st, statErr := os.Stat(dir); statErr != nil || !st.IsDir() {
				findings = append(findings, fmt.Sprintf("%s: account lineage dir %q does not exist — sends will fail; rebind with a valid --account-dir", name, dir))
			}
		}
		// Reachability facts.
		if suspect != "" {
			findings = append(findings, fmt.Sprintf("%s: binding %s SUSPECT (%s since %s) — verify the cloud session and rebind with `gc session bind-cloud` (rebinding clears the marker)", name, id, suspect, suspectAt))
			continue
		}
		// An unsuccessful last outcome is a reachability finding even
		// without a suspect marker (policy refusals and ambiguity never
		// suspect the binding, and a best-effort suspect write can fail).
		if lastOutcome != "" && lastOutcome != string(notify.OutcomeQueuedRemote) {
			findings = append(findings, fmt.Sprintf("%s: last send to %s was %s at %s — the seat may not be receiving wakes", name, id, lastOutcome, lastOutcomeAt))
			continue
		}
		fact := fmt.Sprintf("%s: bound to %s", name, id)
		if lastOutcome != "" {
			fact += fmt.Sprintf("; last send %s at %s", lastOutcome, lastOutcomeAt)
		} else {
			fact += "; no send attempted yet"
		}
		details = append(details, fact)
	}

	r.Details = append(findings, details...)
	if len(findings) > 0 {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = fmt.Sprintf("%d cloud-wake binding finding(s) across %d seat(s)", len(findings), len(cloudAgents))
		return r
	}
	r.Message = fmt.Sprintf("%d cloud-wake seat(s) bound and unsuspected", len(cloudAgents))
	return r
}

// loadCloudWakeSessionBeads lists open session beads (type ∪ label union,
// deduped, narrowed to repairable session beads) — the same inline union the
// session-model diagnostic holds under doctor's raw-bead exemption.
func loadCloudWakeSessionBeads(store beads.Store) ([]beads.Bead, error) {
	seen := make(map[string]bool)
	var all []beads.Bead
	for _, q := range []beads.ListQuery{
		{Type: session.BeadType, Sort: beads.SortCreatedAsc},
		{Label: session.LabelSession, Sort: beads.SortCreatedAsc},
	} {
		items, err := store.List(q)
		if err != nil {
			return nil, fmt.Errorf("session beads: %w", err)
		}
		for _, item := range items {
			if seen[item.ID] || item.Status == "closed" || !session.IsSessionBeadOrRepairable(item) {
				continue
			}
			seen[item.ID] = true
			all = append(all, item)
		}
	}
	return all, nil
}

// newestSessionBeadForAgent finds the most recent open session bead whose
// identity metadata names the agent (agent_name, configured identity, or
// template — the same fields nudge-target resolution reads).
func newestSessionBeadForAgent(sessions []beads.Bead, agent config.Agent, bareNameUnique bool) (beads.Bead, bool) {
	names := map[string]bool{agent.QualifiedName(): true}
	if bareNameUnique {
		names[agent.Name] = true
	}
	var best beads.Bead
	found := false
	for _, b := range sessions {
		matched := false
		for _, key := range []string{"agent_name", session.NamedSessionIdentityMetadata, "template"} {
			if names[strings.TrimSpace(b.Metadata[key])] {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if !found || b.CreatedAt.After(best.CreatedAt) {
			best = b
			found = true
		}
	}
	return best, found
}
