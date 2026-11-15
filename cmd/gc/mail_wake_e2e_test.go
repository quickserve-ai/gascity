package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// ga-y0hv2w.1: from a fast-path-resolved recipient to a managed wake request.
// The recipient resolves through the configured-seat fast path (no session
// listing); that string resolves through the real wake resolver; the real
// worker then records a durable wake request on the seat's session, queues
// one mail wake fenced to that session, and pokes the controller for it. The
// controller, the runtime start and the mail write are not exercised.
func TestMailSendNotifyQueuesAManagedWakeForAFastPathResolvedSeat(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	cfg := oneSeatCity("mayor")
	mgr := newSessionManagerWithConfig(dir, store, fake, cfg)

	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Alias: "mayor", ExplicitName: "mayor", Template: "mayor", Title: "Mayor", Command: "claude", WorkDir: dir, Provider: "claude",
		Hints: runtime.Config{WorkDir: dir},
		ExtraMeta: map[string]string{
			"session_origin":            "named",
			"configured_named_session":  "true",
			"configured_named_identity": "mayor",
			"configured_named_mode":     "always",
		},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	sleepUnheld(t, store, info.ID)
	asleep, err := store.Get(info.ID)
	if err != nil || asleep.Metadata["wake_request"] != "" {
		t.Fatalf("precondition: wake_request = %q, %v; want none before the wake", asleep.Metadata["wake_request"], err)
	}
	epoch := asleep.Metadata["continuation_epoch"]

	// 1. The send resolves the recipient on the fast path.
	counted := &sessionListingCounter{Store: cliSessionStore(store.Store, cfg, dir)}
	recipient, err := resolveMailRecipientIdentityCached(dir, cfg, counted, "mayor", &mailIdentitySessionCache{})
	if err != nil || recipient != "mayor" || counted.sessionListCalls != 0 {
		t.Fatalf("recipient = %q, %v, listing calls %d; want mayor from the fast path", recipient, err, counted.sessionListCalls)
	}

	// 2. The wake resolves that string through the real resolver.
	target, err := resolveMailWakeTarget(recipient, func(id string) (nudgeTarget, error) {
		return resolveNudgeTargetInStore(dir, cfg, store, id, false)
	})
	if err != nil {
		t.Fatalf("resolveMailWakeTarget(%q): %v", recipient, err)
	}
	if target.sessionID != info.ID {
		t.Fatalf("wake target session = %q, want the seat's session %q", target.sessionID, info.ID)
	}

	// 3. The real worker records and queues the wake.
	prevManaged, prevPoke := nudgeCityUsesManagedReconciler, nudgePokeController
	var pokeKeys []reconcilekey.Key
	nudgeCityUsesManagedReconciler = func(cityPath string) bool { return cityPath == dir }
	nudgePokeController = func(cityPath string, key reconcilekey.Key) error {
		if cityPath != dir {
			t.Fatalf("poke city = %q, want %q", cityPath, dir)
		}
		pokeKeys = append(pokeKeys, key)
		return nil
	}
	t.Cleanup(func() { nudgeCityUsesManagedReconciler, nudgePokeController = prevManaged, prevPoke })

	if err := sendMailNotifyWithWorker(target, store, fake, "human", "gc-mail-1"); err != nil {
		t.Fatalf("sendMailNotifyWithWorker: %v", err)
	}
	if len(pokeKeys) != 1 || pokeKeys[0] != reconcilekey.Session(info.ID) {
		t.Fatalf("controller pokes = %v, want one for session %s", pokeKeys, info.ID)
	}
	woken, err := store.Get(info.ID)
	if err != nil || woken.Metadata["wake_request"] != string(session.WakeCauseExplicit) || woken.Metadata["wake_requested_at"] == "" {
		t.Fatalf("wake_request = %q at %q (%v); want an explicit, stamped wake request on the seat's session",
			woken.Metadata["wake_request"], woken.Metadata["wake_requested_at"], err)
	}
	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	if q := pending[0]; q.Source != "mail" || q.SessionID != info.ID || q.ContinuationEpoch != epoch || !strings.Contains(q.Message, "You have mail from human") {
		t.Fatalf("queued wake = %+v; want a mail wake fenced to session %s, epoch %q", q, info.ID, epoch)
	}
}
