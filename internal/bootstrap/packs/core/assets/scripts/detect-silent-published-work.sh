#!/usr/bin/env bash
# detect-silent-published-work — find PUBLISHED WORK WAITING ON NOBODY.
#
# A bead reaches a published state (merge_result=pr_published_awaiting_gate or
# gate_clear_awaiting_merge), a PR exists, and then nothing happens. No owner,
# no gate, and no other mechanism will ever mention it again: the refinery patrol
# explicitly disclaims these states ("nobody owes the refinery anything"), and
# the gate watchers only see gates that were created. Four such PRs were each
# found by a human doing something else, the earliest after 5.5 hours and the
# latest after 30 days.
#
# THE KEY IS ABSENCE OF PROGRESS, CARRYING A CLOCK — deliberately NOT conditioned
# on CI colour, mergeability, or review state. No PR-health signal separates the
# four exhibits, and reading them would rank a red-but-actively-worked PR below a
# green abandoned one. What this reads is whether anything MOVED.
#
# The alarm's primary artifact is ON THE BEAD, because the point is not only that
# stalled work lands: it is that THE NEXT PERSON TO PICK UP THE BEAD LEARNS THE
# WORK EXISTS. Three people once rebuilt the same branch three times, each having
# correctly concluded nothing existed. Mail is delivery; the bead is the record.
#
# ── EVIDENCE DISCIPLINE (ga-mmvpq1 08:10Z, binding) ──────────────────────────
# AN ACTION LOG IS STRUCTURALLY WEAKER EVIDENCE THAN A STATE QUERY. A log records
# an ATTEMPT; a query interrogates WHAT IS NOW TRUE. A log can lose an entry for
# an action that succeeded or keep one for an action that never took effect, and
# neither is detectable from the log alone.
#
# Concretely, and this is the whole shape of the script:
#   * The persisted state file holds OBSERVATIONS — what the last sweep SAW
#     (state, head, base, marker) — and NEVER actions, never "we alarmed this".
#     ONE CARVE-OUT (ga-vh6cbz): the beadless orphan arm latches its MAIL
#     CADENCE on an orphan_mailed_at record, because there is no bead to hang
#     a gate on and the reconciliation phase resolves any gate the bead arm
#     did not re-derive. The orphan CONDITION is still re-derived live from gh
#     every sweep — only the nagging is rate-limited — and a record for a PR
#     that stops being an orphan is dropped at the very next write (NEXT_STATE
#     is rebuilt from {} each sweep), so the action record can never suppress
#     a detection, only a repeat of one mail inside the remind window.
#   * "A push happened" is established by the live head differing from the
#     persisted observed_head. Never by finding a push record.
#   * Gate dedup asks GitHub/beads whether an open gate for this episode EXISTS
#     right now, not whether this script previously created one.
#   * Gate auto-resolve is re-derived from live state every sweep, never inferred
#     from having previously emitted a resolve.
# Built the other way, the detector would confidently report clean sweeps that
# never happened and stalls that had already cleared — the exact failure class it
# exists to catch, reproduced inside the catcher.
#
# ── READ-FAILURE IS NOT ABSENCE ──────────────────────────────────────────────
# Every read that can fail classifies UNKNOWN and is SKIPPED for the sweep. An
# API outage must never be counted as silence (a false alarm on every PR at once)
# and never as health (the fault this detector exists to catch, hidden by the
# thing meant to catch it). This mirrors the refinery predicate's own rule:
# "refusing rather than reporting a read failure as gate-absence".
#
# ── NOT OBSERVE-ONLY ─────────────────────────────────────────────────────────
# This ships ARMED (ga-mmvpq1 ratification, explicit prohibition). There is no
# dry-run knob and one must not be added: a detector that observes and does not
# act is indistinguishable from one that is switched off, and it becomes
# permanent. The tunable is the THRESHOLD.
#
# ── TOWN LOCALITY (2026-09-22 incident, ga-g2at7f) ───────────────────────────
# A PAGING ARTIFACT'S ROUTE IS A PROPERTY OF THE STORE IT LANDS IN, NOT OF WHO
# MINTED IT. On 2026-09-22 this detector minted 30 human gates on the SHARED
# qcore store in a post-resume burst (04:29-05:39Z). That store's notifier plane
# routes to whoever watches THERE, so ANOTHER TOWN'S human inbox was paged with
# our town's detections; the order was held at 05:38Z and all 30 gates closed by
# 05:45Z. Scoping to "our own PRs" would NOT have prevented the class — a
# shared-store artifact about OUR bead still pages THEIR human.
#
# Two rules, and the second is the load-bearing one.
#
#   (a) SUBJECT SCOPE. The alarm leg runs only for a candidate whose assignee
#       POSITIVELY resolves against THIS city's live roster, read through
#       `gc agent is-foreign` — the same in-process predicate the pool sweeper
#       applies, exposed to shell callers precisely so a second implementation
#       cannot drift from it (ga-7dr90m). Never a hardcoded seat list.
#
#       A "local" verdict with reason `not_qualified` IS NOT A RESOLUTION: it is
#       the verb DECLINING to answer about a bare alias, and reading a decline
#       as "ours" is how one town's detector adopts another town's work. Only
#       the reasons that name a MATCHED roster entry count. A bare alias is
#       re-asked once in its scope-qualified form, which is how gc addresses
#       that seat anyway. Everything else — foreign, still unqualified, no
#       assignee at all — is NOT OURS and is skipped for alarming, counted and
#       sampled in a per-sweep line so the narrowing can never be silent.
#
#       ONE CARVE-OUT (katya ruling, 2026-09-22): a subject on THIS city's own
#       town-local store is OURS BY CONSTRUCTION, unassigned and bare-alias
#       subjects included, because rule (b)'s own premise is that this store
#       routes to this town's inbox — a gate about its beads can page nobody
#       else. Every RIG-store scope keeps the strict positive resolution: an
#       unowned shared-store bead is exactly the ambiguous shape that paged
#       another town's human.
#
#       The roster read obeys this file's own read-failure rule. A failed read
#       is UNKNOWN, never "not ours" and never "ours": the RIG-scope alarm leg
#       is skipped for the sweep, loudly and non-zero — the city scope needs no
#       roster (ours by construction) and keeps alarming and reconciling. A ONE-SHOT CONTROL settles
#       that before the sweep starts — an identity no roster can contain must
#       come back `foreign`, and the roster must report more than zero agents,
#       because a roster resolving ZERO is a config failure wearing a successful
#       load and would read every one of our own seats as foreign.
#
#   (b) ARTIFACT LOCALITY. Every gate this detector mints lands on the
#       TOWN-LOCAL city store, whatever store the SUBJECT lives in.
#
#       THAT TAKES AN EXPLICIT `--city` PIN, NOT MERELY DROPPING `--rig`. `gc bd`
#       also auto-detects the store FROM THE BEAD ID, so `gc bd gate create
#       --blocks qc-…` routes ITSELF straight back to the shared store and
#       rebuilds the incident with the rig flags removed. `--city` is the
#       documented true scope override: it forces the city store and disables
#       GC_RIG, cwd AND bead-prefix detection.
#
#       DEDUP FOLLOWS THE MINT, as part of the same invariant. Episode dedup and
#       auto-resolve are city-pinned too. A lookup left on the rig store would
#       find nothing, every sweep would re-mint, and the paging burst would be
#       rebuilt town-locally. The episode key therefore CARRIES THE SCOPE: one
#       store now holds every scope's gates, and reconciliation still has to
#       partition by the scope whose live episode set it just derived, or
#       sweeping rig A resolves the gates raised for rig B.
#
#       A CROSS-STORE SUBJECT CANNOT BE GATED AT ALL. `gate create` requires
#       --blocks and resolves it with a GetIssue against the store it is running
#       in, so a city-pinned gate cannot block a qc- bead — there is no
#       cross-store dependency edge to be had, and a gate that could block one
#       would have to be minted on the shared store, which is the incident.
#       Those subjects get the design's other sanctioned artifact: a TOWN-LOCAL
#       MAIL, latched on a state record exactly like the beadless orphan arm and
#       for the same reason — nothing on this store to hang a gate on. The
#       condition is still re-derived live from the store and gh every sweep;
#       only the nagging is rate-limited.
#
# The bead-attached comment and the metadata stamp are NOT paging rows, and they
# stay on the SUBJECT'S OWN store. They are the record that stops the next person
# rebuilding the work — the reason this detector exists — and following the mint
# to the city store would put them where that person will never read them.
#
# Runs as a 5m cooldown exec order — mechanical, no LLM, loud-fail.
set -euo pipefail

__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "detect-silent-published-work"

# jq decodes every read. Without it each candidate would silently skip, which is
# the "reports health while blind" failure. Fail loud.
if ! command -v jq >/dev/null 2>&1; then
    echo "detect-silent-published-work: jq is required but not found in PATH" >&2
    exit 1
fi
# gh is how PR state is read. Absent, the sweep can classify nothing at all —
# and reporting "no silence found" from a blind sweep is precisely the lie this
# order exists to prevent. Fail loud rather than sweep vacuously.
if ! command -v gh >/dev/null 2>&1; then
    echo "detect-silent-published-work: gh is required but not found in PATH (cannot read PR state; refusing to report a blind sweep as clean)" >&2
    exit 1
fi

CITY="${GC_CITY:-.}"
# The TOWN-LOCAL pin (TOWN LOCALITY (b)). Every gate call is made with
# `--city "$CITY_ABS"`, which is the one argument that disables gc bd's
# bead-prefix store auto-detection; without it a `--blocks qc-…` re-routes the
# mint onto the shared store. Resolved to an absolute path once, here, because
# the pin is what makes the artifact town-local and a pin that fails to resolve
# must stop the sweep rather than silently fall back to prefix routing.
CITY_ABS="$(cd "$CITY" 2>/dev/null && pwd || true)"
if [ -z "$CITY_ABS" ]; then
    echo "detect-silent-published-work: cannot resolve the city directory '$CITY' — without it no gate can be pinned town-local, and an unpinned gate routes by bead prefix onto a SHARED store (ga-g2at7f). Refusing to sweep." >&2
    exit 1
fi
# How long a bead may sit in a silent state with NO progress before it alarms.
# 2h was chosen over a safer 4-6h deliberately: all four exhibits ran 5.5h to 30
# days so 4h would have caught every one, and 2h buys only the first exhibit's
# extra three hours — but a false alarm costs a bead comment and an
# auto-resolving gate, while the miss cost 45,000 lines. Tune DOWN from measured
# volume, never from priors (mandatory noise review, ga-hwk3r9).
THRESHOLD="${GC_SILENT_WORK_THRESHOLD:-2h}"
# Observation entries older than this are pruned so the state file stays bounded.
RETENTION="${GC_SILENT_WORK_STATE_RETENTION:-30d}"
ORPHAN_RECIPIENT="${GC_SILENT_WORK_ORPHAN_RECIPIENT:-mayor}"
# How long one orphan-PR mail stays latched before the reminder re-sends. The
# orphan CONDITION is re-derived live from gh every sweep; only the MAIL is
# rate-limited by this.
ORPHAN_REMIND="${GC_SILENT_WORK_ORPHAN_REMIND:-24h}"
# Where a CROSS-STORE subject's alarm is delivered, and how long one delivery
# stays latched. Town-local by construction (TOWN LOCALITY (b)): a subject on
# another store has no bead HERE to gate on, so the paging artifact is a mail to
# a seat of this town, rate-limited the way the orphan arm's mail is.
XSTORE_RECIPIENT="${GC_SILENT_WORK_XSTORE_RECIPIENT:-mayor}"
XSTORE_REMIND="${GC_SILENT_WORK_XSTORE_REMIND:-24h}"
ESCALATION_RECIPIENT="${GC_ESCALATION_RECIPIENT:-human}"
BRANCH_PATTERNS="${GC_SILENT_WORK_BRANCH_PATTERNS:-polecat/ fix/ nux/ integration/}"

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
STATE_FILE="$PACK_STATE_DIR/detect-silent-published-work-state.json"
mkdir -p "$PACK_STATE_DIR"

# RECORD SEPARATOR: unit separator, NOT tab. `read` with IFS=$'\t' collapses
# CONSECUTIVE tabs, because tab is IFS-whitespace — so a record with an empty
# middle field (e.g. pr_number absent but pr_url present, the single most common
# shape on the live store) silently SHIFTS every later field. Measured: 157 of
# 197 published beads carry pr_url while only 143 carry pr_number, so the shifted
# read would have sent a URL where a PR number was expected, classified UNKNOWN
# forever, and reported a clean sweep while blind to exactly the beads this order
# exists to find. US is not IFS-whitespace and does not collapse.
US="$(printf '\037')"

duration_to_seconds() {
    case "$1" in
        *d) echo $(( ${1%d} * 86400 )) ;;
        *h) echo $(( ${1%h} * 3600 )) ;;
        *m) echo $(( ${1%m} * 60 )) ;;
        *s) echo "${1%s}" ;;
        *)  echo "$1" ;;
    esac
}

# Portable ISO-8601 -> epoch. GNU `date -d` first, then BSD `date -ju -f`.
# Without the BSD fallback every age check fails on macOS and the whole sweep
# passes as clean — the failure shape this order exists to catch.
iso_to_epoch() {
    [ -n "$1" ] || { echo ""; return 0; }
    date -u -d "$1" +%s 2>/dev/null || \
        date -ju -f "%Y-%m-%dT%H:%M:%SZ" "$1" +%s 2>/dev/null || \
        date -ju -f "%Y-%m-%dT%H:%M:%S" "$1" +%s 2>/dev/null || \
        echo ""
}

THRESHOLD_S="$(duration_to_seconds "$THRESHOLD")"
ORPHAN_REMIND_S="$(duration_to_seconds "$ORPHAN_REMIND")"
XSTORE_REMIND_S="$(duration_to_seconds "$XSTORE_REMIND")"
# A garbage duration must fail LOUDLY, not fail open: an unparseable
# ORPHAN_REMIND would make the -lt test error out false and the orphan mail
# would re-send every sweep with the controller none the wiser — the exact
# flood this latch exists to stop.
case "$THRESHOLD_S" in ''|*[!0-9]*)
    echo "detect-silent-published-work: GC_SILENT_WORK_THRESHOLD %r is not a duration: $THRESHOLD" >&2
    exit 1 ;;
esac
case "$ORPHAN_REMIND_S" in ''|*[!0-9]*)
    echo "detect-silent-published-work: GC_SILENT_WORK_ORPHAN_REMIND is not a duration: $ORPHAN_REMIND" >&2
    exit 1 ;;
esac
case "$XSTORE_REMIND_S" in ''|*[!0-9]*)
    echo "detect-silent-published-work: GC_SILENT_WORK_XSTORE_REMIND is not a duration: $XSTORE_REMIND" >&2
    exit 1 ;;
esac
NOW_EPOCH="$(date -u +%s)"
NOW_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
echo "$STATE" | jq -e 'type == "object"' >/dev/null 2>&1 || STATE='{}'
NEXT_STATE='{}'

ALARMED=0; RESOLVED=0; UNKNOWN=0; ORPHANS=0; FAILED=0
# TOWN LOCALITY (a). SKIPPED_NOTOURS is a decision, SKIPPED_UNKNOWN is a failed
# read (and also counts into UNKNOWN, so the sweep exits non-zero). Both are
# counted because a narrowing nobody can see is a fresh instance of the class
# this detector exists to catch — the gate that silently stops alarming is
# indistinguishable from a city with no stalled work.
SKIPPED_NOTOURS=0; SKIPPED_UNKNOWN=0; SKIPPED_ROSTER_BLIND=0; OURS_BY_STORE=0
SKIP_SAMPLE=""; SKIP_SAMPLE_N=0
SKIP_SAMPLE_LIMIT=5

# One bounded sample of the skipped candidates, so a single leaking identity
# cannot turn the per-sweep line into something nobody reads. The COUNTS above
# are always exact; only the names are sampled.
note_skip() {
    [ "$SKIP_SAMPLE_N" -lt "$SKIP_SAMPLE_LIMIT" ] || return 0
    SKIP_SAMPLE="${SKIP_SAMPLE:+$SKIP_SAMPLE, }$1"
    SKIP_SAMPLE_N=$((SKIP_SAMPLE_N + 1))
}

# Carry a prior observation forward untouched. Used on every UNKNOWN path: a read
# that could not be made must neither start nor advance nor DISCARD a clock.
# Dropping it would restart the clock next sweep, so a flapping API would hold a
# real stall permanently below threshold.
carry_forward() {
    local key="$1" prev
    prev="$(echo "$STATE" | jq -c --arg k "$key" '.[$k] // empty' 2>/dev/null || true)"
    if [ -n "$prev" ]; then
        NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$key" --argjson v "$prev" '.[$k] = $v' 2>/dev/null || echo "$NEXT_STATE")"
    fi
    # The cross-store mail latch rides under a companion key; an UNKNOWN read
    # must preserve it too, or a flapping PR read re-mails an unchanged stall
    # on every recovery (its own doctrine: a latch may only suppress a repeat).
    local xkey="silentwork-xstore:$key" xprev
    xprev="$(echo "$STATE" | jq -c --arg k "$xkey" '.[$k] // empty' 2>/dev/null || true)"
    [ -n "$xprev" ] || return 0
    NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$xkey" --argjson v "$xprev" '.[$k] = $v' 2>/dev/null || echo "$NEXT_STATE")"
}

# Carry ONLY the cross-store mail latch forward. The roster-skip branches write
# their own fresh clock record before they branch, so a full carry_forward there
# would revert the clock to the prior sweep; but NEXT_STATE is rebuilt from {}
# each sweep, so a skip that does not re-write the latch DELETES it — and a
# flapping roster then turns the 24h latch into a mail-per-sweep flood on
# recovery (claude review BLOCKER 1, 2026-09-22).
carry_latch() {
    local xkey="silentwork-xstore:$1" xprev
    xprev="$(echo "$STATE" | jq -c --arg k "$xkey" '.[$k] // empty' 2>/dev/null || true)"
    [ -n "$xprev" ] || return 0
    NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$xkey" --argjson v "$xprev" '.[$k] = $v' 2>/dev/null || echo "$NEXT_STATE")"
}

# Episode key — the identity of ONE stall: (bead, PR identity, state). A
# recurrence after resolution is a NEW episode; concurrent sweeps of the same
# stall converge on one gate.
#
# The bead-arm callers pass "$SCOPE_LABEL|$bead" as the first field, so the key
# is SCOPE-QUALIFIED. That is required by TOWN LOCALITY (b): every scope's gates
# now live on the one city store, and the reconciliation pass must be able to
# tell the gates of the scope it just swept from the gates of a scope it did
# not, or one rig's sweep resolves another rig's live gates.
episode_key() { printf 'silentwork:%s:%s:%s' "$1" "$2" "$3"; }

# Open gates carrying a silentwork episode key, as "<gate-id><US><episode-key>".
# A STATE QUERY against the store — never "did I create one" against local state,
# because a local record can be lost for a gate that exists or kept for one that
# never landed, and neither is detectable from the record (08:10Z).
#
# Returns non-zero on a read failure so callers can classify UNKNOWN instead of
# treating an outage as "no gate exists" and creating a duplicate. The explicit
# `|| return 1` is load-bearing: inside a command substitution under `set -e`, a
# bare failing pipeline here would abort the ENTIRE sweep on one unreachable rig.
#
# CITY-PINNED, AND IT TAKES NO SCOPE (TOWN LOCALITY (b)). Dedup has to look
# where the mint lands. Left on the rig store it would find nothing, read that
# as "no gate for this episode", and re-mint every five minutes — the paging
# burst rebuilt town-locally, which is why dedup-follows-mint is part of the
# same invariant rather than a tidy-up after it.
list_episode_gates() {
    local out
    out="$(gc bd --city "$CITY_ABS" gate list --limit 0 --json 2>/dev/null)" || return 1
    [ -n "$out" ] && [ "$out" != "null" ] || return 0
    printf '%s' "$out" | jq -r --arg us "$US" '
        (if type == "array" then . else [.] end)[]
        | select(.status == "open")
        | (.title // "") as $t
        | ($t | capture("(?<k>silentwork:[^ \\]]+)") // empty) as $m
        | "\(.id)\($us)\($m.k)"' 2>/dev/null || return 1
}

# Addressee, RE-EVALUATED every sweep so a dead owner escalates at sweep cadence
# rather than being renudged forever: live-session assignee -> rig project-lead
# -> escalation recipient. Ownership decides WHO is told, never WHETHER the work
# is stalled — the design is progress-anchored, and the first exhibit was
# actively owned and pushed all evening before it went quiet.
resolve_addressee() {
    local assignee="$1" scope="$2"
    if [ -n "$assignee" ] && gc session list --json 2>/dev/null \
        | jq -e --arg a "$assignee" '[(if type=="array" then . else (.sessions // []) end)[]
             | select((.name // "") == $a)] | length > 0' >/dev/null 2>&1; then
        printf '%s' "$assignee"; return 0
    fi
    [ -n "$scope" ] && { printf '%s/oversight.project-lead' "$scope"; return 0; }
    printf '%s' "$ESCALATION_RECIPIENT"
}

# ── B4(i): the record ON THE BEAD, the primary artifact ──────────────────────
# Emitted exactly when a new alarm artifact was raised for the episode — a state
# query, not a "have I commented before" flag. A metadata stamp read back as
# dedup evidence would be action-log reasoning, and it breaks in both
# directions: stamp-succeeds-comment-fails never retries, and a later episode
# never comments because the field is merely non-empty.
#
# THIS IS THE ONE ARTIFACT THAT DOES NOT FOLLOW THE MINT TO THE CITY STORE. It
# is not a paging row; it is the record the next person to pick up the bead
# reads, and it is worthless anywhere but on that bead's own store. Subject
# scope (a) already confines it to beads this city's seats own, so it cannot
# land on another town's work.
#
# It reads the candidate loop's variables directly rather than taking a dozen
# positionals; it is called only from inside that loop, once per raised alarm.
# $1 names the paging artifact that was raised, $2 the addressee it names.
attach_bead_record() {
    local artifact="$1" addr="$2" bead_note=""
    [ "$bstatus" = "closed" ] && bead_note="  <-- bead is CLOSED but its PR is still open"
    if ! gc bd comment "$bead" ${RIG1:+"$RIG1" "$RIG2"} \
        "THIS WORK EXISTS — DO NOT RESTART IT.
Published work waiting on nobody, detected by absence of progress.
  PR:      ${pr_url:-https://github.com/$pr_repo/pull/$pr_number}
  Head:    $live_head  (base $live_base)
  State:   $cls
  Bead:    ${bstatus:-?}${bead_note}
  Silent:  ${age_h}h${age_m}m with no observed progress
  Alarm:   $artifact   addressee: $addr
Progress means a push, a re-render of the gate sticky, a merge/close, a change of
publish state, or a reroute of this bead. Comments, CI runs and review activity
are NOT progress and do not clear this.
Raised by detect-silent-published-work (ga-krso22 / ga-mmvpq1 Half B)." >/dev/null 2>&1; then
        echo "detect-silent-published-work: FAILED to attach evidence comment to $bead" >&2
        return 1
    fi
    # Best-effort convenience stamp for humans and dashboards. NOT used as dedup
    # evidence anywhere.
    gc bd update "$bead" ${RIG1:+"$RIG1" "$RIG2"} \
        --set-metadata "gc.silent_work_alarm=$NOW_ISO/$cls" >/dev/null 2>&1 || true
    return 0
}

# ── The roster read (TOWN LOCALITY (a)) ──────────────────────────────────────
# `gc agent is-foreign` is the pool sweeper's own foreign-identity predicate,
# exposed to shell callers for exactly this kind of use (ga-7dr90m). We ASK it
# rather than re-deriving the answer from `gc agent list`, because a second
# implementation of "whose seat is this" drifts from the one the sweeper
# enforces, and the obvious hand-rolled version — strip the binding prefix and
# compare — is what made another town's canonical "qcore/pool.omp-1" resolve
# against our "qcore/omp" (ga-8yi7ne).
#
# roster_ask echoes "<verdict> <reason>" and returns non-zero when the verb
# could not be asked at all (absent, or output that will not decode). Its own
# documented contract is that a caller must treat a missing verb, exit 2 or
# unparseable output as "protect", never as "local" — which is this file's
# read-failure rule under another name.
roster_ask() {
    local out
    # is-foreign signals its VERDICT in the exit code (1 = foreign), so the exit
    # code cannot also carry "the command ran". The JSON is the answer; its
    # absence is the failure.
    out="$(gc agent is-foreign "$1" --json 2>/dev/null || true)"
    [ -n "$out" ] || return 1
    printf '%s' "$out" | jq -r '"\(.verdict // "") \(.reason // "")"' 2>/dev/null || return 1
}

# The reasons that name a MATCHED roster entry. Everything else the verb can
# say while still reporting "local" is a DECLINE, not a match — see (a).
roster_reason_is_match() {
    case "$1" in
        named_session|agent_template|configured_agent|namepool_instance|agent_instance) return 0 ;;
    esac
    return 1
}

# "ours" | "theirs" | "unknown" for one candidate's assignee.
subject_roster_verdict() {
    local assignee="$1" scope="$2" ans verdict reason
    # An EMPTY assignee is a DEFINITE answer, not a failed read: nothing names
    # this bead as one of ours. It is answered here rather than passed to the
    # verb, which reports bad usage with the SAME exit 2 it uses for "I cannot
    # read the roster" — folding a known not-ours into a sweep-blinding UNKNOWN.
    [ -n "$assignee" ] || { printf 'theirs'; return 0; }
    ans="$(roster_ask "$assignee")" || { printf 'unknown'; return 0; }
    verdict="${ans%% *}"; reason="${ans#* }"
    if [ "$verdict" = "local" ] && roster_reason_is_match "$reason"; then printf 'ours'; return 0; fi
    if [ "$verdict" = "unknown" ]; then printf 'unknown'; return 0; fi
    # A DECLINE ("local" for a bare alias the verb will not reason about) gets
    # ONE re-ask, qualified into the scope the bead was read from: "barry" on
    # rig qcore is "qcore/barry", which is how gc addresses that seat. The
    # re-ask is a NAME COMPOSITION, not a second roster implementation — the
    # verdict still comes from the verb.
    if [ "$reason" = "not_qualified" ] && [ -n "$scope" ]; then
        ans="$(roster_ask "$scope/$assignee")" || { printf 'unknown'; return 0; }
        verdict="${ans%% *}"; reason="${ans#* }"
        if [ "$verdict" = "local" ] && roster_reason_is_match "$reason"; then printf 'ours'; return 0; fi
        if [ "$verdict" = "unknown" ]; then printf 'unknown'; return 0; fi
    fi
    case "$verdict" in
        local|foreign) : ;;
        *)
            # Decodable JSON whose verdict is not a contract word is a DEGRADED
            # READ, not a decision — falling through to "theirs" would count a
            # fault in the decision bucket (claude review finding 4).
            printf 'unknown'; return 0 ;;
    esac
    printf 'theirs'
}

# THE CONTROL, once per sweep. An identity no roster can contain must come back
# `foreign`: that is the reading a working roster produces and a broken one
# cannot. `unknown`, a missing verb or undecodable output all mean the city
# config did not resolve. A roster reporting ZERO agents is the third failure
# and the quiet one — a config-resolution failure wearing a successful load,
# which would read every one of our own seats as foreign and switch the alarm
# leg off while reporting clean sweeps forever.
#
# Both readings come from ONE invocation: two calls could disagree, and a
# control that is not the same read as the thing it certifies certifies nothing.
ROSTER_PROBE_IDENTITY="gcrosterprobe/gcrosterprobe"
ROSTER_OK=0
ROSTER_PROBE_JSON="$(gc agent is-foreign "$ROSTER_PROBE_IDENTITY" --json 2>/dev/null || true)"
ROSTER_PROBE="$(printf '%s' "$ROSTER_PROBE_JSON" | jq -r '.verdict // ""' 2>/dev/null || true)"
ROSTER_AGENTS="$(printf '%s' "$ROSTER_PROBE_JSON" | jq -r '.roster_source // ""' 2>/dev/null \
    | sed -nE 's/.*resolved: ([0-9]+) agents.*/\1/p' || true)"
case "$ROSTER_PROBE" in
    foreign)
        case "$ROSTER_AGENTS" in
            ''|*[!0-9]*) : ;;
            *) [ "$ROSTER_AGENTS" -gt 0 ] && ROSTER_OK=1 ;;
        esac ;;
    *) : ;;
esac
if [ "$ROSTER_OK" -eq 0 ]; then
    echo "detect-silent-published-work: cannot read this city's agent roster (probe verdict '${ROSTER_PROBE:-<none>}', agents '${ROSTER_AGENTS:-<none>}') — a failed roster read is UNKNOWN, never 'not ours' and never 'ours', so NO alarm artifact is raised or auto-resolved this sweep (ga-g2at7f)" >&2
    UNKNOWN=$((UNKNOWN + 1))
fi

# ── Scopes ───────────────────────────────────────────────────────────────────
# HQ plus every non-HQ rig. A rig-discovery FAILURE IS NOT AN EMPTY RIG LIST: the
# published population lives on the rig stores, so sweeping HQ alone and exiting
# 0 would report "nothing is stalled" from a sweep that never looked. That is the
# precise failure this order exists to prevent, so it is loud and non-zero.
SCOPES_FILE="$(mktemp "$PACK_STATE_DIR/.detect-silent-scopes.XXXXXX")"
trap 'rm -f "$SCOPES_FILE"' EXIT
printf '\n' > "$SCOPES_FILE"
RIG_DISCOVERY_OK=1
if RIGS_JSON="$(gc rig list --json 2>/dev/null)"; then
    if ! printf '%s' "$RIGS_JSON" | jq -r '(.rigs // [])[] | select(.hq != true) | .name' \
            >> "$SCOPES_FILE" 2>/dev/null; then
        RIG_DISCOVERY_OK=0
    fi
else
    RIGS_JSON=""
    RIG_DISCOVERY_OK=0
fi
if [ "$RIG_DISCOVERY_OK" -eq 0 ]; then
    echo "detect-silent-published-work: cannot enumerate rigs — the published population lives on rig stores, so this sweep would be blind. Refusing to report a partial sweep as clean." >&2
    exit 1
fi

# Repo for a scope, from its rig checkout's git origin. Derived from state rather
# than configured separately so it cannot drift from where the work really is.
repo_for_scope() {
    local scope="$1" path url
    if [ -z "$scope" ]; then path="$CITY"; else
        path="$(printf '%s' "$RIGS_JSON" | jq -r --arg n "$scope" \
            '(.rigs // [])[] | select(.name == $n) | .path // ""' 2>/dev/null || true)"
    fi
    [ -n "$path" ] && [ -d "$path" ] || { echo ""; return 0; }
    url="$(git -C "$path" remote get-url origin 2>/dev/null || true)"
    [ -n "$url" ] || { echo ""; return 0; }
    printf '%s' "$url" | sed -E 's#^git\+##; s#^(https://|git@)github\.com[:/]##; s#\.git$##'
}

while IFS= read -r scope; do
    RIG1=""; RIG2=""
    CP1=""; CP2=""
    if [ -n "$scope" ]; then
        RIG1="--rig"; RIG2="$scope"
    else
        # The empty scope MUST be pinned too: a bare `gc bd list` is subject to
        # the same GC_RIG / cwd / bead-prefix store auto-detection the gate
        # verbs are pinned against, and an unpinned city sweep that lands on a
        # rig store would hand that rig's beads to the town-local carve-out —
        # "ours by construction" asserted about the shared store (codex review
        # finding, 2026-09-22).
        CP1="--city"; CP2="$CITY_ABS"
    fi
    # "@city" cannot collide with a rig name the way "hq" can (a rig named hq
    # would share the reconciliation partition and the two sweeps would resolve
    # each other's live gates in a mint/resolve loop — codex review finding).
    SCOPE_LABEL="${scope:-@city}"
    SCOPE_REPO="$(repo_for_scope "$scope")"

    BEADS_JSON="$(gc bd ${CP1:+"$CP1" "$CP2"} list ${RIG1:+"$RIG1" "$RIG2"} --limit 0 --json 2>/dev/null)" || {
        echo "detect-silent-published-work: scope $SCOPE_LABEL: cannot list beads (UNKNOWN, skipped)" >&2
        UNKNOWN=$((UNKNOWN + 1)); continue
    }
    [ -n "$BEADS_JSON" ] && [ "$BEADS_JSON" != "null" ] || continue

    if ! CANDIDATES="$(printf '%s' "$BEADS_JSON" | jq -r --arg us "$US" '
        (if type == "array" then . else [.] end)[]
        | select((.metadata."merge_result" // "") as $m
                 | $m == "pr_published_awaiting_gate" or $m == "gate_clear_awaiting_merge")
        | [.id, (.metadata."merge_result" // ""), (.metadata."pr_number" // ""),
           (.metadata."pr_url" // ""), (.assignee // ""), (.status // ""), "END"]
        | join($us)' 2>/dev/null)"; then
        # A decode failure is a blind scope, not an empty one.
        echo "detect-silent-published-work: scope $SCOPE_LABEL: cannot decode bead list (UNKNOWN, skipped)" >&2
        UNKNOWN=$((UNKNOWN + 1)); continue
    fi

    # Current classification per bead, built during the loop and used afterwards
    # to RE-DERIVE which episode gates still apply. Reconciling gates from this
    # map (rather than only from inside the loop) is what makes auto-resolve
    # correct when a bead changes silent class, changes PR, or leaves the
    # published states altogether and vanishes from the candidate set.
    LIVE_EPISODES=""

    while IFS="$US" read -r bead mres pr_number pr_url assignee bstatus _end; do
        [ -n "$bead" ] || continue
        KEY="$SCOPE_LABEL|$bead"

        if [ -z "$pr_number" ] && [ -z "$pr_url" ]; then
            echo "detect-silent-published-work: $bead in state '$mres' carries NO pr_number/pr_url — unverifiable pointer" >&2
            FAILED=$((FAILED + 1)); carry_forward "$KEY"; continue
        fi
        # The URL is self-contained; a bare number is not. Derive BOTH repo and
        # number from pr_url when present, so a cross-repo pointer reads the PR
        # it names instead of an unrelated PR with the same number in the rig's
        # own origin.
        pr_repo="$SCOPE_REPO"
        if [ -n "$pr_url" ]; then
            u_repo="$(printf '%s' "$pr_url" | sed -nE 's#^https://github\.com/([^/]+/[^/]+)/pull/[0-9]+.*$#\1#p')"
            u_num="$(printf '%s' "$pr_url" | sed -nE 's#^https://github\.com/[^/]+/[^/]+/pull/([0-9]+).*$#\1#p')"
            [ -n "$u_repo" ] && pr_repo="$u_repo"
            [ -n "$u_num" ] && pr_number="$u_num"
        fi
        [ -n "$pr_repo" ] && [ -n "$pr_number" ] || { UNKNOWN=$((UNKNOWN + 1)); carry_forward "$KEY"; continue; }

        PR_JSON="$(gh api "repos/$pr_repo/pulls/$pr_number" 2>/dev/null)" || {
            UNKNOWN=$((UNKNOWN + 1)); carry_forward "$KEY"; continue; }
        live_head="$(printf '%s' "$PR_JSON" | jq -r '.head.sha // ""' 2>/dev/null || true)"
        live_base="$(printf '%s' "$PR_JSON" | jq -r '.base.ref // ""' 2>/dev/null || true)"
        pr_state="$(printf '%s' "$PR_JSON" | jq -r '.state // ""' 2>/dev/null || true)"
        pr_merged="$(printf '%s' "$PR_JSON" | jq -r 'if .merged then "true" else "false" end' 2>/dev/null || true)"
        [ -n "$live_head" ] || { UNKNOWN=$((UNKNOWN + 1)); carry_forward "$KEY"; continue; }

        COMMENTS_JSON="$(gh api --paginate --slurp "repos/$pr_repo/issues/$pr_number/comments" 2>/dev/null)" || {
            UNKNOWN=$((UNKNOWN + 1)); carry_forward "$KEY"; continue; }
        if ! GATE_JSON="$(printf '%s' "$COMMENTS_JSON" | jq -c '
            [.[][] | select(.body | startswith("## Sherpa gate"))] | last
            | select(. != null) | {body: .body, login: .user.login}' 2>/dev/null)"; then
            # A parse failure is NOT gate-absence. Classifying it as no-sticky
            # would manufacture a false silent-ungated alarm.
            UNKNOWN=$((UNKNOWN + 1)); carry_forward "$KEY"; continue
        fi

        gate_status=""; gate_head=""; gate_base=""; sticky_unknown=0
        if [ -n "$GATE_JSON" ] && [ "$GATE_JSON" != "null" ]; then
            gl="$(printf '%s' "$GATE_JSON" | jq -r '.login // ""' 2>/dev/null || true)"
            if perm="$(gh api "repos/$pr_repo/collaborators/$gl/permission" --jq '.permission' 2>/dev/null)"; then
                case "$perm" in
                    admin|maintain|write)
                        gbody="$(printf '%s' "$GATE_JSON" | jq -r '.body' 2>/dev/null || true)"
                        gate_status="$(printf '%s' "$gbody" | sed -n 's/^\*\*Gate status\*\*:[^`]*`\([A-Z]*\)`.*/\1/p' | head -1)"
                        gate_head="$(printf '%s' "$gbody" | sed -n 's/^\*\*HEAD\*\*: `\([0-9a-f]*\)`.*/\1/p' | head -1)"
                        gate_base="$(printf '%s' "$gbody" | sed -n 's/.*\*\*Base\*\*: `\([^`]*\)`.*/\1/p' | head -1)"
                        ;;
                    *) : ;;   # insufficient permission => not a certifying sticky
                esac
            else
                sticky_unknown=1   # cannot authenticate => cannot classify
            fi
        fi
        if [ "$sticky_unknown" -eq 1 ]; then
            UNKNOWN=$((UNKNOWN + 1)); carry_forward "$KEY"; continue
        fi

        # ── Classification. NOT read: CI conclusions, mergeability, reviews. ──
        if [ "$pr_state" = "closed" ] && [ "$pr_merged" != "true" ]; then
            echo "detect-silent-published-work: $bead is in state '$mres' but its PR ${pr_url:-$pr_repo#$pr_number} is CLOSED unmerged (bead: ${bstatus:-?})" >&2
            cls="out-of-scope"
        elif [ "$pr_merged" = "true" ]; then
            if [ "$gate_status" = "CLEAR" ]; then cls="out-of-scope"; else cls="bypass-detected"; fi
        elif [ -z "$gate_status" ]; then
            cls="silent-ungated"
        elif [ -n "$gate_head" ] && [ "$gate_head" != "$live_head" ]; then
            cls="silent-stale"
        elif [ -n "$gate_base" ] && [ -n "$live_base" ] && [ "$gate_base" != "$live_base" ]; then
            cls="silent-stale"
        elif [ "$gate_status" = "CLEAR" ]; then
            cls="silent-unexecuted"
        else
            cls="healthy"
        fi

        # ── B3: the clock. Progress is an observed STATE TRANSITION. ─────────
        # The observation carries merge_result and assignee as well as the PR
        # fields, so a publish-state transition or an explicit reroute counts as
        # progress — the alarm text promises rerouting clears it, and an
        # observation that omitted those fields would break that promise.
        marker="${gate_status}@${gate_head}"
        PREV="$(echo "$STATE" | jq -c --arg k "$KEY" '.[$k] // empty' 2>/dev/null || true)"
        first_seen="$NOW_ISO"
        if [ -n "$PREV" ]; then
            same="$(echo "$PREV" | jq -r --arg s "$cls" --arg h "$live_head" --arg b "$live_base" \
                --arg m "$marker" --arg mr "$mres" --arg a "$assignee" --arg p "$pr_number" '
                if (.state // "") == $s and (.observed_head // "") == $h
                   and (.observed_base // "") == $b and (.observed_marker // "") == $m
                   and (.observed_merge_result // "") == $mr
                   and (.observed_assignee // "") == $a
                   and (.observed_pr // "") == $p
                then "same" else "moved" end' 2>/dev/null || echo moved)"
            if [ "$same" = "same" ]; then
                pf="$(echo "$PREV" | jq -r '.first_observed_in_state_at // ""' 2>/dev/null || true)"
                [ -n "$pf" ] && first_seen="$pf"
            fi
        fi
        NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$KEY" \
            --arg f "$first_seen" --arg s "$cls" --arg h "$live_head" --arg b "$live_base" \
            --arg m "$marker" --arg mr "$mres" --arg a "$assignee" --arg p "$pr_number" \
            '.[$k] = {first_observed_in_state_at: $f, state: $s, observed_head: $h,
                      observed_base: $b, observed_marker: $m, observed_merge_result: $mr,
                      observed_assignee: $a, observed_pr: $p}' 2>/dev/null || echo "$NEXT_STATE")"

        case "$cls" in
            silent-ungated|silent-stale|silent-unexecuted) : ;;
            *) continue ;;   # gates for this bead are reconciled below
        esac

        first_epoch="$(iso_to_epoch "$first_seen")"
        [ -n "$first_epoch" ] || continue
        age=$(( NOW_EPOCH - first_epoch ))
        [ "$age" -ge "$THRESHOLD_S" ] || continue
        age_h=$(( age / 3600 )); age_m=$(( (age % 3600) / 60 ))

        # SCOPE-QUALIFIED (TOWN LOCALITY (b)): one store now holds every scope's
        # gates, so the key has to say which scope's live set reconciles it.
        EP="$(episode_key "$SCOPE_LABEL|$bead" "$pr_number" "$cls")"

        # ── TOWN LOCALITY (a): whose work is this? ──────────────────────────
        # Asked HERE, after the threshold, so the question is only put about
        # candidates that would otherwise alarm — and so the counted line below
        # reports skipped ALARMS, not skipped reads.
        if [ -z "$scope" ]; then
            # TOWN-LOCAL CARVE-OUT (katya ruling, 2026-09-22): a subject on
            # THIS city's own store is OURS BY CONSTRUCTION — no roster read,
            # unassigned and bare-alias subjects included. Rule (b)'s premise
            # cuts both ways: this store routes to this town's inbox, so a
            # gate about its beads can page nobody else — and "published work
            # waiting on NOBODY" on our own store is this detector's founding
            # case, which the strict rule below would have skipped.
            # RESIDUAL, deliberate: a LOCAL rig's unassigned beads (e.g. the
            # platform rig's) still skip under the strict rule and are loudly
            # counted — refinable later if the skip counts say so.
            OURS_BY_STORE=$((OURS_BY_STORE + 1))
        elif [ "$ROSTER_OK" -eq 0 ]; then
            # The roster could not be read at all. Protect this episode's
            # existing gate (an unreadable roster is not evidence the stall
            # cleared) and raise nothing. Reported once, at the probe.
            SKIPPED_ROSTER_BLIND=$((SKIPPED_ROSTER_BLIND + 1))
            LIVE_EPISODES="$LIVE_EPISODES$EP
"
            carry_latch "$KEY"
            continue
        else
            case "$(subject_roster_verdict "$assignee" "$scope")" in
                ours) : ;;
                unknown)
                    # A roster read that did not answer is NOT a verdict of
                    # "theirs". Keep the episode live so any gate it already has
                    # survives, and report the sweep as partially blind.
                    LIVE_EPISODES="$LIVE_EPISODES$EP
"
                    UNKNOWN=$((UNKNOWN + 1)); SKIPPED_UNKNOWN=$((SKIPPED_UNKNOWN + 1))
                    note_skip "$bead(${assignee:-<unassigned>}: roster unreadable)"
                    carry_latch "$KEY"
                    continue ;;
                *)
                    # NOT THIS CITY'S. No paging artifact, and deliberately NOT
                    # added to LIVE_EPISODES, so any gate an earlier build left for
                    # it is auto-resolved by the reconciliation phase below.
                    SKIPPED_NOTOURS=$((SKIPPED_NOTOURS + 1))
                    note_skip "$bead(${assignee:-<unassigned>})"
                    continue ;;
            esac
        fi

        LIVE_EPISODES="$LIVE_EPISODES$EP
"
        ADDR="$(resolve_addressee "$assignee" "$scope")"

        if [ -z "$scope" ]; then
            # ── B4(ii): gate upsert, keyed on the episode ───────────────────
            # The subject lives on the city store, so a town-local gate can
            # block it. Existence is a live query. Two concurrent sweeps can
            # still both observe "absent" and both create — atomicity is not
            # available at this seam — so the loop CONVERGES instead: any extra
            # gate for the same episode is resolved on the next sweep by the
            # reconciliation phase below.
            if ! GATES="$(list_episode_gates)"; then
                UNKNOWN=$((UNKNOWN + 1)); continue
            fi
            gid="$(printf '%s' "$GATES" | awk -F"$US" -v k="$EP" '$2 == k {print $1; exit}')"
            if [ -z "$gid" ]; then
                if gc bd --city "$CITY_ABS" gate create --type human --blocks "$bead" \
                    --title "Silent work: $bead stalled ${age_h}h${age_m}m in $cls [$EP]" \
                    --reason "Published work waiting on nobody. PR ${pr_url:-https://github.com/$pr_repo/pull/$pr_number} has shown no progress for ${age_h}h${age_m}m (state: $cls). THE WORK EXISTS — do not restart it." \
                    >/dev/null 2>&1; then
                    GATES="$(list_episode_gates || true)"
                    gid="$(printf '%s' "$GATES" | awk -F"$US" -v k="$EP" '$2 == k {print $1; exit}')"
                else
                    echo "detect-silent-published-work: FAILED to raise gate for $bead episode $EP (will retry next sweep)" >&2
                    FAILED=$((FAILED + 1)); continue
                fi
                attach_bead_record "gate ${gid:-<pending>}" "$ADDR" || FAILED=$((FAILED + 1))
                ALARMED=$((ALARMED + 1))
            fi
        else
            # ── CROSS-STORE SUBJECT (TOWN LOCALITY (b)) ─────────────────────
            # `gate create` REQUIRES --blocks and resolves it with a GetIssue
            # against the store it is running in, so a city-pinned gate cannot
            # block this bead: there is no cross-store dependency edge to be
            # had. The only gate that could block it is one minted on the shared
            # store, which is the 2026-09-22 incident. So the design's other
            # sanctioned artifact is used — a TOWN-LOCAL mail, carrying the
            # subject id and the episode as the link the gate would have been.
            #
            # It LATCHES on a state record, exactly like the beadless orphan arm
            # and for the same reason: there is no bead on this store to hang a
            # gate on, so there is no state QUERY that could dedup it. The
            # carve-out is honest here on the same terms (ga-vh6cbz) — the
            # CONDITION is re-derived live from the store and gh every sweep, a
            # record for an episode that stops being silent is dropped at the
            # very next write because NEXT_STATE is rebuilt from {}, and state
            # loss costs at most one duplicate mail. It can never suppress a
            # detection, only a repeat of one inside the remind window.
            # Keyed on (scope, bead) — NOT the full episode — so every UNKNOWN
            # path can carry it forward knowing only $KEY (a transient PR-read
            # failure must not erase the latch and re-mail an unchanged stall
            # on recovery), and a cls flap inside the window stays one mail.
            XKEY="silentwork-xstore:$KEY"
            xprev="$(echo "$STATE" | jq -c --arg k "$XKEY" '.[$k] // empty' 2>/dev/null || true)"
            xfirst="$NOW_ISO"; xmailed=""
            if [ -n "$xprev" ]; then
                pf="$(echo "$xprev" | jq -r '.first_observed_in_state_at // ""' 2>/dev/null || true)"
                [ -n "$pf" ] && xfirst="$pf"
                xmailed="$(echo "$xprev" | jq -r '.xstore_mailed_at // ""' 2>/dev/null || true)"
            fi
            if [ -n "$xmailed" ]; then
                xm_epoch="$(iso_to_epoch "$xmailed")"
                if [ -n "$xm_epoch" ] && [ $(( NOW_EPOCH - xm_epoch )) -lt "$XSTORE_REMIND_S" ]; then
                    # Mailed within the remind window: keep the record alive
                    # (still silent) and stay quiet.
                    NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$XKEY" --argjson v "$xprev" '.[$k] = $v' 2>/dev/null || echo "$NEXT_STATE")"
                    continue
                fi
            fi
            if gc mail send "$XSTORE_RECIPIENT" --notify \
                -s "Silent work: $bead stalled ${age_h}h${age_m}m ($cls)" \
                -m "Published work waiting on nobody, detected by absence of progress.

  Bead:    $bead   (rig $SCOPE_LABEL, ${bstatus:-?})
  PR:      ${pr_url:-https://github.com/$pr_repo/pull/$pr_number}
  Head:    $live_head  (base $live_base)
  State:   $cls
  Silent:  ${age_h}h${age_m}m with no observed progress
  Owner:   ${assignee:-<unassigned>}   addressee: $ADDR

THE WORK EXISTS — do not restart it. The evidence comment is on the bead itself.

This arrives as MAIL rather than as a human gate because the bead lives on
another store: a gate must block an issue in the store it is created in, and the
only gate that could block this one would have to be minted on the SHARED store,
where it would page whichever town watches there (ga-g2at7f, 2026-09-22).

Raised by detect-silent-published-work (ga-krso22 / ga-mmvpq1 Half B).
Episode: $EP" >/dev/null 2>&1; then
                # Stamp the latch ONLY on a delivered mail; a failed send leaves
                # the prior record (or none) in place so the next sweep retries.
                NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$XKEY" --arg f "$xfirst" --arg m "$NOW_ISO" \
                    '.[$k] = {first_observed_in_state_at: $f, xstore_mailed_at: $m}' 2>/dev/null || echo "$NEXT_STATE")"
                attach_bead_record "mail to $XSTORE_RECIPIENT" "$ADDR" || FAILED=$((FAILED + 1))
                ALARMED=$((ALARMED + 1))
            else
                echo "detect-silent-published-work: FAILED to raise the town-local alarm mail for $bead episode $EP (will retry next sweep)" >&2
                FAILED=$((FAILED + 1))
                [ -n "$xprev" ] && NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$XKEY" --argjson v "$xprev" '.[$k] = $v' 2>/dev/null || echo "$NEXT_STATE")"
            fi
        fi
    done <<CANDIDATE_EOF
$CANDIDATES
CANDIDATE_EOF

    # ── Gate reconciliation: RE-DERIVED every sweep ──────────────────────────
    # Every open silentwork gate in this scope whose episode is not in the live
    # set is resolved. This is what makes auto-resolve correct for the cases the
    # candidate loop cannot see: a bead that changed silent class, that moved to
    # a different PR, that left the published states entirely (and so vanished
    # from the candidate query), and duplicate gates from a concurrent create.
    # It is derived from what is true NOW, never from having previously emitted a
    # resolve.
    #
    # CITY-PINNED AND SCOPE-PARTITIONED (TOWN LOCALITY (b)). The dedup read
    # follows the mint, so this pass now sees EVERY scope's gates and must
    # consider only the ones belonging to the scope whose live episode set it
    # just derived — otherwise sweeping rig A resolves every gate raised for rig
    # B. That partition is why the scope is in the episode key. (A key from an
    # earlier build carries no scope and so matches no partition. None exist:
    # the rig-store gates were all closed during the 2026-09-22 containment and
    # the city store held no silentwork gate at all, verified that day.)
    #
    # CITY PASS ONLY. Gates are minted only for city-store subjects, so every
    # gate key carries the "@city" partition and a rig pass can never match one
    # — running the (city-wide) gate list once per rig was pure noise, and each
    # rig-pass read failure inflated UNKNOWN for no information. And the city
    # pass does NOT need the roster: the town-local carve-out classifies city
    # candidates and fills LIVE_EPISODES with no roster read, so gating this on
    # ROSTER_OK disabled auto-resolve for the only scope that has gates — a
    # persistent probe failure would leak an open human gate per cleared stall,
    # the 2026-09-22 cleanup reproduced town-locally (claude review BLOCKER 2).
    if [ -n "$scope" ]; then
        :
    elif ! GATES="$(list_episode_gates)"; then
        UNKNOWN=$((UNKNOWN + 1))
    else
        seen_eps=""
        while IFS="$US" read -r g_id g_key; do
            [ -n "$g_id" ] && [ -n "$g_key" ] || continue
            case "$g_key" in "silentwork:$SCOPE_LABEL|"*) : ;; *) continue ;; esac
            keep=0
            printf '%s' "$LIVE_EPISODES" | grep -Fxq "$g_key" && keep=1
            # A duplicate of an episode already kept in this pass is resolved too.
            if [ "$keep" -eq 1 ]; then
                case "$seen_eps" in *"[$g_key]"*) keep=0 ;; *) seen_eps="${seen_eps}[${g_key}]" ;; esac
            fi
            [ "$keep" -eq 1 ] && continue
            if gc bd --city "$CITY_ABS" gate resolve "$g_id" >/dev/null 2>&1; then
                RESOLVED=$((RESOLVED + 1))
            else
                echo "detect-silent-published-work: FAILED to auto-resolve gate $g_id ($g_key)" >&2
                FAILED=$((FAILED + 1))
            fi
        done <<GATE_EOF
$GATES
GATE_EOF
    fi

    # ── B1 arm (ii): an open factory PR no bead points at ───────────────────
    [ -n "$SCOPE_REPO" ] || continue
    OPEN_PRS="$(gh pr list -R "$SCOPE_REPO" --state open --limit 200 \
        --json number,headRefName,url 2>/dev/null)" || { UNKNOWN=$((UNKNOWN + 1)); continue; }
    # Pointed-at set includes numbers derived from pr_url, not just pr_number —
    # otherwise a correctly URL-linked PR is reported as an orphan every sweep.
    POINTED="$(printf '%s' "$BEADS_JSON" | jq -r '
        (if type == "array" then . else [.] end)[] | .metadata // {}
        | [(.pr_number // ""), ((.pr_url // "") | capture("/pull/(?<n>[0-9]+)") // {n:""} | .n)][]
        | select(. != "")' 2>/dev/null | sort -u || true)"
    while IFS="$US" read -r opr obranch ourl; do
        [ -n "$opr" ] || continue
        match=0
        for pat in $BRANCH_PATTERNS; do
            case "$obranch" in "$pat"*|*"/$pat"*) match=1; break ;; esac
        done
        [ "$match" -eq 1 ] || continue
        printf '%s\n' "$POINTED" | grep -Fxq "$opr" && continue
        # Dedup surface for THIS arm is the STATE FILE, not a store gate. Arm
        # (i) dedups on a gate bead, but `gate create` REQUIRES --blocks and the
        # orphan case is by definition beadless — there is nothing to block —
        # and the reconciliation phase above resolves any silentwork gate whose
        # episode the candidate loop did not re-derive, so a blocking-nothing
        # gate would be resolved next sweep regardless. This arm previously
        # CHECKED for a gate that nothing anywhere creates, so the check never
        # passed and the mail re-sent every cooldown forever (measured on the
        # westeros deployment: the same nine orphans, ~108 mails/hour). A
        # mailed-at record with a re-remind interval is honest here BECAUSE the
        # condition itself is re-derived live from gh every sweep: state loss
        # costs at most one duplicate mail, and a record for a PR that stops
        # being an orphan is dropped at the very NEXT write — NEXT_STATE is
        # rebuilt from {} each sweep, so only refreshed records survive.
        OEP="$(episode_key "orphan" "$opr" "no-bead")"
        # The state key carries the SCOPE (matching the bead arm's
        # "$SCOPE_LABEL|$bead" convention): the state file is city-wide while
        # PR numbers are per-repo, so an unscoped key would let one rig's
        # latched orphan silence a NEW genuine orphan with the same number on
        # another rig. OEP alone stays in the mail's Episode: line.
        OKEY="$SCOPE_LABEL|$OEP"
        oprev="$(echo "$STATE" | jq -c --arg k "$OKEY" '.[$k] // empty' 2>/dev/null || true)"
        ofirst="$NOW_ISO"; omailed=""
        if [ -n "$oprev" ]; then
            pf="$(echo "$oprev" | jq -r '.first_observed_in_state_at // ""' 2>/dev/null || true)"
            [ -n "$pf" ] && ofirst="$pf"
            omailed="$(echo "$oprev" | jq -r '.orphan_mailed_at // ""' 2>/dev/null || true)"
        fi
        if [ -n "$omailed" ]; then
            om_epoch="$(iso_to_epoch "$omailed")"
            if [ -n "$om_epoch" ] && [ $(( NOW_EPOCH - om_epoch )) -lt "$ORPHAN_REMIND_S" ]; then
                # Mailed within the remind window: keep the record alive
                # (still an orphan) and stay quiet.
                NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$OKEY" --argjson v "$oprev" '.[$k] = $v' 2>/dev/null || echo "$NEXT_STATE")"
                continue
            fi
        fi
        echo "detect-silent-published-work: ORPHAN PR — $ourl (branch $obranch) has no bead pointing at it" >&2
        ORPHANS=$((ORPHANS + 1))
        if gc mail send "$ORPHAN_RECIPIENT" --notify \
            -s "Orphan factory PR with no bead: $ourl" \
            -m "An OPEN PR on a factory branch has no bead pointing at it, so nothing in the system will ever mention it again.

  PR:     $ourl
  Branch: $obranch
  Repo:   $SCOPE_REPO

This is the mirror image of a bead published without a pointer: work that exists
with no record leading anyone to it. Attach it to its bead, or close it.

Raised by detect-silent-published-work (ga-krso22 / ga-mmvpq1 Half B, B1 arm ii).
Episode: $OEP" >/dev/null 2>&1; then
            # Stamp the latch ONLY on a delivered mail; a failed send leaves the
            # prior record (or none) in place so the next sweep retries.
            NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$OKEY" --arg f "$ofirst" --arg m "$NOW_ISO" \
                '.[$k] = {first_observed_in_state_at: $f, orphan_mailed_at: $m}' 2>/dev/null || echo "$NEXT_STATE")"
        else
            echo "detect-silent-published-work: FAILED to report orphan PR $ourl (will retry next sweep)" >&2
            FAILED=$((FAILED + 1))
            [ -n "$oprev" ] && NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$OKEY" --argjson v "$oprev" '.[$k] = $v' 2>/dev/null || echo "$NEXT_STATE")"
        fi
    done <<ORPHAN_EOF
$(printf '%s' "$OPEN_PRS" | jq -r --arg us "$US" '.[] | [(.number|tostring), .headRefName, .url] | join($us)' 2>/dev/null || true)
ORPHAN_EOF
done < "$SCOPES_FILE"

# Prune past retention. On a jq failure KEEP THE UNPRUNED STATE rather than
# writing an empty object: an empty state file resets every clock, so a malformed
# retention value or one bad timestamp would silently guarantee that no alarm
# ever fires again.
RETENTION_S="$(duration_to_seconds "$RETENTION")"
if PRUNED="$(echo "$NEXT_STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select((now - (.value.first_observed_in_state_at | fromdateiso8601)) <= $keep))' 2>/dev/null)" \
    && [ -n "$PRUNED" ]; then
    NEXT_STATE="$PRUNED"
else
    echo "detect-silent-published-work: state prune failed; keeping the unpruned state (an empty state file would reset every clock)" >&2
fi

TMP="$(mktemp "$PACK_STATE_DIR/.detect-silent-published-work-state.XXXXXX")"
printf '%s\n' "$NEXT_STATE" > "$TMP"
mv -f "$TMP" "$STATE_FILE"

SKIPPED_TOTAL=$(( SKIPPED_NOTOURS + SKIPPED_UNKNOWN + SKIPPED_ROSTER_BLIND ))
if [ "$ALARMED" -gt 0 ] || [ "$RESOLVED" -gt 0 ] || [ "$ORPHANS" -gt 0 ] || [ "$SKIPPED_TOTAL" -gt 0 ] || [ "$OURS_BY_STORE" -gt 0 ]; then
    echo "detect-silent-published-work: $ALARMED alarm(s), $RESOLVED gate(s) auto-resolved, $ORPHANS orphan PR(s), $SKIPPED_TOTAL past-threshold candidate(s) not alarmed on subject scope, $OURS_BY_STORE town-local (ours by store)"
fi

# TOWN LOCALITY (a), COUNTED. A subject-scope narrowing that nobody can see is a
# fresh instance of the class this detector exists to catch: an alarm leg that
# has quietly stopped alarming is indistinguishable from a city with no stalled
# work. The skips are named and counted, split by WHY — "not ours" is a
# decision, "roster unreadable" is a fault — and the FAULT shapes always reach
# the operator because they ride UNKNOWN into the non-zero exit below, whose
# output the controller retains. BE HONEST ABOUT THE DECISION shape: on a sweep
# that is otherwise clean this order exits 0 and the controller discards stdout
# entirely (order stdout is stored nowhere), so the routine "not ours" count is
# NOT a per-sweep record — reading its steady state means running the order by
# hand or the ga-hwk3r9 noise review, and any claim stronger than that here
# would be the very invisibility this comment warns about (claude review
# finding 5, 2026-09-22).
if [ "$SKIPPED_TOTAL" -gt 0 ]; then
    echo "detect-silent-published-work: subject scope skipped $SKIPPED_TOTAL candidate(s) past threshold: $SKIPPED_NOTOURS not on this city's roster, $SKIPPED_UNKNOWN with an unreadable identity, $SKIPPED_ROSTER_BLIND with the roster itself unreadable${SKIP_SAMPLE:+ — e.g. $SKIP_SAMPLE}" >&2
fi

# A BLIND SWEEP MUST NOT LOOK LIKE A CLEAN ONE. The controller retains an exec
# order's output only on a non-zero exit, so an UNKNOWN reported to stdout on a
# zero exit is invisible — indistinguishable from "nothing is stalled", which is
# the exact confusion this order exists to prevent. UNKNOWN therefore exits
# non-zero, same as a failure, and says which it was.
if [ "$UNKNOWN" -gt 0 ] || [ "$FAILED" -gt 0 ]; then
    [ "$UNKNOWN" -gt 0 ] && echo "detect-silent-published-work: $UNKNOWN read(s) UNKNOWN this sweep — neither silent nor healthy; this sweep was partially blind" >&2
    [ "$FAILED" -gt 0 ] && echo "detect-silent-published-work: $FAILED action(s) failed (will retry next sweep)" >&2
    exit 1
fi
