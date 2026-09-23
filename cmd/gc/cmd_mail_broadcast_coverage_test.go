package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// ga-dwgz52: a --all broadcast must name the configured seats it did not reach.

// The configured identity and the mailbox alias differ. A string compare would
// report pam as skipped although she got the mail; the join must go through the
// session's configured_named_identity.
func TestUnreachedConfiguredSeatsJoinsByNamedIdentityNotAlias(t *testing.T) {
	cov := &broadcastCoverage{
		configured: []string{"qcore/cherub-law.pam"},
		open:       []session.Info{{ID: "ga-1", Alias: "qcore/pam", ConfiguredNamedIdentity: "qcore/cherub-law.pam"}},
	}
	got := unreachedConfiguredSeats(cov, map[string]bool{"qcore/pam": true}, "gastown.mayor")
	if len(got) != 0 {
		t.Fatalf("unreached = %v, want none: pam's open session received the broadcast under its alias", got)
	}
}

func TestUnreachedConfiguredSeatsNamesSeatsWithoutAnOpenSession(t *testing.T) {
	cov := &broadcastCoverage{
		configured: []string{"qcore/archer", "qcore/barry", "woodhouse", "gastown.mayor"},
		open: []session.Info{
			{ID: "ga-1", Alias: "woodhouse", ConfiguredNamedIdentity: "woodhouse"},
			{ID: "ga-2", Alias: "gastown.mayor", ConfiguredNamedIdentity: "gastown.mayor"},
			// closed: its seat is NOT reached even though the alias matches.
			{ID: "ga-3", Alias: "qcore/barry", ConfiguredNamedIdentity: "qcore/barry", Closed: true},
		},
	}
	// The mayor is the sender: excluded, never reported as unreached.
	got := unreachedConfiguredSeats(cov, map[string]bool{"woodhouse": true}, "gastown.mayor")
	want := []string{"qcore/archer", "qcore/barry"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unreached = %v, want %v", got, want)
	}
}

func TestUnreachedConfiguredSeatsNilCoverageIsSilent(t *testing.T) {
	if got := unreachedConfiguredSeats(nil, map[string]bool{"x": true}, "y"); got != nil {
		t.Fatalf("unreached = %v, want nil when coverage is unknown", got)
	}
}

// End to end through the send loop: the broadcast still sends to every open
// mailbox, and the under-delivery is printed on stderr and carried in --json.
func TestMailSendAllWarnsAndReportsUnreachedSeats(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "gastown.mayor": true, "woodhouse": true}
	cov := &broadcastCoverage{
		configured: []string{"woodhouse", "qcore/archer", "gastown.mayor"},
		open: []session.Info{
			{ID: "ga-1", Alias: "woodhouse", ConfiguredNamedIdentity: "woodhouse"},
			{ID: "ga-2", Alias: "gastown.mayor", ConfiguredNamedIdentity: "gastown.mayor"},
		},
	}

	var stdout, stderr bytes.Buffer
	code := doMailSendAllCoverage(mp, events.Discard, recipients, "gastown.mayor", []string{"FREEZE", "main frozen"}, nil, false, cov, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "to woodhouse") {
		t.Fatalf("broadcast did not reach the open mailbox:\n%s", stdout.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "reached 1 open mailbox(es); 1 configured named seat(s)") || !strings.Contains(errOut, "qcore/archer") {
		t.Fatalf("stderr does not name the unreached seat:\n%s", errOut)
	}

	stdout.Reset()
	stderr.Reset()
	code = doMailSendAllCoverage(mp, events.Discard, recipients, "gastown.mayor", []string{"FREEZE", "main frozen"}, nil, true, cov, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("json code = %d, want 0; stderr: %s", code, stderr.String())
	}
	var res mailActionResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		t.Fatalf("decode json %q: %v", stdout.String(), err)
	}
	if !reflect.DeepEqual(res.Unreached, []string{"qcore/archer"}) {
		t.Fatalf("json unreached = %v, want [qcore/archer]", res.Unreached)
	}
}

// Full coverage stays exactly as quiet as before.
func TestMailSendAllFullCoverageIsQuiet(t *testing.T) {
	store := beads.NewMemStore()
	mp := beadmail.New(store)
	recipients := map[string]bool{"human": true, "gastown.mayor": true, "woodhouse": true}
	cov := &broadcastCoverage{
		configured: []string{"woodhouse", "gastown.mayor"},
		open: []session.Info{
			{ID: "ga-1", Alias: "woodhouse", ConfiguredNamedIdentity: "woodhouse"},
			{ID: "ga-2", Alias: "gastown.mayor", ConfiguredNamedIdentity: "gastown.mayor"},
		},
	}
	var stdout, stderr bytes.Buffer
	if code := doMailSendAllCoverage(mp, events.Discard, recipients, "gastown.mayor", []string{"hi"}, nil, false, cov, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr on full coverage: %q", stderr.String())
	}
}
