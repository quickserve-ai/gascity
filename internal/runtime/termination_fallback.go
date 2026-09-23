package runtime

// THE FALLBACK NOTE (ga-ksac39 S2).
//
// Half (b) of this work made every ending leave a RECORD. This is half (a): a
// seat whose session was ended for it must find out, on its next boot, that its
// predecessor's context is gone and that nobody wrote it a note. Today it finds
// nothing at all — it boots into an empty conversation indistinguishable from a
// fresh start, which is the cold-amnesiac gap Cherub's 2026-09-18 decision names.
//
// THE NOTE IS READ AT BOOT FROM THE VERSIONED RECORD, NOT MAILED AT DEATH — a
// change from design v1, argued in docs/plans/ga-ksac39-handoff-default-termination.md
// revision 4. The short form: a mail wisp is the one surface that cannot carry
// this. Wisps skip DOLT_COMMIT, so a note that goes missing leaves no forensics;
// archive-after-inject DELETES the row on a stdout write that may never have been
// read (ga-vsxs3s); the retention TTL reaps it; and the write would have to happen
// during the stop, in exactly the sick-store windows where force-exits cluster and
// where the mail write is likeliest to fail. The bead record survives all four,
// because it is versioned history — which is the same argument katya's rider R1
// already made for the ratio's source of record, applied one layer out.

// fallbackNoteNotOwed enumerates the endings after which a note ALREADY EXISTS or
// no context was lost. Everything else is owed one.
//
// The discriminator is the kind table's AXIS 1 — did the seat's context end —
// minus the kinds where somebody already composed the note. Note that this is
// deliberately NOT the same question as CountsInDenominator, which also asks axis
// 2 (was a handoff possible). A runtime found already dead could not have been
// asked, so it is out of the RATIO; but its successor is exactly as amnesiac as
// any other, so it is owed the NOTE. Conflating the two would silence the note
// for the endings that destroyed context without anyone deciding to.
var fallbackNoteNotOwed = map[TerminationKind]bool{
	// The seat wrote its own note. That is the whole point of the good ending.
	KindHandoff:      true,
	KindDrainHandoff: true,
	// `gc handoff --target`: the SENDER composed a note for this seat. A second,
	// mechanical one would arrive beside it saying strictly less.
	KindHandoffTarget: true,
	// The context SURVIVES this one — the runtime is stopped only to resume the
	// same conversation on the other side. There is nothing to recover, and a
	// notice claiming otherwise would be actively wrong.
	//
	// It inherits that kind's NAMED CAVEAT: the seam cannot verify the resume
	// actually succeeded. A restart that silently fails to resume is a lost
	// context that will also be silently denied its note. Recorded as an
	// assumption rather than defended as a fact (katya, S1 review condition 2).
	KindInterruptRestart: true,
}

// NeedsFallbackNote reports whether a seat ending this way is owed a MECHANICAL
// notice on its next boot, because its context ended and nobody composed one.
//
// AN UNRECOGNIZED KIND IS OWED A NOTE. This fails toward telling the seat rather
// than toward silence, which is the direction that matters: a kind added later
// without updating the map above gets a notice it might not need, instead of
// silently joining the set of endings that destroy context invisibly. The empty
// kind is the one exception — it means there is no record at all, not an ending
// of unknown type.
func (k TerminationKind) NeedsFallbackNote() bool {
	return k != "" && !fallbackNoteNotOwed[k]
}
