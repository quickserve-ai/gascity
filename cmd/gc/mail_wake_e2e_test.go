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

// ga-y0hv2w.1: gc mail send --notify, end to end for a configured named seat.
// The recipient resolves through the configured-seat fast path (no session
// listing); the wake then resolves that recipient string through the real
// wake resolver and is delivered through the real worker to a fake runtime.
// The seat is asleep under a managed reconciler, so the wake must be queued
// for that session and the controller poked for it.
func TestMailSendNotifyWakesAFastPathResolvedSeatEndToEnd(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	cfg := oneSeatCity("mayor")
	mgr := newSessionManagerWithConfig(dir, store, fake, cfg)

	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Alias: "mayor", Template: "mayor", Title: "Mayor", Command: "claude", WorkDir: dir, Provider: "claude",
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

	// 3. The wake is delivered through the real worker.
	prevManaged, prevPoke := nudgeCityUsesManagedReconciler, nudgePokeController
	var pokeKeys []reconcilekey.Key
	nudgeCityUsesManagedReconciler = func(cityPath string) bool { return cityPath == dir }
	nudgePokeController = func(_ string, key reconcilekey.Key) error {
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
	pending, _, _, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || pending[0].Source != "mail" || !strings.Contains(pending[0].Message, "You have mail from human") {
		t.Fatalf("queued wakes = %+v, want one mail wake", pending)
	}
}
