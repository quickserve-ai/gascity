# PR-S2a Build Spec — beads ConditionalWriter CAS machinery (S2-T1..T8, inert)

Main-loop-authored (the Fable design pass stalled on oversized output; grounding
was complete, so this synthesizes DESIGN §8.1–§8.6 + the 3 resolved decisions +
verified source surface). Tracking bead: **ga-91l4q5**. Umbrella: ga-1ypn4t.

**Invariant for the whole PR:** zero consumers, wire byte-untouched, `off`-mode
behavior byte-identical (there is no mode at this layer — stores just gain a
capability). No code path converts `ErrConditionalWriteUnsupported` into an
unconditional write.

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

## CachingStore forward-and-EVICT (S2-T8) — caching_store_writes.go
Forward to `c.backing` via `ConditionalWriterFor`; not implementing → typed unsupported.
Cache rule (§8.5) DIVERGES from ReleaseIfCurrent's optimistic-patch else-branch (:138-180):
CAS success + refresh ok → refresh; CAS success + refresh FAILED → `delete(c.beads,id)`
+ dirty/deletedSeq/markFreshLocked/clearDependentReadyProjectionsLocked bookkeeping;
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
| 6 | S2-T8 | caching_store_writes.go, caching_store_conditional_test.go | livelock regression (MERGE GATE) |

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
