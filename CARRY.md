# CARRY.md — quickserve-ai/gascity carry model (Cherub's town)

Town-local operational layer on top of the **gas-city-carry** doctrine
(pack `gas-city-operations`, skill `gas-city-carry`, adopted via qc-bridge
PR #12). The doctrine governs judgment (branch contracts, rebase evidence
procedure, contribution hygiene); this file records what is specific to
this fork and this deployment. When they conflict, the doctrine wins on
procedure; Git wins on state.

## Branch state

| Branch | Contract | Current reality (2026-07-18) |
|---|---|---|
| `main` | Mirrors `gastownhall/gascity` main. No carry patches. | Mirror. Refresh BOTH the fork ref (`git push origin upstream/main:main`) and the local branch (`git branch -f main upstream/main`) — a stale local `main` misleads anyone who checks it out. |
| `carry/operational` | Deploy lineage. **Fork default branch** (verify: `gh api repos/quickserve-ai/gascity --jq .default_branch`; keep local `origin/HEAD` matching via `git remote set-head origin carry/operational`). Local work happens on the local `carry/operational` branch tracking origin. | **Transitional shape**: upstream v1.3.5 release lineage + the 29-commit carry stack below — NOT yet the doctrine's "upstream/main + small stack". ga-65wz migrates the base to upstream main (~775 commits ahead at time of writing) using the doctrine's fetch/lease procedure. |
| `v1.3.5-platform` | Historical name of the same lineage. | Kept in sync with `carry/operational` until ga-65wz lands, then retire. |
| `archive/*` tags | Rollback pins + pre-reconciliation tips. | `archive/carry-operational-549e6094d` = the old fork carry branch (its 5 live tmux fixes were ported 2026-07-18, rest verified subsumed — ga-8bfv). **Before every carry rebase or reset, tag the current tip `archive/carry-operational-pre-<yyyymmdd>` and push the tag — that is the rollback checkpoint.** |

Remotes convention (verify with `git remote -v`, never assume):
`origin` = `quickserve-ai/gascity` (shared mirror — carry branch lives here),
`upstream` = `gastownhall/gascity`. Upstream contribution branches are cut
from fetched `upstream/main` and pushed to a **personal** fork, never to
`origin` (see doctrine; ga-prk6 tracks the upstreaming flow).

## Tweak ledger — the carry stack above upstream v1.3.5

Authoritative enumeration: `git log --no-merges v1.3.5..carry/operational`
(29 commits at time of writing). Every entry is an intentional divergence;
a rebase that silently drops one is a regression. Grouped by concern,
oldest first. "Drop when" = the doctrine's absorption evidence, not a
guess — prove with `git patch-id` / `git range-diff` + behavior check.

| Cluster | Commits | What / why | Drop when |
|---|---|---|---|
| Claim-chain performance (ga-kl6, ga-2s6k) | d716a5acf, 6dbf590ad | Work-query deadline 30s→90s; bounded ephemeral in-progress probe pushdown. Un-indexed metadata scans blew the 30s deadline and stranded pool workers. | Upstream lands the beads-side metadata index and the chain measures sub-30s again. |
| Interactive color (ga-od2 + ported carry tmux cluster) | 9cec8e828, 8fa2ae1f8, 4001cf97f, 83efd7ff8, d93c22bdb | FORCE_COLOR=3 default for interactive tmux sessions; `env -u CI -u NO_COLOR` wrapper for claude/codex spawns + adapted tests. | Upstream ships equivalent interactive color handling. |
| Handoff/tmux server lifetime | ac5d81267, 074c4e40b, 0992ead18 | ConfigureServer safe to re-run (wrapper outlives server on self-handoff; once-guard killed the shared server) + regression tests. | Upstream removes the once-guard or restructures ConfigureServer equivalently. |
| Mail durability (ga-1kor) | a56693d01 | 24h sweep TTL for handoff mail; mark-unread reopens closed messages. Handoffs were swept before the successor primed. | Upstream absorbs both halves (TTL + reopen). |
| Reconciler safety (ga-1xiv, ga-kaei) | 93b1a7679 | Worktree prune guards + heartbeat drain cooldown. | Upstream equivalent. |
| Skill fingerprinting (ga-rpf2) | bed3cd24d | Gitignore-aware skill hashing — artifacts can't self-trigger drift drains. | Upstream equivalent. |
| Session identity & resume (ga-e4jb, ga-oe8h, ga-lqk3) | 6adc08447, b2c39ee13, **bb12a1c01** | Named-session shadowing fix; `gc session history/resume`; **hook-stdin session ids accepted for claude — upstream still gates this codex-only. Wake-resume is a no-op for every claude agent without it. THE critical re-apply-on-rebase commit.** | bb12a1c01: upstream drops the codex-only gate. Others: upstream equivalent. |
| Dispatch correctness (ga-d4rb, ga-1pql, ga-frf3, ga-xcqw) | a4bb29247, db64567c3, 29351dfaa | Duplicate-worker dispatch kill (adopt pre-assigned work); per-rig default_merge_strategy metadata; bd slow-timer stop. | Upstream equivalents. |
| Guardrails (ga-sc80, ga-14a/ga-80ij) | 0a4f62cd2, c676bbd49 | Refuse closing beads with confirmed-unmerged PRs; fragment resolution fail-loud. | Upstream equivalents. |
| Provider/model surface (ga-qn6m) | 4398be5ac | gpt-5.6-sol in the codex model enum. | Upstream enum catches up. |
| Test hygiene (ga-utvl) | 667a75424 | Reap test-city tmux servers in TestMain; stale-root sweep. | Upstream equivalent. |
| Dolt/OMP pack carries (qc-lu207) | f4128e404, 354ba4cac, 997b8a563, 9d0353651 | Backup freshness fails closed on push-success stamps; managed gc preferred in OMP PATH; bundled pack pins carrying both. | gascity-packs upstream releases with the fixes; re-pin instead of carrying. |
| Assignee identity (ga-i44k) | 6d6c33382, 68dcb8150 | Claim verb writes the alias identity (not session name); `gc bd` canonicalizes hand-written assignees + warns on unknown shapes. Non-canonical assignees are invisible to find-work and silently strand P1s. | Upstream lands unified identity resolution (the ga-xweq class). |

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

Install — stage on the same filesystem, back up, **atomic rename** (the
supervisor holds the binary open; never `cp` over it):

```sh
cp /tmp/gc-new ~/go/bin/gc.staged
mv ~/go/bin/gc ~/go/bin/gc.bak-$(date +%Y%m%d-%H%M)
mv ~/go/bin/gc.staged ~/go/bin/gc        # ~/.local/bin/gc symlinks here
launchctl kickstart -k gui/$(id -u)/com.gascity.supervisor
```

Kickstart re-adopts (does not respawn) running tmux sessions — long-lived
sessions keep the old binary until individually cycled; fresh subprocess
paths (`gc bd`, `gc hook --claim`) pick the new binary up immediately.
For a change that must reach long-lived sessions (tmux/runtime behavior),
inventory them (`gc session list`) and cycle each deliberately
(`tmux -L gastown kill-session -t <sess>` → supervisor respawns on the new
binary); until then the town intentionally runs mixed versions — roll back
by restoring the `gc.bak-*` binary if the mix misbehaves.
Verify: `gc doctor`, a by-ID bead read, and grep the binary for a string
distinctive to the change you shipped. Push `carry/operational` to
`origin` after every deploy so the deployed lineage always has an off-box
backup.

## Rebase cadence

Refresh from upstream during quiet maintenance windows only, coordinating
both towns, exactly per the doctrine's procedure (explicit refspec fetch,
expected-OID capture, `--force-with-lease`, range-diff evidence per
dropped patch). First catch-up is ga-65wz.

**Replay boundary — do not use a plain `git rebase upstream/main`.** The
merge-base with upstream sits 79 release-lineage commits BELOW the
`v1.3.5` tag, so a naive rebase replays 108 commits, not the 29-commit
carry stack. Replay exactly the stack:

```sh
git tag archive/carry-operational-pre-$(date +%Y%m%d)   # rollback pin, push it
git rebase --onto upstream/main v1.3.5 carry/operational
```

Absorption is judged **per commit, not per cluster**: a multi-commit
cluster (mail durability; the 4-commit Dolt/OMP row) may be partially
absorbed upstream — keep the unabsorbed commits, drop the absorbed ones,
and record per-commit evidence (`git patch-id` / `git range-diff` + the
behavior check) on the cluster's bead. After every rebase, walk this
ledger top to bottom — every commit either survives visibly in
`git log v<new-base>..carry/operational` or has recorded absorption
evidence on its bead.
