package session

import (
	"context"
	"errors"
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
}

func (p *startFailingProvider) Start(context.Context, string, runtime.Config) error {
	_, _, inflight := p.fence.snapshot()
	p.sawInside = inflight > 0
	if p.panicStart {
		panic("provider start blew up")
	}
	return errors.New("provider start failed")
}

// TestManagerStartRuntime_FenceEndsOnPanicAndError: the Manager's start hook
// brackets the provider call, and EndStart runs even when Start panics or
// returns an error, so no failed start is left counted in flight.
func TestManagerStartRuntime_FenceEndsOnPanicAndError(t *testing.T) {
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
			var m *Manager
			if tc.register {
				unregister, ok := RegisterStartFence(sp, fence)
				if !ok {
					t.Fatal("RegisterStartFence refused a pointer provider")
				}
				defer unregister()
				m = NewManagerWithOptions(beads.NewMemStore(), sp)
			} else {
				m = NewManagerWithOptions(beads.NewMemStore(), sp, WithStartFence(fence))
			}
			func() {
				defer func() { _ = recover() }()
				_ = m.startRuntime(context.Background(), "s1", runtime.Config{})
			}()
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
