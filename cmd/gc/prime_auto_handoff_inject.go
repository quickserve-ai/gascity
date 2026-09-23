package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

type primeHookContextInjection struct {
	text          string
	afterDelivery func()
}

// primeHookContextSuffix builds the single provider-hook context owned by gc
// prime. A managed SessionStart receives durable auto-handoff mail here because
// a recycled successor can otherwise idle before any UserPromptSubmit hook.
//
// consumeHandoff gates only the destructive archive: preview callers (--json)
// still render the exact text the hook would emit, but must not consume the
// durable mail out from under the real SessionStart invocation.
func primeHookContextSuffix(cityPath string, hookMode bool, hookContext primeHookContext, stderr io.Writer, consumeHandoff bool) primeHookContextInjection {
	if !hookMode {
		return primeHookContextInjection{}
	}
	injection := primeHookContextInjection{text: wispStepInjectionContent(cityPath)}
	if primeHookSessionStart(hookContext) {
		// FIRST, because it frames everything below it: a seat that was killed
		// rather than finished has to know that before it reads a mailbox and
		// starts inferring what it was doing (ga-ksac39 S2). It renders only
		// when the previous ending composed no note, so in the ordinary
		// handoff case this is empty and costs nothing.
		notice := terminationNoticeForSessionStart(stderr)
		injection.text += notice.text

		autoHandoff, autoHandoffIDs := sessionStartAutoHandoffInjection(stderr)
		injection.text += autoHandoff.text
		if consumeHandoff {
			// BOTH consumers, or the notice repeats on every boot forever. An
			// earlier shape assigned autoHandoff.afterDelivery straight onto the
			// field, so adding a second producer here silently dropped one of
			// them — the kind of loss that only shows up as a seat being told
			// the same thing three times.
			injection.afterDelivery = composeAfterDelivery(autoHandoff.afterDelivery, notice.afterDelivery)
		}
		// dip-bj7pgj: an autonomous/promptless restart runs this SessionStart
		// hook but never the UserPromptSubmit mail hook, so also surface ordinary
		// unread mail here so such a wake is not blind to it (including a
		// priority:1 message that was not sent as an auto-handoff). This block is
		// READ-ONLY — it never archives, so it can never consume/hide a message —
		// and it excludes the auto-handoff messages already rendered above so a
		// beadmail-backed ordinary provider does not double-render them.
		injection.text += primeUnreadMailInjection(autoHandoffIDs)
		injection.text += primeLaurelsInjection(cityPath, strings.TrimSpace(os.Getenv("GC_AGENT")), stderr)
	}
	return injection
}

// primeInjectMailContent returns the unread-mail <system-reminder> block that
// `gc mail check --inject` produces for the current agent, or "" when there is
// no unread mail or the read fails. It is defense-in-depth for the promptless-
// wake gap (gastownhall/gascity dip-bj7pgj): an autonomous/promptless restart
// runs the SessionStart prime hook but NOT the UserPromptSubmit mail hook, so
// without it such a wake starts blind to unread mail — including priority:1
// restart handoffs. It is the standalone (no-exclusion) form of the ordinary-
// mail injection folded into the SessionStart hook context; see
// primeUnreadMailInjection.
func primeInjectMailContent() string {
	return primeUnreadMailInjection(nil)
}

// primeUnreadMailInjection renders the current agent's ordinary unread mail as a
// priority-sorted <system-reminder> block (the same shape the check path emits),
// excluding any message IDs in skip — the auto-handoff messages already rendered
// by sessionStartAutoHandoffInjection, so a beadmail-backed ordinary provider
// does not double-render them. It is READ-ONLY: unlike the check path it never
// archives/mutates mail (so the SessionStart preview cannot consume/hide a
// message), and any error degrades silently to "" so a prime is never blocked.
func primeUnreadMailInjection(skip map[string]bool) string {
	messages := primeUnreadMailMessages()
	if len(skip) > 0 {
		kept := make([]mail.Message, 0, len(messages))
		for _, m := range messages {
			if !skip[m.ID] {
				kept = append(kept, m)
			}
		}
		messages = kept
	}
	if len(messages) == 0 {
		return ""
	}
	return formatInjectOutput(messages)
}

// primeUnreadMailMessages returns the current agent's unread ordinary mail via
// the configured city mail provider, using the same identity candidates as the
// check path (GC_SESSION_ID/GC_ALIAS/GC_AGENT via defaultMailIdentityCandidates)
// but resolved by the provider's own recipient routing rather than by
// resolveMailTargetsWithConfig — so this reads the union of those candidates,
// not the first-resolving target. It is read-only and returns nil on any error.
func primeUnreadMailMessages() []mail.Message {
	mp, _ := openCityMailProvider(io.Discard, "gc prime")
	if mp == nil {
		return nil
	}
	messages, err := collectMailMessages(mp.Check, defaultMailIdentityCandidates())
	if err != nil {
		return nil
	}
	return messages
}

// sessionStartAutoHandoffInjection returns only durable auto-handoff mail for
// the current managed session, along with the set of auto-handoff message IDs it
// rendered (so the ordinary-unread-mail block can dedup against them). It
// intentionally constructs beadmail directly: gc handoff persists this
// continuation class through beadmail regardless of any separately configured
// ordinary-mail provider.
func sessionStartAutoHandoffInjection(stderr io.Writer) (primeHookContextInjection, map[string]bool) {
	store, cityPath, code := openCityStoreWithPath(io.Discard, "gc prime")
	if store == nil || code != 0 {
		return primeHookContextInjection{}, nil
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	msgStore := resolveMailMessagesStore(cliStorageRoutes(cityPath), store, cfg, cityPath, nil)
	sessStore := cliSessionStore(store, cfg, cityPath)
	mp := beadmail.NewWithStores(msgStore, sessStore)
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	target, err := resolveMailTargetsWithConfig(cityPath, cfg, sessStore, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: resolving auto-handoff mailbox: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}, nil
	}
	messages, err := mp.CheckAutoHandoffs(target.recipients)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: checking auto-handoff mail: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}, nil
	}
	if len(messages) == 0 {
		return primeHookContextInjection{}, nil
	}
	ids := make(map[string]bool, len(messages))
	for _, m := range messages {
		ids[m.ID] = true
	}
	injectedMessages := sortMailByPriority(messages)
	if len(injectedMessages) > mailInjectMaxMessages {
		injectedMessages = injectedMessages[:mailInjectMaxMessages]
	}
	return primeHookContextInjection{
		text: formatInjectOutput(messages),
		afterDelivery: func() {
			archiveInjectedAutoHandoffMessages(mp, injectedMessages, stderr)
		},
	}, ids
}

// composeAfterDelivery folds the SessionStart consumers into one callback,
// skipping nil producers. Each runs even if an earlier one panics: they are
// independent consumptions (archiving injected mail, stamping the termination
// notice) and one failing must not silently re-arm the other.
func composeAfterDelivery(fns ...func()) func() {
	live := make([]func(), 0, len(fns))
	for _, fn := range fns {
		if fn != nil {
			live = append(live, fn)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func() {
		for _, fn := range live {
			func() {
				defer func() { _ = recover() }()
				fn()
			}()
		}
	}
}

// terminationNoticeForSessionStart is the seam the wiring test swaps.
//
// A var rather than a direct call because the alternative is no coverage at all
// of whether this producer is still WIRED: the real injector needs a city and a
// GC_SESSION_ID, and with neither it returns empty — indistinguishable from
// having been deleted. "Deleted default wiring" is precisely the mutant class
// that has to stay killable here (ga-ksac39 S1 review), since the whole slice is
// worthless if the notice is composed correctly and never rendered.
var terminationNoticeForSessionStart = terminationNoticeInjection
