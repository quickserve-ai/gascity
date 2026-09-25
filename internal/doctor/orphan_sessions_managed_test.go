package doctor

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-n2f1ph: the orphan-sessions check once counted only template-derived
// names as expected, so supervisor-managed sessions whose runtime name is not
// template-derived (a named session whose alias differs from its template, a
// namepool-themed pool instance) read as orphans and Fix stopped them. These
// tests pin the managed-name lister: a name an open session bead claims is
// never an orphan, a lister failure fails closed, and a nil lister keeps the
// legacy template-only behavior.

const (
	orphanTestTemplateSession = "mayor"         // template-derived (agent "mayor", city "test", default template)
	orphanTestManagedSession  = "qcore--archer" // claimed only by an open session bead
	orphanTestStraySession    = "stale-worker"  // claimed by nothing: the real orphan
)

// orphanTestProvider returns a fake runtime with the given sessions running.
func orphanTestProvider(t *testing.T, names ...string) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	for _, name := range names {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	return sp
}

// orphanTestLister is a managed-name lister that counts its calls.
type orphanTestLister struct {
	names map[string]struct{}
	err   error
	calls int
}

func (l *orphanTestLister) list() (map[string]struct{}, error) {
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return l.names, nil
}

func managedNameSet(names ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[name] = struct{}{}
	}
	return out
}

func orphanTestCheck(sp runtime.Provider, lister *orphanTestLister) *OrphanSessionsCheck {
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor"}}}
	c := NewOrphanSessionsCheck(cfg, "test", "", sp)
	if lister != nil {
		return c.WithManagedSessionNames(lister.list)
	}
	return c
}

func stopCalls(sp *runtime.Fake) []string {
	var out []string
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "Stop" {
			out = append(out, call.Name)
		}
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestOrphanSessionsCheckManagedNamesRun(t *testing.T) {
	listErr := errors.New("dolt unreachable")
	tests := []struct {
		name        string
		running     []string
		lister      *orphanTestLister // nil = no lister installed
		wantStatus  CheckStatus
		wantMessage []string // substrings
		wantDetails []string // exact, order-insensitive
		wantCalls   int      // lister calls, when a lister is installed
	}{
		{
			name:        "session claimed only by an open session bead is not an orphan",
			running:     []string{orphanTestTemplateSession, orphanTestManagedSession},
			lister:      &orphanTestLister{names: managedNameSet(orphanTestManagedSession)},
			wantStatus:  StatusOK,
			wantMessage: []string{"no orphaned sessions"},
			wantCalls:   1,
		},
		{
			name:        "session in neither set is still reported",
			running:     []string{orphanTestTemplateSession, orphanTestManagedSession, orphanTestStraySession},
			lister:      &orphanTestLister{names: managedNameSet(orphanTestManagedSession)},
			wantStatus:  StatusWarning,
			wantMessage: []string{"1 orphaned session(s)"},
			wantDetails: []string{orphanTestStraySession},
			wantCalls:   1,
		},
		{
			name:       "lister error warns that candidates are unconfirmed",
			running:    []string{orphanTestTemplateSession, orphanTestManagedSession, orphanTestStraySession},
			lister:     &orphanTestLister{err: listErr},
			wantStatus: StatusWarning,
			wantMessage: []string{
				"cannot confirm orphaned sessions",
				"listing managed sessions failed",
				"dolt unreachable",
				"2 unconfirmed candidate(s)",
			},
			wantDetails: []string{
				orphanTestManagedSession + " (unconfirmed: not template-derived; open session beads could not be listed)",
				orphanTestStraySession + " (unconfirmed: not template-derived; open session beads could not be listed)",
			},
			wantCalls: 1,
		},
		{
			name:        "no template-only candidate never calls the lister",
			running:     []string{orphanTestTemplateSession},
			lister:      &orphanTestLister{err: listErr},
			wantStatus:  StatusOK,
			wantMessage: []string{"no orphaned sessions"},
			wantCalls:   0,
		},
		{
			name:        "nil lister keeps legacy template-only behavior",
			running:     []string{orphanTestTemplateSession, orphanTestManagedSession, orphanTestStraySession},
			lister:      nil,
			wantStatus:  StatusWarning,
			wantMessage: []string{"2 orphaned session(s)"},
			wantDetails: []string{orphanTestManagedSession, orphanTestStraySession},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := orphanTestProvider(t, tt.running...)
			r := orphanTestCheck(sp, tt.lister).Run(&CheckContext{})
			if r.Status != tt.wantStatus {
				t.Fatalf("status = %d, want %d; msg = %s", r.Status, tt.wantStatus, r.Message)
			}
			for _, want := range tt.wantMessage {
				if !strings.Contains(r.Message, want) {
					t.Errorf("message = %q, want it to contain %q", r.Message, want)
				}
			}
			got := sortedCopy(r.Details)
			want := sortedCopy(tt.wantDetails)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("details = %q, want %q", got, want)
			}
			if tt.lister != nil && tt.lister.calls != tt.wantCalls {
				t.Errorf("lister calls = %d, want %d", tt.lister.calls, tt.wantCalls)
			}
			if n := len(stopCalls(sp)); n != 0 {
				t.Errorf("Run called Stop %d times, want 0", n)
			}
		})
	}
}

func TestOrphanSessionsCheckManagedNamesFix(t *testing.T) {
	all := []string{orphanTestTemplateSession, orphanTestManagedSession, orphanTestStraySession}
	tests := []struct {
		name         string
		lister       *orphanTestLister
		controllerUp bool
		wantErr      []string // substrings; nil = no error
		wantStops    []string
		wantCalls    int
	}{
		{
			name:      "stops exactly the orphan and not the managed session",
			lister:    &orphanTestLister{names: managedNameSet(orphanTestManagedSession)},
			wantStops: []string{orphanTestStraySession},
			wantCalls: 1,
		},
		{
			name:      "lister error refuses and stops nothing",
			lister:    &orphanTestLister{err: errors.New("dolt unreachable")},
			wantErr:   []string{"refusing to stop", "listing managed sessions failed", "dolt unreachable"},
			wantStops: nil,
			wantCalls: 1,
		},
		{
			name:         "controller running refuses before consulting the lister",
			lister:       &orphanTestLister{names: managedNameSet(orphanTestManagedSession)},
			controllerUp: true,
			wantErr:      []string{"controller is running"},
			wantStops:    nil,
			wantCalls:    0,
		},
		{
			name:      "nil lister keeps legacy template-only behavior",
			lister:    nil,
			wantStops: []string{orphanTestManagedSession, orphanTestStraySession},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &CheckContext{}
			if tt.controllerUp {
				cityDir := t.TempDir()
				holdControllerLock(t, cityDir)
				ctx.CityPath = cityDir
			}
			sp := orphanTestProvider(t, all...)
			err := orphanTestCheck(sp, tt.lister).Fix(ctx)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Fix() error = %v, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("Fix() error = nil, want an error containing %q", tt.wantErr)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Fix() error = %q, want it to contain %q", err.Error(), want)
					}
				}
			}
			got := stopCalls(sp)
			want := sortedCopy(tt.wantStops)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("Stop calls = %q, want %q", got, want)
			}
			stopped := make(map[string]bool, len(want))
			for _, name := range want {
				stopped[name] = true
			}
			for _, name := range all {
				if running := sp.IsRunning(name); running == stopped[name] {
					t.Errorf("IsRunning(%q) = %v after Fix, want %v", name, running, !stopped[name])
				}
			}
			if tt.lister != nil && tt.lister.calls != tt.wantCalls {
				t.Errorf("lister calls = %d, want %d", tt.lister.calls, tt.wantCalls)
			}
		})
	}
}
