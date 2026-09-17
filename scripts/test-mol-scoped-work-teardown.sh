#!/usr/bin/env bash
#
# test-mol-scoped-work-teardown.sh — behaviour tests for mol-scoped-work's
# cleanup-worktree secure step (ga-6xehc5).
#
# The step is agent-executed shell inside a TOML basic string, so this test
# runs the RENDERED text: it parses the formula with python3 tomllib, cuts the
# secure block out of the cleanup step, and runs it against a scratch bare
# origin plus clone. Reading the raw file would test the wrong program: TOML
# rewrites `\0`, `\n` and trailing-backslash line joins.
#
# The invariant under test: teardown never destroys the only copy of work,
# and never commits gc's own materializations into the work branch.
#   - sediment only (manifest-recorded skill symlinks, the ownership manifest,
#     a city-synced skill copy, AGENTS-gc.md, .worktree-stale) => no commit,
#     no push
#   - tracked edits under .claude/ and .beads/ => committed and pushed
#   - a new untracked skill, odd filenames => committed and pushed
#   - an authored symlink not in the manifest, and a city-path file whose
#     content differs => committed and pushed
#   - staged rename and deletion => committed and pushed
#   - an unpushed local commit on a clean tree => pushed
#
# Runs under bash, and under zsh when it is installed (agents run formula
# steps in either). Needs git, python3 >= 3.11, jq.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FORMULA="${FORMULA:-$TEST_DIR/../internal/bootstrap/packs/core/formulas/mol-scoped-work.toml}"

for tool in git python3 jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "SKIP: $tool not installed" >&2; exit 0; }
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

BLOCK="$WORK/secure-block.sh"
python3 - "$FORMULA" "$BLOCK" <<'PY' || { echo "FAIL: could not extract the secure block from $FORMULA" >&2; exit 1; }
import sys, tomllib
formula, out = sys.argv[1], sys.argv[2]
with open(formula, "rb") as f:
    data = tomllib.load(f)
steps = [s for s in data.get("steps", []) if "SECURE-BEFORE-DESTROY" in s.get("description", "")]
if len(steps) != 1:
    sys.exit(f"expected exactly one SECURE-BEFORE-DESTROY step, found {len(steps)}")
lines = steps[0]["description"].splitlines()
start = lines.index("  (")
end = next(i for i in range(start, len(lines)) if lines[i].startswith("  ) || {"))
with open(out, "w") as f:
    f.write("\n".join(lines[start:end]) + "\n  )\n")
PY

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export WORK_BEAD_ID=t-1

mkdir -p "$WORK/ext/skill" "$WORK/city/.claude/skills/search-sessions"
echo "city skill" > "$WORK/city/.claude/skills/search-sessions/SKILL.md"
export GC_CITY_PATH="$WORK/city"

failures=0
fail() { echo "FAIL [$SHELL_UNDER_TEST] $CASE: $*" >&2; failures=$((failures + 1)); }

# new_tree builds origin + a clone on branch "work" (already pushed), with
# tracked files under .claude/ and .beads/, then drops in gc sediment.
new_tree() {
  local d="$WORK/$1"
  git init -q --bare "$d/origin.git"
  git clone -q "$d/origin.git" "$d/wt" 2>/dev/null
  cd "$d/wt" || exit 1
  git checkout -q -b work
  mkdir -p .claude/skills/real .beads/formulas
  echo v1 > .claude/skills/real/SKILL.md
  echo f > .beads/formulas/f.toml
  echo base > README
  echo keep > old.txt
  git add -A && git commit -q -m base
  git push -q origin work 2>/dev/null
  git fetch -q origin
  ln -s "$WORK/ext/skill" .claude/skills/pack.one
  ln -s "$WORK/ext/skill" .claude/skills/pack.two
  printf '{"targets":{"pack.one":"x","pack.two":"y"}}' > .claude/skills/.gc-skill-ownership.json
  mkdir -p .claude/skills/search-sessions
  cp "$GC_CITY_PATH/.claude/skills/search-sessions/SKILL.md" .claude/skills/search-sessions/SKILL.md
  echo a > AGENTS-gc.md
  : > .worktree-stale
  export WORKTREE="$d/wt"
}

run_block() { "$SHELL_UNDER_TEST" "$BLOCK" >/dev/null 2>&1; }
head_sha() { git rev-parse HEAD; }
origin_sha() { git -C ../origin.git rev-parse work; }
leftover() { git status --porcelain -uall | cut -c4- | sort | tr '\n' ' '; }

SEDIMENT_LEFT=".claude/skills/.gc-skill-ownership.json .claude/skills/pack.one .claude/skills/pack.two .claude/skills/search-sessions/SKILL.md .worktree-stale AGENTS-gc.md "

assert_secured() {
  [ "$(head_sha)" != "$1" ] || fail "expected a rescue commit, HEAD did not move"
  [ "$(origin_sha)" = "$(head_sha)" ] || fail "origin/work != HEAD after teardown"
  [ "$(leftover)" = "$2" ] || fail "leftover = {$(leftover)}, want {$2}"
}

shells="bash"
command -v zsh >/dev/null 2>&1 && shells="bash zsh"

for SHELL_UNDER_TEST in $shells; do
  CASE="sediment only"
  new_tree "$SHELL_UNDER_TEST-sediment"; base=$(head_sha)
  run_block || fail "block exited non-zero"
  [ "$(head_sha)" = "$base" ] || fail "committed sediment: $(git show --name-only --format= HEAD | tr '\n' ' ')"
  [ "$(origin_sha)" = "$base" ] || fail "pushed to origin"
  [ "$(leftover)" = "$SEDIMENT_LEFT" ] || fail "leftover = {$(leftover)}"

  CASE="tracked .claude and .beads edits"
  new_tree "$SHELL_UNDER_TEST-tracked"; base=$(head_sha)
  echo v2 > .claude/skills/real/SKILL.md
  echo g >> .beads/formulas/f.toml
  run_block || fail "block exited non-zero"
  assert_secured "$base" "$SEDIMENT_LEFT"

  CASE="new skill and odd filenames"
  new_tree "$SHELL_UNDER_TEST-new"; base=$(head_sha)
  mkdir -p .claude/skills/newskill
  echo n > .claude/skills/newskill/SKILL.md
  echo x > "sp ace.go"
  echo y > "$(printf 'n\303\274.go')"
  run_block || fail "block exited non-zero"
  assert_secured "$base" "$SEDIMENT_LEFT"

  CASE="authored symlink and city-path file with different content"
  new_tree "$SHELL_UNDER_TEST-authored"; base=$(head_sha)
  ln -s ../../README .claude/skills/authored-link
  echo changed > .claude/skills/search-sessions/SKILL.md
  run_block || fail "block exited non-zero"
  assert_secured "$base" ".claude/skills/.gc-skill-ownership.json .claude/skills/pack.one .claude/skills/pack.two .worktree-stale AGENTS-gc.md "
  git show --name-only --format= HEAD | grep -qx ".claude/skills/authored-link" || fail "authored symlink not committed"

  CASE="staged rename and deletion"
  new_tree "$SHELL_UNDER_TEST-rename"; base=$(head_sha)
  git mv old.txt new.txt
  git rm -q README
  run_block || fail "block exited non-zero"
  assert_secured "$base" "$SEDIMENT_LEFT"

  CASE="unpushed commit on a sediment-only tree"
  new_tree "$SHELL_UNDER_TEST-unpushed"
  echo local > local.go && git add local.go && git commit -q -m local
  local_sha=$(head_sha)
  run_block || fail "block exited non-zero"
  [ "$(head_sha)" = "$local_sha" ] || fail "sediment was committed on top of the local commit"
  [ "$(origin_sha)" = "$local_sha" ] || fail "local commit not pushed"
done

if [ "$failures" -ne 0 ]; then
  echo "test-mol-scoped-work-teardown: $failures failure(s) (shells: $shells)" >&2
  exit 1
fi
echo "test-mol-scoped-work-teardown: all cases passed (shells: $shells)"
