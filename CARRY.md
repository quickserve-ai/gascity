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
| Beads schema pin | see "Beads pin" below | Since 2026-09-01 `go.mod`'s beads line **equals upstream main's** (`bf97b73749ac`, schema v59); the row stays as the fleet-contract pointer, not as a divergence. | Absorbed 2026-09-01 — the drop condition ("upstream's line unmodified") is met; keep the line equal to upstream's pin at every rebase. |
| Cross-city mail (upstream #5386, ga-d755oq) | `3a8c37580` (fork PR #3) | `gc --context <peer> mail send / reply / inbox` routed to a REMOTE city over the control plane (`cmd/gc/mail_remote.go`, `api.Client.SendMail/ReplyMail/ListMailInboxPage`). Originally carried on this box as `84acc9794` cherry-picked from upstream `cdcd0611c1` (gastownhall/gascity PR #5386). That local copy proved BYTE-IDENTICAL to fork-PR #3's `3a8c37580` — same `git patch-id` `087346b84633ab5aeaee0340744f14b7745b8b53` — and was dropped as a duplicate when the deploy lineage was reconciled onto `origin/carry/operational` (ga-33s83a). The distinction an earlier draft of this row drew between the two copies is not a real one. | Upstream merges #5386 — the range-diff then absorbs it; take upstream's line unmodified. |
| Dolt endpoint identity & credential projection (ga-3qvmjj, ga-uurd84, ga-298g8t, ga-tmhxnd, qc-ow3u50, gc-49ho, hq-mbe2s) | 5b70f17f4, deb7cda61, de62dbae1, 848f5fad2, ed608d79d, e3e944674, 709ad6944, 01e0660fd, bd87ef4be | Ambient Dolt identity travels with its endpoint — declined on a provable host/port mismatch (`doltauth.AmbientIdentityAppliesTo`), in the resolver and on the direct-dial city-store paths; an inferred endpoint must not overwrite or veto a declared one; a remote store keeps its own port; rig-init projects the scope endpoint credentials; the post-init catalog verify resolves the rig scope's declared endpoint, not the city's (a remote-hub rig in a managed-local city). Adapting the pre-`ga-3qvmjj` contract made `TestOrderRunExecPreservesAuthOnlyOverridesForManagedLocal` the branch's known-red test — resolved in `709ad6944`, split into `TestOrderRunExecDeclinesForeignEndpointIdentityForManagedLocal` + `…PreservesBareAuthOverridesForManagedLocal`; the new `GC_DOLT_MANAGED_LOCAL` env read is goldened in `01e0660fd`. | Upstream absorbs the ambient-identity endpoint binding + the scope-aware init verify. |

## Beads pin — the fleet contract

The native store *is* the beads library linked into `gc`, so `go.mod`'s beads
requirement decides the highest Dolt schema version a `gc` binary can open. Our
city databases are migrated by `bd`, and a `gc` pinned **below** the schema `bd`
has written trips beads' schema-skew gate: native-store selection falls back to
the exec store and everything keeps working, slower and differently. Nothing
turns red. That is why the pin is a written contract rather than a preference.

**Current pin — one line, three places that must agree:**

| Where | Value |
|---|---|
| `go.mod` require | `github.com/steveyegge/beads v1.1.1-0.20260805093327-bf97b73749ac` |
| Upstream commit | `bf97b73749ac` on `gastownhall/beads` main (2026-08-05), schema **v59** — the revision upstream `gastownhall/gascity` main pins |
| Every machine's `bd version` | starts `v1.1.1-0.20260805093327-bf97b73749ac`; a carry build appends its identity, e.g. `(bf97b73+carry.8f7471e: carry-v59/be-qfm-be-4at@8f7471e01238)` — the label itself must stay the pin (gc's version_compat gate) |

The pin is a pseudo-version rather than a tag because the fleet pins **whatever
upstream `gastownhall/gascity` main pins at window time** — on-support, never
ahead, never waiting on an announcement — and upstream main pins this commit
(`qc-bridge` `shared/fork-upstream-operating-principles.v1.md`, principle 3).
History: the previous pin was `v1.1.1-0.20260716185344-67652d8b5caf` (schema
v54, 2026-07-16 to 2026-09-01); the sections below that measure v54 stores
are dated and kept as the record of that period.

**Every machine gets v54 from `go build` alone.** That revision is published on
the public Go module proxy and covered by `go.sum`, so a plain checkout builds
the right beads on every laptop, on westeros, and in CI with no per-machine
setup. Do not reintroduce a filesystem-path replace to get it: carry commit
`74407adde` pinned `replace github.com/steveyegge/beads => /Users/cherub/beads-src`,
which resolved on exactly one box and left every other machine silently building
the v1.1.0 require pin — a v53 binary against v54 stores. That replace also put
the carry branch in violation of the repo's own required `make
check-gomod-replace` gate, which blocks local-path replaces by policy; the
require-pin shape passes it. `scripts/beads_module_pin_test.go` covers the half
that gate does not — drift in the `require` line itself — and `.gitignore` keeps
`go.work` out of the tree so a local override cannot be committed by accident.

Note this is a different axis from `deps.env`'s `BD_VERSION`, which pins the bd
*release tarball* the container image and the minimum-supported contract cell
install, and can only name a published tag — `TestBDVersionPins` owns that one,
and it stays where upstream left it. The general CI path no longer rides that
axis at all: since ga-yl326d every `setup-gascity-*` job builds `bd` from this
same `go.mod` pin via `.github/scripts/install-bd-lockstep.sh`, so the CLI
cannot skew from the linked library. It used to — CI ran gc at schema v59
beside the `v1.1.0` tarball at v53, and `bd create` refused the store.

**Moving the pin moves the fleet.** Bump `go.mod`, the `beadsFleetPin` constant
in that test, and the table above together, and redeploy `bd` on every machine
in the same window — a `gc` that migrates a city DB past what the other
machines' `bd` knows produces the same skew from the opposite side. The move
from v54 to v59 happened 2026-09-01: the alex laptop, the westeros box and the
q-core hub advanced in one window (deliberate hub migration past the
migration-interlock remote); Cherub's hq/as stores complete it in their
2026-09-02 unfork window, which also ends the forked 0054 (dropped, not
renumbered). The next move is when upstream gascity main's pin moves.

**Working against a local beads checkout** (patching beads and gascity together)
is the one case for an override. Use an **untracked** `go.work` at the repo
root — Go's own mechanism for exactly this, and unlike a `replace` it cannot be
committed:

```sh
# from the gascity repo root; writes go.work, which .gitignore keeps untracked
go work init . /path/to/your/beads     # e.g. ~/code/beads, checked out at the pin
```

```go
// go.work — untracked, per-machine, never committed
go 1.26.5

use (
	.
	/path/to/your/beads
)
```

While that file exists you are no longer building the fleet pin — you are
building whatever revision that checkout happens to be on — and `go build` will
not say so. `go list -m github.com/steveyegge/beads` reports the local directory
instead of the pinned pseudo-version; that is the check. Delete `go.work` before
producing a deploy candidate.

### Correction: schema slot 0054 is forked (ga-grjijl, measured 2026-08-21)

**The section above assumes there is exactly one migration numbered 0054. There
are two, and this city's databases do not all carry upstream's.** Everything
below is measured, not inferred; the recipe to re-measure is at the end.

`schema_migrations` stores `(version, content_hash)` where `content_hash` is the
sha256 of that migration's `.up.sql`. Two different migrations occupy slot 0054:

| | file | sha256 of `.up.sql` | adds |
|---|---|---|---|
| carry | `0054_add_gc_route_index.up.sql` | `05085d4c…d66cc` | `issues.gc_routed_to_hash` + its index |
| upstream `67652d8b5caf` | `0054_add_lease_columns.up.sql` | `2e51058b…1680` | `lease_expires_at`, `heartbeat_at`, `row_lock` |

Control: version **53** hashes identically on both sides
(`f13909f0…cbcb330`), which proves the hashing method and isolates the fork to
slot 0054 exactly.

State of this city's live databases (dolt `127.0.0.1:51361`, 2026-08-21 15:40 PDT):

| DB | recorded v54 hash | lease columns | `gc_routed_to_hash` |
|---|---|---|---|
| `hq` (city beads: every `ga-` bead, all mail, all wisps) | carry `05085d4c` | **0** | 1 |
| `qcore` | upstream `2e51058b` | 3 | 1 |
| `as` | carry `05085d4c` | **0** | 1 |

**Consequence for the pin above.** In beads `67652d8b5caf` the lease columns are
not dormant: `internal/storage/issueops/update.go` appends `row_lock = ?` to the
SET list of *every* mutating path, unconditionally, outside any branch — the
comment there calls it "the 'every mutating path writes row_lock' invariant".
A `gc` built from the committed pin, writing any bead in `hq`, therefore emits
`UPDATE issues SET …, row_lock = ? WHERE id = ?` against a table with no such
column. Verified read-only against the live server:

    hq:    select row_lock from issues limit 1
           -> Error 1105: column "row_lock" could not be found in any table in scope
    qcore: select row_lock from issues limit 1  -> 0        (control: probe is valid)
    hq:    select gc_routed_to_hash from issues limit 1 -> NULL  (control: query path works)

So the sentence above — "a `gc` pinned **below** the schema `bd` has written
trips beads' schema-skew gate … everything keeps working, slower and
differently" — does not describe this city. There is no graceful degradation
here: `hq` writes fail outright.

**`qcore` is a second, quieter problem.** It physically carries *both* 0054s'
columns but its ledger records only upstream's hash. Carry's 0054 probes
`INFORMATION_SCHEMA` before altering, so it applied its column and left no
ledger row. In `qcore`, `(version, content_hash)` no longer describes the
physical schema — and it is wrong in the reassuring direction.

**What this box does about it, until ga-grjijl is resolved.** The committed
`go.mod` pin stays exactly as upstream set it — it is the fleet contract and it
passes `check-gomod-replace` and `TestBeadsModulePin`. This machine holds the
carry beads revision through the untracked per-machine `go.work` override that
`.gitignore` already contemplates, so builds in `~/gascity-src` link the beads
whose 0054 matches `hq` and `as`. Do **not** "fix" a build here by deleting
`go.work` to match the contract; that is the hazard, not the cure.

**The real fix is a fleet `bd` upgrade, not a `go.mod` edit** — rebase the beads
carry commits onto `67652d8b5caf`, renumber the route-index migration off slot
0054, migrate `hq`/`as`, reconcile `qcore`'s ledger, and redeploy `bd`
everywhere in one window. Tracked on ga-grjijl.

Re-measure before acting; other agents write these databases:

    gc dolt sql -q "use hq; select version, content_hash from schema_migrations where version=54"
    gc dolt sql -q "select count(*) from information_schema.columns where table_schema='hq' and table_name='issues' and column_name in ('lease_expires_at','heartbeat_at','row_lock')"
    shasum -a 256 /Users/cherub/beads-src/internal/storage/schema/migrations/0054_add_gc_route_index.up.sql

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
for whatever you touched. Before building, confirm the beads pin the build
will actually resolve — `go list -m github.com/steveyegge/beads` — matches
installed `bd version`, or the schema-skew gate disables the native store.
Reading `go.mod` is not the same check: an untracked `go.work` overrides it
silently (see "Beads pin" above).


Install — the candidate doctor and atomic swap are one fail-closed shell block.
Any blocking doctor failure aborts before staging. Stage on the same filesystem;
the supervisor holds the binary open, so never `cp` over it:

```sh
set -e
/tmp/gc-new --city ~/gascity doctor --check-timeout 5m
cp /tmp/gc-new ~/go/bin/gc.staged
cp ~/go/bin/gc ~/go/bin/gc.bak-$(date +%Y%m%d-%H%M)
mv ~/go/bin/gc.staged ~/go/bin/gc        # single atomic replacement; ~/.local/bin/gc symlinks here
gc version > /tmp/gcver 2>&1; echo "exit=$? out=$(cat /tmp/gcver)"   # VERIFY before refreshing
gc supervisor install                    # re-registers the launchd job; NOT kickstart
launchctl list | grep gascity            # middle column is LAST EXIT: -9 means SIGKILL
```

**Refresh the supervisor with `gc supervisor install`, never with a bare
`launchctl kickstart -k` (ga-4v3ckk).** A plain kickstart reuses the existing
job registration, and launchd caches that job's launch constraints against the
binary present when it was registered. After an atomic swap the inode is new,
the cached constraint no longer matches, and macOS SIGKILLs the process on
launch as a `CODESIGNING / Launch Constraint Violation`. That is the ga-l8pur
failure mode — a ~4-hour city-wide outage on 2026-08-03, fired again
2026-08-17. The binary being adhoc/linker-signed is normal and is NOT the
cause; do not "fix" it by re-signing.

`gc supervisor install` re-registers instead of reusing: plist preflight, write,
`launchctl bootout` / `bootstrap` / `enable` / `kickstart -p`, then it polls to
confirm the new build ID is actually serving and rolls back if it is not. The
same sequence is `restartSupervisor` in cmd/gc/drift.go, whose comment says it
outright — "a plain kickstart retains launchd's cached launch constraints
across binary swaps".

Read the last-exit column before and after: `launchctl list | grep gascity`
prints `<pid> <last-exit> com.gascity.supervisor`, and a `-9` means the job's
most recent launch was SIGKILLed. The hazard stays armed even when a later
retry happens to survive, which is how it went unnoticed for weeks.

Verify **before** the refresh, and never through a pipe: `gc version | head;
echo $?` reports head's status and prints `0` for a binary SIGKILLed before it
wrote a byte. A bad swap reads `exit=137` with empty output — refreshing onto
one takes the city down.

The refresh re-adopts (does not respawn) running tmux sessions — long-lived
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

