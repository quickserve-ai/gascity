package beads_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The wisp leg of BdStore.Get used to collapse three different outcomes into a
// bare ErrNotFound: the row is absent, the bd query failed, and this bd has no
// "query" subcommand at all. Only the first is evidence of absence. These tests
// pin the distinction, which is what lets a read-after-write guard tell a lost
// write from a verification it could not complete (ga-0ejdbv).

// TestBdStoreGetWispQueryErrorIsIndeterminate covers the load case that made
// the silent mail loss possible: the wisp verification query times out, and the
// old code reported that as "not found".
func TestBdStoreGetWispQueryErrorIsIndeterminate(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json gc-wisp-slow`: {
			err: fmt.Errorf("issue gc-wisp-slow not found"),
		},
		`bd query --json ephemeral=true AND id=gc-wisp-slow --all --limit 1`: {
			err: fmt.Errorf("signal: killed: context deadline exceeded"),
		},
	})
	s := beads.NewBdStore("/city", runner)
	_, err := s.Get("gc-wisp-slow")

	if !errors.Is(err, beads.ErrVerifyIndeterminate) {
		t.Errorf("err = %v, want ErrVerifyIndeterminate — a timed-out lookup is not evidence of absence", err)
	}
	// Compatibility is the whole reason this is a sub-case rather than a new
	// top-level error: every existing not-found caller must be unaffected.
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("err = %v, must still satisfy errors.Is(ErrNotFound) so existing callers are unchanged", err)
	}
}

// TestBdStoreGetWispQueryUnsupportedIsIndeterminate covers the quieter half:
// getEphemeralByID used to return (nil, nil) for a bd with no "query"
// subcommand, which the caller read as an authoritative empty result set.
func TestBdStoreGetWispQueryUnsupportedIsIndeterminate(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json gc-wisp-oldbd`: {
			err: fmt.Errorf("issue gc-wisp-oldbd not found"),
		},
		`bd query --json ephemeral=true AND id=gc-wisp-oldbd --all --limit 1`: {
			err: fmt.Errorf(`unknown subcommand "query"`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	_, err := s.Get("gc-wisp-oldbd")

	if !errors.Is(err, beads.ErrVerifyIndeterminate) {
		t.Errorf("err = %v, want ErrVerifyIndeterminate — never looking is not the same as looking and finding nothing", err)
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("err = %v, must still satisfy errors.Is(ErrNotFound)", err)
	}
}

// TestBdStoreGetGenuinelyAbsentWispIsNotIndeterminate is the polarity guard. A
// completed query that returned no rows IS evidence of absence, and must stay
// distinguishable from the two cases above — otherwise the guard can never
// report a real loss.
func TestBdStoreGetGenuinelyAbsentWispIsNotIndeterminate(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json gc-wisp-gone`: {
			err: fmt.Errorf("issue gc-wisp-gone not found"),
		},
		`bd query --json ephemeral=true AND id=gc-wisp-gone --all --limit 1`: {
			out: []byte(`[]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	_, err := s.Get("gc-wisp-gone")

	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if errors.Is(err, beads.ErrVerifyIndeterminate) {
		t.Errorf("err = %v, must NOT be indeterminate — the query completed and the row is absent", err)
	}
}

// TestBdStoreGetConsultsWispsWhenBdShowReturnsEmptySet covers the second way
// Get could conclude absence without ever querying the wisp tier: bd show
// SUCCEEDS and returns an empty array. bd show reads the issues table only, so
// that result says nothing about a wisp. Found by an independent cross-family
// review (ga-0ejdbv).
func TestBdStoreGetConsultsWispsWhenBdShowReturnsEmptySet(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json gc-wisp-empty`: {
			out: []byte(`[]`),
		},
		`bd query --json ephemeral=true AND id=gc-wisp-empty --all --limit 1`: {
			out: []byte(`[{"id":"gc-wisp-empty","title":"live message","status":"open","issue_type":"message","assignee":"katya","ephemeral":true}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	b, err := s.Get("gc-wisp-empty")
	if err != nil {
		t.Fatalf("Get: %v — an empty bd show must not conclude a wisp is absent", err)
	}
	if b.ID != "gc-wisp-empty" {
		t.Errorf("ID = %q, want gc-wisp-empty", b.ID)
	}
}

// TestBdStoreGetConsultsWispsOnSubstringCollision covers the third exit: bd
// resolved a DIFFERENT issue by substring. That is a statement about the issues
// table and not about whether the requested ID exists as a wisp.
func TestBdStoreGetConsultsWispsOnSubstringCollision(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json gc-wisp-abc`: {
			out: []byte(`[{"id":"gc-wisp-abcdef","title":"a different bead","status":"open","issue_type":"task"}]`),
		},
		`bd query --json ephemeral=true AND id=gc-wisp-abc --all --limit 1`: {
			out: []byte(`[{"id":"gc-wisp-abc","title":"the real message","status":"open","issue_type":"message","assignee":"katya","ephemeral":true}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	b, err := s.Get("gc-wisp-abc")
	if err != nil {
		t.Fatalf("Get: %v — a substring collision must not conclude a wisp is absent", err)
	}
	if b.ID != "gc-wisp-abc" {
		t.Errorf("ID = %q, want gc-wisp-abc", b.ID)
	}
}
