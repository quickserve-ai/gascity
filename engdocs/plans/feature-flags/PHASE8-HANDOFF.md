# Phase 8 Handoff — PR-S2b COMPLETE; checkpoint before S3

**PR-S2b (S2-T10/T11/T12) is code-complete and committed** on
`worktree-reconciler`, local/UNPUSHED, still fully INERT: no production
caller resolves a conditional writer, threads a mode into the factory, or
emits the degraded event. S2-T9 (sqlite graph store ConditionalWriter) stays
deferred out of S2.

## What landed (see PR-S2a-BUILD-SPEC.md Progress for detail)

| Commit | Content |
|--------|---------|
| `dce1360d3` | `internal/rollout/gate` dependency-leaf carve-out (beads→rollout would CYCLE via config→orders→beads; aliases keep rollout's surface identical) |
| C1..C3 (three commits after it) | `condWritesStamp` + carrier/prober interfaces + `ResolveConditionalWriter` seam + `ConditionalWritesRequiredError`; Mem/File, Bd/Doltlite, Native/Caching wiring; factory `ConditionalWrites gate.Mode` option + `stampedResult` on all five paths |
| C4 | `beads.conditional_writes.degraded` typed event REGISTERED (constant + payload + genspec/genclient regen), no emission |
| C5 | `//go:build integration` BdStore conformance row + production-probe bridge; skips cleanly vs bd v1.1.0, verified non-vacuous |

`git log --oneline` from this doc's commit backwards identifies the exact
SHAs. DESIGN.md **§6.4.1 (new)** is the authoritative as-built amendment —
read it before trusting §6.3/§6.4/§7.3/§12.2 verbatim.

## Non-negotiables for the next session

- **Do NOT push. Do NOT start S3 without checking in with Julian** — S3 is
  outward-facing (deploy-lineage sync + the live maintainer-city flip).
- Two untracked dirs are NOT ours: `engdocs/plans/beads-cas/`,
  `engdocs/plans/reconciler-redesign/`. Never `git add` them.

## Stage-3 pickup facts (verified this session)

- **beadPolicyStore blind spot:** every factory store is wrapped by
  `cmd/gc/bead_policy_store.go` (interface embedding — the carrier does NOT
  promote). Consumers must resolve the unwrapped store, or the wrapper must
  forward the carrier.
- **Eight out-of-factory store constructions** resolve as unset→legacy until
  swept: `cmd/gc/cmd_hook_claim.go:579`, `cmd/gc/scoped_store.go:27/36`,
  `cmd/gc/cmd_bd_store_bridge.go:142`, `cmd/gc/bd_env.go:179/198`,
  `internal/runtime/t3bridge/provider.go:1699-1700`,
  `internal/beads/libstore.go:25`, `cmd/gc/cmd_start.go:890`.
- **Loader threading (§6.1 arity change, ~30 sites) is Stage 3**, as are the
  C4/C6 call sites, emission wiring (factory callback → event bus; the
  once-latch `noteConditionalDegradeOnce` ships tested but unwired), doctor
  rows, and the §12.5 status wire.
- The integration row's **day-one failure mode is pre-diagnosed** in the
  conformance leg's comment: embedded-scope lock/busy errors are not in
  `isBdTransientWriteError`'s serialization class — widen the class or use a
  server-mode scope before reading such a failure as a #4682 wire-code break.
- exec.Store is deliberately unstamped; a Require deployment running an exec
  provider needs a Stage-3 decision.

## Gotchas new this session

- `internal/beads` importing `internal/rollout` is a compile-time cycle
  (config→orders→beads). Consumer code imports `internal/rollout/gate`.
- `BeadsDiagnostic` is ON THE HTTP WIRE (`StatusResponse.Beads`,
  huma_types_patches.go:171): adding fields = genspec + genclient +
  decode_status churn. The seam's diagnostic reuses existing fields on a
  fresh value and must never ride the status wire until §12.5.
- Registering an event payload regenerates the OpenAPI spec (new oneOf +
  envelope variants): constant and registration must land in ONE commit or
  genspec panics; `make dashboard-check` is required.
- The conformance harness cannot be invoked from an internal (package beads)
  test file — beadstest imports beads. Integration rows live in package
  beads_test with an integration-tagged bridge for unexported access.
- Mutation batteries: zsh function scope lost coreutils mid-run once
  (restores silently failed and mutations ACCUMULATED — verify restores with
  `cmp` at top level, or run the battery as a bash script file).
