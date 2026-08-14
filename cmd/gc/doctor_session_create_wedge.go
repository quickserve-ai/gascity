package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

// wedgedSessionCreateThreshold is how long a session bead may sit in a
// transitional state (start-pending / creating) before the doctor calls its
// create WEDGED rather than slow.
//
// It is deliberately bound to the reconciler's own never-started rollback
// floor: past pendingCreateNeverStartedTimeout the recovery path that owns
// this bead has already decided the lease expired, so a bead that is STILL
// transitional is one whose recovery did not fire. Deriving the threshold
// from the reconciler constant means the alarm cannot drift away from the
// mechanism it is watching.
const wedgedSessionCreateThreshold = pendingCreateNeverStartedTimeout

// blockedNamedIdentityGrace is the minimum age of a bead squatting a
// configured named session's reserved runtime name before the squat is
// reported. A healthy named-session bead is stamped with
// configured_named_identity in the same create that stamps its session_name,
// so an unattributed holder is anomalous immediately; the grace only absorbs
// a half-written create observed mid-flight.
const blockedNamedIdentityGrace = 2 * time.Minute

// sessionCreateWedgeCheck alarms on session creates that can never complete.
//
// It exists because of ga-2otk73: qcore/gastown.refinery was down for ~15
// minutes because session bead ga-wisp-r4jw5ee sat in state=start-pending
// holding the runtime session name "qcore--gastown__refinery" with no tmux
// session and no refinery process behind it. Every reconciler cycle failed
// with `session name already exists: "qcore--gastown__refinery" already
// belongs to ga-wisp-r4jw5ee` — 62 consecutive failures, ZERO operator-visible
// signal. ga-qcuz36 records the same class from a prior incident where a
// session sat in state=creating for fourteen days unnoticed.
//
// The repair is trivial once a human knows (close the squatting bead); the
// expensive part is that nothing said so. This check is that signal.
//
// It reports two families:
//
//   - blocked: a configured named identity whose reserved runtime session name
//     is held by a session bead that is not attributable to that identity and
//     has no live runtime. That identity CANNOT be created until the squatter
//     is closed — a named session's name is fixed, so a squat is a permanent
//     outage for that agent (unlike a pool worker, whose generated name makes
//     a squat cost exactly one slot).
//   - wedged: any session bead stuck in a transitional state past
//     wedgedSessionCreateThreshold with no live runtime.
//
// Detection only. It never closes a bead: deciding which of two claimants to
// the same identity survives is an operator call.
type sessionCreateWedgeCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
	// newIsRunning builds the provider-runtime liveness probe when the check
	// runs. Liveness is the suppressor that keeps this check quiet: a session
	// name with a live runtime behind it is not a wedged create, whatever the
	// bead metadata says. Constructing it lazily keeps the provider's own setup
	// cost inside this check's per-check time budget instead of charging it to
	// doctor's registration phase. A nil factory or a nil probe means liveness
	// is unknowable, and the check declines to run rather than guess.
	newIsRunning func() func(string) bool
	now          func() time.Time
}

func newSessionCreateWedgeCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error), newIsRunning func() func(string) bool) *sessionCreateWedgeCheck {
	return &sessionCreateWedgeCheck{cfg: cfg, cityPath: cityPath, newStore: newStore, newIsRunning: newIsRunning, now: time.Now}
}

// doctorSessionRuntimeLiveness builds the session-name liveness probe from the
// configured runtime provider. A construction failure yields a nil probe, which
// the wedge check reads as "liveness unknowable" and reports as a skip — never
// as a finding.
func doctorSessionRuntimeLiveness() func(string) bool {
	sp, err := newSessionProvider()
	if err != nil {
		return nil
	}
	return sp.IsRunning
}

// Name returns the check's identifier.
func (c *sessionCreateWedgeCheck) Name() string { return "session-create-wedge" }

// CanFix reports that this check is detection-only.
func (c *sessionCreateWedgeCheck) CanFix() bool { return false }

// Fix is a no-op; closing a squatting session bead is an operator decision.
func (c *sessionCreateWedgeCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run loads the open session-bead set once and derives both finding families
// from it in memory. Every degraded input (no config, no store, no runtime
// probe, unparseable timestamps) yields a skip or a silent pass, never a
// finding — a check that cries wolf on a half-loaded workspace gets ignored,
// which is strictly worse than not having it.
func (c *sessionCreateWedgeCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c == nil || c.cfg == nil || c.newStore == nil || c.newIsRunning == nil {
		return okCheck("session-create-wedge", "no session create wedges detected")
	}
	isRunning := c.newIsRunning()
	if isRunning == nil {
		return skippedSessionCreateWedgeCheck(c.Name(), "no runtime provider")
	}
	store, err := c.newStore(c.cityPath)
	if err != nil {
		return skippedSessionCreateWedgeCheck(c.Name(), fmt.Sprintf("opening bead store: %v", err))
	}
	infos, err := loadSessionCreateWedgeInfos(store)
	if err != nil {
		return skippedSessionCreateWedgeCheck(c.Name(), err.Error())
	}

	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	findings := analyzeSessionCreateWedge(c.cfg, loadedCityName(c.cfg, c.cityPath), infos, isRunning, now)
	return sessionCreateWedgeResult(c.Name(), findings)
}

// skippedSessionCreateWedgeCheck reports a diagnostic that could not run. It
// is a warning, not an error: the outcome is unknown, and the underlying
// cause (missing store, missing provider) is already reported by the core
// check that owns it.
func skippedSessionCreateWedgeCheck(name, reason string) *doctor.CheckResult {
	return &doctor.CheckResult{
		Name:     name,
		Status:   doctor.StatusWarning,
		Severity: doctor.SeverityAdvisory,
		Message:  fmt.Sprintf("session create-wedge diagnostics skipped: %s", reason),
		FixHint:  "fix the reported store/provider access and rerun gc doctor",
	}
}

// loadSessionCreateWedgeInfos reads the OPEN session-bead set as typed
// session.Info rows.
//
// Cost is fixed at two indexed store queries regardless of city size, and the
// result set is bounded by the number of OPEN sessions (closed history is
// deliberately excluded — it grows without bound and a closed bead is not a
// wedged create). No filesystem walk and no log read: the ga-22tvtm /
// ga-ciymkk defect class is a doctor check that outgrows its per-check budget,
// so this one derives everything from one bounded query.
//
// Both legs read TierMode: TierBoth. A half-created session bead can land in
// the wisp tier, where a default single-tier list cannot see it — a name claim
// the reconciler's own list cannot observe but the name-availability check
// can is precisely the deadlock shape of ga-2otk73, and a check that reads the
// same blind list as the mechanism it audits would reproduce the blindness.
// The type leg and the label leg are unioned so a bead that lost its type or
// its label after a crash still surfaces.
func loadSessionCreateWedgeInfos(store beads.Store) ([]session.Info, error) {
	if store == nil {
		return nil, nil
	}
	queries := []beads.ListQuery{
		{Type: session.BeadType, TierMode: beads.TierBoth, Sort: beads.SortCreatedAsc},
		{Label: session.LabelSession, TierMode: beads.TierBoth, Sort: beads.SortCreatedAsc},
	}
	seen := make(map[string]bool)
	var raw []beads.Bead
	for _, q := range queries {
		items, err := store.List(q)
		if err != nil {
			return nil, fmt.Errorf("listing session beads: %w", err)
		}
		for _, item := range items {
			if seen[item.ID] || item.Status == "closed" || !session.IsSessionBeadOrRepairable(item) {
				continue
			}
			seen[item.ID] = true
			raw = append(raw, item)
		}
	}
	rows := session.ReconcileRowsFromBeads(raw)
	infos := make([]session.Info, 0, len(rows))
	for _, row := range rows {
		infos = append(infos, row.Info)
	}
	return infos, nil
}

// sessionCreateWedgeFindings separates the two families so the caller can
// pick a status: a blocked named identity is an outage, a wedged transitional
// session on its own is a warning.
type sessionCreateWedgeFindings struct {
	blocked []string
	wedged  []string
}

// analyzeSessionCreateWedge is the whole diagnosis, as a pure function over an
// already-loaded session set. Everything here is in-memory map work, O(open
// sessions + configured named sessions).
func analyzeSessionCreateWedge(cfg *config.City, cityName string, infos []session.Info, isRunning func(string) bool, now time.Time) sessionCreateWedgeFindings {
	var out sessionCreateWedgeFindings
	if cfg == nil || isRunning == nil {
		return out
	}

	holders := make(map[string][]session.Info, len(infos))
	for _, info := range infos {
		if info.Closed {
			continue
		}
		if name := strings.TrimSpace(info.SessionNameMetadata); name != "" {
			holders[name] = append(holders[name], info)
		}
	}

	// Family 1 — a configured named identity whose reserved runtime name is
	// squatted. This is the ga-2otk73 shape and the one that names the bead
	// the operator has to close.
	reported := make(map[string]bool)
	for i := range cfg.NamedSessions {
		identity := session.NormalizeNamedSessionTarget(cfg.NamedSessions[i].QualifiedName())
		spec, ok := session.FindNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		reserved := strings.TrimSpace(spec.SessionName)
		if reserved == "" {
			continue
		}
		candidates := holders[reserved]
		if len(candidates) == 0 {
			// Nothing holds the name — the identity is free to materialize.
			continue
		}
		var squatters []session.Info
		attributed := false
		for _, holder := range candidates {
			if session.NormalizeNamedSessionTarget(session.NamedSessionIdentityInfo(holder)) == spec.Identity {
				// The canonical bead for this identity owns its own name.
				attributed = true
				break
			}
			// A holder that matches the spec only by its legacy
			// template/agent_name signal but has SETTLED into a live state is
			// plausibly this identity's own pre-identity-stamp bead, so it is
			// left alone. The ga-2otk73 shape is the opposite: it matches by
			// template yet never left start-pending — it owns the name without
			// ever having become the session. Trading that recall for silence
			// is deliberate; a check that fires on legacy-but-fine beads gets
			// muted, and then it catches nothing at all.
			if session.NamedSessionInfoMatchesSpec(holder, spec) && !isTransitionalSessionInfo(holder) {
				attributed = true
				break
			}
			if age, ok := sessionCreateWedgeAge(holder, now); ok && age >= blockedNamedIdentityGrace {
				squatters = append(squatters, holder)
			}
		}
		if attributed || len(squatters) == 0 {
			continue
		}
		// A live runtime under the reserved name means something really is
		// serving this identity (an adopted or legacy session). That is the
		// orphan/zombie checks' territory, not a create wedge.
		if isRunning(reserved) {
			continue
		}
		sort.Slice(squatters, func(a, b int) bool { return squatters[a].ID < squatters[b].ID })
		descriptions := make([]string, 0, len(squatters))
		ids := make([]string, 0, len(squatters))
		for _, squatter := range squatters {
			reported[squatter.ID] = true
			descriptions = append(descriptions, describeSessionCreateWedgeSquatter(squatter, now))
			ids = append(ids, squatter.ID)
		}
		out.blocked = append(out.blocked, fmt.Sprintf(
			"%s: session-bead creation is BLOCKED — reserved runtime name %q is held by %s; no live runtime exists for that name. Close the squatting bead to unblock: gc bd close %s",
			spec.Identity, reserved, strings.Join(descriptions, ", "), strings.Join(ids, " ")))
	}

	// Family 2 — any session stuck mid-create, named or pooled. A squatter
	// already reported above is not repeated here; its blocked-identity line
	// carries strictly more information.
	for _, info := range infos {
		if info.Closed || reported[info.ID] || !isTransitionalSessionInfo(info) {
			continue
		}
		state := strings.TrimSpace(info.MetadataState)
		age, ok := sessionCreateWedgeAge(info, now)
		if !ok || age < wedgedSessionCreateThreshold {
			continue
		}
		name := strings.TrimSpace(info.SessionNameMetadata)
		if name != "" && isRunning(name) {
			continue
		}
		out.wedged = append(out.wedged, fmt.Sprintf(
			"%s: stuck in state=%s for %s with no live runtime (session_name=%q, %s) — the create never completed and no rollback fired",
			info.ID, state, age.Round(time.Second), name, sessionCreateWedgeOwnerLabel(info)))
	}

	sort.Strings(out.blocked)
	sort.Strings(out.wedged)
	return out
}

// sessionCreateWedgeResult renders the findings. A blocked named identity is
// an outage that will not self-heal, so it reports StatusError; it is marked
// SeverityAdvisory so it stays loud in `gc doctor` without gating unrelated
// automation that shells out to the exit code.
func sessionCreateWedgeResult(name string, findings sessionCreateWedgeFindings) *doctor.CheckResult {
	if len(findings.blocked) == 0 && len(findings.wedged) == 0 {
		return okCheck(name, "no session creates are wedged")
	}
	details := append(append([]string{}, findings.blocked...), findings.wedged...)
	hint := "close the squatting session bead named above (gc bd close <bead-id>); the supervisor recreates the session bead and the agent returns within one reconcile tick"
	if len(findings.blocked) == 0 {
		return &doctor.CheckResult{
			Name:     name,
			Status:   doctor.StatusWarning,
			Severity: doctor.SeverityAdvisory,
			Message:  fmt.Sprintf("%d session(s) stuck mid-create with no live runtime", len(findings.wedged)),
			FixHint:  hint,
			Details:  details,
		}
	}
	message := fmt.Sprintf("%d named identity(ies) cannot start: their reserved session name is squatted", len(findings.blocked))
	if len(findings.wedged) > 0 {
		message = fmt.Sprintf("%s (and %d other session(s) stuck mid-create)", message, len(findings.wedged))
	}
	return &doctor.CheckResult{
		Name:     name,
		Status:   doctor.StatusError,
		Severity: doctor.SeverityAdvisory,
		Message:  message,
		FixHint:  hint,
		Details:  details,
	}
}

// isTransitionalSessionInfo reports whether a session bead is mid-create.
// start-pending and creating are the two states a bead occupies before its
// runtime is confirmed; reaching any live state clears them.
func isTransitionalSessionInfo(info session.Info) bool {
	switch strings.TrimSpace(info.MetadataState) {
	case string(session.StateStartPending), string(session.StateCreating):
		return true
	}
	return false
}

// sessionCreateWedgeAge measures how long a session bead has held its create
// claim. pending_create_started_at is the lease anchor the create path stamps;
// CreatedAt is the legacy fallback. A bead with neither is unmeasurable, so it
// is skipped rather than assumed stale — this check must not manufacture a
// finding out of missing data.
func sessionCreateWedgeAge(info session.Info, now time.Time) (time.Duration, bool) {
	if started, ok := parseRFC3339Metadata(info.PendingCreateStartedAt); ok {
		return now.Sub(started), true
	}
	if !info.CreatedAt.IsZero() {
		return now.Sub(info.CreatedAt), true
	}
	return 0, false
}

// describeSessionCreateWedgeSquatter renders one squatting bead with the
// fields an operator needs to decide it is safe to close.
func describeSessionCreateWedgeSquatter(info session.Info, now time.Time) string {
	state := strings.TrimSpace(info.MetadataState)
	if state == "" {
		state = "(unset)"
	}
	age := "unknown age"
	if d, ok := sessionCreateWedgeAge(info, now); ok {
		age = fmt.Sprintf("age=%s", d.Round(time.Second))
	}
	return fmt.Sprintf("session bead %s (state=%s, %s, %s)", info.ID, state, age, sessionCreateWedgeOwnerLabel(info))
}

// sessionCreateWedgeOwnerLabel names whatever identity the bead carries, so a
// finding is legible even when the bead is the half-created kind that lost its
// agent labels.
func sessionCreateWedgeOwnerLabel(info session.Info) string {
	for _, candidate := range []struct {
		key   string
		value string
	}{
		{"identity", session.NamedSessionIdentityInfo(info)},
		{"alias", strings.TrimSpace(info.Alias)},
		{"agent", strings.TrimSpace(info.AgentName)},
		{"template", strings.TrimSpace(info.Template)},
	} {
		if candidate.value != "" {
			return candidate.key + "=" + candidate.value
		}
	}
	return "identity=unknown"
}
