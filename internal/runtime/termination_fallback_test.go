package runtime

import "testing"

// TestFallbackNoteOwedIsEnumeratedNotInferred pins the exemption list the same
// way TestRatioBuckets pins the denominator exclusions: the exempt set is
// written out here, and every OTHER kind must be owed a note. A loop shaped as
// "skip anything exempt" would pass for any future exemption, silently, which is
// the failure it exists to prevent.
func TestFallbackNoteOwedIsEnumeratedNotInferred(t *testing.T) {
	exempt := map[TerminationKind]bool{
		KindHandoff:          true,
		KindDrainHandoff:     true,
		KindHandoffTarget:    true,
		KindInterruptRestart: true,
	}
	checked := 0
	for _, k := range TerminationKinds() {
		checked++
		if exempt[k] {
			if k.NeedsFallbackNote() {
				t.Errorf("kind %q is exempt (a note already exists, or no context was lost) but was owed one", k)
			}
			continue
		}
		if !k.NeedsFallbackNote() {
			t.Errorf("kind %q silently stopped owing a note — that is how an ending goes back to destroying context invisibly", k)
		}
	}
	// go test -run prints "ok" when the regex matches nothing, and a table that
	// silently shrank would pass every assertion above. Assert the count.
	if want := len(TerminationKinds()); checked != want {
		t.Fatalf("checked %d kinds, want the whole closed set of %d", checked, want)
	}
	if checked < 13 {
		t.Fatalf("the closed set shrank to %d kinds; that is a change worth arguing for here", checked)
	}
}

// TestObservedDeadIsOwedANoteEvenThoughItLeavesTheRatio is the distinction that
// justifies a second predicate existing at all. If NeedsFallbackNote were just
// CountsInDenominator, the endings where a runtime was found already dead would
// be silently exempted — and their successors are exactly as amnesiac as any
// other seat's.
func TestObservedDeadIsOwedANoteEvenThoughItLeavesTheRatio(t *testing.T) {
	if KindObservedDead.CountsInDenominator() {
		t.Fatal("premise changed: observed-dead is supposed to be out of the denominator")
	}
	if !KindObservedDead.NeedsFallbackNote() {
		t.Error("observed-dead is out of the RATIO because nothing could be asked of a dead runtime, " +
			"but its successor still lost a context and is owed the NOTE")
	}
}

// TestInterruptRestartIsExemptOnContextSurvival guards the other side of that
// same split: the one excluded kind that is ALSO exempt from the note, and for a
// different reason — the conversation continues across it.
func TestInterruptRestartIsExemptOnContextSurvival(t *testing.T) {
	if KindInterruptRestart.NeedsFallbackNote() {
		t.Error("interrupt-restart resumes the same conversation; a notice claiming the context ended would be wrong")
	}
}

// TestUnknownKindFailsTowardTellingTheSeat. A kind this file has never heard of
// is owed a note. The failure direction is the point: a kind added later without
// updating the exemption map gets a notice it may not need, rather than joining
// the set of endings that destroy context in silence.
func TestUnknownKindFailsTowardTellingTheSeat(t *testing.T) {
	if !TerminationKind("some-future-kind").NeedsFallbackNote() {
		t.Error("an unrecognised kind must be owed a note; silence is the dangerous default here")
	}
	if TerminationKind("").NeedsFallbackNote() {
		t.Error("the empty kind means there is NO record, not an ending of unknown type")
	}
}
