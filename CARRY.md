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
| CI bd lockstep install (ga-yl326d) | 2fee7e6bd, 8dbd02a3c | `.github/scripts/install-bd-lockstep.sh` + both `setup-gascity-*` actions build CI's `bd` from go.mod's beads pin (version-stamped, tool-cached) instead of a separately-pinned tarball; deletes the actions' `bd-version` input and its 28 workflow call sites. Broad workflow-file conflict surface at rebase. | Upstream derives CI's bd from go.mod equivalently (or the affected workflows/actions are themselves absorbed); prove by reading upstream's setup path, then take upstream's line. |
| Silent-work detector (ga-krso22 / ga-mmvpq1 Half B; ga-vh6cbz) | b31cbba15, 6eec3c4de, ce2a6cb48, 58d672f28 | Published-work-waiting-on-nobody sweep: bead-arm gate + comment; orphan-arm mail with a delivered-only, scope-keyed 24h latch and loud-fail duration knobs. | Drop when upstream's core pack ships an equivalent detector (both arms, mail latched) or the order is retired. |
| Cross-city mail (upstream #5386, ga-d755oq) | `3a8c37580` (fork PR #3) | `gc --context <peer> mail send / reply / inbox` routed to a REMOTE city over the control plane (`cmd/gc/mail_remote.go`, `api.Client.SendMail/ReplyMail/ListMailInboxPage`). Originally carried on this box as `84acc9794` cherry-picked from upstream `cdcd0611c1` (gastownhall/gascity PR #5386). That local copy proved BYTE-IDENTICAL to fork-PR #3's `3a8c37580` — same `git patch-id` `087346b84633ab5aeaee0340744f14b7745b8b53` — and was dropped as a duplicate when the deploy lineage was reconciled onto `origin/carry/operational` (ga-33s83a). The distinction an earlier draft of this row drew between the two copies is not a real one. | Upstream merges #5386 — the range-diff then absorbs it; take upstream's line unmodified. |
| Dolt endpoint identity & credential projection (ga-3qvmjj, ga-uurd84, ga-298g8t, ga-tmhxnd, qc-ow3u50, gc-49ho, hq-mbe2s) | 5b70f17f4, deb7cda61, de62dbae1, 848f5fad2, ed608d79d, e3e944674, 709ad6944, 01e0660fd, bd87ef4be | Ambient Dolt identity travels with its endpoint — declined on a provable host/port mismatch (`doltauth.AmbientIdentityAppliesTo`), in the resolver and on the direct-dial city-store paths; an inferred endpoint must not overwrite or veto a declared one; a remote store keeps its own port; rig-init projects the scope endpoint credentials; the post-init catalog verify resolves the rig scope's declared endpoint, not the city's (a remote-hub rig in a managed-local city). Adapting the pre-`ga-3qvmjj` contract made `TestOrderRunExecPreservesAuthOnlyOverridesForManagedLocal` the branch's known-red test — resolved in `709ad6944`, split into `TestOrderRunExecDeclinesForeignEndpointIdentityForManagedLocal` + `…PreservesBareAuthOverridesForManagedLocal`; the new `GC_DOLT_MANAGED_LOCAL` env read is goldened in `01e0660fd`. | Upstream absorbs the ambient-identity endpoint binding + the scope-aware init verify. |
| Reconciler drain keep-alive (gc-mdh0; Overseer directive 2026-09-08; upstream #5731/#5473) | c02b7fe7c (= upstream PR #6168, cherry-pick -x of b3bd73102) | A live pool seat that still holds assigned open/in_progress work is not drained by the reconciler's own timers (no-wake-reason / idle-sleep / config-suppressed); explicit intents still drain; probe error retains. Stops the review plane's mid-molecule drains at the root. | Absorbed when gastownhall #6168 merges — drop at the next rebase (patch-id equal). |
| Routed claim tier priority order (qc-04ff7.98.1, gc-19zc; upstream #5629) | dddd9eade | routedReadyTierCommand no longer pins `--sort oldest`: the pool claim tier rides bd's canonical (priority, created_at, id) order so a routed P0 is served ahead of older P1/P2 rows (the review-expedite lift was unread at dequeue). Adapted by intent from upstream #5629 (9bef3024d), goldens regenerated. Operator direction 2026-09-08. | Absorbed when gastownhall #5629 merges — drop at the next rebase (take upstream's line). |
| Reconciler orphan drain vs heartbeat hold (gc-hzj5, gc-mdh0 symptom 2; syl qc-2x2jk3; upstream #6173) | e6c49e254 (= upstream PR branch 1d26598da, cherry-pick -x; two hunks adapted: carry lacks maxSessionAgeBlockerInfo, and carry's sessionHasOpenAssignedWorkForConfigInfo signature) | A pool seat with a live heartbeat hold (held_until ahead, no sleep_intent) is no longer drained as 'orphaned' when it falls out of the desired set: the not-desired arm, the drain-ack branch and the Phase 2 drain scan honor the hold through a hold cancel lens, ahead of the partial-store deferral; provenance is positive-only (an agent, unknown or unreadable ack source leaves the drain alone); suspend/named/dead-seat paths untouched. Stops the second review-plane drain class at the root. | Absorbed when the gastownhall PR for #6173 merges — drop at the next rebase. |
| Reconciler ack ownership — the reconciler's drain ack under its own keys (gc-hzj5; Cherub strict-review finding on #6178) | 74baf90d1 + e2c5231aa + 533bcfa78 (= upstream PR branch 112dab235 + f1a5cd0ce + 070fbd1fb, cherry-pick -x; 070fbd1fb's regression file adapted: its producer-side test, which targets upstream's request-aware reusablePoolSessionInfosForRequest and protectedPoolSessionBeadAt, neither carried here, stays upstream-only; the three review-finding regressions are verbatim) | The reconciler publishes its ack as source/reason/generation only and never writes GC_DRAIN_ACK (the agent's key); isDrainAcked reads either shape; the hold's tracked and recovered cancels clear only the reconciler's keys and remove GC_DRAIN alone, so an agent ack landing at any interleaving survives and outranks the hold (closes the check-then-act race found on 1d26598da). A failed ack publication cleans up through the own-only clear too (f1a5cd0ce, the second Cherub finding), and the regression tests pin the surviving ack honored end to end (stop-pending, runtime stopped, seat closed). Every other clear keeps its full effect. 070fbd1fb (the Cherub final strict review's three findings, cleared at that head): the hold's cancel leaves GC_DRAIN alone so an explicit drain survives it, the own-only clear re-asserts source=agent whenever GC_DRAIN_ACK reads 1, and the desired ack arm cancels a reconciler-owned ack on a heartbeat-held seat and keeps the seat. | Absorbed with e6c49e254 when the gastownhall PR for #6173 (#6178) merges — drop at the next rebase. |
| Native Dolt update retry on a lost serialization race (syl qc-04ff7.81.23, upstream #6177 RCA) | f5bd61a37 (= upstream 60b5c19f8, PR #5229, cherry-pick -x) | NativeDoltStore.Update wraps retryOnNativeDoltSerializationConflict; an order-fired mint no longer turns Error 1213 into molecule_failed on the first activation write. | Pure upstream pick — drops by patch-id at the next rebase. |
| Native Dolt Close/Reopen retry on serialization conflicts | 751ccdaf2 (= upstream d7e1c3aea, PR #5272, cherry-pick -x) | The Close/Reopen twin of the Update retry. | Pure upstream pick — drops by patch-id at the next rebase. |
| Dispatch vars reach formula validation and instantiation (gc-kxyr / gc-irxm; syl; the box ran this since 09-01 on its local deploy/1b72f9c7e-plus-orderdispatch line) | 98b27548c (= syl 8847e9c7d from the box repo, cherry-pick -x, clean apply on c64976bed) | `gc order run` and the controller's order dispatch pass the delivered vars (webhook args / `--var`) as `molecule.Options{Vars}` to `ValidateRecipeRuntimeVars` and `molecule.Instantiate`; with `Options{}` compile's `{{var}}` placeholders were reported missing and formula defaults poured in place of the delivered values. Seam test `order_dispatch_seam_test.go`. Upstream has the equivalent (`order_dispatch.go:2271` passes `Options{Vars: effectiveVars}` at ae5163dd3) — cadence drift, not a divergence. | Drop at the next rebase once upstream's equivalent is in the base (patch-id differs; verify by behavior: the seam test). |
| The supervisor's per-city loader applies the daemon feature flags (gc-irxm; syl qc-04ff7.109; upstream #6236) | 3a145e88a (= A3Ackerman fix/supervisor-loader-feature-flags 1dc450087, cherry-pick -x, resolved on cc6dcb7b1 for the trace-line hunk) + 01a7b5556 (= 1a02bd04c, the follow-up on syl's Astra pass, clean apply on c09e712a7) | `loadCityConfigWithBuiltinPacks` calls `applyFeatureFlags(cfg)` after validation, as `loadCityConfigFS` does for the CLI (the first pick applied it before `validatePackRuntimeRegistrations`, so a rejected city rewrote the process-global flags; the follow-up moves it and pins the order with a rejected-load test). Without it the flags stayed at their zero value in the supervisor process until the startup config reload or an API request applied them — the startup reload does so on a normal boot (`applyStartupConfigReload` → `tryReloadConfig` → `applyFeatureFlags`, unless that reload fails and keeps the old config); API-server construction (`api.New` → `syncFeatureFlags`) is lazy, on the first request — so this closes a contract gap in the loader rather than an established cause of the sequential controller mints on westeros; the two `GC_SLING_TRACE` lines at `molecule.Instantiate`'s constructor choice and its sequential fallthrough are how a mint's eligibility and path are read in the field. Decided by execution: the loader test fails on cc6dcb7b1 and on stock ae5163dd3 alike. | Drop when the gastownhall PR for #6236 merges (patch-id differs by the resolved hunk; verify by `TestLoadCityConfigWithBuiltinPacks*`). |
| Route recovery leaves fenced graph steps alone (gc-sey7; syl qc-04ff7.109.12; upstream #6221) | 43912d27d (= A3Ackerman fix/route-recovery-skips-fenced-steps 14d8fbb0c, cherry-pick -x on 0f37e0710; the regression test adapted to this branch's pre-lane recoverUnroutedWorkRoutes) | `carriedPoolRoute` yields no route for a bead carrying `gc.instantiating` or `gc.deferred_routed_to`, or a ready-excluded type: a graph-first mint withholds the route behind the instantiation fence and activation restores it, so the backstop no longer stamps the formula's bare pool name (an unclaimable route) onto a fenced entry step — the writer that, riding the native store's read-merge-write (#6222), lost 15 review roots in 10 days on westeros. | Drop when the gastownhall PR for #6221 merges (patch-id equal at the next rebase). |
| Native Dolt metadata merge is a compare-and-swap (gc-sey7; syl qc-04ff7.109.12; upstream #6222) | 0f191c7f2 (= A3Ackerman fix/native-store-metadata-checked-merge 6639fb216, cherry-pick -x, clean apply on cc6dcb7b1; a carry(test) companion commit adds the storage spy's `updateIssueChecked` hook that the picked tests drive — upstream's spy already has it) | `NativeDoltStore.SetMetadataBatch` (and `SetMetadata`) writes the merged map back with `UpdateIssueChecked` carrying the read's `RowVersion` as `ExpectedVersion`, and re-runs the whole read-merge-write on `ErrVersionMismatch` (`retryOnNativeDoltMergeRace`, the serialization-conflict retry's budget), so an `Update` that commits between the read and the write is merged onto instead of replaced by the stale map; the standalone update's audit event is kept (no transaction). The lost update that re-fenced a review root's entry step with a bare route on westeros; protects every metadata stamp that races an update on the same bead. Not picked: upstream #5099's `error 1205` classifier line — the library's own retry covers 1205 on the server backend; its own row if ever needed. | Drop when the gastownhall PR for #6222 merges (patch-id equal at the next rebase). |
| City-imported rig-scoped orders fan out per rig (gc-8mb7; syl qc-04ff7.87.5 / qc-bridge #298; upstream #6218) | 87bd931b6 (= A3Ackerman fix/order-scope-rig-on-city-import ee4b9bf74, cherry-pick -x, clean apply on 0f37e0710) | `Order.IsRigScoped`; `orderdiscovery.ScanAll` expands a city import's `scope = "rig"` orders once per `[[rigs]]` entry with `Rig` stamped and drops the city copy (a rig that imports the pack itself keeps its own instance; unscoped orders unchanged; Env/Params cloned per copy). A city-imported pack's rig-scoped orders fire per rig instead of never; the per-rig `Rig` stamp is what #34's suspended-rig check attributes on. | Drop when the gastownhall PR for #6218 merges (patch-id equal at the next rebase). |
| A failed mint stamps its own root and names what it could not confirm (gc-qum2; syl qc-04ff7.109.14; upstream #6235) | be916c30e (= A3Ackerman fix/molecule-failed-root-stamp f3457d6f7, cherry-pick -x, resolved on 734753329 for the two `validateResidualRoutingVars` sites carry lacks — #5060; 15 sites here, 17 upstream) | `markFailed`/`markFailedReporting` share a two-pass stamper: a bead whose `molecule_failed` stamp fails is retried once after every other bead is stamped, and the id-qualified errors of the stamps that still failed are returned or joined (`errors.Join`) instead of dropped or truncated to the first; every mark-then-return site in `Instantiate`/`InstantiateFragment` goes through `failInstantiation`, which appends `(failure stamp not confirmed: <bead: fault>; …)` to the error, and the order dispatcher logs that error before recording `OrderFailed` (as it already did for a routing failure), so the controller log names the stranded root. The 7-of-8 / 6-of-7 shape on westeros: the root's activation failed, `markFailed`'s first write hit the same row and was dropped, the root stayed fenced and invisible to dispatch, voiding and reporting. | Drop when the gastownhall PR for #6235 merges (patch-id differs from the upstream commit because of the two absent sites; verify by the three tests). |
| Claude account isolation (ga-ai7gz2, ga-xd3bjx; fork PRs #40, #48) | bb981d2f3; 47c763d4d, 9f47485ab, 7b63e700d | A managed session never inherits the controller's ambient `CLAUDE_CONFIG_DIR`: the passthrough baseline resets it to empty (`internal/processenv/provider.go:178`), `RequireDeclaredClaudeAccount` guards create (hard) and resume (warn-only), `respawnAgent` re-applies the empties (`set-environment -u` + the `env -u` prefix) and the values on a reconciler relaunch, the CLI resolver keeps a PATH-missing provider's declared env instead of building a nil env (`worker_handle.go:585-606`, `:944-969`), resume merges `Workspace.Env` in the create path's order, and the claude-account doctor check reads the launchd plist for an ambient value. Multi-account fleet policy; upstream passes `CLAUDE_CONFIG_DIR` through (`internal/processenv/provider.go:187`) and has no reset, so the cluster is fork-only. Open siblings from the fork-owner read of #48: gc-da9g (the API resume resolver keeps the nil-env bypass, `internal/api/session_runtime.go:493-495`), ga-zfllzm (the doctor check reads launchd only; the box's systemd unit is never inspected), ga-kdz9pu (tmux/spec-env hygiene). | Drop when upstream adopts an account-declaration guard, or when every fleet seat declares `CLAUDE_CONFIG_DIR` at provider level and the reset is proven redundant. The respawn half is re-expressed on upstream #5061's `markSessionEnvRemoved` / `durableWithholdKeys` (`cab9da1c8`, 2026-08-06, after the carry base) at the next rebase — upstream withholds only controller-only + `BEADS_*` keys, so a one-line fork delta adding every empty-valued key remains. |

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
| Every machine's `bd version` | carries the pin: a laptop/box build starts `v1.1.1-0.20260805093327-bf97b73749ac` and a carry build appends its identity, e.g. `(bf97b73+carry.8f7471e: carry-v59/be-qfm-be-4at@8f7471e01238)`; a CI lockstep build (`install-bd-lockstep.sh`) stamps the same pin without the leading `v` — `1.1.1-0.20260805093327-bf97b73749ac (dev)`. Either way the commit token in the label must stay the pin's commit: gc's version_compat gate equates versions by that token. |

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


## Branch governance (server-side rulesets, ga-w1ollm)

Two repository rulesets govern pushes to this fork (created 2026-09-09 under
the mayor's Q3 ruling; the authority and canary record live on ga-w1ollm):

| Ruleset | Targets | Rules |
|---|---|---|
| `rig-ref-confinement (ga-w1ollm)` (22632844) | all branches **except** `polecat/**`, `pl/**` | restrict creation / update / deletion |
| `carry-operational merge gate (ga-w1ollm)` (22632861) | `carry/operational` | changes via pull request (0 approvals — the gate is the author-run self-gate + CI, not GitHub reviews), required status check `CI / required`, strict up-to-date |

Bypass on both is the **repository-admin role only** — the audited human
break-glass. The rig/automation identity is deliberately not bypass-capable
and is confined by exclusion: its writable refs are `polecat/**` and `pl/**`.
Never grant the automation identity bypass; a control that exists, reports
green, and never blocks is the advisory-only-audit failure class.

Enforcement is staged: rulesets start in **evaluate** (verdicts recorded,
nothing blocked) and flip to **active** only when the rule-suites record shows
both canary sides from a non-bypass actor — a passing green PR merge and a
refused red direct push. The rule-suites API
(`repos/quickserve-ai/gascity/rulesets/rule-suites`) is the actor/ref-complete
push observation source: it records every evaluated push with authenticated
actor, ref, before/after SHAs, and per-rule verdicts — including refused
attempts, which no client-side or events-feed source can show. If an
evaluate-mode verdict would have blocked legitimate traffic, take the specific
row to the mayor; never widen bypass.
