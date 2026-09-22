package events

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
