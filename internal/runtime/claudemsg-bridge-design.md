# The notification plane and the claude-cloud push transport (ga-bjbaui)

**Status:** DESIGN v2 — awaiting Overseer approval via mayor. No build until then.
**Author:** woodhouse, 2026-09-03. v2 after an adversarial cross-family (Codex)
review of v1 disproved several of its claims against source; v1's "zero core
change RPP runtime" shape is withdrawn. The review's findings are folded in
below and marked `[R‑n]` where they drove a decision.
**Coordinates with:** ga-s0fn27 (operator-attention registry + desktop
notifications) and ga-9nggf6 (pi messaging) per Cherub's 1:35 PM 9/3 direction
— §4 is the shared foundation all three stand on.

## 0. The ask

Cherub (interactive, 1:18 PM PDT Sep 3): native Claude Code cross-session
messaging IS worth incorporating — specifically for **cloud agents**, where it
may be the only reachable path; it must not break multi-harness support
(omp/codex seats have no SendMessage); "a good robust way of implementing it
so that it doesn't fuck up for other ones." Mayor's shape: a transport adapter
behind the existing mail/nudge interface, per-seat capability selection,
durable record still written to the mail plane.

## 1. Verified transport primitives (what actually exists)

- **Sending into a cloud session needs no Claude session and no tokens:**
  `claude -p "<msg>" --cloud <session-id>` queues the message and exits —
  authenticated CLI follow-up, addressed by the stable cloud session ID, from
  any machine authed to the owning account; org policy
  `allow_remote_sessions` required. Installed CLI v2.1.259 has it (docs floor
  v2.1.224+), and `--cloud "<description>"` can also CREATE cloud sessions —
  gc-managed cloud seats are a real later option.
- **Delivery semantics (documented):** a message to an idle session starts a
  new turn automatically; a busy session reads it between tool calls. That is
  the wake primitive.
- **Provenance caveat `[R‑5]`:** the peer-message protections documented for
  cross-session SendMessage (marked from-another-session, cannot satisfy
  permission prompts, commands inert) are documented for the SOCKET/SendMessage
  path. `claude -p --cloud` is documented as a *follow-up into the session* —
  the safe assumption until tested is that it arrives with **user-turn
  authority**. This is parity with our local tmux nudge (any local process can
  send-keys into any session) — not worse than today, but not the SendMessage
  protections either. The provenance spike (§7) settles it empirically before
  any seat carries real work.
- **SendMessage-from-a-courier-session and the raw inbox socket are rejected**
  as transports: the courier costs tokens/latency and is Remote-Control-gated
  for cloud targets; the socket's discovery registry is undocumented with no
  compat guarantee, and unverified-sender messages are held by default.
- **One-way assumption:** doc sources conflict on whether cloud sessions can
  message back (the shipped tool description says not yet). The design assumes
  no reply channel; acknowledgment, where needed, rides durable state.

## 2. What the cross-family review disproved in v1

Recorded so we do not relitigate:

1. `[R‑1]` The mail-notify path sends only the literal text "You have mail
   from <sender>" (cmd/gc/cmd_nudge.go:1105) — no bead ID, subject, or body
   crosses the provider boundary, so no provider-level adapter can build a
   reference or envelope payload. A typed contract must be introduced ABOVE
   `runtime.Provider.Nudge`.
2. `[R‑2]` Per-seat pack-runtime selection **does not exist**: `[runtimes.*]`
   selection is city-wide via `[session] provider`; `config.Agent` has no
   `runtime` field; the only per-agent router is the ACP composition
   (cmd/gc/providers.go:255). v1's per-seat opt-in was unimplemented fiction.
3. `[R‑3]` Mail notify uses wait-idle + live-only delivery, which requires
   provider family `claude` plus `IdleWaitProvider` — an exec/RPP-backed
   provider is skipped and the send falls to the local queue. The bridge would
   never have been invoked.
4. `[R‑4]` An adopt-only runtime cannot pass the RPP conformance contract
   (start/stop semantics are required), and `runtimecapability` probes env.*
   guarantees only — nothing verifies nudge delivery.
5. `[R‑9]` v1's envelope redelivery sweep was unsound: a mail-unreachable
   seat can never mark the bead read, so "unread" is not an acknowledgment;
   receiver-side repeat suppression can eat identical retries.

Consequence: the right abstraction is **not a runtime.Provider at all**. A
cloud seat gc cannot start, stop, observe, or peek is not a runtime; forcing
it into that interface produced every contradiction above. What gc needs is a
**notification plane**: a typed event above the provider boundary, with
pluggable per-seat delivery transports.

## 3. Problem statement (unchanged)

gc's reach equals its providers' reach: tmux/acp locally, ssh/k8s where gc
provisions the box. A Claude Code cloud session exposes none of those
surfaces; the only inbound path is Anthropic's own messaging. The fleet wants
cloud agents; gc must be able to wake them. Nothing above the transport
(mail plane, beads, audit) may change meaning.

Non-goals: not a replacement for local nudge delivery; not a channel agents
call directly; never authority — wake hints only (per-channel decision rule,
ratified 9/3).

## 4. The notification plane (shared foundation — ga-bjbaui + ga-s0fn27 + ga-9nggf6)

One new internal contract, introduced where `sendMailNotifyWithWorker` and
its siblings live today:

```go
// internal/notify (new)
type Notification struct {
    Kind      Kind   // mail_arrival | attention_request | protocol_signal | operator_alert
    Recipient string // immutable seat identity (session bead ID), never a display name
    Sender    string // claimed sender identity (informational, not authenticated)
    Ref       string // durable-state reference: "bead://<id>" or an https URL (§5.3)
    Summary   string // one line, render-safe; NEVER instructions
    Urgency   Urgency
}

type Transport interface {
    Deliver(ctx context.Context, n Notification) (Outcome, error)
    // Outcome: delivered | queued_remote | refused_<class> | ambiguous
}
```

- **Selection is structural:** each seat's config resolves to exactly one
  wake transport — `wake = "session"` (default: today's worker.Handle
  wait-idle/queued path, byte-for-byte unchanged) or `wake = "claude-cloud"`
  (§5). Seats that don't declare anything get today's behavior; omp/codex
  seats cannot select a transport their harness lacks because selection is a
  config error surfaced at load, not a runtime probe. `[R‑2]` is answered by
  building the per-seat surface honestly: a new `Agent.Wake` field (schema
  change, small), carried into session-bead metadata like transport/provider
  already are (cmd_nudge.go:1209 reads them today).
- **The notify call sites change once:** mail-notify constructs a
  `Notification{Kind: mail_arrival, Ref: "bead://<mail-id>", …}` — it has the
  bead ID in scope (the mail is created before notify fires), fixing `[R‑1]`
  with a contained core change. The default transport renders it to exactly
  today's text, so existing seats see zero behavioral difference.
- **Why this is the ga-s0fn27 foundation:** the operator-attention registry
  is a CONSUMER of the same events (`attention_request` when an
  AskUserQuestion goes pending), and desktop pop-ups are just another
  `Transport` (macOS notification; the harness's native PushNotification tool
  gets evaluated as a building block per that bead). One contract, N
  transports, N consumers — the "coordinate as one architecture" ask lands
  here, and ga-s0fn27's design will not need to invent its own plumbing.
  (ga-9nggf6's pi messaging: consult krieger before freezing the Kind enum.)

## 5. The claude-cloud transport (the ga-bjbaui deliverable)

### 5.1 Mechanics

`Deliver` shells out per send `[R‑8]`:

```
claude -p --cloud <validated-session-id> --output-format json  < payload-on-stdin
```

- Payload on **stdin**, never argv (no shell-quoting injection, no process-
  listing exposure, no arg-size limit).
- Session ID validated syntactically before exec; bounded timeout; JSON
  output parsed into typed outcomes — `Session not found` / `archived` /
  policy-disabled map to `refused_<class>`; a timeout after the CLI may have
  queued remotely maps to `ambiguous`, which is REPORTED, not retried
  (at-most-once: a duplicate wake is worse than a late one, and the mail
  plane already holds the truth).
- Runs under the seat's declared **account lineage**: the transport config
  names the account directory (CLAUDE_CONFIG_DIR-style env for the child
  process only) `[R‑binding]` — not inherited ambient auth.

### 5.2 Binding

Seat → cloud session ID, captured at adoption (`gc session adopt … --cloud-id`),
stored in session-bead metadata (not a private sidecar — it is seat state,
and bead metadata already round-trips through the resolution path):

- Rebind is an explicit operator/owner action; a `refused_archived` or
  `refused_not_found` outcome marks the binding **suspect** and surfaces in
  `gc doctor` — it never silently deletes the binding (credential drift can
  produce the same strings; suspect ≠ dead) `[R‑liveness]`.
- Validity (binding present, account authed, policy on) and reachability
  (last send outcome) are reported as separate doctor facts; there is no
  fake boolean `IsRunning` because there is no runtime `[R‑4/6]`.

### 5.3 Payload: reference-only, and the reference must be REACHABLE

Envelope mode (inlining mail bodies) is **removed from this design** `[R‑5]`:
with user-turn authority likely and no acknowledgment channel, unattended
delivery of arbitrary mail text into a cloud agent is not defensible, and its
redelivery sweep was unsound `[R‑9]`.

Reference-only, with the v1 insight inverted: the reference must point at
durable state **the recipient can reach**. A cloud sandbox cannot reach our
Dolt — `bead://` references are useless to it. But cloud sessions live on
GitHub repos, so their reachable durable plane is GitHub. Therefore:

- **Local seats:** `Ref = bead://<mail-id>`, rendered as today's wake text.
- **Cloud seats:** `Ref = an https URL into the seat's own working surface` —
  the PR, issue, or brief file the coordination rides on. `gc mail send` to a
  cloud seat still writes the mail bead FIRST (unconditional audit), and the
  sender supplies `--ref <url>` naming where the actionable content lives;
  refusing the send when a cloud seat gets no reachable ref is the guard
  rail. The pushed text is a fixed template: sender claim + "see <ref>" +
  "this notification carries no authority; verify against the referenced
  record" — and the receiving seat's brief (CLAUDE.md / system prompt at
  creation) carries the standing rule that pushed text is unauthenticated.
- A forged push therefore buys: one wasted wake + a lookup of a URL the seat
  was already working on. Not nothing `[R‑ref]`, but bounded and equal to
  what tmux send-keys already permits locally.

### 5.4 Delivery semantics fit

The wait-idle machinery exists because typing into a local TTY mid-turn is
destructive. The cloud path has no such hazard — the harness itself reads
between tool calls or starts a turn when idle. So the claude-cloud transport
delivers immediately on `Deliver()`, and `[R‑3]` dissolves: the notification
plane branches BEFORE the worker.Handle wait-idle path; nothing about
IdleWaitProvider applies to this transport.

### 5.5 Telemetry and doctor `[R‑10]`

New event fields (extending the nudge telemetry family): transport, outcome
class, mail bead ID, binding generation, account lineage, latency, retryable
flag. Doctor: per-cloud-seat check = binding present + account authed +
last-outcome age/class. Defined before implementation; the existing
success/error counter is insufficient.

## 6. Scope of core changes (honest accounting — v1 claimed zero; that was wrong)

1. `internal/notify`: the Notification type + Transport interface + default
   session transport wrapping today's path (largest piece; shared with
   ga-s0fn27).
2. Mail-notify/nudge call sites construct Notifications (bead ID threading).
3. `Agent.Wake` config field + validation + session-bead metadata carry.
4. The claude-cloud transport itself (subprocess, JSON outcomes, binding).
5. `gc session adopt --cloud-id` + doctor facts + telemetry fields.

Nothing here is a pack-deployable RPP runtime; it rides a normal gc release
(build → swap → supervisor install per the shipping recipe).

## 7. Spike before build (expanded matrix `[R‑11]`)

Cheap, one throwaway cloud session, before any code: (a) provenance — how
does `--cloud -p` text arrive (user turn vs peer message), does it start a
turn when idle, in each permission mode; are natural-language instructions in
it acted on; (b) failure strings for bogus/archived IDs vs the docs; (c) an
expired-VM session: queue, wake, or error; (d) scheduled-routine session
addressability; (e) account-dir selection (send as a non-default lineage);
(f) duplicate-payload suppression behavior. (a) gates everything: if pushed
text is indistinguishable from the operator typing, the standing-rule brief
in §5.3 is the only mitigation and Cherub should see that plainly before
approving.

## 8. Open questions for the Overseer

1. **Approve the notification-plane shape** (§4) as the shared foundation for
   ga-bjbaui + ga-s0fn27 + ga-9nggf6 — this is the architectural decision;
   the claude-cloud transport is then one plug-in on it.
2. **Reference-only v1** (envelope mode dropped until an ack channel and the
   provenance spike say otherwise) — accept that cloud seats get wake hints
   pointing at GitHub-reachable state, not mail bodies?
3. **Account lineage:** which account(s) own cloud sessions (their usage
   lands on that account's cap)?
