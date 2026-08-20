# CARRY.md — quickserve-ai/gascity carry model (Cherub's town)

Town-local operational layer on top of the **gas-city-carry** doctrine
(pack `gas-city-operations`, skill `gas-city-carry`, adopted via qc-bridge
PR #12). The doctrine governs judgment (branch contracts, rebase evidence
procedure, contribution hygiene); this file records what is specific to
this fork and this deployment. When they conflict, the doctrine wins on
procedure; Git wins on state.

## Branch state

| Branch | Contract | Current reality (2026-08-02) |
|---|---|---|
| `main` | Mirrors `gastownhall/gascity` main. No carry patches. | Refresh both the fork ref (`git push origin upstream/main:main`) and the local branch (`git branch -f main upstream/main`) after a verified carry deploy. |
| `carry/operational` | Deploy lineage and fork default branch. Local deployment work tracks origin. | Rebased onto `upstream/main` under ga-xyuer. The stack is now the explicit commits from `git log --no-merges upstream/main..carry/operational`, not a release-lineage replay. |
| `v1.3.5-platform` | Historical release-lineage name. | Retired; never use as a replay boundary. |
| `archive/*` tags | Rollback pins + pre-reconciliation tips. | `archive/carry-operational-pre-20260802-upgrade` is the rollback pin for this upstream-main rebase. Before every future carry rebase/reset, tag and push the current tip first. |

Remotes convention (verify with `git remote -v`, never assume):
`origin` = `quickserve-ai/gascity` (shared mirror — carry branch lives here),
`upstream` = `gastownhall/gascity`. Upstream contribution branches are cut
from fetched `upstream/main` and pushed to a **personal** fork, never to
`origin` (see doctrine; ga-prk6 tracks the upstreaming flow).

## Tweak ledger — the carry stack above upstream main

Authoritative enumeration: `git log --no-merges upstream/main..carry/operational`.
Every entry is an intentional divergence; a rebase that silently drops one is a
regression. Commit cells below name the current rebased commits. "Drop when" is
the doctrine's absorption evidence, not a guess — prove with patch-id/range-diff
and the behavior check.

| Cluster | Commits | What / why | Drop when |
|---|---|---|---|
| Claim-chain performance (ga-kl6, ga-2s6k) | e9be5c902, 652b599e9 | Work-query deadline 30s→90s; bounded ephemeral in-progress probe pushdown. | Upstream lands the metadata index and the chain measures sub-30s again. |
| Interactive color (ga-od2) | 601bd0aad, ad1dcda2b | FORCE_COLOR=3 for interactive tmux sessions plus coverage. | Upstream ships equivalent interactive color handling. |
| Mail durability (ga-1kor) | 470b0f457 | 24h handoff TTL; mark-unread reopens closed mail. | Upstream absorbs both halves. |
| Reconciler safety (ga-1xiv, ga-kaei) | d82969adb | Worktree prune guards + heartbeat drain cooldown. | Upstream equivalent. |
| Skill fingerprinting (ga-rpf2) | e6d5658b7 | Gitignore-aware skill hashing. | Upstream equivalent. |
| Session identity & resume (ga-e4jb, ga-oe8h, ga-lqk3) | d0a01ea09, e942f8e96, **01ac00df4** | Named-session shadow fix; history/resume; Claude hook-stdin session IDs. The last remains the critical wake-resume carry. | Upstream drops the Codex-only hook-stdin gate. |
| Dispatch correctness (ga-d4rb, ga-1pql, ga-frf3, ga-xcqw) | 7eecacd36, f30c6575f, e002c63ec, daee627dd | Adopt pre-assigned work, same-formula convoy dedupe, merge-strategy metadata, timer hygiene, singleton target canonicalization. | Upstream equivalent behavior and tests. |
| Guardrails (ga-sc80, ga-14a/ga-80ij) | 2f78ea92b, cc943b76b | Refuse closing beads with confirmed-unmerged PRs; fragment resolution fail-loud. | Upstream equivalents. |
| Test hygiene (ga-utvl) | 25322586c | Reap test-city tmux servers in TestMain. | Upstream equivalent. |
| Dolt/OMP packs (qc-lu207, ga-3zxvr) | fe074dc53, 2ae8a3fb2, 6e45ddd39, 1e38d8078, 7e0667d48, abcf2b639, 26576784b | Push-success freshness, separate local/mirror health, managed gc in OMP PATH, worst-case 1800s mirror health. | Upstream pack release absorbs each behavior. |
| Assignee identity (ga-i44k) | 062ab7220, 1309ae48d | Alias identity claims and gc-bd assignee canonicalization. | Upstream unified identity resolution. |
| Formula scope (ga-96qs) | 7029c9314 | Formula scope honors GC_RIG while preserving explicit --city. | Upstream equivalent. |
| Reconciler crash policy (ga-9n5hj, ga-2aq43) | 10d1de02b | Lazy FPExtra drift, two-per-tick stagger, stale reset healing. Regression suite: `cmd/gc/session_reconciler_crash_policy_test.go`. | Upstream passes this suite unmodified. |
| Drift-wave mail | 87fef6410 | Durable coordinator notice once per wave. | Upstream equivalent notify path. |
| Context guidance | e6ac0111a, f5c53048e, 138186d31, 9ddae51b3 | Launch-model stamping and current Claude model mapping. | Upstream authoritative launch metadata/context recognition. |
| Stopped named-home reaper (ga-ijun) | 4cf480760, 4507a0369, 1455a2841, 556af06ef, 4ece728bc | Opt-in, fail-closed named/namepool home reclamation with typed session snapshots and non-force git removal. | Upstream ships equivalent stopped-home lifecycle cleanup. |
| Doctor live-rig classification (ga-w7xo) | 5886594b1 | Prevent a live rig DB on the managed endpoint from being labeled orphan. | Upstream equivalent topology classification. |
| Navigator contract | 73d410d71, 0cdd157f7 | Stable navigator classification fields and regenerated API/dashboard clients. | Upstream exposes the same wire contract. |
| Bundled pack pin | 6b747120d, fe59ba7c0 | Canonical core/bd pin names a real carry commit containing current embedded pack content. | Re-pin whenever current carry pack content changes. |
| Cross-city mail (upstream #5386 cherry-pick, ga-d755oq) | 84acc9794 | `gc --context <peer> mail send / reply / inbox` routed to a REMOTE city over the control plane (`cmd/gc/mail_remote.go`, `api.Client.SendMail/ReplyMail/ListMailInboxPage`). Carried as the single upstream commit `cdcd0611c1` (gastownhall/gascity PR #5386, still OPEN at carry time) per Cherub/Alex-town direction — deliberately NOT the fork-PR #3 copy (`3a8c37580`) and NOT fork-PR #4's portable beads pin, which ride together on `origin/carry/operational`. Only conflict: reply help text (kept our `--notify` wording, appended the remote paragraph). | Upstream merges #5386 — the range-diff then absorbs it; take upstream's line unmodified. |

## Deploy recipe

Build (the icu4c include must be on **CGO_CXXFLAGS** — it's a C++ compile):

```sh
cd ~/gascity-src   # on carry/operational
ICU=/opt/homebrew/opt/icu4c@78
export CGO_CFLAGS="-I$ICU/include" CGO_CXXFLAGS="-I$ICU/include" CGO_LDFLAGS="-L$ICU/lib"
go build -o /tmp/gc-new ./cmd/gc          # ~2-3 min
```

Gate: the repo's own AGENTS.md is authoritative and mandates more than the
cmd/gc suite — `go vet ./...`, the `.githooks/pre-commit` hook, `make test`
(or `make test-fast-parallel`), and `make dashboard-check` when API or
dashboard surfaces changed. Minimum for a code deploy: `go vet ./...` +
full `go test ./cmd/gc/ -timeout 35m` (same CGO env; 17–19 min — run it
detached, never as a foreground tool call) + the targeted package suites
for whatever you touched. Before building, confirm `go.mod`'s beads pin
matches installed `bd version` or the version-compat gate disables the
native store.


Install — the candidate doctor and atomic swap are one fail-closed shell block.
Any blocking doctor failure aborts before staging. Stage on the same filesystem;
the supervisor holds the binary open, so never `cp` over it:

```sh
set -e
/tmp/gc-new --city ~/gascity doctor --check-timeout 5m
cp /tmp/gc-new ~/go/bin/gc.staged
cp ~/go/bin/gc ~/go/bin/gc.bak-$(date +%Y%m%d-%H%M)
mv ~/go/bin/gc.staged ~/go/bin/gc        # single atomic replacement; ~/.local/bin/gc symlinks here
gc version > /tmp/gcver 2>&1; echo "exit=$? out=$(cat /tmp/gcver)"   # VERIFY before kickstart
launchctl kickstart -k gui/$(id -u)/com.gascity.supervisor
```

Verify **before** the kickstart, and never through a pipe: `gc version | head;
echo $?` reports head's status and prints `0` for a binary SIGKILLed before it
wrote a byte. A bad swap reads `exit=137` with empty output — kickstarting onto
one takes the city down.

Kickstart re-adopts (does not respawn) running tmux sessions — long-lived
sessions keep the old binary until individually cycled; fresh subprocess
paths (`gc bd`, `gc hook --claim`) pick the new binary up immediately.
For a change that must reach long-lived sessions (tmux/runtime behavior),
inventory them (`gc session list`) and cycle each deliberately
(`tmux -L gastown kill-session -t <sess>` → supervisor respawns on the new
binary); until then the town intentionally runs mixed versions — roll back
by restoring the `gc.bak-*` binary if the mix misbehaves. **Roll back by
rename, never `cp`**: `cp ~/go/bin/gc.bak-<stamp> ~/go/bin/gc.rollback && mv -f
~/go/bin/gc.rollback ~/go/bin/gc`, then verify unpiped. Copying a backup
*directly* over the live path writes into the running inode — during ga-l8pur
that made a known-good binary exit 137 anyway.
Verify the full effective configuration loads cleanly before trusting narrower
commands: run `gc config show >/dev/null`, then `gc doctor`, a by-ID `gc bd show <id>`,
and `gc status`. The config check catches transient pack-lock/cache skew after
an import or binary transition; any failure triggers rollback rather than a
retrying mixed-version window. Finally, grep the binary for a string distinctive
to the change and push `carry/operational` to `origin` so the deployed lineage
has an off-box backup.

## Rebase cadence

Refresh from upstream only in a coordinated maintenance window. Capture the
expected origin OID and push a rollback tag before rewriting. Never use a stale
release tag as the replay boundary. Compute the boundary from Git:

```sh
old_base=$(git merge-base upstream/main carry/operational)
git tag archive/carry-operational-pre-$(date +%Y%m%d)
git push origin refs/tags/archive/carry-operational-pre-$(date +%Y%m%d)
git rebase --onto upstream/main "$old_base" carry/operational
```

Absorption is judged per commit. After every rebase, walk this ledger top to
bottom: each behavior either survives in `upstream/main..carry/operational` or
has recorded patch-id/range-diff plus behavioral absorption evidence.

