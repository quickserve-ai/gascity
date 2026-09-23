package events

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		var got string
		if needles := typeNeedles(Filter{Type: typ}, nil); len(needles) == 1 {
			got = string(needles[0])
		}
		if got != want {
			t.Errorf("typeNeedles(%q) = %q, want %q", typ, got, want)
		}
	}
	if typeNeedles(Filter{}, []string{OrderFailed, "a&b"}) != nil {
		t.Error("one unencodable type must turn the whole prefilter off")
	}
	if n := typeNeedles(Filter{}, []string{OrderFailed, OrderCompleted}); len(n) != 2 {
		t.Errorf("two plain types want two needles, got %d", len(n))
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

func TestReadFilteredTypesReadsSeveralTypesInOneWalk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	ts := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	writeArchiveWithEvents(t, dir, "20260920T000000Z", 1, 4,
		Event{Seq: 1, Type: OrderCompleted, Ts: ts},
		Event{Seq: 2, Type: OrderFired, Ts: ts},
		Event{Seq: 3, Type: ControllerStarted, Ts: ts},
		Event{Seq: 4, Type: OrderFailed, Ts: ts},
	)
	active := `{"seq":5,"type":"order.fired","ts":"2026-09-21T00:00:00Z","actor":"t","message":"\"order.failed\""}
{"seq":6,"type":"order.failed","ts":"2026-09-21T00:00:00Z","actor":"t"}
`
	if err := os.WriteFile(path, []byte(active), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFilteredTypes(path, Filter{}, OrderCompleted, OrderFailed, ControllerStarted)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, e := range got {
		seqs = append(seqs, e.Seq)
	}
	if fmt.Sprint(seqs) != "[1 3 4 6]" {
		t.Fatalf("seqs = %v, want [1 3 4 6] (in log order, decoys excluded)", seqs)
	}

	if _, err := ReadFilteredTypes(path, Filter{Type: OrderFailed}, OrderCompleted); err == nil {
		t.Error("a filter.Type alongside types must be refused, not silently ANDed")
	}
	if _, err := ReadFilteredTypes(path, Filter{}); err == nil {
		t.Error("no types must be refused, not read as every type")
	}
}

// Codex on #136: a writer other than encoding/json may escape a plain type.
// Such a line must still be decoded, not rejected by the literal needle.
func TestReadFilteredTypePrefilterKeepsEscapedTypes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	// "order" + backslash + "u002efailed": the dot spelled as a JSON escape.
	escapedType := "order" + string(rune(92)) + "u002efailed"
	if !strings.ContainsRune(escapedType, 92) {
		t.Fatal("fixture lost its escape")
	}
	active := `{"seq":1,"type":"` + escapedType + `","ts":"2026-09-21T00:00:00Z","actor":"t"}` + "\n" +
		`{"seq":2,"type":"order.completed","ts":"2026-09-21T00:00:00Z","actor":"t"}` + "\n"
	if err := os.WriteFile(path, []byte(active), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFiltered(path, Filter{Type: OrderFailed})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("escaped order.failed must match, got %+v", got)
	}
}
