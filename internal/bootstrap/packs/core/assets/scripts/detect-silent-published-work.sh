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
# volume, never from priors (mandatory noise review one week after enablement).
THRESHOLD="${GC_SILENT_WORK_THRESHOLD:-2h}"
# Observation entries older than this are pruned so the state file stays bounded.
# Must comfortably exceed THRESHOLD: a live silent bead's entry is refreshed
# every sweep, so only a resolved/merged bead's entry ever reaches this.
RETENTION="${GC_SILENT_WORK_STATE_RETENTION:-30d}"
# Where an "orphan PR, no bead" alarm goes — it is nobody's bead by definition,
# so it cannot be bead-attached and must address a person.
ORPHAN_RECIPIENT="${GC_SILENT_WORK_ORPHAN_RECIPIENT:-mayor}"
# Fallback addressee when a bead has no live assignee and no rig project-lead.
ESCALATION_RECIPIENT="${GC_ESCALATION_RECIPIENT:-human}"
# Branch-name prefixes that mark a PR as factory-published work. The
# reconciliation arm uses these to decide which open PRs SHOULD have a bead.
BRANCH_PATTERNS="${GC_SILENT_WORK_BRANCH_PATTERNS:-polecat/ fix/ nux/ integration/}"

PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
STATE_FILE="$PACK_STATE_DIR/detect-silent-published-work-state.json"
mkdir -p "$PACK_STATE_DIR"

# Convert a simple Go-style duration (Ns/Nm/Nh/Nd) to whole seconds.
duration_to_seconds() {
    case "$1" in
        *d) echo $(( ${1%d} * 86400 )) ;;
        *h) echo $(( ${1%h} * 3600 )) ;;
        *m) echo $(( ${1%m} * 60 )) ;;
        *s) echo "${1%s}" ;;
        *)  echo "$1" ;;
    esac
}

# Parse an ISO-8601 UTC timestamp to epoch seconds. Portable across GNU and
# BSD/macOS: GNU `date -d` first, then BSD `date -ju -f`. Without the BSD
# fallback every candidate would fail its age check on macOS and the whole sweep
# would silently pass as clean — the exact shape of bug this order detects.
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

# Load OBSERVATION state. Keys are "<scope>|<bead-id>"; values carry what the
# previous sweep SAW. A missing or corrupt file resets to empty — which restarts
# every clock rather than inventing ages, the conservative direction (a restarted
# clock delays an alarm by at most THRESHOLD; a fabricated one fires falsely).
STATE="$(cat "$STATE_FILE" 2>/dev/null || true)"
echo "$STATE" | jq -e 'type == "object"' >/dev/null 2>&1 || STATE='{}'
NEXT_STATE='{}'

ALARMED=0
RESOLVED=0
UNKNOWN=0
ORPHANS=0
FAILED=0

# Scopes: HQ (empty) plus every non-HQ rig, matching the sibling sweeps. The
# published-work population lives on the rig stores, not HQ, so a bare
# HQ-only query would sweep nothing and report clean.
SCOPES_FILE="$(mktemp "$PACK_STATE_DIR/.detect-silent-scopes.XXXXXX")"
trap 'rm -f "$SCOPES_FILE"' EXIT
printf '\n' > "$SCOPES_FILE"
RIGS_JSON="$(gc rig list --json 2>/dev/null || true)"
if [ -n "$RIGS_JSON" ]; then
    printf '%s' "$RIGS_JSON" \
        | jq -r '(.rigs // [])[] | select(.hq != true) | .name' 2>/dev/null \
        >> "$SCOPES_FILE" || true
fi

# Resolve owner/repo for a scope from its rig path's git origin. Derived from
# state (the checkout's actual remote), not configured separately, so it cannot
# drift from the repo the work really lives in.
repo_for_scope() {
    local scope="$1" path url
    if [ -z "$scope" ]; then
        path="$CITY"
    else
        path="$(printf '%s' "$RIGS_JSON" | jq -r --arg n "$scope" \
            '(.rigs // [])[] | select(.name == $n) | .path // ""' 2>/dev/null)"
    fi
    [ -n "$path" ] && [ -d "$path" ] || { echo ""; return 0; }
    url="$(git -C "$path" remote get-url origin 2>/dev/null || true)"
    [ -n "$url" ] || { echo ""; return 0; }
    printf '%s' "$url" | sed -E 's#^git\+##; s#^(https://|git@)github\.com[:/]##; s#\.git$##'
}

# Episode key — the identity of ONE stall. (bead, PR identity, state): a
# recurrence after resolution is a NEW episode and must alarm again, while
# concurrent sweeps of the SAME stall must converge on one gate.
episode_key() { printf 'silentwork:%s:%s:%s' "$1" "$2" "$3"; }

# Does an OPEN gate for this episode already exist? Asked as a STATE QUERY
# against the store, never "did I create one" against local state — a local
# record can be lost for a gate that exists or kept for one that never landed,
# and neither is detectable from the record (08:10Z).
open_gate_for_episode() {
    local scope="$1" key="$2" rig1="" rig2=""
    [ -n "$scope" ] && { rig1="--rig"; rig2="$scope"; }
    gc bd gate list ${rig1:+"$rig1" "$rig2"} --limit 0 --json 2>/dev/null \
        | jq -r --arg k "$key" '(if type == "array" then . else [.] end)[]
             | select(.status == "open" and (.title // "" | contains($k)))
             | .id' 2>/dev/null | head -1
}

# Addressee, RE-EVALUATED every sweep so a dead owner escalates at sweep cadence
# instead of being renudged forever: live-session assignee -> rig project-lead ->
# escalation recipient.
resolve_addressee() {
    local assignee="$1" scope="$2"
    if [ -n "$assignee" ] && gc session list --json 2>/dev/null \
        | jq -e --arg a "$assignee" '[(if type=="array" then . else (.sessions // []) end)[]
             | select((.name // "") == $a)] | length > 0' >/dev/null 2>&1; then
        printf '%s' "$assignee"; return 0
    fi
    if [ -n "$scope" ]; then
        printf '%s/oversight.project-lead' "$scope"; return 0
    fi
    printf '%s' "$ESCALATION_RECIPIENT"
}

while IFS= read -r scope; do
    RIG1=""; RIG2=""
    [ -n "$scope" ] && { RIG1="--rig"; RIG2="$scope"; }
    SCOPE_LABEL="${scope:-hq}"
    REPO="$(repo_for_scope "$scope")"

    # ── B1 arm (i): beads the factory says are published and waiting ─────────
    # --limit 0: a scope past the default page would silently drop candidates,
    # and a dropped candidate reads as "no silence here".
    BEADS_JSON="$(gc bd list ${RIG1:+"$RIG1" "$RIG2"} --limit 0 --json 2>/dev/null)" || {
        echo "detect-silent-published-work: scope $SCOPE_LABEL: cannot list beads (UNKNOWN, skipped this sweep)" >&2
        UNKNOWN=$((UNKNOWN + 1)); continue
    }
    [ -n "$BEADS_JSON" ] && [ "$BEADS_JSON" != "null" ] || continue

    CANDIDATES="$(printf '%s' "$BEADS_JSON" | jq -r '
        (if type == "array" then . else [.] end)[]
        | select((.metadata."merge_result" // "") as $m
                 | $m == "pr_published_awaiting_gate" or $m == "gate_clear_awaiting_merge")
        | [.id, (.metadata."merge_result" // ""), (.metadata."pr_number" // ""),
           (.metadata."pr_url" // ""), (.assignee // ""), (.status // "")]
        | @tsv' 2>/dev/null)" || CANDIDATES=""

    while IFS="$(printf '\t')" read -r bead mres pr_number pr_url assignee bstatus; do
        [ -n "$bead" ] || continue
        KEY="$SCOPE_LABEL|$bead"

        # B5 mirror: a published state with no re-derivable pointer is itself
        # the alarm. A pointer that resolves to an action record is
        # insufficient — pr_url must be fetchable RIGHT NOW (08:10Z).
        if [ -z "$pr_number" ] && [ -z "$pr_url" ]; then
            echo "detect-silent-published-work: $bead in state '$mres' carries NO pr_number/pr_url — unverifiable pointer" >&2
            FAILED=$((FAILED + 1))
            continue
        fi
        [ -n "$pr_number" ] || pr_number="$(printf '%s' "$pr_url" | sed -E 's#.*/pull/([0-9]+).*#\1#')"
        [ -n "$REPO" ] || { UNKNOWN=$((UNKNOWN + 1)); continue; }

        # ── B2: cheap live reads. Any failure => UNKNOWN, skip. ─────────────
        PR_JSON="$(gh api "repos/$REPO/pulls/$pr_number" 2>/dev/null)" || {
            UNKNOWN=$((UNKNOWN + 1))
            # Carry the prior observation forward untouched: a read failure must
            # neither start nor advance a clock.
            PREV="$(echo "$STATE" | jq -c --arg k "$KEY" '.[$k] // empty')"
            [ -n "$PREV" ] && NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$KEY" --argjson v "$PREV" '.[$k] = $v')"
            continue
        }
        live_head="$(printf '%s' "$PR_JSON" | jq -r '.head.sha // ""')"
        live_base="$(printf '%s' "$PR_JSON" | jq -r '.base.ref // ""')"
        pr_state="$(printf '%s' "$PR_JSON" | jq -r '.state // ""')"
        pr_merged="$(printf '%s' "$PR_JSON" | jq -r 'if .merged then "true" else "false" end')"

        COMMENTS_JSON="$(gh api --paginate --slurp "repos/$REPO/issues/$pr_number/comments" 2>/dev/null)" || {
            UNKNOWN=$((UNKNOWN + 1))
            PREV="$(echo "$STATE" | jq -c --arg k "$KEY" '.[$k] // empty')"
            [ -n "$PREV" ] && NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$KEY" --argjson v "$PREV" '.[$k] = $v')"
            continue
        }
        # The renderer-produced gate comment, last wins. Selection mirrors the
        # refinery predicate exactly ("## Sherpa gate" prefix).
        GATE_JSON="$(printf '%s' "$COMMENTS_JSON" | jq -c '
            [.[][] | select(.body | startswith("## Sherpa gate"))] | last
            | select(. != null) | {body: .body, login: .user.login}' 2>/dev/null)" || GATE_JSON=""

        gate_status=""; gate_head=""; gate_base=""
        if [ -n "$GATE_JSON" ] && [ "$GATE_JSON" != "null" ]; then
            # AUTHENTICATION, not association: a read-only member can post a
            # lookalike. Require effective repo permission >= write, matching
            # the predicate. An unauthenticated sticky classifies as NO sticky.
            gl="$(printf '%s' "$GATE_JSON" | jq -r '.login // ""')"
            perm="$(gh api "repos/$REPO/collaborators/$gl/permission" --jq '.permission' 2>/dev/null || true)"
            case "$perm" in
                admin|maintain|write)
                    gbody="$(printf '%s' "$GATE_JSON" | jq -r '.body')"
                    gate_status="$(printf '%s' "$gbody" | sed -n 's/^\*\*Gate status\*\*:[^`]*`\([A-Z]*\)`.*/\1/p' | head -1)"
                    gate_head="$(printf '%s' "$gbody" | sed -n 's/^\*\*HEAD\*\*: `\([0-9a-f]*\)`.*/\1/p' | head -1)"
                    gate_base="$(printf '%s' "$gbody" | sed -n 's/.*\*\*Base\*\*: `\([^`]*\)`.*/\1/p' | head -1)"
                    ;;
                "") UNKNOWN=$((UNKNOWN + 1)); gate_status="__UNKNOWN__" ;;
                *)  : ;;   # insufficient permission => treat as no sticky
            esac
        fi
        if [ "$gate_status" = "__UNKNOWN__" ]; then
            PREV="$(echo "$STATE" | jq -c --arg k "$KEY" '.[$k] // empty')"
            [ -n "$PREV" ] && NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$KEY" --argjson v "$PREV" '.[$k] = $v')"
            continue
        fi

        # ── Classification. NOT read: CI conclusions, mergeability, reviews. ──
        cls=""
        if [ "$pr_state" = "closed" ] && [ "$pr_merged" != "true" ]; then
            # B3 enumerates "PR merge/close" as progress, so this leaves the
            # silent class and any episode gate auto-resolves below. Surfaced on
            # stderr rather than dropped quietly: a bead still sitting in a
            # published state behind a CLOSED, unmerged PR is a real
            # inconsistency, just not the silence this detector alarms on.
            echo "detect-silent-published-work: $bead is in state '$mres' but its PR ${pr_url:-$REPO#$pr_number} is CLOSED unmerged (bead status: ${bstatus:-?})" >&2
            cls="out-of-scope"
        elif [ "$pr_merged" = "true" ]; then
            if [ "$gate_status" = "CLEAR" ]; then
                cls="out-of-scope"
            else
                cls="bypass-detected"
            fi
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
        # Enumerated progress: the classification changed, the head moved, the
        # base moved, the sticky marker changed, or the PR merged/closed.
        # Explicitly NOT progress: this detector's own activity, renudges,
        # comments, CI/mergeability/review changes. A detector whose own writes
        # reset the clock hides the stall it exists to find.
        marker="${gate_status}@${gate_head}"
        PREV="$(echo "$STATE" | jq -c --arg k "$KEY" '.[$k] // empty')"
        first_seen="$NOW_ISO"
        if [ -n "$PREV" ]; then
            p_state="$(printf '%s' "$PREV" | jq -r '.state // ""')"
            p_head="$(printf '%s' "$PREV" | jq -r '.observed_head // ""')"
            p_base="$(printf '%s' "$PREV" | jq -r '.observed_base // ""')"
            p_marker="$(printf '%s' "$PREV" | jq -r '.observed_marker // ""')"
            p_first="$(printf '%s' "$PREV" | jq -r '.first_observed_in_state_at // ""')"
            if [ "$p_state" = "$cls" ] && [ "$p_head" = "$live_head" ] \
               && [ "$p_base" = "$live_base" ] && [ "$p_marker" = "$marker" ]; then
                # Nothing moved — the clock keeps running from its first sighting.
                [ -n "$p_first" ] && first_seen="$p_first"
            fi
        fi

        # Persist the OBSERVATION (never an action record).
        NEXT_STATE="$(echo "$NEXT_STATE" | jq -c --arg k "$KEY" \
            --arg f "$first_seen" --arg s "$cls" --arg h "$live_head" \
            --arg b "$live_base" --arg m "$marker" \
            '.[$k] = {first_observed_in_state_at: $f, state: $s,
                      observed_head: $h, observed_base: $b, observed_marker: $m}')"

        EP="$(episode_key "$bead" "$pr_number" "$cls")"

        # A bead that is no longer silent must have its gate re-derived closed —
        # every sweep, from live state, never from "we resolved it already".
        if [ "$cls" = "healthy" ] || [ "$cls" = "out-of-scope" ]; then
            for st in silent-ungated silent-stale silent-unexecuted; do
                gid="$(open_gate_for_episode "$scope" "$(episode_key "$bead" "$pr_number" "$st")")"
                if [ -n "$gid" ]; then
                    if gc bd gate resolve "$gid" ${RIG1:+"$RIG1" "$RIG2"} >/dev/null 2>&1; then
                        RESOLVED=$((RESOLVED + 1))
                    else
                        echo "detect-silent-published-work: FAILED to auto-resolve gate $gid for $bead (will retry next sweep)" >&2
                        FAILED=$((FAILED + 1))
                    fi
                fi
            done
            continue
        fi

        # A merge that landed without a CLEAR gate is the escape hatch made
        # visible. Labeled DETECTION, not reconstruction: a later scan cannot
        # prove merge-time state, and claiming otherwise would be the same
        # log-as-evidence error this design forecloses.
        if [ "$cls" = "bypass-detected" ]; then
            if ! gc bd comment "$bead" ${RIG1:+"$RIG1" "$RIG2"} \
                "MERGE WITHOUT A CLEAR GATE — detected, not reconstructed.
PR: ${pr_url:-$REPO#$pr_number}
Merged head: $live_head into $live_base
Gate status observed now: ${gate_status:-<none>}
This is a post-hoc DETECTION: the gate state at merge time cannot be proven by a
later scan, so this records what is true now and names the gap rather than
claiming forensic precision. See ga-mmvpq1 A6." >/dev/null 2>&1; then
                echo "detect-silent-published-work: FAILED to record bypass on $bead (will retry next sweep)" >&2
                FAILED=$((FAILED + 1))
            fi
            continue
        fi

        # ── Threshold ────────────────────────────────────────────────────────
        first_epoch="$(iso_to_epoch "$first_seen")"
        [ -n "$first_epoch" ] || continue
        age=$(( NOW_EPOCH - first_epoch ))
        [ "$age" -ge "$THRESHOLD_S" ] || continue
        age_h=$(( age / 3600 )); age_m=$(( (age % 3600) / 60 ))

        # ── B4(ii): idempotent gate upsert, keyed on the episode ────────────
        # The existence check is a live query, so two concurrent sweeps converge
        # on one gate instead of racing to create two.
        gid="$(open_gate_for_episode "$scope" "$EP")"
        if [ -z "$gid" ]; then
            GTITLE="Silent work: $bead stalled ${age_h}h${age_m}m in $cls [$EP]"
            GREASON="Published work waiting on nobody. PR ${pr_url:-$REPO#$pr_number} has shown no progress for ${age_h}h${age_m}m (state: $cls). THE WORK EXISTS — do not restart it."
            if gc bd gate create ${RIG1:+"$RIG1" "$RIG2"} --type human --blocks "$bead" \
                --title "$GTITLE" --reason "$GREASON" >/dev/null 2>&1; then
                gid="$(open_gate_for_episode "$scope" "$EP")"
            else
                echo "detect-silent-published-work: FAILED to raise gate for $bead episode $EP (will retry next sweep)" >&2
                FAILED=$((FAILED + 1))
                continue
            fi
        fi

        # ── B4(i): the record ON THE BEAD — the primary artifact ────────────
        # Stamped every time the episode is live; the comment is what the NEXT
        # CLAIMANT reads, and it is the anti-duplication signal.
        ADDR="$(resolve_addressee "$assignee" "$scope")"
        # A CLOSED bead behind an OPEN PR is the strongest "nobody is coming
        # back to this" signal there is: the tracker says done, the work says
        # otherwise, and no one will look at the bead again.
        bead_note=""
        [ "$bstatus" = "closed" ] && bead_note="  <-- bead is CLOSED but its PR is still open"
        if ! gc bd update "$bead" ${RIG1:+"$RIG1" "$RIG2"} \
            --set-metadata "gc.silent_work_alarm=$NOW_ISO/$cls" >/dev/null 2>&1; then
            echo "detect-silent-published-work: FAILED to stamp gc.silent_work_alarm on $bead" >&2
            FAILED=$((FAILED + 1))
        fi
        PREV_ALARM="$(printf '%s' "$BEADS_JSON" | jq -r --arg b "$bead" '
            (if type == "array" then . else [.] end)[]
            | select(.id == $b) | .metadata."gc.silent_work_alarm" // ""' 2>/dev/null)"
        # Comment once per episode: re-stamping metadata each sweep is cheap,
        # but a comment per sweep would bury the bead it is trying to explain.
        if [ -z "$PREV_ALARM" ]; then
            if ! gc bd comment "$bead" ${RIG1:+"$RIG1" "$RIG2"} \
                "THIS WORK EXISTS — DO NOT RESTART IT.
Published work waiting on nobody, detected by absence of progress.
  PR:      ${pr_url:-https://github.com/$REPO/pull/$pr_number}
  Head:    $live_head  (base $live_base)
  State:   $cls
  Bead:    ${bstatus:-?}${bead_note}
  Silent:  ${age_h}h${age_m}m with no observed progress
  Gate:    ${gid:-<pending>}   addressee: $ADDR
Progress means a push, a re-render of the gate sticky, a merge/close, or a
reroute of this bead. Comments, CI runs and review activity are NOT progress and
do not clear this.
Raised by detect-silent-published-work (ga-krso22 / ga-mmvpq1 Half B)." >/dev/null 2>&1; then
                echo "detect-silent-published-work: FAILED to attach evidence comment to $bead" >&2
                FAILED=$((FAILED + 1))
            fi
        fi
        ALARMED=$((ALARMED + 1))
    done <<CANDIDATE_EOF
$CANDIDATES
CANDIDATE_EOF

    # ── B1 arm (ii): reconciliation — an open factory PR no bead points at ───
    # Promoted from residual because all four exhibits were found by humans
    # doing something else. One REST list per sweep.
    [ -n "$REPO" ] || continue
    OPEN_PRS="$(gh pr list -R "$REPO" --state open --limit 200 \
        --json number,headRefName,url 2>/dev/null)" || {
        UNKNOWN=$((UNKNOWN + 1)); continue
    }
    POINTED="$(printf '%s' "$BEADS_JSON" | jq -r '
        (if type == "array" then . else [.] end)[]
        | (.metadata."pr_number" // "") | select(. != "")' 2>/dev/null | sort -u)"
    while IFS="$(printf '\t')" read -r opr obranch ourl; do
        [ -n "$opr" ] || continue
        match=0
        for pat in $BRANCH_PATTERNS; do
            case "$obranch" in "$pat"*|*"/$pat"*) match=1; break ;; esac
        done
        [ "$match" -eq 1 ] || continue
        printf '%s\n' "$POINTED" | grep -Fxq "$opr" && continue
        echo "detect-silent-published-work: ORPHAN PR — $ourl (branch $obranch) has no bead pointing at it" >&2
        ORPHANS=$((ORPHANS + 1))
        gc mail send "$ORPHAN_RECIPIENT" --notify \
            -s "Orphan factory PR with no bead: $ourl" \
            -m "An OPEN PR on a factory branch has no bead pointing at it, so nothing in the system will ever mention it again.

  PR:     $ourl
  Branch: $obranch
  Repo:   $REPO

This is the mirror image of a bead published without a pointer: work that exists
with no record that would lead anyone to it. Someone should either attach it to
its bead or close it.

Raised by detect-silent-published-work (ga-krso22 / ga-mmvpq1 Half B, B1 arm ii)." \
            >/dev/null 2>&1 || {
                echo "detect-silent-published-work: FAILED to report orphan PR $ourl (will retry next sweep)" >&2
                FAILED=$((FAILED + 1))
            }
    done <<ORPHAN_EOF
$(printf '%s' "$OPEN_PRS" | jq -r '.[] | [(.number|tostring), .headRefName, .url] | @tsv' 2>/dev/null)
ORPHAN_EOF
done < "$SCOPES_FILE"

# Prune observations past retention so the file stays bounded. A live silent
# bead is refreshed every sweep, so only settled ones age out.
RETENTION_S="$(duration_to_seconds "$RETENTION")"
NEXT_STATE="$(echo "$NEXT_STATE" | jq --argjson keep "$RETENTION_S" \
    'with_entries(select((now - (.value.first_observed_in_state_at | fromdateiso8601)) <= $keep))')" || true

TMP="$(mktemp "$PACK_STATE_DIR/.detect-silent-published-work-state.XXXXXX")"
printf '%s\n' "$NEXT_STATE" > "$TMP"
mv -f "$TMP" "$STATE_FILE"

if [ "$ALARMED" -gt 0 ] || [ "$RESOLVED" -gt 0 ] || [ "$ORPHANS" -gt 0 ]; then
    echo "detect-silent-published-work: $ALARMED silent-work alarm(s), $RESOLVED gate(s) auto-resolved, $ORPHANS orphan PR(s)"
fi
# UNKNOWN is reported but is NOT a failure: a read that could not be made is
# honestly unknown, and the whole point is that it is never counted as silence
# OR as health. It is surfaced so a sweep that went blind is visible as blind
# rather than as clean.
if [ "$UNKNOWN" -gt 0 ]; then
    echo "detect-silent-published-work: $UNKNOWN candidate(s)/scope(s) UNKNOWN this sweep (read failure — neither silent nor healthy)" >&2
fi

# Loud-fail: state is already written, so a non-zero exit surfaces the per-item
# failures above to the controller log without losing recorded observations. The
# controller logs an exec order's output only on a non-zero exit (#4543).
if [ "$FAILED" -gt 0 ]; then
    echo "detect-silent-published-work: $FAILED action(s) failed (see above; will retry next sweep)" >&2
    exit 1
fi
