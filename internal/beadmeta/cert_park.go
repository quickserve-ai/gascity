package beadmeta

import "strings"

// CertWaitHoldLabel parks an OWNED bead on a westeros certification wait under
// the park-and-pivot doctrine (ga-5zosxs): the owner keeps the bead in_progress,
// the cert-landing-patrol watches it, and the owner pivots to other work.
//
// The patrol's resume is a label flip, written durably BEFORE it nudges the
// owner: it removes this label and adds one of CertResumeLabels. That flip —
// not the nudge — is the restart-safe signal that the owner has work again,
// which is why every consumer that stops treating a parked bead as wake demand
// must key on the LABELS and let their removal restore demand on its own
// (ga-mzovhi).
const CertWaitHoldLabel = "hold:cert-wait"

// CertLandedLabel and CertActionLabel are the cert-landing-patrol's two resume
// flips (ga-5zosxs doctrine v1.1): every summoned lane landed clean, or some
// lane landed with findings to adjudicate. Either one means the owner must be
// woken for this bead.
const (
	CertLandedLabel = "cert:landed"
	CertActionLabel = "cert:action"
)

// CertResumeLabels is the complete set of resume flips. A resume label outranks
// CertWaitHoldLabel wherever both appear, so a flip that added its resume label
// but has not (yet) removed the hold still wakes the owner.
var CertResumeLabels = []string{CertLandedLabel, CertActionLabel}

// CertParkSuppressesAssignedWake reports whether labels park an owned bead on a
// certification wait that has not resumed: CertWaitHoldLabel is present and no
// CertResumeLabels value is.
//
// It answers exactly one question — "should this assignment, by itself, wake
// its sleeping owner?" — and the answer for a parked bead is no, because the
// owner can do nothing with it until the patrol flips it (ga-mzovhi: a parked
// pool seat was respawned every 1-2 minutes, found nothing actionable, and
// drained again). It is deliberately NOT a serve or ownership rule: the
// assignment is still a real ownership fact, so existence, orphan-release and
// crash-recovery accounting must keep counting the bead (the serve/exist split
// documented on HoldLabelPrefix and DispatchHoldLabels).
//
// Matching is exact after trimming, the same spelling the patrol writes. An
// absent or empty label set is not parked, so a read that could not carry
// labels fails toward waking — the pre-park behavior — never toward a stall.
func CertParkSuppressesAssignedWake(labels []string) bool {
	parked := false
	for _, label := range labels {
		switch strings.TrimSpace(label) {
		case CertWaitHoldLabel:
			parked = true
		case CertLandedLabel, CertActionLabel:
			return false
		}
	}
	return parked
}
