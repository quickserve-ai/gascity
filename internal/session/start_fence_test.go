package session

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

type countingStartFence struct {
	mu       sync.Mutex
	begins   int
	ends     int
	inflight int
}

func (f *countingStartFence) BeginStart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins++
	f.inflight++
}

func (f *countingStartFence) EndStart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends++
	f.inflight--
}

func (f *countingStartFence) snapshot() (begins, ends, inflight int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begins, f.ends, f.inflight
}

// startFailingProvider fails or panics in Start, and records whether the
// fence showed the start in flight when Start ran.
type startFailingProvider struct {
	*runtime.Fake
	fence      *countingStartFence
	panicStart bool
	sawInside  bool
	starts     int
}

func (p *startFailingProvider) Start(context.Context, string, runtime.Config) error {
	p.starts++
	_, _, inflight := p.fence.snapshot()
	p.sawInside = inflight > 0
	if p.panicStart {
		panic("provider start blew up")
	}
	return errors.New("provider start failed")
}

// TestManagerRuntimeStart_FenceEndsOnPanicAndError: a Manager's start brackets
// the provider call, and EndStart runs even when Start panics or returns an
// error, so no failed start is left counted in flight. It drives the
// reconciler's start bridge (StartRuntimeOnly), one of the places a Manager
// asks its provider to start a runtime; cmd/gc's source ratchet
// (TestSessionStartFenceRatchet) holds every such place to the same bracket.
func TestManagerRuntimeStart_FenceEndsOnPanicAndError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		panicStart bool
		register   bool
	}{
		{name: "error via WithStartFence"},
		{name: "panic via WithStartFence", panicStart: true},
		{name: "error via RegisterStartFence", register: true},
		{name: "panic via RegisterStartFence", panicStart: true, register: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fence := &countingStartFence{}
			sp := &startFailingProvider{Fake: runtime.NewFake(), fence: fence, panicStart: tc.panicStart}
			store := beads.NewMemStore()
			var m *Manager
			if tc.register {
				unregister, ok := RegisterStartFence(sp, fence)
				if !ok {
					t.Fatal("RegisterStartFence refused a pointer provider")
				}
				defer unregister()
				m = newTestManager(t, store, sp)
			} else {
				m = newTestManager(t, store, sp, WithStartFence(fence))
			}
			id := seedFenceTestSession(t, store)
			by := runtimeOnlyActor(t, m, id)
			inc := mintUnder(t, m, by.Lease, id) // the incarnation the reconciler's PreWake mints
			func() {
				defer func() { _ = recover() }()
				_ = m.StartRuntimeOnly(context.Background(), by, id, inc, "claude", runtime.Config{WorkDir: t.TempDir()})
			}()
			if sp.starts != 1 {
				t.Fatalf("provider Start ran %d time(s), want once", sp.starts)
			}
			begins, ends, inflight := fence.snapshot()
			if !sp.sawInside {
				t.Fatal("provider Start ran outside the fence bracket")
			}
			if begins != 1 || ends != 1 || inflight != 0 {
				t.Fatalf("fence begins=%d ends=%d inflight=%d, want 1/1/0", begins, ends, inflight)
			}
		})
	}
}

// seedFenceTestSession stores an asleep session row a runtime-only start can
// bring up, and returns its id.
func seedFenceTestSession(t *testing.T, store beads.Store) string {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":    string(StateAsleep),
			"provider": "claude",
			"work_dir": t.TempDir(),
			"command":  "claude",
		},
	})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", sessionNameFor(b.ID)); err != nil {
		t.Fatalf("SetMetadata(session_name): %v", err)
	}
	return b.ID
}

// TestRegisterStartFence_UnregisterStopsBracketing: after unregister, a start
// through the provider no longer touches the fence.
func TestRegisterStartFence_UnregisterStopsBracketing(t *testing.T) {
	fence := &countingStartFence{}
	sp := runtime.NewFake()
	unregister, ok := RegisterStartFence(sp, fence)
	if !ok {
		t.Fatal("RegisterStartFence refused a pointer provider")
	}
	BracketProviderStart(sp)()
	unregister()
	unregister() // idempotent
	BracketProviderStart(sp)()
	if begins, ends, _ := fence.snapshot(); begins != 1 || ends != 1 {
		t.Fatalf("fence begins=%d ends=%d, want 1/1 (only the start before unregister)", begins, ends)
	}
}

// funcFieldProvider is a provider value that cannot be hashed: its func field
// makes the type uncomparable.
type funcFieldProvider struct {
	runtime.Provider
	onStart func()
}

func (p funcFieldProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.onStart != nil {
		p.onStart()
	}
	return p.Provider.Start(ctx, name, cfg)
}

// interfaceFieldProvider is a provider value whose type is comparable (its
// only field is an interface) while its contents may not be. It is the shape of
// a profile wrapper around a test backend (runtime.NewFakeProfile over
// internal/worker's inputHookBackend), which panicked "hash of unhashable type"
// when startFenceKey checked only the static type.
type interfaceFieldProvider struct {
	runtime.Provider
}

// TestStartFenceKey_RefusesProviderWithUnhashableContents: a provider whose
// static type is comparable but whose value holds an uncomparable one is not
// keyed, RegisterStartFence refuses it, and a start through it is still
// bracketed by its direct fence without panicking.
func TestStartFenceKey_RefusesProviderWithUnhashableContents(t *testing.T) {
	sp := interfaceFieldProvider{Provider: funcFieldProvider{Provider: runtime.NewFake(), onStart: func() {}}}
	if !reflect.TypeOf(sp).Comparable() {
		t.Fatal("fixture: the provider's static type must be comparable, or this test does not reach the value check")
	}
	if _, ok := startFenceKey(sp); ok {
		t.Fatal("startFenceKey keyed a provider whose value cannot be hashed")
	}
	registered := &countingStartFence{}
	unregister, ok := RegisterStartFence(sp, registered)
	defer unregister()
	if ok {
		t.Fatal("RegisterStartFence accepted a provider whose value cannot be hashed")
	}
	direct := &countingStartFence{}
	bracketStart(direct, sp)()
	if begins, ends, inflight := direct.snapshot(); begins != 1 || ends != 1 || inflight != 0 {
		t.Fatalf("direct fence begins=%d ends=%d inflight=%d, want 1/1/0", begins, ends, inflight)
	}
	if begins, _, _ := registered.snapshot(); begins != 0 {
		t.Fatalf("refused fence begins=%d, want 0", begins)
	}
	BracketProviderStart(sp)()

	// A comparable value of the same wrapper shape is still keyed.
	keyed := interfaceFieldProvider{Provider: runtime.NewFake()}
	if _, ok := startFenceKey(keyed); !ok {
		t.Fatal("startFenceKey refused a wrapper whose contents are comparable (a pointer)")
	}
}
