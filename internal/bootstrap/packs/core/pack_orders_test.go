package core

import (
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

// readOrder parses an order TOML from the embedded pack FS and restores the
// Name the scanner would normally derive from the filename (Parse leaves it
// blank because Name is not a TOML field).
func readOrder(t *testing.T, file string) orders.Order {
	t.Helper()
	data, err := fs.ReadFile(PackFS, "orders/"+file)
	if err != nil {
		t.Fatalf("reading orders/%s: %v", file, err)
	}
	o, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("parsing orders/%s: %v", file, err)
	}
	o.Name = strings.TrimSuffix(file, ".toml")
	return o
}

// TestCoreOrdersValidate asserts every embedded order TOML parses and
// passes structural validation, so a malformed order can never ship in the gc
// binary's bundled core pack.
func TestCoreOrdersValidate(t *testing.T) {
	entries, err := fs.ReadDir(PackFS, "orders")
	if err != nil {
		t.Fatalf("reading orders dir: %v", err)
	}
	saw := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		saw = true
		o := readOrder(t, e.Name())
		if err := orders.Validate(o); err != nil {
			t.Errorf("order %s failed validation: %v", e.Name(), err)
		}
	}
	if !saw {
		t.Fatal("no order TOML files found in embedded pack")
	}
}

// assertEventExecOrder checks an event-triggered exec order: it must validate,
// listen for the expected event type, dispatch via exec (not a formula/pool),
// and point at a script that is actually embedded in the pack.
func assertEventExecOrder(t *testing.T, orderFile, eventType, scriptBase string) {
	t.Helper()
	o := readOrder(t, orderFile)
	if err := orders.Validate(o); err != nil {
		t.Fatalf("%s failed validation: %v", orderFile, err)
	}
	if o.Trigger != "event" {
		t.Errorf("%s: trigger = %q, want %q", orderFile, o.Trigger, "event")
	}
	if o.On != eventType {
		t.Errorf("%s: on = %q, want %q", orderFile, o.On, eventType)
	}
	if !o.IsExec() {
		t.Errorf("%s: want exec dispatch, got formula %q", orderFile, o.Formula)
	}
	if o.Pool != "" {
		t.Errorf("%s: exec orders must not set a pool, got %q", orderFile, o.Pool)
	}
	wantSuffix := "assets/scripts/" + scriptBase
	if !strings.HasSuffix(o.Exec, wantSuffix) {
		t.Errorf("%s: exec = %q, want suffix %q", orderFile, o.Exec, wantSuffix)
	}
	if _, err := fs.ReadFile(PackFS, "assets/scripts/"+scriptBase); err != nil {
		t.Errorf("%s: referenced script not embedded: %v", orderFile, err)
	}
}

// TestNudgeOnRouteOrder pins the nudge-on-route order's event contract: it wakes
// on bead.updated and runs the nudge-on-route script.
func TestNudgeOnRouteOrder(t *testing.T) {
	assertEventExecOrder(t, "nudge-on-route.toml", "bead.updated", "nudge-on-route.sh")
}

// TestCascadeNudgeOnBlockerCloseOrder pins the cascade-nudge order's event
// contract: it wakes on bead.closed — the event the close transition actually
// emits — and runs the cascade-nudge script.
func TestCascadeNudgeOnBlockerCloseOrder(t *testing.T) {
	assertEventExecOrder(t, "cascade-nudge-on-blocker-close.toml", "bead.closed", "cascade-nudge-on-blocker-close.sh")
}

// TestCascadeNudgeRoutesCrossRig guards the cascade order's cross-rig
// routing. Two properties must hold or cross-rig cascades break silently
// (failures are soft-skipped via `|| continue`, so a regression is invisible
// at runtime): (1) the dependent lookup runs through the `gc bd` wrapper, not
// bare `bd` — `--rig` is a gc flag, not a bd flag, and the wrapper runs bd in
// the owning rig's directory; (2) the prefix->rig lookup excludes the HQ entry
// (`gc rig list` reports the city root as an hq=true pseudo-rig that
// `gc --rig <cityName>` cannot resolve), matching orphan-sweep.sh's
// `select(.hq == false)` convention.
func TestCascadeNudgeRoutesCrossRig(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/cascade-nudge-on-blocker-close.sh")
	if err != nil {
		t.Fatalf("reading cascade-nudge-on-blocker-close.sh: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "gc bd dep list") {
		t.Error("cascade-nudge script must route the dep lookup through `gc bd dep list`; missing")
	}
	if strings.Contains(body, "$(bd dep list") {
		t.Error("cascade-nudge script must not run bare `bd dep list` (--rig is a gc flag, not a bd flag)")
	}
	if !strings.Contains(body, ".hq != true") {
		t.Error("cascade-nudge script must exclude the HQ entry from the prefix->rig lookup; missing `.hq != true`")
	}
}

// TestNudgeOnRouteResolvesPoolMembers guards the pool-base fan-out: a
// multi-session pool routes to the pool BASE (sling's NormalizePoolRouteTarget
// collapses slot -> base), which is the members' template, not a session name
// `gc session nudge` can resolve. The script must therefore enumerate pool
// members by template before nudging — a naive `gc session nudge "$routed_to"`
// silently no-ops for exactly the warm-idle pool workers this order targets.
func TestNudgeOnRouteResolvesPoolMembers(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/nudge-on-route.sh")
	if err != nil {
		t.Fatalf("reading nudge-on-route.sh: %v", err)
	}
	body := string(data)
	for _, want := range []string{"gc session list", "--template"} {
		if !strings.Contains(body, want) {
			t.Errorf("nudge-on-route.sh must resolve pool members; missing %q", want)
		}
	}
}

// TestNotifyOnHumanGateCreationOrder pins the notify-on-human-gate-creation
// order's event contract: it wakes on bead.created — the event synthesized for
// any newly-appeared bead — and runs the notify-on-human-gate-creation script.
func TestNotifyOnHumanGateCreationOrder(t *testing.T) {
	assertEventExecOrder(t, "notify-on-human-gate-creation.toml", "bead.created", "notify-on-human-gate-creation.sh")
}

// TestNotifyOnHumanGateCreationScriptContract guards the load-bearing behaviors
// of the notify script. Each property, if it regresses, breaks the order
// silently (failures are best-effort and swallowed at runtime), so they are
// pinned here:
//
//   - The bead.created payload does NOT carry await_type, so a human gate is
//     indistinguishable from a timer/gh gate at the event alone. The script
//     must re-fetch the bead via `gc bd show` and gate on await_type == "human"
//     AND status == "open" — otherwise it would notify on every gate creation
//     (or none).
//   - Addressee resolution must consult gc.deferred_assignee: formula/molecule
//     gates strip the assignee to that metadata key at create time, so a naive
//     `.assignee`-only lookup finds an empty addressee and misroutes to the
//     human fallback for exactly the automated gates that name a real one.
//   - Notification must ride `gc mail send --notify`, the one primitive that
//     mails AND nudges a real session while natively skipping the tmux-nudge
//     for the sessionless "human" recipient (cmd_mail.go `to != "human"`). A
//     hand-rolled `gc session nudge` would fail on the human channel.
//   - The prefix->rig lookup must exclude the HQ entry (`gc rig list` reports
//     the city root as an hq=true pseudo-rig `gc --rig <cityName>` cannot
//     resolve), matching the cross-rig convention in the sibling scripts.
//   - Event-shape robustness: the API envelope wraps the bead under
//     .payload.bead, but the `gc events` local fallback (API down) emits the
//     bead fields directly under .payload. The filter must read both via
//     `(.payload.bead // .payload)` or it silently finds no gates in fallback
//     mode — exactly when notifications matter most.
//   - Loud-fail: an undeliverable send must surface and NOT be recorded as
//     done. Surfacing requires a NON-ZERO exit — the controller logs an exec
//     order's captured output only on a non-zero exit — so the script must
//     exit non-zero when any send failed (gastownhall/gascity#4543).
func TestNotifyOnHumanGateCreationScriptContract(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/notify-on-human-gate-creation.sh")
	if err != nil {
		t.Fatalf("reading notify-on-human-gate-creation.sh: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		"(.payload.bead // .payload)", // normalize API-envelope vs local-fallback event shape
		`$b.issue_type == "gate"`,     // filter events to gate creations
		"gc bd show",                  // re-fetch (event lacks await_type)
		`"$AWAIT_TYPE" = "human"`,     // human gates only
		`"$STATUS" = "open"`,          // skip already-resolved gates
		`gc.deferred_assignee`,        // formula/molecule addressee
		"--notify",                    // mail + nudge, human-safe primitive
		".hq != true",                 // exclude HQ from prefix->rig lookup
	} {
		if !strings.Contains(body, want) {
			t.Errorf("notify-on-human-gate-creation.sh missing load-bearing element %q", want)
		}
	}

	// Loud-fail: the send must be conditional (retry on failure), and the
	// failure path must surface to stderr rather than silently record the gate
	// as notified. The dedup record must live on the SUCCESS branch only.
	if !strings.Contains(body, "if gc mail send") {
		t.Error("notify-on-human-gate-creation.sh must branch on the mail-send result (loud-fail retry), not fire-and-forget")
	}
	if !strings.Contains(body, "will retry next sweep") {
		t.Error("notify-on-human-gate-creation.sh must surface an undeliverable send to stderr (loud-fail #4543)")
	}
	// The controller captures an exec order's combined output but logs it only
	// on a NON-ZERO exit (order_dispatch.go), so a fire-and-forget exit 0 would
	// swallow the failure lines above. The script must exit non-zero when any
	// send failed — after writing state, so recorded successes are not lost.
	if !strings.Contains(body, `"$FAILED" -gt 0`) {
		t.Error("notify-on-human-gate-creation.sh must exit non-zero when a send failed, or the loud-fail message is never logged (#4543)")
	}
}

// assertCooldownExecOrder checks a cooldown-triggered exec order: it must
// validate, run on a cooldown trigger with a parseable interval, dispatch via
// exec (not a formula/pool), and point at a script embedded in the pack.
func assertCooldownExecOrder(t *testing.T, orderFile, scriptBase string) {
	t.Helper()
	o := readOrder(t, orderFile)
	if err := orders.Validate(o); err != nil {
		t.Fatalf("%s failed validation: %v", orderFile, err)
	}
	if o.Trigger != "cooldown" {
		t.Errorf("%s: trigger = %q, want %q", orderFile, o.Trigger, "cooldown")
	}
	if _, err := time.ParseDuration(o.Interval); err != nil {
		t.Errorf("%s: interval %q is not a valid duration: %v", orderFile, o.Interval, err)
	}
	if !o.IsExec() {
		t.Errorf("%s: want exec dispatch, got formula %q", orderFile, o.Formula)
	}
	if o.Pool != "" {
		t.Errorf("%s: exec orders must not set a pool, got %q", orderFile, o.Pool)
	}
	wantSuffix := "assets/scripts/" + scriptBase
	if !strings.HasSuffix(o.Exec, wantSuffix) {
		t.Errorf("%s: exec = %q, want suffix %q", orderFile, o.Exec, wantSuffix)
	}
	if _, err := fs.ReadFile(PackFS, "assets/scripts/"+scriptBase); err != nil {
		t.Errorf("%s: referenced script not embedded: %v", orderFile, err)
	}
}

// TestRenudgeStaleHumanGatesOrder pins the staleness-sweep order's contract: it
// is a cooldown-triggered exec order running the renudge-stale-human-gates
// script. It is the repeating companion to notify-on-human-gate-creation (which
// fires once, on bead.created); this one re-fires on a cooldown for gates left
// open.
func TestRenudgeStaleHumanGatesOrder(t *testing.T) {
	assertCooldownExecOrder(t, "renudge-stale-human-gates.toml", "renudge-stale-human-gates.sh")
}

// TestRenudgeStaleHumanGatesScriptContract guards the load-bearing behaviors of
// the staleness re-nudge script. Like the creation-notify script its failures
// are best-effort and swallowed at runtime, so the contract is pinned here:
//
//   - Enumeration is over OPEN gates (`gc bd gate list`, open-only by default)
//     with `--limit 0` so a rig past the default 50-gate page is not silently
//     truncated — a truncated page would drop stale gates from the sweep.
//   - It re-nudges ONLY open human gates: await_type == "human" AND
//     status == "open". The live town carries dozens of legacy await_type=null
//     workflow gates that must never be mailed about.
//   - Both the staleness threshold and the repeat interval are configurable
//     (GC_STALE_GATE_THRESHOLD / GC_STALE_GATE_RENUDGE_INTERVAL) — the order's
//     purpose is "open past a configurable threshold, repeating on the
//     interval".
//   - Addressee resolution consults gc.deferred_assignee (formula/molecule
//     gates strip the assignee there), matching the creation notify so a gate
//     is re-nudged at the same address it was first notified.
//   - The list projection omits assignee/metadata, so the script must re-fetch
//     via `gc bd show` to resolve the addressee.
//   - Notification rides `gc mail send --notify`, the one primitive that mails
//     AND nudges a real session while natively skipping the tmux-nudge for the
//     sessionless "human" recipient (cmd_mail.go `to != "human"`).
//   - The prefix->rig enumeration excludes the HQ pseudo-rig (`.hq != true`),
//     matching the sibling scripts' cross-rig convention.
//   - Timestamp parsing is portable: GNU-only `date -d` returns empty on
//     BSD/macOS, skipping every gate and silently disabling the sweep, so the
//     BSD `date -ju -f` fallback (matching wisp-compact.sh) is required.
//   - Loud-fail: an undeliverable send must surface and NOT be recorded. As
//     with the creation notify, surfacing requires a NON-ZERO exit (the
//     controller logs an exec order's output only on a non-zero exit), so the
//     script must exit non-zero when any re-nudge failed (#4543).
func TestRenudgeStaleHumanGatesScriptContract(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/renudge-stale-human-gates.sh")
	if err != nil {
		t.Fatalf("reading renudge-stale-human-gates.sh: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		"gc bd gate list",                // enumerate OPEN gates (not events)
		"--limit 0",                      // no silent 50-gate truncation
		`.await_type == "human"`,         // human gates only
		`.status == "open"`,              // skip already-resolved gates
		"GC_STALE_GATE_THRESHOLD",        // configurable staleness threshold
		"GC_STALE_GATE_RENUDGE_INTERVAL", // configurable repeat interval
		"gc bd show",                     // re-fetch (list omits assignee)
		"gc.deferred_assignee",           // formula/molecule addressee
		"--notify",                       // mail + nudge, human-safe primitive
		".hq != true",                    // exclude HQ from prefix->rig lookup
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renudge-stale-human-gates.sh missing load-bearing element %q", want)
		}
	}

	// Loud-fail: the send must be conditional (retry on failure), and the
	// failure path must surface to stderr rather than silently record the gate
	// as re-nudged. The dedup record must live on the SUCCESS branch only.
	if !strings.Contains(body, "if gc mail send") {
		t.Error("renudge-stale-human-gates.sh must branch on the mail-send result (loud-fail retry), not fire-and-forget")
	}
	if !strings.Contains(body, "will retry next sweep") {
		t.Error("renudge-stale-human-gates.sh must surface an undeliverable send to stderr (loud-fail #4543)")
	}
	// Timestamp parsing must be portable: GNU-only `date -d` returns empty on
	// BSD/macOS, which skips every gate at the age check and silently disables
	// the whole sweep. The BSD `date -ju -f` fallback (matching wisp-compact.sh)
	// is load-bearing.
	if !strings.Contains(body, "date -ju -f") {
		t.Error("renudge-stale-human-gates.sh must parse timestamps portably via the BSD `date -ju -f` fallback; GNU-only `date -d` disables the sweep on macOS")
	}
	// Same loud-fail exit contract as the creation notify: the controller logs
	// an exec order's output only on a non-zero exit.
	if !strings.Contains(body, `"$FAILED" -gt 0`) {
		t.Error("renudge-stale-human-gates.sh must exit non-zero when a re-nudge failed, or the loud-fail message is never logged (#4543)")
	}
}

// TestCoreEscalationScriptContract guards the escalation contract shared by the
// two exec-order scripts that mail somebody when they detect trouble. Core
// ships no coordinator or work-health role, so both properties below are
// load-bearing and neither is visible at runtime when it regresses:
//
//   - The recipient must be configurable via $GC_ESCALATION_TARGET and must
//     default to the reserved `human` alias, which resolves in every city. A
//     hardcoded `mayor/` or `<rig>/witness` names a role core does not ship, so
//     the send fails in every core-only city — and it fails into a discarded
//     stream, which is the second half of this contract.
//   - Loud-fail: an undeliverable escalation must surface to stderr AND the
//     script must exit non-zero. The controller captures an exec order's
//     combined output but logs it only on a non-zero exit
//     (cmd/gc/order_dispatch.go), so a fire-and-forget exit 0 discards the
//     stderr line and the escalation evaporates without a trace
//     (gastownhall/gascity#4543). The non-zero exit must come AFTER the
//     script's own state write / summary output, so neither is lost.
func TestCoreEscalationScriptContract(t *testing.T) {
	for _, script := range []string{
		"spawn-storm-detect.sh",
		"orphan-sweep.sh",
	} {
		t.Run(script, func(t *testing.T) {
			data, err := fs.ReadFile(PackFS, "assets/scripts/"+script)
			if err != nil {
				t.Fatalf("reading %s: %v", script, err)
			}
			body := string(data)

			for _, want := range []string{
				`ESCALATION_TARGET="${GC_ESCALATION_TARGET:-human}"`, // configurable, human-safe default
				`gc mail send "$ESCALATION_TARGET"`,                  // send to the configured target
				"if ! gc mail send",                                  // branch on the result, not fire-and-forget
				`"$FAILED" -gt 0`,                                    // non-zero exit so the failure is logged
			} {
				if !strings.Contains(body, want) {
					t.Errorf("%s missing load-bearing escalation element %q", script, want)
				}
			}

			// Roles core does not ship. Either one routes the escalation to a
			// recipient that cannot resolve in a core-only city.
			for _, banned := range []string{
				"gc mail send mayor/",
				"<rig>/witness",
			} {
				if strings.Contains(body, banned) {
					t.Errorf("%s hardcodes %q; core ships no such role — route via $ESCALATION_TARGET", script, banned)
				}
			}
		})
	}
}

// TestDetectSilentPublishedWorkOrder pins the silence detector's dispatch
// contract: a mechanical cooldown sweep run via exec, no pool, pointing at an
// embedded script.
func TestDetectSilentPublishedWorkOrder(t *testing.T) {
	assertCooldownExecOrder(t, "detect-silent-published-work.toml", "detect-silent-published-work.sh")
}

// TestDetectSilentPublishedWorkScriptContract guards the behaviors that make
// this detector trustworthy. Its runtime failures are best-effort and its whole
// purpose is to be believed when it says "nothing is stalled", so the load-
// bearing elements are pinned here rather than left to review:
//
//   - EVIDENCE DISCIPLINE (ga-mmvpq1 08:10Z, binding): progress is established
//     by comparing live state against a persisted OBSERVATION, never by reading
//     a record of what a prior sweep did. The persisted keys are therefore
//     observed_* and first_observed_in_state_at, and gate dedup/auto-resolve go
//     through a live gate query. An action log can lose an entry for an action
//     that succeeded or keep one for an action that never took effect, and
//     neither is detectable from the log — so a log-based detector reports clean
//     sweeps that never happened, which is the exact class it exists to catch.
//   - READ-FAILURE IS NOT ABSENCE: a failed read classifies UNKNOWN and is
//     skipped, never counted as silence (false alarm on every PR at once) and
//     never as health (the fault, hidden by its own detector). Mirrors the
//     refinery predicate's "refusing rather than reporting a read failure as
//     gate-absence".
//   - STICKY IDENTITY IS AUTHENTICATED: a bare startswith match is not enough —
//     a read-only member can post a lookalike gate comment. The author's
//     EFFECTIVE repo permission must be write or higher, matching the predicate.
//   - HEALTH-BLIND: the detector must not read CI conclusions, mergeability, or
//     review verdicts. No PR-health signal separates the four exhibits, and
//     reading them would rank a red-but-worked PR below a green abandoned one.
//   - NOT OBSERVE-ONLY (explicit ratification prohibition): there must be no
//     dry-run knob. A detector that observes and does not act is
//     indistinguishable from one that is switched off.
//   - Portable timestamps: GNU-only `date -d` returns empty on BSD/macOS, which
//     would fail every age check and pass the whole sweep as clean.
//   - Loud-fail: surfacing requires a NON-ZERO exit, since the controller logs
//     an exec order's output only on failure (#4543).
func TestDetectSilentPublishedWorkScriptContract(t *testing.T) {
	data, err := fs.ReadFile(PackFS, "assets/scripts/detect-silent-published-work.sh")
	if err != nil {
		t.Fatalf("reading detect-silent-published-work.sh: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		"pr_published_awaiting_gate", // B1 arm (i): the published states
		"gate_clear_awaiting_merge",
		"first_observed_in_state_at",           // B3: the clock, anchored on observation
		"observed_head",                        // B3: push detected by head comparison
		"observed_marker",                      // B3: re-render detected by marker change
		"GC_SILENT_WORK_THRESHOLD",             // B3: the one knob
		"collaborators/",                       // B2: effective-permission authentication
		"## Sherpa gate",                       // B2: sticky selection, predicate-identical
		`\*\*Gate status\*\*`,                  // B2: the canonical verdict line (sed-escaped in the script)
		`\*\*HEAD\*\*`,                         // B2: the pinned-head freshness marker (sed-escaped)
		"silent-ungated",                       // B2: STATE 1
		"silent-stale",                         // B2: STATE 2
		"silent-unexecuted",                    // B2: STATE 2' (refinery as dead owner)
		"bypass-detected",                      // A6: merged with no CLEAR gate
		`gc bd --city "$CITY_ABS" gate create`, // B4(ii)/locality: the mint, town-pinned
		"gc.silent_work_alarm",                 // B4(i): the bead-attached record
		"THIS WORK EXISTS",                     // B4(i): the anti-duplication signal
		"gh pr list",                           // B1 arm (ii): reconciliation
		"--limit 0",                            // no silent truncation of candidates
		"date -ju -f",                          // portable timestamps (BSD/macOS)
		".hq != true",                          // cross-rig convention
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detect-silent-published-work.sh missing load-bearing element %q", want)
		}
	}

	// Health-blind by construction. If any of these ever appear, the detector
	// has started ranking PRs by health, which demonstrably does not separate
	// the exhibits it exists to catch.
	for _, forbidden := range []string{
		"mergeable",
		"check-runs",
		"statusCheckRollup",
		"reviewDecision",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("detect-silent-published-work.sh reads PR health signal %q; the detector must key on absence of progress only", forbidden)
		}
	}

	// The explicit ratification prohibition: no observe-only escape hatch.
	for _, forbidden := range []string{"DRY_RUN", "dry_run", "--dry-run"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("detect-silent-published-work.sh contains %q; ga-mmvpq1 ratification prohibits shipping this detector in observe-only mode", forbidden)
		}
	}

	// Read-failure must classify UNKNOWN and skip, never fall through to a
	// silence verdict or a health verdict.
	if !strings.Contains(body, "UNKNOWN") {
		t.Error("detect-silent-published-work.sh must classify read failures as UNKNOWN rather than as absence of work")
	}
	// The 08:10Z constraint, pinned as a mechanism rather than a comment: gate
	// dedup and auto-resolve must ask the store what exists NOW.
	if !strings.Contains(body, "list_episode_gates") {
		t.Error("detect-silent-published-work.sh must resolve gate existence with a live query (08:10Z: a state query, not an action log)")
	}
	// Loud-fail exit contract (#4543): the controller logs an exec order's
	// output only on a non-zero exit, so failures must exit non-zero.
	if !strings.Contains(body, `"$FAILED" -gt 0`) {
		t.Error("detect-silent-published-work.sh must exit non-zero when an action failed, or the loud-fail messages are never logged (#4543)")
	}

	// A BLIND SWEEP MUST NOT LOOK LIKE A CLEAN ONE. UNKNOWN reported on a zero
	// exit is invisible to the controller, so it is indistinguishable from
	// "nothing is stalled" — the exact confusion this order exists to prevent.
	if !strings.Contains(body, `"$UNKNOWN" -gt 0 ] || [ "$FAILED" -gt 0`) {
		t.Error("detect-silent-published-work.sh must exit non-zero on UNKNOWN too; a partially blind sweep that exits 0 is indistinguishable from a clean one")
	}

	// RECORD SEPARATOR. `read` with IFS=$'\t' COLLAPSES consecutive tabs,
	// because tab is IFS-whitespace — so a record with an empty middle field
	// (pr_number absent but pr_url present, the most common shape on the live
	// store) shifts every later field and the detector goes UNKNOWN forever on
	// exactly the beads it exists to find. Verified by construction; the unit
	// separator is not IFS-whitespace and does not collapse.
	if strings.Contains(body, `IFS="$(printf '\t')"`) {
		t.Error("detect-silent-published-work.sh must not split records on TAB: IFS-whitespace collapses consecutive tabs and shifts fields when a middle field is empty")
	}
	if !strings.Contains(body, `US="$(printf '\037')"`) {
		t.Error("detect-silent-published-work.sh must split records on the unit separator, which does not collapse")
	}

	// Gate auto-resolve must be RE-DERIVED from live state each sweep, across
	// every case the candidate loop cannot see: a bead that changed silent
	// class, moved to a different PR, or left the published states entirely and
	// so vanished from the candidate query.
	if !strings.Contains(body, "LIVE_EPISODES") {
		t.Error("detect-silent-published-work.sh must reconcile open episode gates against the live episode set, not only resolve from inside the candidate loop")
	}

	// A rig-discovery failure is not an empty rig list: the published population
	// lives on the rig stores, so sweeping HQ alone and exiting 0 reports
	// "nothing is stalled" from a sweep that never looked.
	if !strings.Contains(body, "RIG_DISCOVERY_OK") {
		t.Error("detect-silent-published-work.sh must fail loudly when rigs cannot be enumerated rather than sweeping HQ only and exiting clean")
	}

	// An UNKNOWN read must CARRY the prior observation forward. Dropping it
	// restarts that bead's clock next sweep, so a flapping API would hold a real
	// stall permanently below threshold.
	if !strings.Contains(body, "carry_forward") {
		t.Error("detect-silent-published-work.sh must carry a prior observation forward on UNKNOWN, or a flapping read keeps resetting the clock")
	}

	// Pruning must never write an empty state on failure: an empty state file
	// resets every clock, which silently guarantees no alarm ever fires again.
	if !strings.Contains(body, "keeping the unpruned state") {
		t.Error("detect-silent-published-work.sh must keep the unpruned state when the prune fails; writing an empty state resets every clock")
	}

	// The orphan arm's mail must LATCH. It cannot dedup on a store gate — gate
	// create requires --blocks and the orphan case is beadless, and the
	// reconciliation phase resolves any episode the candidate loop did not
	// re-derive — so it latches on a mailed-at state record with a re-remind
	// interval, stamped only when the mail actually delivered. Without this
	// the same orphans re-mail every cooldown forever (measured on the
	// westeros deployment: nine orphans, ~108 mails/hour).
	if !strings.Contains(body, "orphan_mailed_at") {
		t.Error("detect-silent-published-work.sh orphan arm must latch its mail on an orphan_mailed_at state record; a gate nothing creates never dedups and the mail re-sends every cooldown")
	}
	if !strings.Contains(body, "ORPHAN_REMIND_S") {
		t.Error("detect-silent-published-work.sh orphan arm must honor a re-remind interval so a latched orphan still re-surfaces instead of being silenced forever")
	}

	// ── TOWN LOCALITY (ga-g2at7f, 2026-09-22) ───────────────────────────────
	// A paging artifact's ROUTE is a property of the STORE IT LANDS IN. On
	// 2026-09-22 this detector minted 30 human gates on the SHARED qcore store
	// and paged another town's human inbox with our town's detections.
	//
	// The mint, the dedup read and the auto-resolve must ALL be pinned to the
	// town-local city store. `--city` and not merely the absence of `--rig`:
	// gc bd also auto-detects the store FROM THE BEAD ID, so a bare
	// `gate create --blocks qc-…` routes itself straight back onto the shared
	// store. And dedup must follow the mint — a lookup left on the rig store
	// finds nothing, reads that as "no gate exists", and re-mints every sweep,
	// rebuilding the burst town-locally.
	for _, want := range []string{
		`gc bd --city "$CITY_ABS" gate list`,
		`gc bd --city "$CITY_ABS" gate resolve`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detect-silent-published-work.sh must pin %q to the town-local city store; dedup and auto-resolve follow the mint or every sweep re-mints", want)
		}
	}
	// No gate call may carry rig-store routing. These are the exact shapes the
	// incident shipped.
	for _, forbidden := range []string{
		`gate create ${RIG1`,
		`gate list ${rig1`,
		`gate resolve "$g_id" ${RIG1`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("detect-silent-published-work.sh routes a gate call to a rig store (%q); every paging artifact it mints must land town-local (ga-g2at7f)", forbidden)
		}
	}
	// Subject scope: a LIVE roster read, never a hardcoded seat list, and the
	// pool sweeper's own predicate rather than a second implementation of it
	// (ga-7dr90m / ga-8yi7ne).
	if !strings.Contains(body, "gc agent is-foreign") {
		t.Error("detect-silent-published-work.sh must scope the alarm leg to subjects owned by this city's LIVE roster, read through gc agent is-foreign")
	}
	// A failed roster read is UNKNOWN — never "not ours" and never "ours" — so
	// the alarm leg and the auto-resolve are both skipped for the sweep.
	if !strings.Contains(body, "ROSTER_OK") {
		t.Error("detect-silent-published-work.sh must gate its alarm leg on a roster read that can FAIL; an unreadable roster is UNKNOWN, not a verdict")
	}
	// The narrowing must be counted and named. A subject-scope filter nobody
	// can see is indistinguishable from a city with no stalled work — the exact
	// confusion this detector exists to prevent, reproduced in its own scoping.
	if !strings.Contains(body, "SKIPPED_NOTOURS") {
		t.Error("detect-silent-published-work.sh must COUNT the candidates its subject scope skipped; a silent narrowing is a detector that has quietly switched itself off")
	}
	// The cross-store arm's mail must latch: bd gate create requires --blocks
	// and resolves it against the store it runs in, so a town-local gate cannot
	// block a bead on another store and there is no state query to dedup on.
	if !strings.Contains(body, "xstore_mailed_at") {
		t.Error("detect-silent-published-work.sh cross-store arm must latch its town-local mail on a state record; a gate cannot block a bead on another store, so nothing else dedups it")
	}
	// TOWN-LOCAL CARVE-OUT (katya ruling, 2026-09-22): a subject on THIS
	// city's own store is ours by construction — the city scope bypasses the
	// roster read (unassigned subjects included; "published work waiting on
	// NOBODY" on our own store is the founding case), while every rig-store
	// scope keeps the strict positive-resolution rule. The carve-out is
	// counted separately so the read-out can see how often it fires.
	if !strings.Contains(body, "OURS_BY_STORE") {
		t.Error("detect-silent-published-work.sh must count the town-local ours-by-construction carve-out separately (OURS_BY_STORE); an uncounted carve-out cannot be reviewed")
	}
	if !strings.Contains(body, `if [ -z "$scope" ]; then
            # TOWN-LOCAL CARVE-OUT`) {
		t.Error("detect-silent-published-work.sh city-scope candidates must bypass the roster read (ours by construction) BEFORE the roster-blind and is-foreign branches; rig-scope candidates must still take them")
	}
}
