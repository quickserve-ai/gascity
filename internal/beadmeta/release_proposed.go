package beadmeta

// ReleaseProposedLabel marks a work bead whose release a session teardown
// WITHHELD because the work belongs to a named agent (ga-9n8hjv, fence #1). The
// teardown kept assignee and status exactly as they were; the label is the
// proposal a judge reads, and it is the key a city order queries to wake one.
// Nothing in gc removes it: the judge does, when it releases or keeps the work.
const ReleaseProposedLabel = "release:proposed"

// First-sight record of a withheld release, written alongside
// ReleaseProposedLabel by the same Update. ReleaseProposedAtMetadataKey is also
// the idempotence key: a bead that already carries it is not rewritten, so the
// per-tick orphan sweep proposes once, not once per tick.
const (
	ReleaseProposedAtMetadataKey       = "gc.release_proposed_at"
	ReleaseProposedAssigneeMetadataKey = "gc.release_proposed_assignee"
	ReleaseProposedPathMetadataKey     = "gc.release_proposed_path"
	ReleaseProposedReasonMetadataKey   = "gc.release_proposed_reason"
)
