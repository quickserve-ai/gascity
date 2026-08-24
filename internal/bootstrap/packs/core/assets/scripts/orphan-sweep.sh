#!/usr/bin/env bash
# orphan-sweep — reset beads assigned to dead agents.
#
# Replaces the deacon patrol town-orphan-sweep step. Cross-references
# in-progress beads against all known agents. Beads assigned to agents
# that don't exist in ANY rig get reset to open/unassigned so they are
# routable again.
#
# Does NOT do worktree salvage — cities that staff a work-health patrol role
# (e.g. the gastown pack's witness) handle that separately.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
case "${BASH_SOURCE[0]}" in
    */*) __SCRIPT_DIR="$(cd "${BASH_SOURCE[0]%/*}" && pwd)" ;;
    *) __SCRIPT_DIR="$(pwd)" ;;
esac
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "orphan-sweep"

# Where the recovery summary goes. Cities that staff a coordinator role (e.g.
# the gastown pack's mayor) can point this at it; "human" is the reserved
# recipient alias that resolves in every city, including core-only ones.
ESCALATION_TARGET="${GC_ESCALATION_TARGET:-human}"

# Step 1: Collect in-progress beads from HQ and every rig whose session
# liveness can be determined.
# `gc bd list` without --rig is HQ-scoped from the city cwd, so per-rig
# beads are invisible to a bare query — walk every rig explicitly.
TMP=$(mktemp) || exit 0
SESSION_TMP=$(mktemp) || {
    rm -f "$TMP"
    exit 0
}
trap 'rm -f "$TMP" "$SESSION_TMP"' EXIT

RIG_NAMES=""
RIG_LIST=$(gc rig list --json 2>/dev/null) || RIG_LIST=""
if [ -n "$RIG_LIST" ]; then
    RIG_NAMES=$(echo "$RIG_LIST" | jq -r '.rigs[] | select(.hq == false) | .name' 2>/dev/null) || RIG_NAMES=""
fi

append_session_list() {
    local session_fetch_tmp
    local row_count
    session_fetch_tmp=$(mktemp) || return 1
    if "$@" >"$session_fetch_tmp" 2>/dev/null; then
        # An exit-0 response that parses to zero session rows carries no
        # liveness evidence at all. A city always has at least the calling
        # session live, so zero rows is never a legitimate steady state --
        # treat it the same as a hard failure so the caller skips this
        # scope's beads instead of staging them with no evidence to check
        # against.
        row_count=$(jq -r '.sessions // [] | length' "$session_fetch_tmp" 2>/dev/null) || row_count=""
        case "$row_count" in
            '' | *[!0-9]*) row_count=0 ;;
        esac
        if [ "$row_count" -eq 0 ]; then
            rm -f "$session_fetch_tmp"
            return 1
        fi
        cat "$session_fetch_tmp" >>"$SESSION_TMP"
        rm -f "$session_fetch_tmp"
        return 0
    fi
    rm -f "$session_fetch_tmp"
    return 1
}

append_hq_scope() {
    local bead_fetch_tmp
    bead_fetch_tmp=$(mktemp) || return 1
    append_session_list gc session list --json || {
        rm -f "$bead_fetch_tmp"
        return 1
    }
    gc bd list --status=in_progress --json --limit=0 2>/dev/null >"$bead_fetch_tmp" || true
    append_session_list gc session list --json || {
        rm -f "$bead_fetch_tmp"
        return 1
    }
    cat "$bead_fetch_tmp" >>"$TMP"
    rm -f "$bead_fetch_tmp"
}

append_rig_scope() {
    local rig="$1"
    local bead_fetch_tmp
    bead_fetch_tmp=$(mktemp) || return 1
    append_session_list gc --rig "$rig" session list --json || {
        rm -f "$bead_fetch_tmp"
        return 1
    }
    gc bd list --rig "$rig" --status=in_progress --json --limit=0 2>/dev/null >"$bead_fetch_tmp" || true
    append_session_list gc --rig "$rig" session list --json || {
        rm -f "$bead_fetch_tmp"
        return 1
    }
    cat "$bead_fetch_tmp" >>"$TMP"
    rm -f "$bead_fetch_tmp"
}

# Step 1b: Get all known live session identities around each bead-list query.
# The second liveness pass closes the session-list-before-bd-list race where a
# newly spawned session can claim work after the first pass but before bd-list.
# HQ liveness is required; per-rig failures only skip that rig's staged bead
# rows so one stale or unreachable rig does not disable cleanup elsewhere.
append_hq_scope || exit 0

while IFS= read -r rig; do
    [ -z "$rig" ] && continue
    if ! append_rig_scope "$rig"; then
        echo "orphan-sweep: skipping rig $rig after session-list failure" >&2
    fi
done <<<"$RIG_NAMES"

IN_PROGRESS=$(jq -c -s 'add // []' "$TMP" 2>/dev/null) || IN_PROGRESS="[]"
if [ "$IN_PROGRESS" = "[]" ]; then
    exit 0
fi

# Step 2: Get all known agent identities from resolved config.
# `gc config explain` prints Agent.QualifiedName(), including import binding
# and rig scope. Fall back to the older config-show parser for older binaries.
# ROSTER_FROM_FALLBACK records whether the rig scope was LOST, and is set at the
# point the fallback is actually taken. It must not be inferred from the roster's
# shape: a modern city whose agents are all city-scoped legitimately emits bare
# names from `gc config explain`, because Agent.QualifiedName() omits the rig when
# Dir is empty. Treating "no slash anywhere" as evidence of the fallback would
# enable degraded leaf matching on a city that lost nothing — and a foreign
# "qcore/worker" would then match a local city-scoped "worker" and be released,
# reopening the cross-city defect this guard exists to close.
ROSTER_FROM_FALLBACK=0
AGENTS=$(gc config explain 2>/dev/null | awk '/^Agent: /{print $2}') || AGENTS=""
if [ -z "$AGENTS" ]; then
    AGENTS=$(gc config show 2>/dev/null | awk '/^\[\[agent\]\]/{a=1} a && /^[[:space:]]*name[[:space:]]*=/{print; a=0}' | sed 's/.*=[[:space:]]*"\(.*\)"/\1/') || exit 0
    ROSTER_FROM_FALLBACK=1
fi
if [ -z "$AGENTS" ]; then
    exit 0
fi

# Step 2b: Parse identities of every session row that `gc session list --json`
# reports as open so that pool-spawned ephemeral assignees (e.g.
# gastown__polekitten-gc-q9j0om) are treated as known. The Go-side
# releaseOrphanedPoolAssignments path validates these from session beads via
# liveOpenSessionAssignmentExists, but this shell sweep only has the CLI JSON
# contract available. That means it protects the exposed wire identities below;
# it cannot see bead-only fields such as configured_named_identity or
# alias_history unless the CLI starts exporting them.
#
# The default CLI path already omits closed sessions. The closed/state guards
# below keep explicit or future session-list producers from making terminal
# rows live while preserving any non-closed row the CLI reports.
#
# This shell sweep ran without a live-session guard before ga-nvx: an ephemeral
# assignee whose template-stripped form did not match any agent name was
# incorrectly reset, racing against active polekitten work and producing a
# false-orphan loop.
# Two bugs the chronic strip pattern (gastownhall/gascity#2363) revealed:
# (1) The JSON shape is {"sessions":[...], "summary":..., "filters":..., "schema_version":...},
#     so `.[]` iterated four top-level scalar keys instead of session objects.
# (2) Field names vary by runtime/API path. The current CLI emits snake_case
#     (.closed/.id/.session_name/.alias/.agent_name); PascalCase is accepted
#     only as forward-compatible hardening so a casing change cannot make
#     LIVE_SESSION_IDS empty and strip active pool claims.
LIVE_SESSION_IDS=$(jq -r -s '
    def pick($snake; $pascal; $default):
      if has($snake) and .[$snake] != null then .[$snake]
      elif has($pascal) and .[$pascal] != null then .[$pascal]
      else $default end;
    .[] | .sessions[]?
    | select(
        (pick("closed"; "Closed"; false) == false)
        and ((pick("state"; "State"; "") | ascii_downcase) != "closed")
      )
    | [
        pick("id"; "ID"; null),
        pick("session_name"; "SessionName"; null),
        pick("alias"; "Alias"; null),
        pick("agent_name"; "AgentName"; null),
        pick("template"; "Template"; null),
        pick("name"; "Name"; null)
      ]
    | .[]
    | select(. != null and . != "")
' "$SESSION_TMP" 2>/dev/null) || exit 0

agent_exists() {
    local candidate="$1"
    [ -n "$candidate" ] && printf '%s\n' "$AGENTS" | grep -Fxq -- "$candidate"
}

live_session_match() {
    local candidate="$1"
    [ -n "$candidate" ] && [ -n "$LIVE_SESSION_IDS" ] \
        && printf '%s\n' "$LIVE_SESSION_IDS" | grep -Fxq -- "$candidate"
}

CURRENT_BEAD_JSON=""

first_bead_jq='if type == "array" then .[0] else . end'

work_bead_still_resettable() {
    local bead_id="$1"
    local expected_assignee="$2"
    local current_status
    local current_assignee

    CURRENT_BEAD_JSON=$(gc bd show "$bead_id" --json 2>/dev/null) || return 1
    current_status=$(printf '%s\n' "$CURRENT_BEAD_JSON" | jq -r "$first_bead_jq | .status // empty" 2>/dev/null) || return 1
    current_assignee=$(printf '%s\n' "$CURRENT_BEAD_JSON" | jq -r "$first_bead_jq | .assignee // empty" 2>/dev/null) || return 1

    [ "$current_status" = "in_progress" ] || return 2
    [ "$current_assignee" = "$expected_assignee" ] || return 2
}

session_bead_candidates() {
    local assignee="$1"
    local work_json="$2"

    if [ -n "$assignee" ]; then
        printf '%s\n' "$assignee"
    fi

    if [[ "$assignee" == *-mc-* ]]; then
        printf 'mc-%s\n' "${assignee##*-mc-}"
    fi

    printf '%s\n' "$work_json" | jq -r "$first_bead_jq | [
        .metadata[\"gc.session_id\"],
        .metadata[\"gc.session_bead_id\"],
        .metadata[\"session_id\"],
        .metadata[\"gc.session_name\"]
    ] | .[]? | select(. != null and . != \"\")" 2>/dev/null || true

    printf '%s\n' "$assignee" | grep -Eo '[[:alnum:]]+-wisp-[[:alnum:]][[:alnum:]-]*$' || true
}

# A pool seat's runtime name comes from CONFIG, not from a session:
# poolRuntimeSessionName is SanitizeQualifiedNameForSession("<agent>[-<slot>]")
# plus the "-pool" suffix (cmd/gc/session_name_lookup.go). That sanitizer
# encodes "/" as "--" (rig-scope) and "." as "__" (pack-qualified city-scope),
# so reconstructing seat names from $AGENTS recognizes both encodings without
# guessing from the separator alone — an ephemeral session name carries a
# per-session token ("...-gc-<token>") and never reconstructs this way, so it
# still resets when it goes missing.
assignee_is_configured_pool_seat() {
    local name="$1"
    case "$name" in
        *-pool) ;;
        *) return 1 ;;
    esac
    [ -n "$AGENTS" ] || return 1

    local seat="${name%-pool}"
    local slotless="${seat%-[0-9]*}"
    local cfg_agent sanitized
    while IFS= read -r cfg_agent; do
        [ -z "$cfg_agent" ] && continue
        sanitized="${cfg_agent//\//--}"
        sanitized="${sanitized//./__}"
        if [ "$sanitized" = "$seat" ] || [ "$sanitized" = "$slotless" ]; then
            return 0
        fi
    done <<<"$AGENTS"
    return 1
}

session_probe_failure_is_unverifiable() {
    local session_id="$1"
    local assignee="$2"

    [ "$session_id" != "$assignee" ] && return 0
    [[ "$session_id" == mc-* ]] && return 0
    # A session-name-shaped self-probe (e.g. "beads--deployer-pool") has
    # nothing else resolvable to ask about -- bead ids never contain a
    # double dash, so this shape means the probe could not be performed,
    # not that the candidate resolved and came back dead.
    [[ "$session_id" == *--* ]] && return 0
    # Same reasoning for a city-scope pool seat, whose name encodes "." as
    # "__" and so carries no double dash. Recognizing only the double-dash
    # form released live city-scope seats mid-work (ga-dei7xx).
    assignee_is_configured_pool_seat "$session_id" && return 0
    return 1
}

session_bead_shows_live_assignment() {
    local assignee="$1"
    local work_json="$2"
    local session_id
    local session_json
    local live_match
    local probe_failed=0

    while IFS= read -r session_id; do
        [ -n "$session_id" ] || continue
        if ! session_json=$(gc bd show "$session_id" --json 2>/dev/null); then
            if session_probe_failure_is_unverifiable "$session_id" "$assignee"; then
                probe_failed=1
            fi
            continue
        fi
        # Any live session bead at a probed candidate preserves the work. The
        # CLI does not expose every Go-side identity field, so the fail-safe
        # direction is to treat candidate liveness as sufficient.
        if ! live_match=$(printf '%s\n' "$session_json" | jq -r --arg assignee "$assignee" --arg session_id "$session_id" "
            $first_bead_jq
            | select((.issue_type // \"\") == \"session\")
            | select((.status // \"\") != \"closed\")
            | ((.metadata.state // \"\") | ascii_downcase) as \$state
            | select(\$state != \"closed\" and \$state != \"orphaned\" and \$state != \"failed-create\" and \$state != \"failed_create\")
            | select(((.metadata.closed // \"\") | tostring | ascii_downcase) != \"true\")
            | select(
                (.id // \"\") == \$assignee
                or (.id // \"\") == \$session_id
                or (.metadata.session_name // \"\") == \$assignee
                or (.metadata.alias // \"\") == \$assignee
                or (.metadata.agent_name // \"\") == \$assignee
                or (\$assignee | contains(\$session_id))
            )
            | .id // empty
        " 2>/dev/null); then
            probe_failed=1
            continue
        fi
        if [ -n "$live_match" ]; then
            return 0
        fi
    done < <(session_bead_candidates "$assignee" "$work_json")

    [ "$probe_failed" -eq 0 ] || return 2
    return 1
}

reset_orphan_if_current() {
    local bead_id="$1"
    local expected_assignee="$2"
    local reset_output
    local reset_state

    reset_output=$(gc bd release-if-current "$bead_id" "$expected_assignee" 2>/dev/null) || return 1
    reset_state=$(printf '%s\n' "$reset_output" | awk 'NF { print $1; exit }')
    case "$reset_state" in
        released)
            if ! gc bd update "$bead_id" --append-notes "orphan-sweep: reset from assignee $expected_assignee -- no live session matched" >/dev/null 2>&1; then
                echo "orphan-sweep: failed to record cause note on $bead_id" >&2
            fi
            return 0
            ;;
        skipped) return 2 ;;
        *) return 1 ;;
    esac
}

# Step 3: Find orphaned beads (assigned to non-existent agents).
# Pool instances use names like "worker-3"; strip the -N suffix to match
# the template name from config.
# AGENT_PUBLIC_FORMS / LOCAL_BINDINGS — the identity forms an assignee may
# legitimately carry for a LOCALLY CONFIGURED agent.
#
# $AGENTS holds Agent.QualifiedName() from `gc config explain`, which is
# BINDING-qualified: "qcore/cherub-law.krieger", "qcore/gastown.polecat",
# "qcore/gc.run-operator". Real assignees are the PUBLIC identity:
# "qcore/krieger". Comparing the two directly reads this city's own named crew
# as foreign — measured, before this was added — so derive every public form:
# the qualified name itself, <rig>/<leaf-after-the-last-dot>, and the bare leaf.
#
# LOCAL_BINDINGS is the set of import bindings this city actually uses
# (cherub-law, gastown, gc, ...). It is what lets a NAMEPOOL instance be
# recognised: `gc config explain` does not emit namepool names at all, so
# "qcore/gastown.furiosa" matches no roster entry — but "gastown" is a binding
# this city imports, and no other city's crew names arrive in that shape.
# Without this, every dead polecat claim would be protected forever and the
# sweep would stop doing its actual job.
AGENT_PUBLIC_FORMS=$(printf '%s\n' "$AGENTS" | awk '
    function emit(v) { if (v != "" && !seen[v]++) { print v } }
    NF {
        emit($0)
        rig = ""; leaf = $0
        i = index($0, "/")
        if (i > 0) { rig = substr($0, 1, i - 1); leaf = substr($0, i + 1) }
        n = leaf
        while (index(n, ".") > 0) { n = substr(n, index(n, ".") + 1) }
        if (rig != "") { emit(rig "/" n) }
        emit(n)
    }' ) || AGENT_PUBLIC_FORMS="$AGENTS"

LOCAL_BINDINGS=$(printf '%s\n' "$AGENTS" | awk '
    NF {
        leaf = $0
        i = index($0, "/")
        if (i > 0) { leaf = substr($0, i + 1) }
        if (index(leaf, ".") > 0) {
            b = substr(leaf, 1, index(leaf, ".") - 1)
            if (b != "" && !seen[b]++) { print b }
        }
    }' ) || LOCAL_BINDINGS=""

# The degraded-roster mode below keys off ROSTER_FROM_FALLBACK, set above at the
# point `gc config show` is actually used. See that comment for why the roster's
# SHAPE cannot stand in for the fact of the fallback.
#
# Why the mode is needed at all: `gc config show` yields only each agent's BARE
# name, so with that roster no rig-qualified assignee can match and EVERY locally
# configured agent is classified foreign, its dead claims protected forever.
# Measured on a simulated fallback roster {worker, polecat, krieger}:
# project/worker, qcore/krieger AND qcore/dalinar were all protected. That is the
# stranding defect this guard was narrowed to avoid, returning through a path
# nobody was looking at.
#
# The population that hits it is precisely the one running older gc — the same
# deployments most likely to install a core-pack fix without updating anything
# else, so the rare-path assumption behind a low severity does not hold here.
#
# ACCEPTED LIMITATION, stated rather than hidden: in that mode the rig
# information does not exist and cannot be reconstructed, so a foreign
# "other-rig/worker" is indistinguishable from a local "local-rig/worker" and is
# released. Strictly better than the alternatives — protecting it strands local
# work on every old deployment, disabling the guard protects nothing — and
# foreign crew names whose leaf is unknown locally (the shape both observed
# incidents took) are still protected.

agent_public_form_exists() {
    local candidate="$1"
    [ -n "$candidate" ] && printf '%s\n' "$AGENT_PUBLIC_FORMS" | grep -Fxq -- "$candidate"
}

local_binding_exists() {
    local candidate="$1"
    [ -n "$candidate" ] && [ -n "$LOCAL_BINDINGS" ] \
        && printf '%s\n' "$LOCAL_BINDINGS" | grep -Fxq -- "$candidate"
}

# strip_instance_suffix — remove a suffix gc mints for a pool instance.
# Two generators exist: poolInstanceName's numeric slot ("-11") and
# GenerateAdhocIdentity ("-adhoc-<token>"). Deliberately a closed grammar: a
# third generator added later would have its claims PROTECTED and reported in
# the per-sweep summary rather than silently stripped, which is the safe
# direction. Add new generators here.
strip_instance_suffix() {
    local name="$1"
    case "$name" in
        *-adhoc-*) printf '%s' "${name%-adhoc-*}"; return 0 ;;
    esac
    local base="${name%-[0-9]*}"
    if [ "$base" != "$name" ]; then printf '%s' "$base"; return 0; fi
    printf '%s' "$name"
}

# is_locally_configured_identity — CONFIG membership ONLY, deliberately with no
# liveness check.
#
# is_known_agent below answers "is this a live/known agent", which conflates two
# different questions. For the foreign-identity guard we need only the first:
# does this city's own configuration DEFINE this identity at all? A locally
# configured agent whose session is dead must still answer YES here, so that its
# claims keep being reset exactly as they are today.
is_locally_configured_identity() {
    local name="$1"
    [ -z "$name" ] && return 1
    if [ "$name" = "human" ]; then return 0; fi
    if agent_public_form_exists "$name"; then return 0; fi

    local stripped
    stripped=$(strip_instance_suffix "$name")
    if [ "$stripped" != "$name" ] && agent_public_form_exists "$stripped"; then return 0; fi

    # Degraded roster (see ROSTER_FROM_FALLBACK): the rig scope was LOST upstream,
    # so match the leaf or every qualified local agent reads as foreign.
    if [ "$ROSTER_FROM_FALLBACK" = "1" ]; then
        local bare="${name##*/}"
        if [ "$bare" != "$name" ]; then
            if agent_public_form_exists "$bare"; then return 0; fi
            local bare_stripped
            bare_stripped=$(strip_instance_suffix "$bare")
            if [ "$bare_stripped" != "$bare" ] && agent_public_form_exists "$bare_stripped"; then return 0; fi
        fi
    fi

    # Binding-qualified leaf: "<rig>/<binding>.<anything>" where <binding> is an
    # import this city uses. Covers namepool instances, which the roster cannot
    # enumerate.
    local leaf="${name##*/}"
    if [ "$leaf" != "$name" ] || [ "${name#*.}" != "$name" ]; then
        case "$leaf" in
            *.*)
                if local_binding_exists "${leaf%%.*}"; then return 0; fi
                ;;
        esac
    fi
    return 1
}

# is_foreign_qualified_identity — a well-formed <rig>/<name> that this city does
# not configure.
#
# THE DEFECT THIS GUARDS. Step 1 above walks EVERY rig explicitly, including rigs
# whose bead store is SHARED with another Gas City. A claim in a shared store is
# visible to every attached city, but the evidence that its owner is alive — this
# city's config and sessions — is not federated. So "doesn't exist in ANY rig",
# which is correct on a single-city store, silently means "an agent I cannot see
# from here" on a shared one, and this sweep resets another city's live claim.
#
# Measured: fifteen beads in the shared qcore store were reset this way between
# 2026-08-21 and 2026-08-24, including workflow children.
#
# The discriminator is the AGENT IDENTITY against the local roster, NOT the rig
# prefix: the shared rig is precisely the one both cities use, so a prefix test
# would protect nothing.
is_foreign_qualified_identity() {
    local name="$1"
    local rig="${name%/*}"
    local leaf="${name##*/}"
    # Not <rig>/<name> at all: bare aliases, session names and ephemeral ids are
    # this city's own naming and were never the cross-city hazard. Leave them on
    # the existing path so local behaviour is unchanged.
    [ "$rig" = "$name" ] && return 1
    [ -z "$rig" ] && return 1
    [ -z "$leaf" ] && return 1
    # WELL-FORMED means exactly one slash and two components drawn from the agent
    # identity grammar — not merely "contains a slash". Without this, anything
    # slash-shaped is protected: "a/b/c", "a//worker", "./worker" and
    # "/tmp/worker" all split into a rig and a dotless leaf and would be treated
    # as another city's crew, stranding malformed LOCAL claims forever. The rig
    # is taken from the LAST slash, so a nested path leaves separators in $rig
    # and is rejected here.
    case "$rig" in
        *[!A-Za-z0-9_-]*) return 1 ;;
    esac
    case "$leaf" in
        *[!A-Za-z0-9_-]*) return 1 ;;
    esac
    # DELIBERATE NARROWING: only a BARE leaf qualifies. "<rig>/<binding>.<name>"
    # is an identity minted by a PACK IMPORT, and a binding this city does not
    # import is ambiguous — it is equally consistent with another city's pack and
    # with a LOCAL import that has since been retired. Protecting that shape
    # would strand a stale local agent's work forever on every single-city
    # deployment, which is the overwhelming majority of them, and this script
    # runs on all of them. The existing contract (a stale qualified local
    # assignee is still cleaned up) is preserved rather than changed under a
    # core pack.
    # "<rig>/<bare-name>" is by contrast a NAMED SESSION public identity — the
    # crew-name shape. A crew name this city does not configure is another
    # city's crew, and that is precisely the shape both observed incidents took
    # (qcore/dalinar, qcore/pattern, qcore/szeth inbound; qcore/barry,
    # qcore/lana outbound).
    # RESIDUAL, stated rather than hidden: a foreign POOL instance
    # ("westeros/gastown.furiosa") is not protected by this. It is already
    # unprotectable by roster membership anyway, because two cities importing the
    # same pack share that identity space verbatim.
    # (A dotted leaf is already rejected by the grammar check above: '.' is not
    # in the permitted character set. Kept explicit in the comment because the
    # NARROWING is a deliberate design choice, not a side effect of the grammar.)
    # Match the FULL <rig>/<name>, never the bare leaf. Stripping the rig would
    # let a foreign "other-rig/worker" match this city's bare "worker" form and
    # fall through to be reset — preserving the very cross-city defect this guard
    # exists to close whenever two cities share a common agent leaf under
    # different rig prefixes. The rig prefix is signal here; it is only useless
    # as a SOLE discriminator (the shared rig is common to both cities).
    if is_locally_configured_identity "$name"; then return 1; fi
    return 0
}

# PROTECTED_TMP accumulates one "<identity>\t<bead>" line per protected claim so
# the skip can be reported ONCE PER SWEEP naming identities.
#
# The skip MUST be observable. A locally DECOMMISSIONED agent is also "not in the
# local roster", so its claims become protected too — correct as a default, but a
# permanent SILENT leak if nobody can see it. Fifty protected claims for an
# identity nobody recognises has to read as a decommissioned agent leaking, from
# the log alone, without knowing to look for it. A silent skip here would be a
# fresh instance of the very class this guard exists to fix.
PROTECTED=0
PROTECTED_TMP=$(mktemp) || PROTECTED_TMP=""
if [ -n "$PROTECTED_TMP" ]; then
    trap 'rm -f "$TMP" "$SESSION_TMP" "$PROTECTED_TMP"' EXIT
fi

record_protected_identity() {
    local identity="$1"
    local bead="$2"
    PROTECTED=$((PROTECTED + 1))
    # A tab or newline inside either value would re-key the grouping or forge an
    # extra record in the summary, which is the ONLY signal that a decommissioned
    # local agent is leaking. Neutralise rather than trust the input: these are
    # read off beads that another city may have written.
    identity=${identity//$'\t'/ }
    identity=${identity//$'\n'/ }
    bead=${bead//$'\t'/ }
    bead=${bead//$'\n'/ }
    if [ -n "$PROTECTED_TMP" ]; then
        printf '%s\t%s\n' "$identity" "$bead" >>"$PROTECTED_TMP" || PROTECTED_TMP=""
    fi
    # MUST return 0: this is called unguarded in the sweep loop, and under
    # `set -e` a function whose last command fails (no temp file, or a failed
    # write) would terminate the entire sweep — turning a degraded summary into
    # a dead patrol.
    return 0
}

is_known_agent() {
    local name="$1"
    # The human operator. "human" is the canonical operator alias across gc
    # (mail alias `human`, `gc bd human <id>`), and beads assigned to the
    # operator are action items for a person, not agent work. The operator
    # is not a configured agent and never has a session — but is also never
    # a dead agent. Without this guard the sweep resets any in_progress
    # human-assigned bead to open/unassigned, silently wiping the operator's
    # claim. Exact match only: agents that merely start with "human" still
    # resolve through the normal paths below.
    if [ "$name" = "human" ]; then return 0; fi
    # Direct match against a configured agent template name.
    if agent_exists "$name"; then return 0; fi
    # Pool instance: strip trailing -<digits> and check template name.
    local base="${name%-[0-9]*}"
    if [ "$base" != "$name" ] && agent_exists "$base"; then return 0; fi
    # City-qualified assignee (gastown.deacon): strip everything through the
    # last dot and re-check. This relies on flattened pack binding chains.
    # Defense-in-depth for older binaries that fall through to `gc config show`
    # and emit unqualified names. Also covers pool patterns like
    # "gastown.dog-3" by re-stripping the -N suffix.
    local short="${name##*.}"
    if [ "$short" != "$name" ]; then
        if agent_exists "$short"; then return 0; fi
        local short_base="${short%-[0-9]*}"
        if [ "$short_base" != "$short" ] && agent_exists "$short_base"; then return 0; fi
    fi
    # Live ephemeral session names like gastown__polekitten-gc-q9j0om won't
    # match any template form — accept them as known when a non-closed session
    # is currently running with a matching ID, SessionName, Alias, or
    # AgentName. Mirrors liveOpenSessionAssignmentExists in the Go path.
    if live_session_match "$name"; then return 0; fi
    # Bare short-form assignee whose canonical agent is LIVE: an assignee like
    # "backend_dev" can be the last-dot segment of a configured qualified agent
    # ("thriva/devpipeline.backend_dev") whose live session is known only by the
    # qualified name. Without this, such an assignee looks like a dead agent and
    # the LIVE owner's in-progress work is reset every cycle. Match the assignee
    # against each configured agent's short form, and accept ONLY when that
    # qualified agent currently has a live session (so genuinely dead-agent work
    # still resets). Mirrors the Go reconciler's multi-identity resolution
    # (sessionBeadAssigneeIdentities / liveOpenSessionAssignmentExists).
    if [ -n "$name" ] && [ -n "$AGENTS" ]; then
        while IFS= read -r _cfg_agent; do
            [ -z "$_cfg_agent" ] && continue
            if [ "${_cfg_agent##*.}" = "$name" ] && live_session_match "$_cfg_agent"; then
                return 0
            fi
        done <<<"$AGENTS"
    fi
    return 1
}

ORPHANED=0
UNVERIFIABLE=0
# Count of undeliverable summaries. Load-bearing for the exit code below.
FAILED=0
# Process substitution (not a pipe) keeps the loop body in the parent
# shell so $ORPHANED survives for the summary message below.
while IFS=$'\t' read -r bead_id assignee; do
    if ! is_known_agent "$assignee"; then
        # Foreign-identity guard, ahead of every liveness and reset step: for an
        # identity this city does not configure, a missing session is
        # UNOBSERVABLE rather than absent, and the checks below cannot tell those
        # apart. Locally configured identities fall through unchanged.
        if is_foreign_qualified_identity "$assignee"; then
            record_protected_identity "$assignee" "$bead_id"
            continue
        fi
        if work_bead_still_resettable "$bead_id" "$assignee"; then
            :
        else
            recheck_status=$?
            if [ "$recheck_status" != "2" ]; then
                UNVERIFIABLE=$((UNVERIFIABLE + 1))
            fi
            continue
        fi
        if session_bead_shows_live_assignment "$assignee" "$CURRENT_BEAD_JSON"; then
            continue
        else
            session_status=$?
            if [ "$session_status" = "2" ]; then
                UNVERIFIABLE=$((UNVERIFIABLE + 1))
                continue
            fi
        fi
        if reset_orphan_if_current "$bead_id" "$assignee"; then
            ORPHANED=$((ORPHANED + 1))
        else
            reset_status=$?
            if [ "$reset_status" != "2" ]; then
                UNVERIFIABLE=$((UNVERIFIABLE + 1))
            fi
        fi
    fi
done < <(echo "$IN_PROGRESS" | jq -r '.[] | select(.assignee != null and .assignee != "") | "\(.id)\t\(.assignee)"' 2>/dev/null)

if [ "$ORPHANED" -gt 0 ] || [ "$UNVERIFIABLE" -gt 0 ]; then
    SUMMARY="orphan-sweep: reset $ORPHANED orphaned beads"
    if [ "$UNVERIFIABLE" -gt 0 ]; then
        SUMMARY="$SUMMARY, skipped $UNVERIFIABLE unverifiable"
    fi
    echo "$SUMMARY"
    if [ "$ORPHANED" -gt 0 ]; then
        if ! gc mail send "$ESCALATION_TARGET" \
            -s "orphan-sweep: reset $ORPHANED orphaned beads" \
            -m "$SUMMARY

orphan-sweep resets in-progress beads whose assignee has no live session or
known agent. Each reset bead now carries a one-line cause note (see its
history). Repeated resets of the same bead may indicate a stuck or
misidentified live session -- inspect via gc bd show <id> --json." \
            2>/dev/null; then
            # Do not swallow an undeliverable summary — a vanished escalation
            # is invisible. Surfacing it requires a non-zero exit (see below).
            echo "orphan-sweep: could not mail escalation target '$ESCALATION_TARGET': $SUMMARY" >&2
            FAILED=$((FAILED + 1))
        fi
    fi
fi

# Loud-fail: the summary has been printed above, so a non-zero exit now
# surfaces the failure line to the controller log without losing the sweep's
# own output. The controller captures an exec order's combined output but logs
# it only on a non-zero exit (order_dispatch.go), so exit 0 would swallow it
# (gastownhall/gascity#4543).
if [ "$FAILED" -gt 0 ]; then
    echo "orphan-sweep: $FAILED escalation summary(ies) could not be delivered to '$ESCALATION_TARGET' (see above)" >&2
    exit 1
fi

# Emitted ONCE PER SWEEP and only when something was protected: at a 5m cadence an
# unconditional line is ~288 entries a day saying nothing, which is how a channel
# stops being read. Identities are sorted so consecutive sweeps are comparable,
# and each carries its claim count plus a bounded sample of bead ids.
if [ "$PROTECTED" -gt 0 ]; then
    # One POSIX awk pass, and deliberately NO external sort/cut/wc: this script
    # runs under a restricted PATH in the pack test harness, where `sort` is not
    # present. Depending on it printed "sort: command not found" to stderr on
    # every run and broke six existing subtests. awk is already a hard dependency
    # of this script; nothing new is assumed.
    PROTECTED_SUMMARY=""
    PROTECTED_SUMMARY_FAILED=0
    if [ -n "$PROTECTED_TMP" ] && [ -s "$PROTECTED_TMP" ]; then
        if ! PROTECTED_SUMMARY=$(awk -F'\t' '
            {
                if (!($1 in count)) { keys[++nkeys] = $1 }
                count[$1]++
                if (count[$1] <= 5) { ids[$1] = (ids[$1] == "" ? $2 : ids[$1] ", " $2) }
            }
            END {
                # Insertion sort over the key list so consecutive sweeps render
                # comparably. asorti() is a gawk extension and absent from the BSD
                # awk on macOS, where this order runs on the operator box.
                for (i = 2; i <= nkeys; i++) {
                    k = keys[i]
                    for (j = i - 1; j >= 1 && keys[j] > k; j--) { keys[j + 1] = keys[j] }
                    keys[j + 1] = k
                }
                out = ""
                for (i = 1; i <= nkeys; i++) {
                    k = keys[i]
                    extra = (count[k] > 5) ? sprintf(" +%d more", count[k] - 5) : ""
                    entry = sprintf("%s (%d: %s%s)", k, count[k], ids[k], extra)
                    out = (out == "" ? entry : out ", " entry)
                }
                printf "%d\t%s", nkeys, out
            }' "$PROTECTED_TMP"); then
            PROTECTED_SUMMARY_FAILED=1
            PROTECTED_SUMMARY=""
        fi
    elif [ "$PROTECTED" -gt 0 ]; then
        # Claims were protected but nothing reached the spool — the temp file was
        # unavailable or a write failed. That is a failure, not a zero.
        PROTECTED_SUMMARY_FAILED=1
    fi
    IDENTITY_COUNT="${PROTECTED_SUMMARY%%	*}"
    PROTECTED_DETAIL="${PROTECTED_SUMMARY#*	}"
    case "$IDENTITY_COUNT" in
        ''|*[!0-9]*) IDENTITY_COUNT=0; PROTECTED_DETAIL="" ;;
    esac
    # A FAILED summary must never render as "0 identities". The count is the only
    # evidence that a decommissioned local agent is accumulating protected
    # claims, so reporting zero when the aggregation broke would suppress exactly
    # the signal this mechanism exists to produce — and it would contradict the
    # claim count printed beside it. Say it is unavailable instead. awk's stderr
    # is deliberately NOT discarded so the cause is visible.
    if [ "$PROTECTED_SUMMARY_FAILED" = "1" ]; then
        echo "orphan-sweep: protected $PROTECTED claims this pass, but the per-identity summary is UNAVAILABLE (aggregation failed; see stderr above) — identities NOT listed"
    fi
    if [ "$PROTECTED_SUMMARY_FAILED" != "1" ]; then
        if [ -n "$PROTECTED_DETAIL" ]; then
            echo "orphan-sweep: protected $IDENTITY_COUNT foreign/unknown identities this pass ($PROTECTED claims): $PROTECTED_DETAIL"
        else
            echo "orphan-sweep: protected $IDENTITY_COUNT foreign/unknown identities this pass ($PROTECTED claims)"
        fi
    fi
fi
