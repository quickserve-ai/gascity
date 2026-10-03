package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The demand count skips the steps gc hook --claim skips (ga-b2oxyf) by arming
// the router's own closed-root gate over the demand group's store. Read-only.
//
// Every root read costs a backing round trip (a closed root is not cached, and
// the policy store overlays a liveness read), so each pass reads at most
// demandRootResolveBudget roots and each verdict is remembered across passes,
// keyed by store identity: a CLOSED verdict (with its tail) or a NOT-FOUND for
// demandClosedRootTTL — a root reopened inside it is under-counted for at most
// that TTL — and an OPEN verdict for demandOpenRootTTL — a root that closes
// inside it is over-counted (the safe direction) for at most that TTL. A step
// whose root is unresolved, including a root whose teardown tail cannot be
// read, is COUNTED, as it was before this guard.

// Knobs, replaced in tests.
var (
	demandClosedRootTTL     = 10 * time.Minute
	demandOpenRootTTL       = 60 * time.Second
	demandRootResolveBudget = 8
	demandClosedRootClock   = time.Now
)

var errDemandRootUnresolved = errors.New("demand: root not resolved this pass")

type demandRootMemo struct {
	root    beads.Bead
	err     error
	tail    func(beads.Bead) bool
	expires time.Time
}

// demandRootMemos is keyed by the store's identity plus
// hookRootStoreKey(group store key, root id).
var demandRootMemos = struct {
	sync.Mutex
	m map[string]demandRootMemo
}{m: map[string]demandRootMemo{}}

func demandRootRemembered(key string) (demandRootMemo, bool) {
	demandRootMemos.Lock()
	defer demandRootMemos.Unlock()
	memo, ok := demandRootMemos.m[key]
	if ok && !demandClosedRootClock().Before(memo.expires) {
		delete(demandRootMemos.m, key)
		return demandRootMemo{}, false
	}
	return memo, ok
}

func demandRootRemember(key string, memo demandRootMemo, ttl time.Duration) {
	memo.expires = demandClosedRootClock().Add(ttl)
	demandRootMemos.Lock()
	demandRootMemos.m[key] = memo
	demandRootMemos.Unlock()
}

// newDemandClosedRootGate arms the router's closed-root gate over one demand
// group's store. budget is shared by every group of the pass. The gate's
// diagnostic lines name gc hook --claim, so they are discarded here.
func newDemandClosedRootGate(store beads.Store, budget *int) *hookClosedRootGate {
	tails := map[string]func(beads.Bead) bool{}
	keyFor := func(dir, rootID string) string {
		return fmt.Sprintf("%p\x00", store) + hookRootStoreKey(dir, nil, rootID)
	}
	read := func(_ context.Context, dir string, _ []string, rootID, _ string) (beads.Bead, error) {
		key := keyFor(dir, rootID)
		if memo, ok := demandRootRemembered(key); ok {
			tails[key] = memo.tail
			return memo.root, memo.err
		}
		if *budget <= 0 {
			return beads.Bead{}, errDemandRootUnresolved
		}
		*budget--
		root, err := store.Get(rootID)
		if errors.Is(err, beads.ErrNotFound) {
			demandRootRemember(key, demandRootMemo{err: err}, demandClosedRootTTL)
		}
		if err != nil {
			return root, err
		}
		if !strings.EqualFold(strings.TrimSpace(root.Status), "closed") {
			demandRootRemember(key, demandRootMemo{root: root}, demandOpenRootTTL)
			return root, nil
		}
		tail, err := hookClaimTeardownTail(store, rootID)
		if err != nil {
			return beads.Bead{}, errDemandRootUnresolved
		}
		tails[key] = tail
		demandRootRemember(key, demandRootMemo{root: root, tail: tail}, demandClosedRootTTL)
		return root, nil
	}
	return &hookClosedRootGate{
		read: read,
		tailFor: func(_ context.Context, dir string, _ []string, rootID, _ string) (func(beads.Bead) bool, error) {
			if tail := tails[keyFor(dir, rootID)]; tail != nil {
				return tail, nil
			}
			return hookClaimTeardownTail(store, rootID)
		},
		stderr:   io.Discard,
		verdicts: map[string]*hookRootVerdict{},
		byRoot:   map[string][]*hookRootVerdict{},
		skipped:  map[string][]string{},
		seen:     map[string]map[string]struct{}{},
	}
}
