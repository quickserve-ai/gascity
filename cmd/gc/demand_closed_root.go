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
// the policy store overlays a liveness read), so verdicts are remembered across
// passes, keyed by store identity, and each pass reads at most
// demandRootResolveBudget never-seen roots plus demandRootRefreshBudget
// refreshes of expired OPEN verdicts. A CLOSED or NOT-FOUND verdict lasts
// demandClosedRootTTL: a root reopened inside it is under-counted for at most
// that TTL. An expired OPEN verdict is still answered as open until its refresh
// turn (open and unresolved both mean "counted"), so refreshes never starve new
// roots, and a root that closes is noticed within demandOpenRootTTL plus its
// turn in the refresh queue (over-count only). A step whose root is unresolved,
// including a root whose teardown tail cannot be read, is COUNTED, as it was
// before this guard; a pass that leaves roots unresolved for lack of budget says
// so at most once per demandRootUnresolvedLogEvery.

// Knobs, replaced in tests.
var (
	demandClosedRootTTL          = 10 * time.Minute
	demandOpenRootTTL            = 5 * time.Minute
	demandRootResolveBudget      = 8
	demandRootRefreshBudget      = 2
	demandRootMemoSweepAt        = 512
	demandRootUnresolvedLogEvery = 10 * time.Minute
	demandClosedRootClock        = time.Now
)

var (
	errDemandRootUnresolved  = errors.New("demand: root not resolved this pass")
	errDemandRootsUnresolved = errors.New("demand closed-root gate")
)

// demandRootMemo keeps a verdict, not the bead: "closed", "open" or "notfound".
type demandRootMemo struct {
	status  string
	tail    func(beads.Bead) bool
	expires time.Time
}

// demandRootMemos is keyed by the store's identity plus
// hookRootStoreKey(group store key, root id).
var demandRootMemos = struct {
	sync.Mutex
	m      map[string]demandRootMemo
	logged time.Time
}{m: map[string]demandRootMemo{}}

func demandRootRemember(key, status string, tail func(beads.Bead) bool, ttl time.Duration) {
	now := demandClosedRootClock()
	demandRootMemos.Lock()
	defer demandRootMemos.Unlock()
	if len(demandRootMemos.m) >= demandRootMemoSweepAt {
		for k, memo := range demandRootMemos.m {
			if !now.Before(memo.expires) {
				delete(demandRootMemos.m, k)
			}
		}
	}
	demandRootMemos.m[key] = demandRootMemo{status: status, tail: tail, expires: now.Add(ttl)}
}

// demandRootPass is one demand pass's read budgets, shared by every group.
type demandRootPass struct{ resolve, refresh, unresolved int }

func newDemandRootPass() *demandRootPass {
	return &demandRootPass{resolve: demandRootResolveBudget, refresh: demandRootRefreshBudget}
}

// report returns the pass's one log line, at most once per log interval.
func (p *demandRootPass) report() error {
	if p.unresolved == 0 {
		return nil
	}
	now := demandClosedRootClock()
	demandRootMemos.Lock()
	defer demandRootMemos.Unlock()
	if !demandRootMemos.logged.IsZero() && now.Before(demandRootMemos.logged.Add(demandRootUnresolvedLogEvery)) {
		return nil
	}
	demandRootMemos.logged = now
	return fmt.Errorf("%w: %d roots unresolved this pass (budget %d); their steps are counted", errDemandRootsUnresolved, p.unresolved, demandRootResolveBudget)
}

// newDemandClosedRootGate arms the router's closed-root gate over one demand
// group's store. The gate's diagnostic lines name gc hook --claim, so they are
// discarded here.
func newDemandClosedRootGate(store beads.Store, pass *demandRootPass) *hookClosedRootGate {
	tails := map[string]func(beads.Bead) bool{}
	keyFor := func(dir, rootID string) string {
		return fmt.Sprintf("%p\x00", store) + hookRootStoreKey(dir, nil, rootID)
	}
	read := func(_ context.Context, dir string, _ []string, rootID, _ string) (beads.Bead, error) {
		key := keyFor(dir, rootID)
		demandRootMemos.Lock()
		memo, seen := demandRootMemos.m[key]
		demandRootMemos.Unlock()
		switch {
		case seen && demandClosedRootClock().Before(memo.expires):
			tails[key] = memo.tail
			if memo.status == "notfound" {
				return beads.Bead{}, beads.ErrNotFound
			}
			return beads.Bead{ID: rootID, Status: memo.status}, nil
		case seen && memo.status == "open":
			if pass.refresh <= 0 {
				return beads.Bead{ID: rootID, Status: "open"}, nil
			}
			pass.refresh--
		case pass.resolve <= 0:
			pass.unresolved++
			return beads.Bead{}, errDemandRootUnresolved
		default:
			pass.resolve--
		}
		root, err := store.Get(rootID)
		if errors.Is(err, beads.ErrNotFound) {
			demandRootRemember(key, "notfound", nil, demandClosedRootTTL)
		}
		if err != nil {
			return root, err
		}
		if !strings.EqualFold(strings.TrimSpace(root.Status), "closed") {
			demandRootRemember(key, "open", nil, demandOpenRootTTL)
			return root, nil
		}
		tail, err := hookClaimTeardownTail(store, rootID)
		if err != nil {
			return beads.Bead{}, errDemandRootUnresolved
		}
		tails[key] = tail
		demandRootRemember(key, "closed", tail, demandClosedRootTTL)
		return root, nil
	}
	return &hookClosedRootGate{
		read: read,
		// read sets the tail of every closed verdict, so a missing one is
		// unresolved rather than an uncharged backing read.
		tailFor: func(_ context.Context, dir string, _ []string, rootID, _ string) (func(beads.Bead) bool, error) {
			if tail := tails[keyFor(dir, rootID)]; tail != nil {
				return tail, nil
			}
			return nil, errDemandRootUnresolved
		},
		stderr:   io.Discard,
		verdicts: map[string]*hookRootVerdict{},
		byRoot:   map[string][]*hookRootVerdict{},
		skipped:  map[string][]string{},
		seen:     map[string]map[string]struct{}{},
	}
}
