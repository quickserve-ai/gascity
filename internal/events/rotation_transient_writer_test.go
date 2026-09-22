package events

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A recorder opened BEFORE another recorder rotated the log must write into
// the NEW active log, not into the renamed file the compressor is archiving
// (Codex, PR #111 r3). Two FileRecorders on one path stand in for the
// supervisor's long-lived recorder and a per-invocation writer: each holds its
// own open file description, which is what flock and the rename act on.
func TestTransientWriterOpenedBeforeRotationFollowsTheActiveLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer

	longLived, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer longLived.Close() //nolint:errcheck // test cleanup
	for i := 0; i < 3; i++ {
		longLived.Record(Event{Type: BeadCreated, Actor: "human", Subject: "before"})
	}

	transient, err := NewFileRecorder(path, &stderr, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer transient.Close() //nolint:errcheck // test cleanup

	res, err := longLived.ForceRotate()
	if err != nil || !res.Rotated {
		t.Fatalf("ForceRotate = %+v, %v", res, err)
	}

	if err := transient.RecordAck(Event{Type: SessionNudged, Actor: "t", Subject: "after-rotation"}); err != nil {
		t.Fatalf("RecordAck: %v", err)
	}

	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(active), "after-rotation") {
		t.Fatalf("the transient row is not in the active log; it went to the rotated inode. active log:\n%s", active)
	}
	// And it must not claim a seq inside the archived window.
	all, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if e.Subject == "after-rotation" && e.Seq <= res.LastSeq {
			t.Fatalf("transient row seq %d falls inside the archived window [%d,%d]", e.Seq, res.FirstSeq, res.LastSeq)
		}
	}
}

// A writer that is released from the rotated inode must not append to the new
// active log ahead of the rotation anchor (Codex, PR #111 r4). Before the fix,
// closing the rotated file released its lock while the replacement was
// unlocked: a released writer followed the path, read its seq from the empty
// new file, and wrote a row whose seq duplicated the anchor's, before it.
//
// The property is checked directly rather than by racing a writer against a
// timer: at the instant the rotated inode is released, the new active log must
// already exist at the path and already be locked, so any writer that reaches
// it waits for the anchor.
func TestWriterReleasedByRotationWaitsForTheAnchor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer

	rotator, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rotator.Close() //nolint:errcheck // test cleanup
	for i := 0; i < 3; i++ {
		rotator.Record(Event{Type: BeadCreated, Actor: "human", Subject: "before"})
	}
	transient, err := NewFileRecorder(path, &stderr, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer transient.Close() //nolint:errcheck // test cleanup

	var probeErr error
	probed := false
	rotateBeforeAnchorHook = func() {
		probed = true
		if _, err := os.Stat(path); err != nil {
			probeErr = fmt.Errorf("new active log is not at the path when the rotated inode is released: %w", err)
			return
		}
		// Every append takes the sidecar lock first, so while the rotator
		// holds it no writer can reach the new log ahead of the anchor.
		f, err := os.OpenFile(path+".lock", os.O_RDWR, 0o644)
		if err != nil {
			probeErr = fmt.Errorf("opening the sidecar lock: %w", err)
			return
		}
		defer f.Close() //nolint:errcheck // test probe
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			probeErr = errors.New("the append lock is FREE when the rotated inode is released; a waiting writer would append ahead of the anchor")
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			probeErr = fmt.Errorf("probing the sidecar lock: %w", err)
		}
	}
	defer func() { rotateBeforeAnchorHook = nil }()

	res, err := rotator.ForceRotate()
	if err != nil || !res.Rotated {
		t.Fatalf("ForceRotate = %+v, %v", res, err)
	}
	if !probed {
		t.Fatal("rotateBeforeAnchorHook never ran")
	}
	if probeErr != nil {
		t.Fatal(probeErr)
	}

	// And the writer that follows lands after the anchor, with a higher seq.
	if err := transient.RecordAck(Event{Type: SessionNudged, Actor: "t", Subject: "released"}); err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	active, _, err := ReadFrom(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || active[0].Type != EventsRotated {
		t.Fatalf("new active log = %+v, want the %s anchor then the released row", active, EventsRotated)
	}
	if active[1].Subject != "released" || active[1].Seq <= active[0].Seq {
		t.Fatalf("released row = %q seq %d, anchor seq %d; want it after the anchor with a higher seq",
			active[1].Subject, active[1].Seq, active[0].Seq)
	}
}

// A recorder CONSTRUCTED in the rename gap (a transient per-invocation writer
// starting at the wrong instant) creates the active path itself. It must still
// not append before the rotation anchor, and its rows must not reuse archived
// sequence numbers (Codex, PR #111 r5): its own ReadLatestSeq ran while the
// active path was absent and cannot see the in-flight rotating file.
func TestRecorderConstructedInTheRenameGapCannotAppendBeforeTheAnchor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer

	rotator, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer rotator.Close() //nolint:errcheck // test cleanup
	for i := 0; i < 3; i++ {
		rotator.Record(Event{Type: BeadCreated, Actor: "human", Subject: "before"})
	}

	var gap *FileRecorder
	var inGapErr error
	rotateAfterRenameHook = func() {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("active path should be absent right after the rename, stat err = %v", err)
		}
		g, err := NewFileRecorder(path, &stderr, WithoutStartupSweep())
		if err != nil {
			t.Errorf("NewFileRecorder in the gap: %v", err)
			return
		}
		gap = g
		// The rotator holds the append lock, so this must fail (bounded wait),
		// not land a row ahead of the anchor.
		inGapErr = gap.RecordAck(Event{Type: SessionNudged, Actor: "t", Subject: "in-gap"})
	}
	defer func() { rotateAfterRenameHook = nil }()

	res, err := rotator.ForceRotate()
	rotateAfterRenameHook = nil
	if err != nil || !res.Rotated {
		t.Fatalf("ForceRotate = %+v, %v", res, err)
	}
	if gap == nil {
		t.Fatal("the gap recorder was never constructed")
	}
	defer gap.Close() //nolint:errcheck // test cleanup
	if inGapErr == nil {
		t.Fatal("a recorder constructed mid-rotation appended while the rotation held the lock")
	}

	if err := gap.RecordAck(Event{Type: SessionNudged, Actor: "t", Subject: "after"}); err != nil {
		t.Fatalf("RecordAck after the rotation: %v", err)
	}
	active, _, err := ReadFrom(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || active[0].Type != EventsRotated || active[1].Subject != "after" {
		t.Fatalf("new active log = %+v, want the anchor then the gap recorder's row", active)
	}
	if active[1].Seq <= active[0].Seq || active[1].Seq <= res.LastSeq {
		t.Fatalf("gap row seq %d, anchor seq %d, archived last %d: want the row after both",
			active[1].Seq, active[0].Seq, res.LastSeq)
	}
}
