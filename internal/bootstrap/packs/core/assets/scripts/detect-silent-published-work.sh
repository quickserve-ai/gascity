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
NOW_EPOCH="$(date -u +%s)"
NOW_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
echo "$STATE" | jq -e 'type == "object"' >/dev/null 2>&1 || STATE='{}'
NEXT_STATE='{}'

ALARMED=0; RESOLVED=0; UNKNOWN=0; ORPHANS=0; FAILED=0

# Carry a prior observation forward untouched. Used on every UNKNOWN path: a read
# that could not be made must neither start nor advance nor DISCARD a clock.
# Dropping it would restart the clock next sweep, so a flapping API would hold a
# real stall permanently below threshold.
carry_forward() {
    local key="$1" prev
    prev="$(echo "$STATE" | jq -c --arg k "$key" '.[$k] // empty' 2>/dev/null || true)"
    [ -n "$prev" ] || return 0
    NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$key" --argjson v "$prev" '.[$k] = $v' 2>/dev/null || echo "$NEXT_STATE")"
}

# Episode key — the identity of ONE stall: (bead, PR identity, state). A
# recurrence after resolution is a NEW episode; concurrent sweeps of the same
# stall converge on one gate.
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
list_episode_gates() {
    local scope="$1" rig1="" rig2="" out
    [ -n "$scope" ] && { rig1="--rig"; rig2="$scope"; }
    out="$(gc bd gate list ${rig1:+"$rig1" "$rig2"} --limit 0 --json 2>/dev/null)" || return 1
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
    [ -n "$scope" ] && { RIG1="--rig"; RIG2="$scope"; }
    SCOPE_LABEL="${scope:-hq}"
    SCOPE_REPO="$(repo_for_scope "$scope")"

    BEADS_JSON="$(gc bd list ${RIG1:+"$RIG1" "$RIG2"} --limit 0 --json 2>/dev/null)" || {
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

        EP="$(episode_key "$bead" "$pr_number" "$cls")"
        LIVE_EPISODES="$LIVE_EPISODES$EP
"
        # ── B4(ii): gate upsert, keyed on the episode ───────────────────────
        # Existence is a live query. Two concurrent sweeps can still both observe
        # "absent" and both create — atomicity is not available at this seam — so
        # the loop CONVERGES instead: any extra gate for the same episode is
        # resolved on the next sweep by the reconciliation phase below.
        if ! GATES="$(list_episode_gates "$scope")"; then
            UNKNOWN=$((UNKNOWN + 1)); continue
        fi
        gid="$(printf '%s' "$GATES" | awk -F"$US" -v k="$EP" '$2 == k {print $1; exit}')"
        if [ -z "$gid" ]; then
            if gc bd gate create ${RIG1:+"$RIG1" "$RIG2"} --type human --blocks "$bead" \
                --title "Silent work: $bead stalled ${age_h}h${age_m}m in $cls [$EP]" \
                --reason "Published work waiting on nobody. PR ${pr_url:-https://github.com/$pr_repo/pull/$pr_number} has shown no progress for ${age_h}h${age_m}m (state: $cls). THE WORK EXISTS — do not restart it." \
                >/dev/null 2>&1; then
                GATES="$(list_episode_gates "$scope" || true)"
                gid="$(printf '%s' "$GATES" | awk -F"$US" -v k="$EP" '$2 == k {print $1; exit}')"
            else
                echo "detect-silent-published-work: FAILED to raise gate for $bead episode $EP (will retry next sweep)" >&2
                FAILED=$((FAILED + 1)); continue
            fi
            # ── B4(i): the record ON THE BEAD, the primary artifact ─────────
            # Emitted exactly when the EPISODE GATE was created — a state query,
            # not a "have I commented before" flag. A metadata stamp read back as
            # dedup evidence would be action-log reasoning, and it breaks in both
            # directions: stamp-succeeds-comment-fails never retries, and a later
            # episode never comments because the field is merely non-empty.
            ADDR="$(resolve_addressee "$assignee" "$scope")"
            bead_note=""
            [ "$bstatus" = "closed" ] && bead_note="  <-- bead is CLOSED but its PR is still open"
            if ! gc bd comment "$bead" ${RIG1:+"$RIG1" "$RIG2"} \
                "THIS WORK EXISTS — DO NOT RESTART IT.
Published work waiting on nobody, detected by absence of progress.
  PR:      ${pr_url:-https://github.com/$pr_repo/pull/$pr_number}
  Head:    $live_head  (base $live_base)
  State:   $cls
  Bead:    ${bstatus:-?}${bead_note}
  Silent:  ${age_h}h${age_m}m with no observed progress
  Gate:    ${gid:-<pending>}   addressee: $ADDR
Progress means a push, a re-render of the gate sticky, a merge/close, a change of
publish state, or a reroute of this bead. Comments, CI runs and review activity
are NOT progress and do not clear this.
Raised by detect-silent-published-work (ga-krso22 / ga-mmvpq1 Half B)." >/dev/null 2>&1; then
                echo "detect-silent-published-work: FAILED to attach evidence comment to $bead" >&2
                FAILED=$((FAILED + 1))
            fi
            # Best-effort convenience stamp for humans and dashboards. NOT used
            # as dedup evidence anywhere.
            gc bd update "$bead" ${RIG1:+"$RIG1" "$RIG2"} \
                --set-metadata "gc.silent_work_alarm=$NOW_ISO/$cls" >/dev/null 2>&1 || true
            ALARMED=$((ALARMED + 1))
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
    if GATES="$(list_episode_gates "$scope")"; then
        seen_eps=""
        while IFS="$US" read -r g_id g_key; do
            [ -n "$g_id" ] && [ -n "$g_key" ] || continue
            keep=0
            printf '%s' "$LIVE_EPISODES" | grep -Fxq "$g_key" && keep=1
            # A duplicate of an episode already kept in this pass is resolved too.
            if [ "$keep" -eq 1 ]; then
                case "$seen_eps" in *"[$g_key]"*) keep=0 ;; *) seen_eps="${seen_eps}[${g_key}]" ;; esac
            fi
            [ "$keep" -eq 1 ] && continue
            if gc bd gate resolve "$g_id" ${RIG1:+"$RIG1" "$RIG2"} >/dev/null 2>&1; then
                RESOLVED=$((RESOLVED + 1))
            else
                echo "detect-silent-published-work: FAILED to auto-resolve gate $g_id ($g_key)" >&2
                FAILED=$((FAILED + 1))
            fi
        done <<GATE_EOF
$GATES
GATE_EOF
    else
        UNKNOWN=$((UNKNOWN + 1))
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
        # Dedup against the store, not a local record: one open orphan gate per
        # PR. Without this the mail re-sends every five minutes forever.
        OEP="$(episode_key "orphan" "$opr" "no-bead")"
        if ! GATES="$(list_episode_gates "$scope")"; then UNKNOWN=$((UNKNOWN + 1)); continue; fi
        if printf '%s' "$GATES" | awk -F"$US" -v k="$OEP" '$2 == k {found=1} END {exit !found}'; then
            continue
        fi
        echo "detect-silent-published-work: ORPHAN PR — $ourl (branch $obranch) has no bead pointing at it" >&2
        ORPHANS=$((ORPHANS + 1))
        gc mail send "$ORPHAN_RECIPIENT" --notify \
            -s "Orphan factory PR with no bead: $ourl" \
            -m "An OPEN PR on a factory branch has no bead pointing at it, so nothing in the system will ever mention it again.

  PR:     $ourl
  Branch: $obranch
  Repo:   $SCOPE_REPO

This is the mirror image of a bead published without a pointer: work that exists
with no record leading anyone to it. Attach it to its bead, or close it.

Raised by detect-silent-published-work (ga-krso22 / ga-mmvpq1 Half B, B1 arm ii).
Episode: $OEP" >/dev/null 2>&1 || {
                echo "detect-silent-published-work: FAILED to report orphan PR $ourl (will retry next sweep)" >&2
                FAILED=$((FAILED + 1)); }
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

if [ "$ALARMED" -gt 0 ] || [ "$RESOLVED" -gt 0 ] || [ "$ORPHANS" -gt 0 ]; then
    echo "detect-silent-published-work: $ALARMED alarm(s), $RESOLVED gate(s) auto-resolved, $ORPHANS orphan PR(s)"
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
