#!/usr/bin/env bash
# wisp-compact — TTL-based cleanup of expired ephemeral beads.
#
# Wisps are short-lived work items (heartbeats, pings, patrols) that
# accumulate and bloat the database. This script applies retention policy:
# - Closed wisps past TTL → deleted. IRREVERSIBLE: wisp rows skip
#   DOLT_COMMIT, so there is no AS OF history, no dolt_diff, and nothing
#   to recover a wrong delete from. Each delete therefore re-derives its
#   disposal from a fresh read immediately before the write, and only
#   confirmed deletions are counted.
# - Non-closed wisps past TTL → promoted to permanent (stuck detection)
# - Wisps with comments or "keep" label → promoted (proven value)
#
# TTL by wisp_type label:
#   heartbeat, ping: 6h
#   patrol, gc_report: 24h
#   recovery, error, escalation: 7d
#   default (untyped): 24h
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "wisp-compact"

CITY="${GC_CITY:-.}"

# Get all ephemeral beads.
ALL=$(gc bd list --json --all -n 0 2>/dev/null) || exit 0
EPHEMERALS=$(echo "$ALL" | jq '[.[] | select(.ephemeral == true)]' 2>/dev/null) || exit 0

if [ -z "$EPHEMERALS" ] || [ "$EPHEMERALS" = "[]" ]; then
    exit 0
fi

NOW=$(date +%s)
PROMOTED=0
DELETED=0
SKIPPED=0
RECHECK_SKIPPED=0
PROMOTE_FAILED=0
DELETE_FAILED=0

# Process each ephemeral bead. Capturing jq output into BEADS first
# (instead of piping into the loop) preserves the original pipefail
# fail-loud on jq error AND keeps PROMOTED/DELETED/SKIPPED in the parent
# shell so they survive to the summary echo below. EPHEMERALS is
# pre-validated as a non-empty array on lines 22-27, so BEADS is
# guaranteed non-empty here.
BEADS=$(echo "$EPHEMERALS" | jq -c '.[]' 2>/dev/null)
while IFS= read -r bead; do
    id=$(echo "$bead" | jq -r '.id')
    status=$(echo "$bead" | jq -r '.status')
    updated_at=$(echo "$bead" | jq -r '.updated_at // .created_at')
    comment_count=$(echo "$bead" | jq -r '.comment_count // 0')
    labels=$(echo "$bead" | jq -r '.labels // [] | .[]' 2>/dev/null)

    # Determine TTL from wisp_type label.
    TTL_SECONDS=$((24 * 3600))  # default: 24h
    for label in $labels; do
        case "$label" in
            wisp_type:heartbeat|wisp_type:ping) TTL_SECONDS=$((6 * 3600)) ;;
            wisp_type:patrol|wisp_type:gc_report) TTL_SECONDS=$((24 * 3600)) ;;
            wisp_type:recovery|wisp_type:error|wisp_type:escalation) TTL_SECONDS=$((7 * 24 * 3600)) ;;
            keep) TTL_SECONDS=0 ;;  # force promote
        esac
    done

    # Calculate age. bd emits RFC3339 timestamps with a trailing 'Z'; the
    # second BSD `date -ju -f` fallback handles that explicitly and forces
    # UTC semantics to match GNU `date -d`. The third layout supports older
    # no-Z timestamps without interpreting them in the local timezone.
    BEAD_TS=$(date -d "$updated_at" +%s 2>/dev/null || \
              date -ju -f "%Y-%m-%dT%H:%M:%SZ" "$updated_at" +%s 2>/dev/null || \
              date -ju -f "%Y-%m-%dT%H:%M:%S" "$updated_at" +%s 2>/dev/null) || continue
    AGE=$((NOW - BEAD_TS))

    # Skip if within TTL (unless force-promote via keep label).
    if [ "$TTL_SECONDS" -gt 0 ] && [ "$AGE" -lt "$TTL_SECONDS" ]; then
        SKIPPED=$((SKIPPED + 1))
        continue
    fi

    # Promote if has comments, keep label, or non-closed. The counter follows
    # the outcome, never the attempt: a failed promote is reported, not
    # counted as done.
    if [ "$comment_count" -gt 0 ] || echo "$labels" | grep -q '^keep$' || [ "$status" != "closed" ]; then
        REASON="proven value"
        [ "$status" != "closed" ] && REASON="open past TTL (stuck detection)"
        if gc bd update "$id" --persistent 2>/dev/null; then
            gc bd comment "$id" "Promoted from wisp: $REASON" 2>/dev/null || true
            PROMOTED=$((PROMOTED + 1))
        else
            echo "wisp-compact: FAILED promote $id" >&2
            PROMOTE_FAILED=$((PROMOTE_FAILED + 1))
        fi
        continue
    fi

    # Closed + past TTL + no special attributes → delete. The snapshot at the
    # top of the script may be arbitrarily old by the time the loop reaches
    # this row, and a wisp delete is unrecoverable (no DOLT_COMMIT leg), so
    # re-derive the disposal from a fresh read immediately before the
    # irreversible write and skip on any doubt. An unchanged updated_at means
    # the age can only have grown, so the TTL verdict still holds; comments
    # do NOT bump updated_at, which is why comment_count is re-checked
    # explicitly rather than inferred from the timestamp.
    FRESH=$(gc bd show "$id" --json 2>/dev/null) || FRESH=""
    STILL=$(echo "$FRESH" | jq -r --arg u "$updated_at" '
        (if type == "array" then .[0] else . end) as $b
        | if $b == null then "no"
          elif $b.ephemeral == true and $b.status == "closed"
               and (($b.comment_count // 0) == 0)
               and ((($b.labels // []) | index("keep")) == null)
               and (($b.updated_at // $b.created_at) == $u)
          then "yes" else "no" end' 2>/dev/null) || STILL="no"
    if [ "$STILL" != "yes" ]; then
        RECHECK_SKIPPED=$((RECHECK_SKIPPED + 1))
        continue
    fi
    if gc bd delete "$id" --force 2>/dev/null; then
        DELETED=$((DELETED + 1))
    else
        echo "wisp-compact: FAILED delete $id" >&2
        DELETE_FAILED=$((DELETE_FAILED + 1))
    fi
done <<< "$BEADS"

TOTAL=$((PROMOTED + DELETED + RECHECK_SKIPPED + PROMOTE_FAILED + DELETE_FAILED))
if [ "$TOTAL" -gt 0 ]; then
    SUMMARY="wisp-compact: promoted=$PROMOTED deleted=$DELETED skipped=$SKIPPED"
    if [ "$RECHECK_SKIPPED" -gt 0 ]; then
        SUMMARY="$SUMMARY recheck_skipped=$RECHECK_SKIPPED"
    fi
    if [ "$PROMOTE_FAILED" -gt 0 ]; then
        SUMMARY="$SUMMARY promote_failed=$PROMOTE_FAILED"
    fi
    if [ "$DELETE_FAILED" -gt 0 ]; then
        SUMMARY="$SUMMARY delete_failed=$DELETE_FAILED"
    fi
    echo "$SUMMARY"
fi

# A run with failed writes is a failed run; the order history row must say so.
if [ $((PROMOTE_FAILED + DELETE_FAILED)) -gt 0 ]; then
    exit 1
fi
