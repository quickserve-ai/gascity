package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The controller's demand count skips the steps gc hook --claim skips: an open
// step of a molecule whose root was observed closed, outside that root's
// teardown tail (qc-z0fmn0n). Counting one spawns a seat that reads empty and
// drains, every tick (ga-b2oxyf). The demand side arms the router's own gate
// (closedRootOf, the teardown tail), so spawn and claim cannot disagree. It is
// read-only: it reads roots and teardown members and writes nothing.
//
// Cost. A closed root is not in the controller's cache, so each read of one —
// and its IncludeClosed teardown query — goes to the backing store. So:
//
//   - A CLOSED verdict (with its tail) and a NOT-FOUND result are remembered
//     across passes for demandClosedRootTTL. A root reopened inside that window
//     is under-counted (its steps are not demand) for at most the TTL.
//   - An OPEN verdict is not remembered: the cached store answers it cheaply,
//     and a root that closes is then seen on the next pass.
//   - At most demandRootResolveBudget roots that are not remembered and turn
//     out closed or unreadable are resolved per pass (an open root is refunded).
//     A step whose root is not resolved this pass is COUNTED, as it was before
//     this guard existed.
//   - A root whose teardown tail cannot be read is unresolved, so its steps are
//     counted: a retry attempt the router would serve is never dropped.

var (
	// demandClosedRootTTL bounds how long a closed or not-found root verdict is
	// remembered across demand passes.
	demandClosedRootTTL = 10 * time.Minute
	// demandRootResolveBudget caps the uncached root resolutions of one pass.
	demandRootResolveBudget = 8
	// demandClosedRootClock is the TTL clock, replaced in tests.
	demandClosedRootClock = time.Now
)

var errDemandRootUnresolved = errors.New("demand: root not resolved this pass")

type demandRootMemo struct {
	root    beads.Bead
	err     error
	tail    func(beads.Bead) bool
	expires time.Time
}

// demandRootMemos is the process-level cache, keyed by hookRootStoreKey over
// the demand group's store key and the root id.
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

func demandRootRemember(key string, memo demandRootMemo) {
	memo.expires = demandClosedRootClock().Add(demandClosedRootTTL)
	demandRootMemos.Lock()
	demandRootMemos.m[key] = memo
	demandRootMemos.Unlock()
}

// newDemandClosedRootGate arms the router's closed-root gate over one demand
// group's store. budget is shared by every group of the pass. The gate's
// diagnostic lines name gc hook --claim, so they are discarded here.
func newDemandClosedRootGate(store beads.Store, budget *int) *hookClosedRootGate {
	tails := map[string]func(beads.Bead) bool{}
	read := func(_ context.Context, dir string, _ []string, rootID, _ string) (beads.Bead, error) {
		key := hookRootStoreKey(dir, nil, rootID)
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
			demandRootRemember(key, demandRootMemo{err: err})
		}
		if err != nil {
			return root, err
		}
		if !strings.EqualFold(strings.TrimSpace(root.Status), "closed") {
			*budget++ // an open root is a cached read, not a resolution
			return root, nil
		}
		tail, err := hookClaimTeardownTail(store, rootID)
		if err != nil {
			return beads.Bead{}, errDemandRootUnresolved
		}
		tails[key] = tail
		demandRootRemember(key, demandRootMemo{root: root, tail: tail})
		return root, nil
	}
	return &hookClosedRootGate{
		read: read,
		tailFor: func(_ context.Context, dir string, _ []string, rootID, _ string) (func(beads.Bead) bool, error) {
			if tail := tails[hookRootStoreKey(dir, nil, rootID)]; tail != nil {
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
