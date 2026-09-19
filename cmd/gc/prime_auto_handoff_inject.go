package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
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
//
// sessionStartStore, when non-nil, is the city store the caller already opened
// (and proved the live managed session against) for this SessionStart; the
// mail context reuses it instead of resolving the city and opening it again.
func primeHookContextSuffix(cityPath string, sessionStartStore beads.Store, hookMode bool, hookContext primeHookContext, stderr io.Writer, consumeHandoff bool) primeHookContextInjection {
	if !hookMode {
		return primeHookContextInjection{}
	}
	injection := primeHookContextInjection{text: wispStepInjectionContent(cityPath)}
	if primeHookSessionStart(hookContext) {
		var (
			autoHandoff          primeHookContextInjection
			autoHandoffIDs       map[string]bool
			ordinaryMailProvider mail.Provider
		)
		if sessionStartStore != nil {
			autoHandoff, autoHandoffIDs, ordinaryMailProvider = sessionStartAutoHandoffInjectionWithStore(sessionStartStore, cityPath, stderr)
		} else {
			autoHandoff, autoHandoffIDs, ordinaryMailProvider = sessionStartAutoHandoffInjection(stderr)
		}
		injection.text += autoHandoff.text
		if consumeHandoff {
			injection.afterDelivery = autoHandoff.afterDelivery
		}
		// dip-bj7pgj: an autonomous/promptless restart runs this SessionStart
		// hook but never the UserPromptSubmit mail hook, so also surface ordinary
		// unread mail here so such a wake is not blind to it (including a
		// priority:1 message that was not sent as an auto-handoff). This block is
		// READ-ONLY — it never archives, so it can never consume/hide a message —
		// and it excludes the auto-handoff messages already rendered above so a
		// beadmail-backed ordinary provider does not double-render them.
		injection.text += primeUnreadMailInjectionWithProvider(autoHandoffIDs, ordinaryMailProvider)
		injection.text += primeLaurelsInjection(strings.TrimSpace(os.Getenv("GC_DIR")))
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
	return primeUnreadMailInjectionWithProvider(skip, nil)
}

// primeUnreadMailInjectionWithProvider is primeUnreadMailInjection over a
// caller-supplied ordinary-mail provider. SessionStart passes the provider it
// already built over its open stores; nil opens the configured city provider.
func primeUnreadMailInjectionWithProvider(skip map[string]bool, mp mail.Provider) string {
	messages := primeUnreadMailMessages(mp)
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
// A nil mp opens the configured city mail provider.
func primeUnreadMailMessages(mp mail.Provider) []mail.Message {
	if mp == nil {
		mp, _ = openCityMailProvider(io.Discard, "gc prime")
	}
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
// the current managed session, the set of auto-handoff message IDs it rendered
// (so the ordinary-unread-mail block can dedup against them), and the
// configured ordinary-mail provider built over the same stores (nil when it
// could not be built, which sends the caller back to the standalone opener). It
// intentionally constructs beadmail directly for the auto-handoff read: gc
// handoff persists this continuation class through beadmail regardless of any
// separately configured ordinary-mail provider.
func sessionStartAutoHandoffInjection(stderr io.Writer) (primeHookContextInjection, map[string]bool, mail.Provider) {
	store, cityPath, code := openCityStoreWithPath(io.Discard, "gc prime")
	if store == nil || code != 0 {
		return primeHookContextInjection{}, nil, nil
	}
	return sessionStartAutoHandoffInjectionWithStore(store, cityPath, stderr)
}

// sessionStartAutoHandoffInjectionWithStore is sessionStartAutoHandoffInjection
// over a caller-owned city store opened at cityPath. Class-store routing runs
// exactly as it does for a freshly opened store; only the redundant city
// resolution and store open are skipped.
func sessionStartAutoHandoffInjectionWithStore(store beads.Store, cityPath string, stderr io.Writer) (primeHookContextInjection, map[string]bool, mail.Provider) {
	if store == nil {
		return primeHookContextInjection{}, nil, nil
	}
	cfg, cfgErr := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	msgStore := resolveMailMessagesStore(cliStorageRoutes(cityPath), store, cfg, cityPath, nil)
	sessStore := cliSessionStore(store, cfg, cityPath)
	// The ordinary provider is exactly what openCityMailProvider would build:
	// the same name precedence as mailProviderName (GC_MAIL, then [mail]
	// provider), the same class-routed stores, and the cached beadmail. A config
	// that failed to load leaves it nil so the standalone opener decides.
	var ordinaryMailProvider mail.Provider
	if cfgErr == nil && cfg != nil {
		providerName := cfg.Mail.Provider
		if override := os.Getenv("GC_MAIL"); override != "" {
			providerName = override
		}
		ordinaryMailProvider = newMailProviderNamedWithSessionStore(providerName, msgStore, sessStore, true)
	}
	mp := beadmail.NewWithStores(msgStore, sessStore)
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	target, err := resolveMailTargetsWithConfig(cityPath, cfg, sessStore, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: resolving auto-handoff mailbox: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}, nil, ordinaryMailProvider
	}
	messages, err := mp.CheckAutoHandoffs(target.recipients)
	if err != nil {
		fmt.Fprintf(stderr, "gc prime: checking auto-handoff mail: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
		return primeHookContextInjection{}, nil, ordinaryMailProvider
	}
	if len(messages) == 0 {
		return primeHookContextInjection{}, nil, ordinaryMailProvider
	}
	ids := make(map[string]bool, len(messages))
	for _, m := range messages {
		ids[m.ID] = true
	}
	return primeHookContextInjection{
		text: formatInjectOutput(messages),
		afterDelivery: func() {
			archiveInjectedAutoHandoffMessages(mp, selectMailInjectWindow(messages), stderr)
		},
	}, ids, ordinaryMailProvider
}

// Laurels v0 (ga-9obb1h): a seat may keep a short laurels.md holding praise about
// its work that a PERSON originated (the operator, a customer, a partner).
// SessionStart surfaces it with nothing attached: no task, no bead, no priority.
// It is read here, at hook time, and never rendered into the prompt template, so
// adding a laurel changes no prompt hash and never drifts or restarts a seat. An
// absent, empty or non-regular file injects nothing.
//
// Two homes, checked in order: $GC_DIR/seat/laurels.md, the upstream seat home
// (gitignored as /seat/, #5776), then $GC_DIR/laurels.md, but only when GC_DIR is
// a city seat's .gc/agents/<name> home. A rig seat's GC_DIR is its project
// checkout, whose root laurels.md is repository content, not recognition.
const (
	laurelsFileName = "laurels.md"
	// The spec bounds a seat's laurels at one paragraph; the cap keeps a file
	// that outgrew it from taxing every boot, and bounds the read itself.
	laurelsMaxBytes = 1200
)

func primeLaurelsInjection(seatDir string) string {
	if seatDir == "" {
		return ""
	}
	paths := []string{filepath.Join(seatDir, "seat", laurelsFileName)}
	if isCitySeatHome(seatDir) {
		paths = append(paths, filepath.Join(seatDir, laurelsFileName))
	}
	for _, path := range paths {
		if text := readLaurels(path); text != "" {
			return "\n\n<laurels>\nRecognition from people this seat has worked for. It carries no task, no bead and no priority; nothing here asks you to do anything.\n\n" +
				text + "\n</laurels>\n"
		}
	}
	return ""
}

// isCitySeatHome reports whether dir is a city seat's .gc/agents/<name> home.
func isCitySeatHome(dir string) bool {
	parent := filepath.Dir(filepath.Clean(dir))
	return filepath.Base(parent) == "agents" && filepath.Base(filepath.Dir(parent)) == ".gc"
}

// readLaurels returns the trimmed, capped text of a REGULAR file, or "". It opens
// non-blocking and checks the OPENED descriptor, so a FIFO swapped in at any
// moment cannot block gc prime, and it reads at most laurelsMaxBytes+1 bytes.
// O_NOFOLLOW refuses a symlinked laurels.md: following one would send whatever
// it points at (a credential, a .env) to the provider at SessionStart.
func readLaurels(path string) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, laurelsMaxBytes+1))
	if err != nil {
		return ""
	}
	truncated := false
	if len(data) > laurelsMaxBytes {
		// Cut the raw bytes, not the trimmed text: data holds laurelsMaxBytes+1
		// bytes here, so data[cut] is always in range.
		cut := laurelsMaxBytes
		for cut > 0 && !utf8.RuneStart(data[cut]) {
			cut--
		}
		truncated = info.Size() > int64(laurelsMaxBytes+1) || strings.TrimSpace(string(data[cut:])) != ""
		data = data[:cut]
	}
	text := strings.TrimSpace(string(data))
	if text != "" && truncated {
		text += " [truncated]"
	}
	return text
}
