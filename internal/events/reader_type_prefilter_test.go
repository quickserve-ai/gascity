package events

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ga-4mu4k5: forward reads skip lines that cannot hold filter.Type before
// decoding them. The skip must never drop a real match.
func TestTypeNeedle(t *testing.T) {
	for typ, want := range map[string]string{
		"":             "",
		OrderFailed:    `"` + OrderFailed + `"`,
		"a&b":          "",
		"a<b":          "",
		`a"b`:          "",
		`a\b`:          "",
		"café":         "",
		"tab\there":    "",
		"plain.type-1": `"plain.type-1"`,
	} {
		if got := string(typeNeedle(Filter{Type: typ})); got != want {
			t.Errorf("typeNeedle(%q) = %q, want %q", typ, got, want)
		}
	}
}

func TestReadFilteredTypePrefilterKeepsEveryMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ts := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	// Archived: two matches among decoys, one of which names the type only in
	// its message (passes the prefilter, rejected by the decoded match).
	writeArchiveWithEvents(t, dir, "20260920T000000Z", 1, 4,
		Event{Seq: 1, Type: OrderCompleted, Ts: ts},
		Event{Seq: 2, Type: OrderFailed, Ts: ts},
		Event{Seq: 3, Type: OrderFired, Ts: ts},
		Event{Seq: 4, Type: OrderFailed, Ts: ts},
	)
	// Active: a match, a decoy, and a decoy whose message quotes the type.
	active := `{"seq":5,"type":"order.failed","ts":"2026-09-21T00:00:00Z","actor":"t"}
{"seq":6,"type":"order.completed","ts":"2026-09-21T00:00:00Z","actor":"t"}
{"seq":7,"type":"order.completed","ts":"2026-09-21T00:00:00Z","actor":"t","message":"after \"order.failed\""}
`
	if err := os.WriteFile(path, []byte(active), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFiltered(path, Filter{Type: OrderFailed})
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, e := range got {
		seqs = append(seqs, e.Seq)
	}
	if len(seqs) != 3 || seqs[0] != 2 || seqs[1] != 4 || seqs[2] != 5 {
		t.Fatalf("ReadFiltered(order.failed) seqs = %v, want [2 4 5]", seqs)
	}
}
