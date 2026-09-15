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
