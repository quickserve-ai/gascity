package exec

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// ga-knhu61: the exec protocol must carry await_type both ways, or a machinery
// gate created through an exec provider lands unclassified and reads back blank.
func TestExecProtocolCarriesAwaitType(t *testing.T) {
	payload, err := marshalCreate(beads.Bead{Title: "gate", Type: "gate", AwaitType: beads.AwaitBead})
	if err != nil {
		t.Fatalf("marshalCreate: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal create request: %v", err)
	}
	if got := req["await_type"]; got != "bead" {
		t.Fatalf("create request await_type = %v, want bead", got)
	}

	var w beadWire
	if err := json.Unmarshal([]byte(`{"id":"gc-1","title":"gate","type":"gate","await_type":"timer"}`), &w); err != nil {
		t.Fatalf("unmarshal beadWire: %v", err)
	}
	if got := w.toBead().AwaitType; got != beads.AwaitTimer {
		t.Fatalf("toBead AwaitType = %q, want %q", got, beads.AwaitTimer)
	}
}
