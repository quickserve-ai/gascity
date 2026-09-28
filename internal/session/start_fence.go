package session

import (
	"context"
	"reflect"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime"
)

// StartFence is bracketed around every runtime start a Manager makes:
// BeginStart immediately before the provider call and EndStart after it
// returns (deferred, so a panic, an early error return or a cancelled context
// cannot leave a start counted as in flight).
//
// The controller uses it to keep its background worktree reaper from removing
// a tree a session started in while the reaper was deciding (ga-yuiof4 item 3):
// it installs one fence per runtime provider with RegisterStartFence, and every
// Manager built on that provider — the controller's reconciler, its control
// dispatcher, and the in-process API server's session wakes alike — brackets
// its starts with it, with no per-call-site wiring.
type StartFence interface {
	BeginStart()
	EndStart()
}

// startFenceRegistry maps a runtime provider (by identity) to the fences
// registered for it. A provider is keyed only when its dynamic type is
// comparable; production providers are pointers.
var startFenceRegistry = struct {
	mu         sync.RWMutex
	byProvider map[any][]*registeredStartFence
}{byProvider: map[any][]*registeredStartFence{}}

type registeredStartFence struct{ fence StartFence }

func startFenceKey(sp runtime.Provider) (any, bool) {
	if sp == nil {
		return nil, false
	}
	if !reflect.TypeOf(sp).Comparable() {
		return nil, false
	}
	return sp, true
}

// RegisterStartFence brackets every runtime start any Manager makes through
// sp with fence, until the returned unregister func is called. Several fences
// may be registered for one provider; each is bracketed. It reports false
// (and registers nothing) when sp cannot be keyed by identity.
func RegisterStartFence(sp runtime.Provider, fence StartFence) (unregister func(), ok bool) {
	key, ok := startFenceKey(sp)
	if !ok || fence == nil {
		return func() {}, false
	}
	entry := &registeredStartFence{fence: fence}
	startFenceRegistry.mu.Lock()
	startFenceRegistry.byProvider[key] = append(startFenceRegistry.byProvider[key], entry)
	startFenceRegistry.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			startFenceRegistry.mu.Lock()
			defer startFenceRegistry.mu.Unlock()
			entries := startFenceRegistry.byProvider[key]
			for i, e := range entries {
				if e == entry {
					entries = append(entries[:i:i], entries[i+1:]...)
					break
				}
			}
			if len(entries) == 0 {
				delete(startFenceRegistry.byProvider, key)
			} else {
				startFenceRegistry.byProvider[key] = entries
			}
		})
	}, true
}

// WithStartFence installs fence on this Manager directly, in addition to any
// fence registered for its provider.
func WithStartFence(fence StartFence) ManagerOption {
	return func(m *Manager) {
		m.startFence = fence
	}
}

// BracketProviderStart begins every fence registered for sp and returns the
// func that ends them. It is for runtime-creating provider calls made outside
// a Manager; callers defer the returned func.
func BracketProviderStart(sp runtime.Provider) func() {
	return bracketStart(nil, sp)
}

func bracketStart(direct StartFence, sp runtime.Provider) func() {
	var fences []StartFence
	if direct != nil {
		fences = append(fences, direct)
	}
	if key, ok := startFenceKey(sp); ok {
		startFenceRegistry.mu.RLock()
		for _, e := range startFenceRegistry.byProvider[key] {
			fences = append(fences, e.fence)
		}
		startFenceRegistry.mu.RUnlock()
	}
	for _, f := range fences {
		f.BeginStart()
	}
	return func() {
		for _, f := range fences {
			f.EndStart()
		}
	}
}

// startRuntime is the ONLY place a Manager asks its provider to start a
// runtime. Every fence that applies (WithStartFence, RegisterStartFence) is
// begun before the call and ended after it, deferred.
func (m *Manager) startRuntime(ctx context.Context, name string, cfg runtime.Config) error {
	defer bracketStart(m.startFence, m.sp)()
	return m.sp.Start(ctx, name, cfg)
}
