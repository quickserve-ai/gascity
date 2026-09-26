#!/usr/bin/env bash
#
# test-mol-scoped-work-teardown.sh — behaviour tests for mol-scoped-work's
# cleanup-worktree step (ga-6xehc5, pl-4k0).
#
# The step is agent-executed shell inside a TOML basic string, so this test
# runs the RENDERED text: it parses the formula with python3 tomllib, cuts the
# bash block out of the cleanup-worktree step, substitutes {{convoy_id}}, and
# runs it with a stub `gc` first on PATH. Reading the raw file would test the
# wrong program: TOML rewrites escapes and trailing-backslash line joins.
#
# The stub records every call and answers from per-case state files, so each
# case asserts what the block DID: which gc commands ran, what it wrote to the
# bead, and whether the tree survived. The rescue and removal themselves are
# gc worktree rescue/teardown, tested in internal/worktree; this pins the
# decisions the formula makes around them.
#
# The invariant under test: the step never secures or removes a worktree while
# its work bead is open with a live convoy and the tree holds anything, and a
# status it cannot read counts as live.
#   - open bead + live convoy + non-empty tree => declined: no rescue, no
#     teardown, work_dir kept, one comment, one event, a DECLINED line
#   - a repeat decline for the same bead and tree => still one comment
#   - an unreadable tree, or a failed comment lookup => still declined
#   - closed bead, or finished convoy => rescue, then teardown (control)
#   - open + live + empty or absent tree => proceeds to rescue
#   - failed bead or convoy read, or an unreadable status => stops before
#     rescue, work_dir kept
#
# Runs under bash, and under zsh when it is installed (agents run formula
# steps in either). Needs python3 >= 3.11 and jq.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FORMULA="${FORMULA:-$TEST_DIR/../internal/bootstrap/packs/core/formulas/mol-scoped-work.toml}"

for tool in python3 jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "SKIP: $tool not installed" >&2; exit 0; }
done
python3 -c 'import tomllib' 2>/dev/null || { echo "SKIP: python3 has no tomllib (needs >= 3.11)" >&2; exit 0; }

WORK="$(mktemp -d)"
trap 'chmod -R u+rwx "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

BLOCK="$WORK/cleanup-block.sh"
python3 - "$FORMULA" "$BLOCK" <<'PY' || { echo "FAIL: could not extract the cleanup-worktree block from $FORMULA" >&2; exit 1; }
import sys, tomllib
formula, out = sys.argv[1], sys.argv[2]
with open(formula, "rb") as f:
    data = tomllib.load(f)
steps = [s for s in data.get("steps", []) if s.get("id") == "cleanup-worktree"]
if len(steps) != 1:
    sys.exit(f"expected exactly one cleanup-worktree step, found {len(steps)}")
desc = steps[0]["description"]
start = desc.index("```bash\n") + len("```bash\n")
end = desc.index("\n```", start)
# The block runs after the shell's startup files, which may put a real gc
# ahead of the stub on PATH; set PATH inside the script and refuse to run
# unless gc resolves to the stub, so no case can reach a live city.
header = (
    'PATH="$STUB_BIN:$PATH"\n'
    '[ "$(command -v gc)" = "$STUB_BIN/gc" ] || { echo "stub gc is not first on PATH" >&2; exit 97; }\n'
)
with open(out, "w") as f:
    f.write(header + desc[start:end].replace("{{convoy_id}}", "c-1") + "\n")
PY

# The stub gc. Every call is appended to calls.log; reads answer from the
# state files, and a <name>.fail file makes that read exit non-zero.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/gc" <<'STUB'
#!/usr/bin/env bash
S="$STUB_STATE"
printf '%s\n' "$*" >> "$S/calls.log"
flag() { # flag <name> <args...>: print the value after --<name>
  local name="$1"; shift
  while [ $# -gt 0 ]; do
    [ "$1" = "--$name" ] && { printf '%s' "$2"; return; }
    shift
  done
}
case "$1 $2" in
  "convoy status") [ -e "$S/convoy.fail" ] && exit 1; cat "$S/convoy.json" ;;
  "bd show") [ -e "$S/bead.fail" ] && exit 1; cat "$S/bead.json" ;;
  "bd comments") [ -e "$S/comments.fail" ] && exit 1; cat "$S/comments.json" ;;
  "bd comment")
    jq --arg t "$4" '. + [{text: $t}]' "$S/comments.json" > "$S/tmp" && mv -f "$S/tmp" "$S/comments.json" ;;
  "bd update")
    shift 3
    while [ $# -gt 0 ]; do
      case "$1" in
        --set-metadata)
          jq --arg k "${2%%=*}" --arg v "${2#*=}" '.[0].metadata[$k] = $v' "$S/bead.json" > "$S/tmp" && mv -f "$S/tmp" "$S/bead.json"
          shift 2 ;;
        --unset-metadata)
          jq --arg k "$2" '.[0].metadata |= del(.[$k])' "$S/bead.json" > "$S/tmp" && mv -f "$S/tmp" "$S/bead.json"
          shift 2 ;;
        *) shift ;;
      esac
    done ;;
  "event emit") printf '%s\n' "$3" >> "$S/events.log" ;;
  "worktree rescue")
    if [ -e "$(flag path "$@")" ]; then
      echo '{"absent":false,"rescue_ref":"refs/rescue/t-1","rescue_sha":"abc123","repo":"/repo","taint":[]}'
    else
      echo '{"absent":true}'
    fi ;;
  "worktree teardown") rm -rf "$(flag path "$@")" ;;
  *) echo "stub gc: unexpected call: $*" >&2; exit 2 ;;
esac
STUB
chmod +x "$WORK/bin/gc"
export STUB_BIN="$WORK/bin"

failures=0
fail() { echo "FAIL [$SHELL_UNDER_TEST] $CASE: $*" >&2; failures=$((failures + 1)); }

# new_case <name> <bead-status> <convoy-status> <tree: full|empty|absent>
# builds the state for one run: a work bead t-1 whose work_dir is $TREE, and
# a convoy c-1 tracking it.
new_case() {
  STUB_STATE="$WORK/$SHELL_UNDER_TEST-$1"
  export STUB_STATE
  mkdir -p "$STUB_STATE"
  TREE="$STUB_STATE/wt"
  case "$4" in
    full) mkdir -p "$TREE" && echo wip > "$TREE/edit.go" ;;
    empty) mkdir -p "$TREE" ;;
    absent) ;;
  esac
  jq -n --arg s "$2" --arg wd "$TREE" '[{id: "t-1", status: $s, metadata: {work_dir: $wd}}]' > "$STUB_STATE/bead.json"
  jq -n --arg s "$3" --arg bs "$2" '{convoy: {id: "c-1", status: $s}, children: [{id: "t-1", status: $bs}]}' > "$STUB_STATE/convoy.json"
  echo '[]' > "$STUB_STATE/comments.json"
  : > "$STUB_STATE/calls.log"
  : > "$STUB_STATE/events.log"
}

run_block() {
  "$SHELL_UNDER_TEST" "$BLOCK" > "$STUB_STATE/out" 2> "$STUB_STATE/err"
  local rc=$?
  [ "$rc" -ne 97 ] || { echo "ABORT: stub gc is not first on PATH under $SHELL_UNDER_TEST" >&2; exit 1; }
  return "$rc"
}

called() { grep -q "^$1" "$STUB_STATE/calls.log"; }
work_dir() { jq -r '.[0].metadata.work_dir // ""' "$STUB_STATE/bead.json"; }
comments() { jq 'length' "$STUB_STATE/comments.json"; }
events() { wc -l < "$STUB_STATE/events.log" | tr -d ' '; }

DECLINED_LINE="DECLINED: work bead t-1 open with live convoy c-1; worktree preserved"

# assert_declined <run exit status> <want events>: the step stopped with the
# DECLINED line and touched nothing.
assert_declined() {
  [ "$1" -ne 0 ] || fail "block exited 0; a decline must stop the step"
  grep -qxF "$DECLINED_LINE" "$STUB_STATE/err" || fail "no DECLINED line on stderr: {$(cat "$STUB_STATE/err")}"
  assert_untouched
  [ "$(events)" = "$2" ] || fail "events = $(events), want $2"
  if [ -s "$STUB_STATE/events.log" ] && grep -vqx "mol-scoped-work.cleanup-declined" "$STUB_STATE/events.log"; then
    fail "unexpected event type: {$(tr '\n' ' ' < "$STUB_STATE/events.log")}"
  fi
}

# assert_untouched: nothing secured, nothing removed, the pointer kept.
assert_untouched() {
  called "worktree rescue" && fail "rescue ran"
  called "worktree teardown" && fail "teardown ran"
  [ -e "$TREE" ] || fail "the worktree was removed"
  [ "$(work_dir)" = "$TREE" ] || fail "work_dir = {$(work_dir)}, want $TREE"
}

# assert_unobserved: a read failed, so the step stops untouched and says
# nothing about a state it did not see: no comment, no event, no DECLINED.
assert_unobserved() {
  assert_untouched
  [ "$(comments)" = 0 ] || fail "commented without reading the state"
  [ "$(events)" = 0 ] || fail "emitted a decline event without reading the state"
  grep -q "^DECLINED:" "$STUB_STATE/err" && fail "reported DECLINED without reading the state"
}

# assert_torn_down <run exit status>: rescue, record, teardown, clear.
assert_torn_down() {
  [ "$1" -eq 0 ] || fail "block exited $1: {$(cat "$STUB_STATE/err")}"
  called "worktree rescue" || fail "rescue did not run"
  called "worktree teardown" || fail "teardown did not run"
  [ "$(grep -n "^worktree" "$STUB_STATE/calls.log" | cut -d' ' -f2 | tr '\n' ' ')" = "rescue teardown " ] ||
    fail "worktree calls out of order: {$(grep "^worktree" "$STUB_STATE/calls.log" | tr '\n' ';')}"
  [ -e "$TREE" ] && fail "the worktree survived teardown"
  [ "$(jq -r '.[0].metadata.rescue_sha // ""' "$STUB_STATE/bead.json")" = "abc123" ] || fail "rescue_sha not recorded"
  [ -z "$(work_dir)" ] || fail "work_dir not cleared"
  [ "$(comments)" = 0 ] || fail "commented on a teardown"
  [ "$(events)" = 0 ] || fail "emitted a decline event on a teardown"
}

shells="bash"
command -v zsh >/dev/null 2>&1 && shells="bash zsh"

for SHELL_UNDER_TEST in $shells; do
  CASE="open bead, live convoy, non-empty tree"
  new_case declined in_progress open full
  run_block; rc=$?
  assert_declined "$rc" 1
  [ "$(comments)" = 1 ] || fail "comments = $(comments), want 1"
  note=$(jq -r '.[0].text // ""' "$STUB_STATE/comments.json")
  for want in t-1 c-1 "$TREE"; do
    case "$note" in *"$want"*) ;; *) fail "comment does not name $want: {$note}" ;; esac
  done

  CASE="repeat decline for the same bead and tree"
  new_case repeat open open full
  run_block; run_block; rc=$?
  assert_declined "$rc" 2
  [ "$(comments)" = 1 ] || fail "comments = $(comments) after two declines, want 1"

  CASE="unreadable tree"
  new_case unreadable in_progress open full
  chmod 000 "$TREE"
  if ls -A "$TREE" >/dev/null 2>&1; then
    echo "SKIP [$SHELL_UNDER_TEST] $CASE: permissions are not enforced for this user" >&2
  else
    run_block; rc=$?
    assert_declined "$rc" 1
  fi
  chmod 755 "$TREE"

  CASE="comment lookup fails"
  new_case lookup in_progress open full
  : > "$STUB_STATE/comments.fail"
  run_block; rc=$?
  assert_declined "$rc" 1
  called "bd comment " && fail "commented without knowing whether the comment already exists"

  CASE="closed bead (control)"
  new_case closed-bead closed open full
  run_block; rc=$?
  assert_torn_down "$rc"

  CASE="finished convoy (control)"
  new_case closed-convoy in_progress closed full
  run_block; rc=$?
  assert_torn_down "$rc"

  CASE="open bead, live convoy, empty tree"
  new_case empty in_progress open empty
  run_block; rc=$?
  assert_torn_down "$rc"

  CASE="open bead, live convoy, absent tree"
  new_case absent in_progress open absent
  run_block; rc=$?
  [ "$rc" -eq 0 ] || fail "block exited $rc: {$(cat "$STUB_STATE/err")}"
  called "worktree rescue" || fail "rescue did not decide absence"
  called "worktree teardown" && fail "teardown ran on an absent tree"
  [ -z "$(work_dir)" ] || fail "work_dir not cleared for an absent tree"
  [ "$(comments)" = 0 ] || fail "commented on an absent tree"

  for failing in bead convoy; do
    CASE="failed $failing read"
    new_case "fail-$failing" in_progress open full
    : > "$STUB_STATE/$failing.fail"
    run_block; rc=$?
    [ "$rc" -ne 0 ] || fail "block exited 0"
    assert_unobserved
  done

  CASE="bead status unreadable"
  new_case no-bead-status in_progress open full
  jq '.[0] |= del(.status)' "$STUB_STATE/bead.json" > "$STUB_STATE/tmp" && mv -f "$STUB_STATE/tmp" "$STUB_STATE/bead.json"
  run_block; rc=$?
  [ "$rc" -ne 0 ] || fail "block exited 0"
  assert_unobserved

  CASE="convoy status unreadable"
  new_case no-convoy-status in_progress open full
  jq '.convoy |= del(.status)' "$STUB_STATE/convoy.json" > "$STUB_STATE/tmp" && mv -f "$STUB_STATE/tmp" "$STUB_STATE/convoy.json"
  run_block; rc=$?
  [ "$rc" -ne 0 ] || fail "block exited 0"
  assert_unobserved
done

if [ "$failures" -ne 0 ]; then
  echo "test-mol-scoped-work-teardown: $failures failure(s) (shells: $shells)" >&2
  exit 1
fi
echo "test-mol-scoped-work-teardown: all cases passed (shells: $shells)"
