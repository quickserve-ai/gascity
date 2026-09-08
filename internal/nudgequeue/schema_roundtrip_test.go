package nudgequeue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A state.json written by a newer gc build: one field on an item and one
// top-level bucket that this build's structs do not declare (ga-aj9auz).
const newerSchemaState = `{
  "pending": [
    {
      "id": "n-1",
      "agent": "woodhouse",
      "source": "test",
      "sender": "woodhouse",
      "message": "hello",
      "created_at": "2026-09-08T10:00:00Z",
      "deliver_after": "2026-09-08T10:00:00Z",
      "expires_at": "2026-09-09T10:00:00Z",
      "future_field": {"nested": true},
      "another_new": "keep me"
    }
  ],
  "future_bucket": [{"id": "fb-1"}]
}`

func TestItemRoundTripPreservesUnknownFields(t *testing.T) {
	var state State
	if err := json.Unmarshal([]byte(newerSchemaState), &state); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].Sender != "woodhouse" {
		t.Fatalf("known fields not decoded: %+v", state.Pending)
	}
	out, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"future_field"`, `"nested":true`, `"another_new":"keep me"`, `"future_bucket"`, `"fb-1"`, `"sender":"woodhouse"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("round-trip lost %s; got: %s", want, out)
		}
	}
}

func TestItemMarshalKnownFieldWinsOverStaleUnknown(t *testing.T) {
	// A key that IS known to this build must never be shadowed by a stray
	// captured copy: mutate the known field after unmarshal and confirm the
	// mutated value is what marshals.
	var item Item
	if err := json.Unmarshal([]byte(`{"id":"n-2","agent":"a","source":"s","message":"m","future_field":1}`), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	item.Message = "changed"
	out, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"message":"changed"`) {
		t.Errorf("known field did not win: %s", out)
	}
	if !strings.Contains(string(out), `"future_field":1`) {
		t.Errorf("unknown field lost: %s", out)
	}
}

func TestCurrentSchemaRoundTripUnchanged(t *testing.T) {
	item := Item{ID: "n-3", Agent: "a", Source: "s", Message: "m",
		CreatedAt: time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)}
	out, err := json.Marshal(State{Pending: []Item{item}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back State
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Pending[0].unknown != nil || back.unknown != nil {
		t.Errorf("current-schema round trip populated unknown overlays: %+v", back)
	}
	out2, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(out) != string(out2) {
		t.Errorf("current-schema encoding not stable: %s vs %s", out, out2)
	}
}

func TestWithStateMaintenancePassPreservesNewerSchema(t *testing.T) {
	// The measured failure: an old-schema process runs a maintenance pass
	// (mutating the queue) and re-saves. Newer-schema fields must survive.
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(StatePath(city)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(city), []byte(newerSchemaState), 0o644); err != nil {
		t.Fatal(err)
	}
	err := WithState(city, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "n-new", Agent: "b", Source: "s", Message: "added"})
		return nil
	})
	if err != nil {
		t.Fatalf("WithState: %v", err)
	}
	data, err := os.ReadFile(StatePath(city))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"future_field"`, `"another_new"`, `"future_bucket"`, `"n-new"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("maintenance pass lost %s; state.json: %s", want, data)
		}
	}
}
