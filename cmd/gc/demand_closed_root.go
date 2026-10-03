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

// The demand count skips the steps gc hook --claim skips (ga-b2oxyf) by arming
// the router's own closed-root gate over the demand group's store. Read-only.
//
// A closed root is not in the controller's cache, so its Get and IncludeClosed
// teardown query go to the backing store. Hence: a CLOSED verdict (with its
// tail) and a NOT-FOUND are remembered across passes for demandClosedRootTTL —
// a root reopened inside that window is under-counted for at most the TTL. An
// OPEN verdict is not remembered (the cache answers it cheaply). At most
// demandRootResolveBudget new closed or unreadable roots are resolved per pass;
// a step whose root is unresolved — including a root whose teardown tail cannot
// be read — is COUNTED, as it was before this guard.

// Knobs, replaced in tests.
var (
	demandClosedRootTTL     = 10 * time.Minute
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

// demandRootMemos is keyed by hookRootStoreKey(group store key, root id).
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
