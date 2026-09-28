package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Named-agent release guard (ga-9n8hjv, fence #1 enforcement 2 of 2).
//
// THE RULE. Session teardown never UNASSIGNS or resets a NAMED agent's beads. It
// leaves assignee and status exactly as they are and PROPOSES the release instead:
// a label plus first-sight metadata on the bead, and an audit line. A judge (a
// person, or a seat woken by a city order that reads the label) decides. Only a
// POOL/ephemeral session's work is released autonomously. (Cherub's typed Q12
// re-ruling, recorded on ga-kashye 2026-09-27T03:33Z; spec in katya's
// 2026-09-26T21:53Z comment on ga-9n8hjv.)
//
// WHY AT THE WRITER. On 2026-09-11 gc's own per-bead writer cleared 50 beads off a
// named agent 8 s after its session bead closed, 11 of them in_progress -> open, and
// the owner's resume check then reported "no work". The guards added since sat in
// two of the callers (releasableAssigneeIdentities, and the per-identity skip in
// unclaimWorkAssignedToRetiredSessionBead) and covered only a LIVE configured seat
// under its exact identity. The drain-ack release, the stranded-repair sweep and
// the orphan-close tie-break had no named check at all, and a suspended or removed
// named agent, work held under the session bead ID or alias_history, and the
// runtime-name spelling were released by design. Guarding callers one by one is
// how those gaps happened, so the guard is a REQUIRED argument of the two release
// writers (ReleaseWorkBead and releaseOrphanedPoolAssignment): a release path added
// later has to construct one, and cannot inherit no-guard by omission.
//
// FAIL CLOSED. The zero value withholds every release. Only the constructors below,
// which look at the torn-down session and the city config, can answer "this is pool
// work, release it".
//
// WHAT COUNTS AS NAMED. Either fact is enough:
//   - the torn-down session is a named session (configured_named_session, or a
//     non-empty configured_named_identity), read from the session's own metadata,
//     so it holds with no config and for an agent since removed from it; or
//   - the bead's assignee resolves to a configured [[named_session]] through
//     findNamedSessionSpecForAssignee, which accepts the runtime-name form
//     ("qcore--archer") and deliberately IGNORES suspension: a suspended named
//     agent's work is exactly what the ruling moves from "released" to "proposed".
//
// RESIDUAL. With no session and no config entry (the orphan sweep meeting a bare
// assignee of an agent removed from config) nothing identifies the assignee as
// named, and the pool release proceeds as before. The sweep's roster gate still
// protects the rig-qualified form.
// mutantGuardDisabled is the MUTANT switch (do not merge).
var mutantGuardDisabled = true

type namedReleaseGuard struct {
	built        bool
	cfg          *config.City
	cityName     string
	sessionID    string
	sessionNamed bool
}

// namedReleaseGuardForSessionBead builds the guard for work held by a torn-down
// session bead. cfg may be nil; the session's own metadata still decides.
func namedReleaseGuardForSessionBead(cfg *config.City, sb beads.Bead) namedReleaseGuard {
	g := newNamedReleaseGuard(cfg, sb.ID)
	g.sessionNamed = isNamedSessionBead(sb) ||
		strings.TrimSpace(sb.Metadata["configured_named_identity"]) != "" ||
		g.anyResolvesToNamedSession(sb.Metadata["session_name"], sb.Metadata["alias"])
	return g
}

// namedReleaseGuardForSessionInfo is the session.Info mirror of
// namedReleaseGuardForSessionBead.
func namedReleaseGuardForSessionInfo(cfg *config.City, info session.Info) namedReleaseGuard {
	g := newNamedReleaseGuard(cfg, info.ID)
	g.sessionNamed = isNamedSessionInfo(info) ||
		strings.TrimSpace(info.ConfiguredNamedIdentity) != "" ||
		g.anyResolvesToNamedSession(info.SessionName, info.Alias)
	return g
}

// namedReleaseGuardForAssignee builds the guard for a release that has no session
// in hand (the orphan sweep): only the assignee itself can mark the work as named.
func namedReleaseGuardForAssignee(cfg *config.City) namedReleaseGuard {
	return newNamedReleaseGuard(cfg, "")
}

func newNamedReleaseGuard(cfg *config.City, sessionID string) namedReleaseGuard {
	g := namedReleaseGuard{built: true, cfg: cfg, sessionID: strings.TrimSpace(sessionID)}
	if cfg != nil {
		g.cityName = cfg.EffectiveCityName()
	}
	return g
}

// anyResolvesToNamedSession reports whether one of a SESSION's own names is
// exactly a configured named session's qualified identity or runtime session
// name. It deliberately does NOT use findNamedSessionSpecForAssignee, whose first
// step accepts the V2 bare shorthand ("ray" for "team.ray"): a pool session whose
// alias happens to equal such a shorthand would otherwise mark the WHOLE pool
// session as named and withhold every release it makes, stranding pool work.
func (g namedReleaseGuard) anyResolvesToNamedSession(values ...string) bool {
	if g.cfg == nil {
		return false
	}
	for i := range g.cfg.NamedSessions {
		identity := g.cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(g.cfg, g.cityName, identity)
		if !ok {
			continue
		}
		for _, v := range values {
			if v = strings.TrimSpace(v); v != "" && namedSessionAssigneeMatchesSpec(spec, identity, v) {
				return true
			}
		}
	}
	return false
}

// withholdReasonForBead is withholdReason plus one fact only the bead carries: a
// release proposal already pending on it. A pending proposal is the judge's, so
// no writer releases the bead until the judge clears it. Without this, a named
// session's handle-held work proposed at teardown would be released by the next
// orphan sweep, which sees only a dead session ID it cannot resolve as named.
func (g namedReleaseGuard) withholdReasonForBead(item beads.Bead) string {
	if mutantGuardDisabled {
		return "" // MUTANT: guard disabled
	}
	if releaseProposalPending(item) {
		return "a release proposal is pending for a judge"
	}
	return g.withholdReason(item.Assignee)
}

// releaseProposalPending reports whether item carries an unjudged release
// proposal: the label, or the first-sight stamp (labels are not hydrated on
// every read path; metadata is).
func releaseProposalPending(item beads.Bead) bool {
	return strings.TrimSpace(item.Metadata[beadmeta.ReleaseProposedAtMetadataKey]) != "" ||
		hasLabel(item.Labels, beadmeta.ReleaseProposedLabel)
}

// withholdReason reports why releasing work held under assignee must become a
// proposal instead, or "" when the release may proceed.
func (g namedReleaseGuard) withholdReason(assignee string) string {
	if mutantGuardDisabled {
		return "" // MUTANT: guard disabled
	}
	if !g.built {
		return "no named-release guard was constructed for this release (fails closed)"
	}
	if g.sessionNamed {
		return fmt.Sprintf("session %s serves a named agent", g.sessionID)
	}
	if g.cfg != nil {
		if spec, ok := findNamedSessionSpecForAssignee(g.cfg, g.cityName, strings.TrimSpace(assignee)); ok {
			return fmt.Sprintf("assignee resolves to named session %s", spec.Identity)
		}
	}
	return ""
}

// proposeNamedRelease records a withheld release on the bead instead of making it:
// it adds beadmeta.ReleaseProposedLabel and stamps the first-sight proposal
// metadata, leaving assignee and status untouched. It writes only when no
// proposal is pending on the snapshot (releaseProposalPending), so the per-tick
// orphan sweep does not rewrite the bead every tick; the audit line is emitted
// only when it writes. It returns whether it wrote.
//
// The judge discharges a proposal by removing the label AND clearing
// ReleaseProposedAtMetadataKey (the order's mail gives the exact command); a
// cleared proposal re-arms, so a later teardown proposes afresh. Because a
// present label also counts as pending, the label is only ever added to a bead
// that lacks it, so a store that appends labels does not accumulate duplicates.
//
// beads.Store has no comment verb, so the proposal is label + metadata. Both are
// visible on `gc bd show` and the label is queryable by the judge's order.
func proposeNamedRelease(store beads.Store, item beads.Bead, reason, releasePath string, audit io.Writer) (bool, error) {
	if audit == nil {
		audit = io.Discard
	}
	if releaseProposalPending(item) {
		return false, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := store.Update(item.ID, beads.UpdateOpts{
		Labels: []string{beadmeta.ReleaseProposedLabel},
		Metadata: map[string]string{
			beadmeta.ReleaseProposedAtMetadataKey:       now,
			beadmeta.ReleaseProposedAssigneeMetadataKey: item.Assignee,
			beadmeta.ReleaseProposedPathMetadataKey:     releasePath,
			beadmeta.ReleaseProposedReasonMetadataKey:   reason,
		},
	}); err != nil {
		return false, fmt.Errorf("proposing release of %q: %w", item.ID, err)
	}
	fmt.Fprintf(audit, "%s session beads: WITHHELD work %s: assignee %q and status %s kept, release PROPOSED (label %s), path=%s, reason=%s\n", //nolint:errcheck // best-effort audit; the proposal itself already landed
		now, item.ID, item.Assignee, item.Status, beadmeta.ReleaseProposedLabel, releasePath, reason)
	return true, nil
}

// assigneeIsDurableNamedIdentity reports whether work held under assignee is held
// under a named agent's DURABLE identity (the session's own
// configured_named_identity, or any spelling that resolves to a configured
// [[named_session]]), as opposed to a session handle such as a bead ID.
//
// Duplicate named-session repair uses it to leave such work where it is. It used
// to re-home everything the losing bead held onto the WINNER's session bead ID,
// and a bead ID is always releasable, so the next close of the winner stripped
// the whole portfolio even with every close-path guard intact (ga-9n8hjv). The
// winner serves the same identity, so work left on the identity is already its.
func assigneeIsDurableNamedIdentity(cfg *config.City, sessionIdentity, assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return false
	}
	if id := strings.TrimSpace(sessionIdentity); id != "" && id == assignee {
		return true
	}
	return namedReleaseGuardForAssignee(cfg).withholdReason(assignee) != ""
}
