package beads_test

import (
	"bytes"
	"errors"
	"log"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// captureStdLog redirects the standard logger for the test's duration.
func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// inheritingBdRunner fakes bd's create-time label inheritance: the create
// response carries the explicit labels plus every parent label, the way
// `bd create --parent` without --no-inherit-labels reports them.
type inheritingBdRunner struct {
	createJSON string
	updateErr  error
	calls      []string
}

func (r *inheritingBdRunner) run(_, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	switch {
	case len(args) > 0 && args[0] == "create":
		return []byte(r.createJSON), nil
	case len(args) > 0 && args[0] == "update":
		if r.updateErr != nil {
			return nil, r.updateErr
		}
		return []byte(`{}`), nil
	}
	return nil, errors.New("unexpected command: " + call)
}

const inheritedChildCreateJSON = `{"id":"qc-88kjhl.1","title":"Follow-up","status":"open","issue_type":"task","created_at":"2026-09-17T09:52:47Z","labels":["own","town:x","ready:y","hold:cert-wait","cert:action","needs-summon"]}`

func TestBdStoreCreateChildRemovesInheritedStateLabels(t *testing.T) {
	logs := captureStdLog(t)
	r := &inheritingBdRunner{createJSON: inheritedChildCreateJSON}
	s := beads.NewBdStore(t.TempDir(), r.run)

	got, err := s.Create(beads.Bead{Title: "Follow-up", ParentID: "qc-88kjhl", Labels: []string{"own"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"own", "town:x", "ready:y"}; !slices.Equal(got.Labels, want) {
		t.Fatalf("returned labels = %q, want %q", got.Labels, want)
	}
	if len(r.calls) != 2 {
		t.Fatalf("bd calls = %q, want create then one label removal", r.calls)
	}
	wantUpdate := "bd update --json qc-88kjhl.1 --remove-label hold:cert-wait --remove-label cert:action --remove-label needs-summon"
	if r.calls[1] != wantUpdate {
		t.Fatalf("removal call = %q, want %q", r.calls[1], wantUpdate)
	}
	if !strings.Contains(logs.String(), "qc-p9m8oa9") || !strings.Contains(logs.String(), "needs-summon") {
		t.Fatalf("log = %q, want a line naming the removed labels and qc-p9m8oa9", logs.String())
	}
}

func TestBdStoreCreateChildKeepsExplicitStateLabel(t *testing.T) {
	captureStdLog(t)
	r := &inheritingBdRunner{createJSON: inheritedChildCreateJSON}
	s := beads.NewBdStore(t.TempDir(), r.run)

	got, err := s.Create(beads.Bead{Title: "Follow-up", ParentID: "qc-88kjhl", Labels: []string{"own", "hold:cert-wait"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Labels, "hold:cert-wait") {
		t.Fatalf("returned labels = %q lost the explicit hold:cert-wait", got.Labels)
	}
	wantUpdate := "bd update --json qc-88kjhl.1 --remove-label cert:action --remove-label needs-summon"
	if len(r.calls) != 2 || r.calls[1] != wantUpdate {
		t.Fatalf("bd calls = %q, want removal %q", r.calls, wantUpdate)
	}
}

func TestBdStoreCreateWithoutStateLabelsMakesNoExtraCall(t *testing.T) {
	logs := captureStdLog(t)
	for _, tc := range []struct {
		name string
		bead beads.Bead
		json string
	}{
		{
			name: "child of a parent without state labels",
			bead: beads.Bead{Title: "t", ParentID: "qc-1"},
			json: `{"id":"qc-1.1","title":"t","status":"open","issue_type":"task","created_at":"2026-09-17T09:52:47Z","labels":["town:x","ready:y"]}`,
		},
		{
			name: "non-parent create with explicit state labels",
			bead: beads.Bead{Title: "t", Labels: []string{"hold:cert-wait", "needs-summon"}},
			json: `{"id":"qc-2","title":"t","status":"open","issue_type":"task","created_at":"2026-09-17T09:52:47Z","labels":["hold:cert-wait","needs-summon"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &inheritingBdRunner{createJSON: tc.json}
			s := beads.NewBdStore(t.TempDir(), r.run)
			if _, err := s.Create(tc.bead); err != nil {
				t.Fatal(err)
			}
			if len(r.calls) != 1 {
				t.Fatalf("bd calls = %q, want the create alone", r.calls)
			}
		})
	}
	if logs.Len() != 0 {
		t.Fatalf("log = %q, want silent", logs.String())
	}
}

func TestBdStoreCreateChildRemovalFailureKeepsTheCreateAndLogs(t *testing.T) {
	logs := captureStdLog(t)
	r := &inheritingBdRunner{createJSON: inheritedChildCreateJSON, updateErr: errors.New("dolt: connection refused")}
	s := beads.NewBdStore(t.TempDir(), r.run)

	got, err := s.Create(beads.Bead{Title: "Follow-up", ParentID: "qc-88kjhl", Labels: []string{"own"}})
	if err != nil {
		t.Fatalf("Create returned %v; the bead exists, so a failed label removal must not report the create as failed", err)
	}
	if got.ID != "qc-88kjhl.1" || !slices.Contains(got.Labels, "hold:cert-wait") {
		t.Fatalf("returned bead = %+v, want the created bead with its labels as they actually are", got)
	}
	if !strings.Contains(logs.String(), "connection refused") || !strings.Contains(logs.String(), "qc-p9m8oa9") {
		t.Fatalf("log = %q, want the removal failure and qc-p9m8oa9", logs.String())
	}
}
