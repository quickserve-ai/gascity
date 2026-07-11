# PR-S2a Build Spec — beads ConditionalWriter CAS machinery (S2-T1..T8, inert)

Main-loop-authored (the Fable design pass stalled on oversized output; grounding
was complete, so this synthesizes DESIGN §8.1–§8.6 + the 3 resolved decisions +
verified source surface). Tracking bead: **ga-91l4q5**. Umbrella: ga-1ypn4t.

**Invariant for the whole PR:** zero consumers, wire byte-untouched, `off`-mode
behavior byte-identical (there is no mode at this layer — stores just gain a
capability). No code path converts `ErrConditionalWriteUnsupported` into an
unconditional write.

## Progress (committed on worktree-reconciler, local/unpushed)
- **docs** `44eb2ab70` — this spec.
- **Phase 1 (S2-T1)** `bec9156b1` — interface + 4 typed errors + `Bead.Revision json:"-"` + `bdIssue` stamping + `ConditionalWriterFor`. Red-teamed (F1 contract carve-out, F5/F4a/F6 doc fixes folded; F2 Doltlite promotion = bd `ga-zj78gu`).
- **Phase 2+3a (S2-T2, S2-T3-mem)** `ec0bccd04` — conformance harness + MemStore native CAS. Red-teamed + teeth-proven by mutation (T1 success-path subtest, T2 adaptive close/reopen, T3 Expected-unconditional + `SuppliesCurrent`, T4 16-racer+winner-value, T5 wide bump table, D1 FileStore shadow, D2 disable-under-lock).
- **Phase 3b (S2-T3-file)** — FileStore native CAS (flock-wrapped reload→save→rollback, replacing the 3a shadow) + out-of-band `fileData.Revisions` persistence (F4b). Red-teamed: production code confirmed correct; the cross-process teeth were missing (all tests were single-handle), so added `TestFileStoreConditionalWriteCrossHandle` (two handles, kills a deleted reload AND a deleted save — both mutation-proven), a 2-bead reopen test (kills per-bead map bugs, mutation-proven), and a legacy-file (no `revisions` key) compat test.
- **Phase 4 (S2-T4/T5 BdStore classifier + probe)** — new `bdstore_conditional.go` (`classifyConditionalWriteResult`, `parseBdConditionalErrorBody`/`decodeBdConditionalBody`, `conditionalWritesCapable` lazy four-verb probe, `markConditionalWritesUnsupported` latch) + `condWrite*` struct fields on `bdstore.go` + white-box `bdstore_conditional_internal_test.go`. Grounded against real bd v1.1.0-rc.1: NO `--if-revision`, exits 1 for all errors, envelope `{"error","hint","schema_version"}` flat or `{"schema_version","data":{...}}` wrapped, no `code` field yet — Decision 2 (message/body, not exit-code) confirmed at source. Fable **design** critique found 5 real defects, all folded before commit: **F1** anchored the unknown-flag match to `unknown flag: --if-revision` so a capable bd's cobra usage-echo (which lists the flag) can't latch it incapable; **F2** machine code dominates revision fields (a coded refusal with an informational `current_revision` is a gate refusal, not a precondition); **F3** ambiguous (may-have-committed) and not-found outrank a gate-refusal code; **F4** the two-source (out+err) parse prefers the body carrying a discriminator; **F5** `json.Decoder` tolerates trailing log bytes. 7-mutation teeth battery all killed (F1 unanchor, probe verb-drop, latch-precedence, F3a ambiguous>code, F2 code-dominance, F4 discriminator-pref, F5 Decoder). No `var _ ConditionalWriter = (*BdStore)(nil)` yet — needs the Phase-5 verbs.
- **Phase 5 (S2-T6/T7 verbs + CAS emulation + F2)** — `bdstore_conditional.go` gains the three `*IfMatch` verbs, the dedicated `runConditionalWrite` retry wrapper, `finalizeConditionalWrite` (centralized ID/Expected stamp + latch), `CompareAndSetMetadataKey` bounded emulation, the `conditionalWriteSleep` seam + `conditionalWriteBackoff`, and `var _ ConditionalWriter = (*BdStore)(nil)`; `bdstore.go` extracts `bdUpdateArgs` (shared by `Update`/`UpdateIfMatch`/CAS, so a new `UpdateOpts` field wires into all three). **F2 (`ga-zj78gu`, CLOSED):** the four CAS methods are shadowed on `*DoltliteReadStore` to return the typed unsupported veto (loud degrade, interface still satisfied) so the SQL-read/fenced-write revision-source disagreement can't false-promote pre-#4682. Retry policy validated by a bounded Fable design pass (ambiguous-before-transient ordering + never-re-fence-with-fresh-revision are the two load-bearing choices). Fable red-team ran on the diff: folded the CAS `--json` omission (now builds argv via `bdUpdateArgs`), added CAS-ambiguous-surfaces-as-is, serialization-exhaustion-bound, delete-on-missing→ErrNotFound, gate-refusal-stamping tests, and a reflection-driven F2 completeness guard (a future 5th CAS verb that silently promotes fails loudly). 7-mutation pre-red-team battery all killed (ambiguous-first defeat, replay-stale-fence, drop-override-Expected, value-mismatch-true, exhaustion-off-by-one, drop-latch, F2-delegate-to-embedded). Red-team findings NOT folded, with cause: `expectedRevision==0` guard (premise contradicts F2 — rev 0 precondition-fails, doesn't false-succeed; a `<=0` reject would violate the opaque-equality revision contract; pre-#4682 the probe gates it); whole-bead-fence starvation (settled DESIGN §8.4, exhaustion path is tested); key-containing-`=` (pre-existing store-wide `bdUpdateArgs` behavior, code-controlled keys); empty-opts no-op (spec-mandated). All gates green (full package both build tags, `-race` on Conditional, vet, gofumpt, golangci-lint 0-net-new, wire gate).
- **Phase 6 (S2-T8 CachingStore forward-and-EVICT + the livelock MERGE GATE)** — new `caching_store_conditional.go` (four verbs forwarding to `ConditionalWriterFor(c.backing)`, `applyConditionalWriteFailure` error-class map, `evictForConditionalWrite`, `adoptConditionalRefresh`, `var _ ConditionalWriter = (*CachingStore)(nil)`) + internal `caching_store_conditional_internal_test.go` (merge gate `TestCachingStoreCASRetryLoopConverges` with counting-wrapper anti-vacuity, both legs) + external `caching_store_conditional_test.go` (conformance row `SuppliesCurrent:true` + `OpenDisabled` + capability-absent interface-stripping leg). **File placement diverges from this spec** (new file per the bdstore_conditional.go convention, not caching_store_writes.go — Fable design-pass D5). **Design-pass divergences from the PHASE6 handoff, all deliberate:** (1) `(false,nil)` CAS value-loss ALSO evicts (D3 override — a cross-process loser otherwise re-reads its own losing `expected` through the cache until reconcile); (2) ambiguous may-have-committed errors mark `dirty` (D4 folded, WITH `noteLocalMutationLocked` seq protection — see red-team below); (3) success+refresh adopts the fresh read **with write-through of exactly what the verb proved committed** (opts/status/swapped key — NOT verbatim adoption: `staleAfterCloseStore` tests pin that backing reads can lag this process's own committed write; revision always from the fresh read, never synthesized). **Pre-existing bug found and fixed en route (own commit):** unconditional `Close`/`Reopen` patched only the cached entry's Status and kept serving the PRE-close Revision (scratch-test proven: cache rev 1 vs backing 2/3); they now adopt the successful refresh read with status written through, synthesis fallback preserved for failed refreshes. Fable red-team (read-only) found **1 real BUG folded before commit** — the ambiguous-dirty mark wasn't seq-protected, so a concurrent `List(Live)` merge-back (or prime `==` rebuild) erased it, deterministically reproduced via an in-test List hook (`TestCachingStoreAmbiguousConditionalFailureDirtySurvivesConcurrentScan`, red→fix→green) — plus 4 surviving mutants, all killed with new legs (lagged-close status write-through, lagged-CAS nil-metadata guard, CAS-success-refresh-fail evict, fenced-close clears dependent ready projection) and a notification-payload ordering assert. **Red-team residue, documented not fixed:** (a) tolerated-CloseIfMatch over a close-hiding backing leaves an orphan `dirty` entry only the quiet-window reconcile reaps, degrading cached list serving until then — the reap fix belongs in `runReconciliation`'s `!=` branch (pre-existing infrastructure; orphan dirty entries already arise from Update's not-cached refresh-fail branch); (b) the write-through advance hazard (refresh observing a LATER state gets stomped) is accepted as the price of the lag defense and documented in the file header. 14 mutants total, all killed (M1-M7 + loser-evict + ambiguous-dirty + 5 red-team). All gates green (full package both build tags, `-race`, vet, gofumpt, golangci-lint 0-net-new incl. native baseline diff, wire gate).
- **PR-S2b (S2-T10/T11/T12)** — committed 2026-07-11, five commits:
  - `dce1360d3` **C0 (forced refactor)** — `internal/rollout/gate` dependency-leaf carve-out: beads→rollout is an import CYCLE (rollout→config→orders→beads), so Mode/ParseMode/Capability/Decision/ResolveCapability moved to a stdlib-only leaf; rollout re-exports via aliases (zero PR-S1 surface change); boundary test allowlists + scans gate/. DESIGN §6.4.1 records this and every deviation below.
  - **C1** — `conditional_writes_resolve.go`: `condWritesStamp` (mode+defaulted+degrade-once latch, own mutex, stamp write reports landed), unexported carrier + prober interfaces, `ResolveConditionalWriter(store)` seam (off/unset legacy zero-cost; auto/require × capable → OUTER store's writer; auto∧incapable → diag EVERY call; require∧incapable → diag + `ConditionalWritesRequiredError` §12.3 grammar, Origin absent by design), MemStore embed + prober; FileStore inherits via *MemStore (stamp survives reload — pinned).
  - **C2** — BdStore embed + prober adapting the four-verb probe + latch, split THREE ways (latch / probe-subprocess-failure / probe-miss — red-team F1: `condWriteProbeErr` memoizes the runner error so a broken bd is never reported as an old bd); DoltliteReadStore prober shadow (F2) with the fatal-runner teeth.
  - **C3** — factory: `StoreOpenOptions.ConditionalWrites gate.Mode`, `stampedResult` wraps all five selection paths, unset→Off+defaulted at debug (NO BeadsDiagnostic wire field — it is on the HTTP wire; §6.4.1 deviation), NativeDoltStore embed, CachingStore delegation (landed=false into carrier-less backing — red-team F2).
  - `(amended)` **C4 (S2-T11)** — `beads.conditional_writes.degraded` constant + KnownEventTypes + `ConditionalWritesDegradedPayload` (6 fields per DESIGN §12.2; store_kind uses the design wire vocabulary) registered from `internal/events/rollout_payloads.go`; genspec+genclient regenerated; red-first (coverage test named the constant before registration); dashboard-check green, no dist churn.
  - **C5 (S2-T12)** — in-tree `//go:build integration` row `bdstore_conditional_integration_test.go` (package beads_test; the beadstest harness can't be called from an internal test — import cycle) + integration-tagged bridge exposing the PRODUCTION probe + classifier. Scaffold subtest always runs (verified PASS vs real bd 1.1.0); conformance leg skips via the production probe (verified; and verified NON-VACUOUS by inverting the gate — it runs and fails on unsupported); adversarial cell (A) live below the capability gate; (B)/(C) stay white-box by documented design; env pinned (BEADS_DIR + dolt-server knobs cleared + GIT_DIR stripped); day-one contention failure mode pre-diagnosed in a comment (embedded-lock errors absent from the bd serialization retry class).
  - Teeth: 14 mutation kills across two batteries (seam short-circuit, refusal drop, diag drop, defaulted drop, latch drop, caching-prober-delegation, bd latch-reason, fallback-stamp, doltlite shadow, caching-fallback-flip, exec-direct-stamp, stamp-mutex under -race, caching-stamp-lies-landed, probeErr-not-memoized). Two Fable red-teams (T10: 8 findings, F1/F2/F5/F6/F7/F8 folded, F3/F4 accepted; T11+T12: 4 findings, all folded). All gates green both tags, -race, zero net-new lint vs the native baseline, wire gate, dashboard-check.
- **Remaining:** S2-T9 sqlite deferred out of S2. Stage 3 (consumers C4/C6, loader threading, emission wiring) NOT started — outward-facing; checkpoint with Julian first.

### S2-T7 SQL-spike verdict (2026-07-11): emulation ships, SQL path DROPPED
Evaluated replacing `CompareAndSetMetadataKey`'s emulation loop with a single
conditional SQL `UPDATE ... WHERE json_value(metadata,key)=expected` (the
`ReleaseIfCurrent` template at bdstore.go:1116 + its `releaseIfCurrentViaEmbeddedDoltSQL`
fallback). **Disqualifier stands, unmet:** the raw SQL bypasses bd's write layer, so
the same statement must ALSO `revision = revision + 1` atomically or it silently breaks
the revision contract for every OTHER conditional writer (a fenced write by another
store would not observe the SQL-path bump). bd #4682 (which adds the revision column)
is unlanded, so there is no column to bump today. Half-adopting would also fork the
write path (bypassing journal/hooks/auto-commit handling) with divergent invariants.
**Verdict: the bounded emulation loop is the shipping implementation; the SQL sidestep
is dropped, not half-adopted.** It is naturally superseded by bd-native revision-CAS
when #4682 lands (the post-#4682 upgrade path already noted for F2's DoltliteReadStore).

## Resolved decisions (OVERRIDE stale plan wording)
1. `Revision int64 \`json:"-"\`` on `beads.Bead`. Verified: `beads.Bead` IS the
   Huma response type (`ListOutput[beads.Bead]`, huma_handlers_beads.go:18/211),
   so `json:"-"` is invisible to OpenAPI reflection → `TestOpenAPISpecInSync`
   stays green (exit gate #1). S4 flips the tag. Populate internally: BdStore
   from bd JSON when present (pre-#4682 → 0); Mem/File per-bead counter bumped on
   every mutation (the counter IS the `Revision` field on the stored Bead value).
2. bd classifier = **message-substring matching**, not a numeric exit path
   (BdStore has none; `isBdTransientWriteError`/`isBdNotFound`/… all match on the
   message string). The plan's "exit-9/exit-13" is a misnomer for this codebase —
   say so in the PR.
3. Method names mirror `Update`/`Close`/`Delete`:
   `UpdateIfMatch(id, expectedRevision, opts)`, `CloseIfMatch(id, expectedRevision)`,
   `DeleteIfMatch(id, expectedRevision)`, `CompareAndSetMetadataKey(id,key,expected,next)(bool,error)`.
   Interface modeled on `ConditionalAssignmentReleaser` (beads.go:109-114);
   discovery mirrors `GraphApplyFor` (graph_apply.go:24-35) →
   `ConditionalWriterFor(store)(ConditionalWriter,bool)` + a
   `ConditionalWriterHandleProvider`. Optional interfaces are NOT promoted through
   embedded-Store wrappers (class_store.go:14-20) — assert on unwrapped `.Store`.

## Interface + errors (S2-T1, beads.go)
```go
Revision int64 `json:"-"` // last field of Bead; store-internal until S4.

type ConditionalWriter interface {
    UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error
    CloseIfMatch(id string, expectedRevision int64) error
    DeleteIfMatch(id string, expectedRevision int64) error
    CompareAndSetMetadataKey(id, key, expected, next string) (bool, error)
}
type ConditionalWriterHandleProvider interface {
    ConditionalWriterHandle() (ConditionalWriter, bool)
}
func ConditionalWriterFor(store Store) (ConditionalWriter, bool) // direct assert → provider → (nil,false)
```
Doc comment carries the NORMATIVE revision contract + granularity contract
(§8.1 verbatim): opaque int64, equality-only; EVERY issue-row mutation bumps
(field updates, label add/remove, metadata writes any key, assign, close, reopen,
delete); reads never bump; cross-bead writes never bump; monotonic, never reused.
Granularity: callers may assume NEITHER value-level nor revision-level conflict
semantics.

Typed errors beside the existing sentinels (beads.go:12-46), §8.1 verbatim:
- `ErrConditionalWriteUnsupported = errors.New(...)` — sentinel; latching veto.
- `PreconditionFailedError{ID string; Expected, Current int64; Raw string}` —
  `Error()` includes ID/Expected/Current.
- `GateRefusalError{ID, Verb, Code, Raw string}` — per-write, never latches.
- `CASRetriesExhaustedError{ID, Key string; Attempts int; LastRevision int64}` —
  MUST NOT be an `errors.Is`/`As` match for `PreconditionFailedError` (distinct
  types, no wrapping between them).
Unexported accessor for revision if needed by tests; `PreconditionFailedError.Current`
is the public revision surface.

**Tests (red-first):** `TestConditionalWriterErrorIdentity` (As/Is matrix over the
four types; exhaustion ≠ precondition), `TestBeadRevisionDecodesFromBDJSON`
(present/absent/non-numeric-tolerant via StringMap precedent). Re-run
corpus_decoder_test.go. **Wire gate:** `go test ./internal/api/ -run 'OpenAPISpecInSync|EventPayload'` green.

## Conformance harness (S2-T2, beadstest/conditional_writer_conformance.go)
`RunConditionalWriterConformance(t *testing.T, name string, open func(t *testing.T) beads.Store)`.
Subtests map §8.6 one-to-one: `every_mutation_bumps_revision` (verb matrix:
update, labels, metadata, assign-via-Update, close, reopen; CompareAndSetMetadataKey
itself bumps), `reads_never_bump`, `revision_monotonic_never_reused`,
`stale_revision_is_precondition_failed` (typed; Expected/Current where backend can
supply), `cas_empty_expected_claims_absent_or_empty_only`,
`cas_value_mismatch_is_false_nil_not_error`, `cas_winner_value_visible_to_loser_reread`,
`contention` (two goroutines, exactly one true),
`disable_toggle_returns_typed_unsupported_with_interfaces_intact`. NO cross-key
interference assertions (granularity contract). Capability-absent tested with a
purpose-built minimal store type in the test file, never a wrapper (§7.3). Verified
red-first by wiring MemStore in Phase 3.

## Mem/File native impls (S2-T3)
Bump `Revision` on every issue-row mutator. **MemStore:** Create(sets Revision=1),
Update, ReleaseIfCurrent(on success), Close, Reopen, CloseAll, SetMetadata,
SetMetadataBatch; CompareAndSetMetadataKey/*IfMatch bump too. **NOT bumped:**
DepAdd/DepRemove (dependency-graph edges, not the issue row — matches the contract's
enumerated verb list; bd bumps issue revision only on row mutations). Delete removes
the bead (no bump). `Tx`: verify writes inside route through the bumping mutators.
**FileStore** delegates to an inner MemStore (verify) → inherits in-session bumps,
BUT `Revision` does NOT persist via `fileData.Beads []Bead`: `json:"-"` drops it
from the on-disk JSON, and `reloadFromDisk()` runs before every locked write in
cross-process flock mode, so revisions would reset to 0 mid-session and violate
"monotonic, never reused" (red-team F4b). FileStore MUST persist revision
**out of band** — e.g. add a `Revisions map[string]int64` to `fileData`, populate
it from the inner store on `save()`, restore it on load. Test the cross-process
leg explicitly. Add `DisableConditionalWrites bool` to both:
when true all four CAS methods return `ErrConditionalWriteUnsupported`, other
optional interfaces stay intact (no hiding wrapper). Compile asserts
`var _ ConditionalWriter = (*MemStore)(nil)` / `(*FileStore)(nil)`.
FileStore extra test: revision survives close/reopen of the store.

## BdStore classifier + probe (S2-T4, T5) — bdstore_conditional.go (new) + bdstore.go
`classifyConditionalWriteResult(out []byte, err error) error`, PURE, message-substring
table (Decision 2). Enumerate exact substrings at build time from real bd + existing
classifiers; classes: precondition-failed → `*PreconditionFailedError` (parse
`{expected_revision,current_revision}` from body via the `extractJSON` idiom, tolerate
noise, misparse → zero-valued with Raw); unsupported (body code
`conditional-write-unsupported` OR usage/unknown-flag mentioning `--if-revision`) →
`ErrConditionalWriteUnsupported` (LATCHES); gate-refusal (policy, e.g. close-authority)
→ `*GateRefusalError` (never latches); ambiguous (`isBdAmbiguousWriteError`) → as-is;
else → existing classification (`isBdNotFound`→ErrNotFound). Latch decision is
body-code-gated, never bare. Probe (§8.3): `condWriteMu/condWriteProbed/condWriteCapable/
condWriteLatched` on BdStore struct + `conditionalWritesCapable()(bool,error)` lazy,
memoized, four-verb (`update`/`close`/`assign`/`delete` `--help` grep for `--if-revision`)
through the EXISTING `s.runner` seam (mirror bdReadyProjectionEnabled:69). Latch
authoritative over probe. No construction-time subprocess. No second probe seam.
Fake: extend the scripted `fakeRunner` (bdstore_test.go:19) with per-call exit/err +
an apply-func that mutates fake backing before returning err (committed-but-ambiguous cell).

## BdStore verbs + CAS emulation (S2-T6, T7) — bdstore_conditional.go
`UpdateIfMatch/CloseIfMatch/DeleteIfMatch`: check `conditionalWritesCapable()`; build
`--if-revision N --json` argv (doltlite `--dolt-auto-commit` prefix preserved); run
through a NEW `runConditionalWrite` wrapper that NEVER routes through
`runBDTransientWrite`/`isBdTransientWriteError`. Retry policy (§8.2): connection/
serialization errors → RE-READ revision before re-attempt (bounded, jittered); exit-9/
precondition → surface immediately; ambiguous → surface as-is; never downgrade to
unconditional. `CompareAndSetMetadataKey`: bounded emulation loop (§8.4 verbatim):
`casEmulationMaxAttempts=4`, `casEmulationBaseBackoff=25ms` doubling+jittered; Get →
value check (`""≡absent`) → runConditionalWrite update --set-metadata; nil→(true,nil);
PreconditionFailed→retry; exhaustion→`*CASRetriesExhaustedError` (NOT PreconditionFailed,
NOT (false,nil)); other→(false,err) as-is. Compile assert `var _ ConditionalWriter = (*BdStore)(nil)`.
**SPIKE (§8.4):** evaluate a single conditional SQL UPDATE (ReleaseIfCurrent template
bdstore.go:1097 + embedded-dolt fallback) with a JSON-path value predicate. Disqualifier:
it MUST also `revision = revision + 1` atomically or it breaks the contract for every
other writer. **Recommended verdict (confirm against bd schema at build):** emulation
loop SHIPS; SQL path dropped unless the atomic revision bump is provable — record dated
note in engdocs/plans/feature-flags/.

## CachingStore forward-and-EVICT (S2-T8) — caching_store_writes.go (or a new
## caching_store_conditional.go mirroring bdstore_conditional.go — writes.go is ~990
## lines; PHASE6-HANDOFF.md D5 recommends the new file; record whichever is taken here)
Forward to `c.backing` via `ConditionalWriterFor`; not implementing → typed unsupported.
Cache rule (§8.5) DIVERGES from ReleaseIfCurrent's optimistic-patch else-branch (:138-180):
CAS success + refresh ok → refresh; CAS success + refresh FAILED → EVICT =
`delete(c.beads,id)` + `delete(c.deps,id)` + `c.dirty[id]=struct{}{}` +
markFreshLocked/clearDependentReadyProjectionsLocked bookkeeping — **NEVER stamp
`deletedSeq` on an evict** (Get short-circuits a deletedSeq id to ErrNotFound WITHOUT
consulting backing, caching_store_reads.go:386-389 — the exact livelock the evict
exists to break; only DeleteIfMatch SUCCESS stamps deletedSeq, mirroring Delete's
full scrub). [Corrected 2026-07-11 during the Phase-6 handoff verification pass: an
earlier revision of this line listed "dirty/deletedSeq/..." as evict bookkeeping,
which contradicts DESIGN §8.5 and the Get semantics.]
EVERY `PreconditionFailedError` from backing → evict (cached revision proven stale).
NEVER patch a cached bead after a conditional write. **MERGE GATE test:**
`TestCachingStoreCASRetryLoopConverges` (CAS succeeds, refresh forced to fail once →
entry evicted → next Get hits backing → retry converges), + evict-on-PreconditionFailed.
Compile assert `var _ ConditionalWriter = (*CachingStore)(nil)`.

## Phase order (each ≤5 files; red-first → green → gates → Fable red-team → commit)
| Ph | Task | Files | Red-first test |
|----|------|-------|----------------|
| 1 | S2-T1 | beads.go, beads_test.go | error-identity + revision-decode |
| 2 | S2-T2 | beadstest/conditional_writer_conformance.go | (harness; red via Mem in Ph3) |
| 3 | S2-T3 | memstore.go, filestore.go, memstore_test.go, filestore_test.go | conformance over Mem/File |
| 4 | S2-T4/T5 | bdstore_conditional.go, bdstore.go, bdstore_conditional_internal_test.go | classifier table + probe |
| 5 | S2-T6/T7 | bdstore_conditional.go, bdstore_conditional_internal_test.go, engdocs spike note | verbs/argv + emulation |
| 6 | S2-T8 | caching_store_writes.go (or new caching_store_conditional.go), caching_store_conditional_internal_test.go (package beads: merge gate + evict white-box), caching_store_conditional_test.go (package beads_test: conformance row — beadstest imports beads, so the harness can't be invoked from an internal test) | livelock regression (MERGE GATE) |

(S2-T9 sqlite is deferred out of S2 per plan; S2-T10..T12 are PR-S2b, next session/PR.)

## Gate checklist (every phase)
- `go build ./internal/beads/...`
- `go test ./internal/beads/...` (FULL package — not `-run`; surfaces latent failures)
- `go vet ./internal/beads/...`
- `golangci-lint run ./internal/beads/...` (retry on parallel-lock message)
- `gofumpt -l <changed>` (binary at /home/ubuntu/go/bin/gofumpt) → empty
- Wire gate: `go test ./internal/api/ -run 'OpenAPISpecInSync|EventPayload'` green
- Fable red-team on the actual diff (isolated worktree or read-only per
  [redteam-mutation-shared-worktree]); fold confirmed findings; document residue.
- Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`

## Open questions / risks (Phase-1 red-team hardened these)
- **Revision contract vs bd's is_blocked (F1, RESOLVED in Phase 1).** bd keeps a
  denormalized is_blocked ON the issue row that cross-bead dep/close/route writes
  recompute (bd pins updated_at during that recompute). The contract now carves
  derived-projection columns OUT of the bump guarantee; the conformance suite must
  NOT assert whether is_blocked/dep-edge changes bump (interference is undefined per
  the granularity contract — it already excludes such assertions).
- **F2 — DoltliteReadStore promotion (Phase 5 MUST-FIX, bd tracking bead).**
  `internal/beads/doltlite_read_store.go` embeds a concrete `*BdStore`, so once
  BdStore implements ConditionalWriter the methods PROMOTE through the embedding and
  `ConditionalWriterFor` asserts true on the wrapper — but its SQL `scanBead`
  (:1356) never populates Revision → `Get`→0 → every CAS `PreconditionFailedError`
  forever in the `GC_NATIVE_DOLTLITE_BEADS` deployment; promoted writes also bypass
  the wrapper's `resetOrderRunCache()` (:523). When BdStore lands the interface,
  DoltliteReadStore must EITHER populate Revision in scanBead AND override the CAS
  verbs to invalidate its cache, OR expose a `ConditionalWriterHandle()` returning
  (nil,false) so it does not falsely claim capability. Secondary: `internal/beads/
  exec/exec.go:136` (`beadWire.toBead`) is a second bd-JSON envelope that drops
  revision — lower risk (exec stores won't claim the capability) but audit before
  any exec-store CAS.
- **F3 — CachingStore event-patch staleness (Phase 6/S3).** Event payloads are
  `json.Marshal(b)` → Revision excluded by `json:"-"` → `mergeCacheEventPatch`
  preserves the OLD cached revision; `beadChanged` ignores Revision. The §8.5
  evict-never-patch rule + "every PreconditionFailed evicts" converges CAS retries,
  but a cache `Get` between an event and the next reconcile can hand a consumer a
  stale revision (one wasted CAS attempt, then evict-and-converge). Acceptable under
  §8.5; the field doc now states the caveat. No unconditional-write path may result.
- **DepAdd/DepRemove bump?** NO (dependency-graph edges, not this bead's issue-row
  fields; consistent with the F1 carve-out). Not in the conformance verb matrix.
- **Create initial revision** = 1 (opaque; conformance reads via Get, never a literal).
- **Tx writes** — confirm Tx mutations route through bumping mutators; a raw slice
  patch inside Tx would skip the bump (false-green conformance).
- **`assign` verb probe** — MemStore has no Assign method (assignment is via Update);
  the BdStore probe still checks `assign --help` (a consumer uses assign). Keep all four.
- **F6 — "revision" wire key provisional.** bd #4682 unlanded; key name unconfirmed.
  Absent-key→0 == legacy behavior, so a mismatch fails ONLY at the integration
  conformance row against #4682-capable bd — that row is the guard, not silent drift.
- **Phase-4 classifier substring port diverges from DESIGN §8.2's exit-code table
  (2026-07-11).** §8.2 is written as an exit-9/exit-13 discriminator; the real bd
  (v1.1.0-rc.1) has NO exit-code path — it exits 1 for every error and there is no
  `code` field in its envelope yet, so the port is body-code + message-substring
  (Decision 2). The three inputs that bypass the provisional substrings entirely, and
  which the `//go:build integration` conformance row (S2-T12) against a #4682-capable
  bd MUST include, are: **(a)** a *capable* bd's cobra usage-echo naming
  `--if-revision` while reporting some *other* unknown flag (must NOT latch — the F1
  anchored match); **(b)** a policy gate refusal (e.g. close-authority) that carries
  an informational `current_revision` (must classify as `*GateRefusalError`, never a
  precondition — the F2 code-dominance rule); **(c)** a coded gate refusal whose
  human message contains "not found" (e.g. "lease not found for holder") — the
  machine code must win over the loose not-found substring, or a permanent refusal
  is swallowed as idempotent success (the red-team D3 hazard; the classifier resolves
  the gate-code branch before the message not-found heuristic). The body scanner also
  tolerates bracketed (`[WARN] ...`) and leading-JSON log lines around the envelope
  (red-team D1/D2). Provisional machine codes assumed:
  `precondition-failed`, `conditional-write-unsupported`. Confirm/rename against the
  landed #4682 bd; a rename fails loudly at the integration row, not silently.
