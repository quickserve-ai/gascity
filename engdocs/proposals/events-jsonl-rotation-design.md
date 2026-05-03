---
title: "events.jsonl Rotation: Design Survey & Recommendation"
description: "Survey of mechanisms for bounding events.jsonl growth, applying the Primitive Test to each, and a recommended SDK+CLI bundle."
---

> **Status:** Proposed (draft 2026-05-02)
> **Author:** gascity/furiosa (polecat) for the Overseer + crew review
> **Bug bead:** `gc-n0mqg` · **Work bead:** `gc-juwa3` · **Upstream:** [#1118](https://github.com/gastownhall/gascity/issues/1118)
> **Scope:** the file lifecycle of `.gc/events.jsonl` (and the supervisor's analogue). Out of scope: payload-shrinking on `bead.updated` events ([gc-zykvg](#references)), the `/v0/city/{name}/events` HTTP timeout (a separate root cause in `internal/events/multiplexer.go` — see [#1487](https://github.com/gastownhall/gascity/issues/1487)).

## 1. Problem statement

### 1.1 The acknowledged gap

`engdocs/architecture/event-bus.md` (Known Limitations, lines 391–394) names the gap directly:

> **No event retention or rotation.** The JSONL file grows without bound. There is no built-in log rotation, retention policy, or compaction. For long-running cities, manual truncation or external log rotation is needed.

`internal/events/recorder.go:32-53` opens the file with `os.O_APPEND|os.O_CREATE|os.O_WRONLY`, mode `0o644`, and never closes/renames it short of process exit. Every `Record()` call (recorder.go:57-94) takes a `flock(LOCK_EX)`, re-reads the latest seq from the file tail, marshals one JSON line, and appends. There is no size check, no time check, no archival hook.

### 1.2 The local symptom (qlandia, gascity rig)

- `~/qlandia/.gc/events.jsonl` reached **408 MB / 290 k lines in 4 days** (~100 MB/day) after the controller started 2026-04-28. `gc doctor` warns at 100 MB; we are 4× that.
- Histogram of the last 50 k events:
  - 43% `bead.closed`
  - 42% `bead.updated` (each embeds the full ~1.1 KB bead payload, often for heartbeat-only field changes — tracked separately in `gc-zykvg`)
  - 10% `bead.created`
  - ~3% order events
  - <1% session/mail
- Top subjects: `gastown.boot` and `control-dispatcher` session beads being constantly updated.

### 1.3 Operational impact (calibrated)

The naïve framing — "events.jsonl bloat causes the controller HTTP timeouts on `gc mail inbox` etc." — is **partially falsified by direct measurement**. From our [#1487](https://github.com/gastownhall/gascity/issues/1487) comment (2026-05-03):

> Direct timing of `ReadFilteredTail` against a 408 MB / 290 k-line `events.jsonl`: **342 µs for limit=1, 403 µs for limit=100**. The tail-read path is fast even at >4× the doctor warning threshold.
> … `/v0/cities` returns 200 in 16 ms during the same window `/v0/city/{name}/events` hangs >60 s.
> Specifically: the multiplexer's fan-out has no per-provider timeout (`internal/events/multiplexer.go` `ListTail` lines 97-129; same pattern in `ListAll` 76-92, `LatestCursor` 133-147, `Watch` attach 156-213).

So the controller HTTP timeout is a **separate root cause** in the multiplexer's serial fan-out. Rotation **will not fix the timeout symptom**. It is, however, still warranted on independent grounds:

1. **Disk pressure.** ~100 MB/day on a single rig is enough to fill any laptop in months and any test runner in days.
2. **Doctor signal credibility.** The doctor check at `internal/doctor/checks_semantic.go:139-184` warns at 100 MB with `FixHint = "consider truncating or archiving .gc/events.jsonl"`. A warning whose only remediation is manual truncation is operator-hostile.
3. **Audit-trail manageability.** `engdocs/contributors/reconciler-debugging.md:65` makes `events.jsonl` load-bearing for forensics. A 400 MB+ JSONL is impractical to grep, ship in a bug report, or load in a viewer.
4. **Two installations are already running launchd/cron workarounds** (azanar's "truncate to last 10 k lines at 50 MB"; jwp23's `~/Library/LaunchAgents/` variant). jwp23 specifically notes pack-edits are reverted by `gc reload`, which is why their workaround lives outside the pack.
5. **Future filtering / index work** (event-bus.md:396-400 acknowledges `ReadFiltered scans the entire file`) becomes increasingly expensive as the file grows. Rotation bounds the worst case.

### 1.4 The maintainer ask

[Issue #1118](https://github.com/gastownhall/gascity/issues/1118) is **OPEN, P2, no maintainer comment in 7 days**. The issue body and jwp23's comment between them name three options:

> Built-in `gc events rotate` subcommand with configurable threshold and keep-count. Could also be handled by the maintenance pack or controller patrol.

…and one workaround:

> Custom order + script that truncates to last 10 k lines when file exceeds 50 MB.

The space is unclaimed — no maintainer comment, no PR — so the design framing wins by being clear and well-reasoned, not by deferring to a stated preference.

## 2. Constraints

The design will be evaluated against six non-negotiable Gas City principles. Bouncing PRs reach for these, and the surrounding cautionary tales (#1275, #1557) confirm that "ship a thing that mostly works" is not a winning strategy.

1. **ZFC (Zero Framework Cognition)** — `engdocs/architecture/glossary.md`:
   > Go handles transport, not reasoning. If a line of Go contains a judgment call, it's a violation.
   `engdocs/architecture/controller.md:196-198`:
   > **No role names in Go code**: The controller operates on resolved config, runtime session names, and provider state. No line of Go references a specific role name.
   For rotation, this means: thresholds and policy must come from **TOML config or a user-supplied script**, not be hard-coded in `internal/events/`.

2. **Bitter Lesson alignment** — `engdocs/architecture/nine-concepts.md`:
   > Every primitive must become MORE useful as models improve.
   For rotation: pure size-based splitting passes (a smarter model can't improve "split when 1 GB"); content-aware retention ("keep events that changed agent state, discard heartbeats") fails because a smarter model would do the same job from a prompt with more context.

3. **The Primitive Test** — `engdocs/contributors/primitive-test.md` (verbatim, lines 9–64):
   > A capability belongs in the SDK only if all three hold. If any condition fails, it belongs in the consumer layer.
   > 1. **Atomicity** — can agents do it safely without races? …
   > 2. **Bitter Lesson** — does it become MORE useful as models improve? … Imagine a model 10× more capable. Does this capability become less necessary (→ consumer layer) or exactly as necessary (→ primitive)?
   > 3. **ZFC** — is it transport or cognition? … Does any line of Go contain a judgment call? If yes, the decision belongs in the prompt, not the code.

4. **Five primitives + four derived** — `engdocs/architecture/nine-concepts.md`. The Event Bus is a Layer-0/1 primitive. Rotation is provably **derived** (see §3 atomicity walkthrough): it composes from `Config` (thresholds) + Event Bus (read/write) + Controller (scheduling), exactly as Health Patrol composes from `Agent Protocol` + `Config` + Event Bus.

5. **SDK self-sufficiency** — `engdocs/architecture/health-patrol.md:242-244` and `engdocs/architecture/controller.md:203`:
   > All controller operations … function with only the controller process running. No user-configured agent role is required for any infrastructure operation.
   Test for any rotation option: *if the user removes their maintenance pack, does events.jsonl still stop growing?*

6. **Pack content vs SDK code distinction** — pack: `examples/gastown/packs/...`, SDK: `internal/`, `cmd/gc/`. Per the upstream evidence:
   - Pack-shipped defaults that depend on operator setup are an anti-pattern (#1275 — every default install escalates HIGH every 45 minutes because `mol-dog-jsonl` pushes to an unconfigured remote).
   - `gc reload` reverts edits to pack files (jwp23 on #1118), so operator-side patches *can't* live in the pack.
   - Formulas without executors silently no-op (#1557).

Two further invariants from the architecture deserve named billing:

- **Best-effort recording** (`event-bus.md:180-181`):
  > Record is best-effort. Recording errors are logged to stderr but never returned to callers. The caller's operation must not fail because event recording failed.
  Rotation must inherit this — a failed rotation must never break a `Record()` call.

- **Append-only invariant** (`event-bus.md:310-318`): the file is `O_APPEND`-opened, locked with `flock`, multi-process safe. Rotation must preserve all four properties (append, lock, multi-process, monotonic seq).

## 3. Survey of mechanisms

This survey is exhaustive on purpose. Each option is described in concrete code/config terms and then scored in §4.

### A. Provider-level (within the SDK)

#### A1. RotatingFileRecorder wrapping FileRecorder

A new type `RotatingFileRecorder` in `internal/events/rotating.go` that satisfies the existing `events.Provider` interface and delegates writes to an internal `*FileRecorder`. After each `Record()` call, the wrapper checks `os.Stat(path).Size() > maxSize` while still holding the FileRecorder mutex; if so, it **rotates inline** by closing the active file, performing a numbered rename shift (`events.jsonl` → `events.jsonl.1`, `.1` → `.2`, …), pruning beyond `keepFiles`, and reopening a fresh `events.jsonl`. Read methods (`List`, `ListTail`, `Watch`, `LatestSeq`) merge across the active file plus retained siblings.

Configuration via `[events] max_size` and `[events] keep_files` in `city.toml`; both default to zero (= disabled). Constructor `NewRotatingFileRecorder(path, maxSize, keepFiles, stderr)` returns a `*FileRecorder` directly when `maxSize == 0`, preserving the current behavior bit-for-bit when rotation is opted out.

This is the *closest in-tree precedent* — `cmd/gc/session_reconciler_trace_store.go:439-452` already implements `rotateSegment` for trace files (segment-bytes + batch-count thresholds, rename-and-reopen on the same write path).

#### A2. New TailProvider with size budget

Bound the on-disk file by **truncating** in place (drop oldest lines until `size < budget`), implemented as a new `Provider` shape that overrides `ListTail` with a budget-aware backing file. Lossier and simpler than rotation: history is destroyed in place, no siblings exist, no archive ladder.

#### A3. Compaction (rewrite events.jsonl with content-aware filtering)

A new path that *rewrites* events.jsonl, dropping/summarizing the noisy 42% `bead.updated` heartbeat events while keeping `bead.created`/`bead.closed`/state-changing updates. Different in shape from rotation: it produces a smaller file at the same path, no `.1` siblings, but requires content-classification rules (which events are "noise") that are inherently judgment.

The adjacent payload-delta work in [`gc-zykvg`](#references) attacks the same 42% slice from a different angle: shrink the `bead.updated` payload at write-time rather than re-classify at compaction-time.

### B. CLI surface (under `gc`)

#### B1. `gc events rotate` subcommand

A new sub-command in `cmd/gc/cmd_events.go` that triggers a rotation against the active recorder via the API control plane (per `engdocs/architecture/api-control-plane.md`'s "object model at the center" rule). Flags: `--force` (rotate regardless of current size), `--max-size SIZE` (override config for this invocation), `--keep N` (override `keep_files`), `--out FORMAT` (json|text). Backed by the same code that A1's auto-rotation calls.

This is the maintainer-suggested shape verbatim from #1118: *"Built-in `gc events rotate` subcommand with configurable threshold and keep-count."*

#### B2. `gc events compact` subcommand

CLI surface for A3. Could be lossy (drop heartbeats older than X) or lossless (delta-compress repeated payloads).

#### B3. `gc doctor --fix` integration

The doctor already detects bloat (`internal/doctor/checks_semantic.go:139-184`); have `--fix` invoke the rotation path. Discoverability win, but `--fix` already exists for other checks and adding event rotation here couples doctor to a write-path concern it does not currently own.

### C. Controller patrol (built-in scheduled task)

#### C1. Background goroutine in controller (Wisp-GC pattern)

A new tracker interface `eventsRotator { shouldRun(now) bool; runRotation(now) (RotateResult, error) }`, a 5th `if rotator != nil { rotator.runRotation(...) }` line in `controllerLoop()` (`cmd/gc/controller.go:226`), and a `newEventsRotator(interval, maxSize, keepFiles)` factory that returns nil when any field is zero. Identical-shape twin of `cmd/gc/wisp_gc.go:14-42`:

```go
// wispGC pattern (existing) — events rotator would mirror this exactly.
type wispGC interface {
    shouldRun(now time.Time) bool
    runGC(store beads.Store, now time.Time) (int, error)
}

func newWispGC(interval, ttl time.Duration) wispGC {
    if interval <= 0 || ttl <= 0 { return nil }
    return &memoryWispGC{interval: interval, ttl: ttl}
}
```

Differs from A1 in *when* the size check fires: A1 fires inline on the write path (immediate); C1 fires on the controller tick (every 30 s by default per `engdocs/architecture/controller.md`). Burst-write windows can overshoot the threshold by minutes between ticks.

#### C2. Patrol expressed as an order

Instead of a hardcoded controller-tick check, ship a built-in SDK order with `trigger = "cooldown", interval = "5m"` that the controller dispatches against itself. More observable in the events stream (every rotation gets an order-tracking bead), but adds a layer of indirection for an SDK-internal concern.

### D. Pack-level (in user-customizable maintenance pack)

#### D1. New `mol-dog-rotate` formula + exec order

A pack-shipped `mol-dog-rotate.toml` with `trigger = "cooldown", interval = "1h", exec = "$PACK_DIR/assets/scripts/rotate.sh"`. The script calls `truncate -s 0` or moves files. Mirrors the `mol-dog-reaper`/`mol-dog-jsonl` pattern exactly.

#### D2. Extend `mol-dog-jsonl` to rotate before pushing

Currently broken-by-default per #1275 (pushes to an unconfigured remote). Adding rotation responsibilities to a script that escalates HIGH on every default install is doubling down on a known anti-pattern.

#### D3. Doctor-warning event hook

Have the maintenance pack ship a hook that listens for the `events.log.size.warning` event (newly emitted by doctor) and triggers rotation. Reactive rather than scheduled.

#### D4. Plugin/script invoked by deacon

Deacon's prompt grows a clause: *"if doctor reports >100 MB events.jsonl, run `gc events rotate`."* Maximum LLM-judgment latitude; minimum determinism.

### E. External / out-of-process

#### E1. logrotate / launchd / cron

No code change. Document it in `engdocs/contributors/`. This is what jwp23 is *currently* doing as a workaround on #1118 — explicitly outside the pack to survive `gc reload`.

#### E2. Sidecar daemon

Separate long-running process owns the file. Heavyweight; introduces a new lifecycle.

### F. Hybrid

#### F1. SDK provides B1 (`gc events rotate`); pack provides D1 (`mol-dog-rotate` order calling B1)

Layered: SDK exposes the primitive, pack composes the schedule. The maintenance pack already follows this shape for `gc dolt gc-nudge`, `gc dolt cleanup`, etc.

#### F2. SDK provides A1 (auto-rotation in the recorder); SDK also exposes B1 (`gc events rotate` for manual/scripted overrides)

**SDK-only hybrid.** Every component lives in `internal/events/` + `cmd/gc/`. Pack involvement is optional (a pack may *additionally* schedule `gc events rotate` if it wants finer scheduling, but the SDK doesn't depend on it).

### G. Options the brief did not list (added during the survey)

#### G1. Daemon executor with ZFC-exempt formula (mol-dog-compactor pattern, per #1557)

Ship a `mol-dog-rotate.toml` formula that **documents observable structure** while a Go daemon (or controller goroutine) is the actual executor. The formula is "for observability only" per the precedent in `examples/dolt/formulas/mol-dog-compactor.toml`:

> This formula is used for observability tracking only — the compactor daemon code is the executor. The compactor is exempt from agent-driven formula execution (ZFC) because: operations require SQL connections via database/sql (agents lack SQL access), transactional state spans multiple queries, branch creation/deletion requires cleanup-on-failure error paths, concurrent write retry needs error classification, integrity verification compares row counts before/after.

For rotation the same logic applies: file rename + flock are not LLM-amenable. But this option only adds value if the observability framing is independently desired, and it duplicates the controller-tick log of "I rotated at 16:04 PDT, freed 100 MB."

#### G2. Hardcoded-constant rotation in FileRecorder itself

Bake `maxSize := 100 * 1024 * 1024` and `keepFiles := 5` directly into `recorder.go`, like `cmd/gc/session_reconciler_trace_store.go:24-25`'s `sessionReconcilerTraceMaxSegmentBytes = 16 << 20`. **Fails ZFC** — these are operationally tunable thresholds, and event-volume varies by 100× across cities. Listed for completeness so the design doc closes that door explicitly.

#### G3. Rotation by a side-channel (separate process via fork/exec from the controller)

The controller forks `gc events rotate` as a subprocess on a tick rather than rotating in-process. Avoids any in-process synchronization risk; pays a process-spawn per rotation. This is essentially "C1, but exec'd to itself" — strictly worse than C1 unless we believe in-process rotation is risky enough to justify the fork.

## 4. Pros / cons matrix

Each option is scored on the nine criteria below. The first three are the Primitive Test (`engdocs/contributors/primitive-test.md`); the rest are operational.

| Option | ZFC | Bitter Lesson | Primitive Test | Layering (SDK / pack / external / hybrid) | Impl cost | Operator ergonomics | Recoverability | Observability | SDK self-sufficient |
|---|---|---|---|---|---|---|---|---|---|
| **A1** RotatingFileRecorder (size-only) | ✅ pass — Go is transport | ✅ pass — model-invariant primitive | ✅ atomic+transport+derived | SDK | M (1 new file, 1 conformance suite extension, providers.go branch) | Transparent — works while user sleeps | ✅ inline best-effort, errors → stderr like FileRecorder | Emit `events.rotated` event per rotation | ✅ |
| **A2** TailProvider with size budget | ✅ pass | ✅ pass | ⚠ destructive: history erased without consent | SDK | M | Same as A1 | ✅ | Same | ✅ |
| **A3** Compaction (content-aware) | ❌ fail — "is this event noise?" is judgment | ❌ fail — model would do this better | ❌ | SDK | L | Lossy, surprising | ⚠ partial rewrites possible | Same | ✅ |
| **B1** `gc events rotate` CLI | ✅ pass | ✅ pass | ✅ | SDK + CLI | S | Manual / scripted only | ✅ | ✅ via stdout summary | n/a (not a scheduler — companion to A1/C1) |
| **B2** `gc events compact` CLI | ❌ same as A3 | ❌ same | ❌ | SDK + CLI | S | — | ⚠ | — | n/a |
| **B3** `doctor --fix` runs rotation | ✅ if delegating to A1 | ✅ | ✅ if delegating | SDK | XS | Discoverability win | ✅ | ✅ | ✅ |
| **C1** Controller goroutine (wisp-GC twin) | ✅ pass | ✅ pass | ✅ | SDK | M | Transparent | ✅ | ✅ tick log | ✅ |
| **C2** Built-in SDK order | ✅ | ✅ | ✅ | SDK | M+ (order plumbing) | Transparent | ✅ | ✅ structured order beads | ✅ |
| **D1** `mol-dog-rotate` exec order | ✅ if script is pure transport | ✅ for size-based | ⚠ — depends on pack | Pack | S | Transparent only if pack present | ⚠ #1275 pattern: silent failure → late escalation | ⚠ shell stdout | ❌ — fails if user removes pack |
| **D2** Extend `mol-dog-jsonl` | — | — | — | Pack | S | ❌ already broken by default (#1275) | ❌ | ❌ | ❌ |
| **D3** Hook on doctor warning event | ✅ if pack-side | ✅ | ⚠ | Pack | M | Reactive only — no warning, no rotation | ⚠ | ⚠ | ❌ |
| **D4** Deacon prompt clause | ✅ judgment in prompt is fine | ✅ | ⚠ | Pack | XS | Latency depends on deacon health | ⚠ | ⚠ | ❌ |
| **E1** logrotate / launchd | ✅ (no Go) | ✅ | ✅ but external | External | XS (docs) | ❌ requires per-OS setup; jwp23's existing workaround | ✅ | ❌ | ❌ but admittedly the bar is "controller alone runs" — external is a non-answer |
| **E2** Sidecar daemon | ✅ | ✅ | ⚠ adds new lifecycle primitive | External | XL | New process to manage | ✅ | ✅ | ❌ |
| **F1** SDK B1 + pack D1 | ✅ | ✅ | ✅ via composition | Hybrid | S+M | Transparent if pack is present | ⚠ pack-conditional | ✅ | ❌ — drops to D1's failure if pack absent |
| **F2** SDK A1 + SDK B1 (SDK-only hybrid) | ✅ | ✅ | ✅ | SDK | M+S | Transparent + scriptable | ✅ | ✅ | ✅ |
| **G1** Daemon executor + ZFC-exempt formula | ✅ | ✅ | ⚠ over-engineered for the size of the problem | SDK + pack-doc | M+ | Transparent | ✅ | ✅ via formula step beads | ✅ |
| **G2** Hardcoded constants in FileRecorder | ❌ thresholds in code | ⚠ borderline | ❌ | SDK | XS | ❌ same threshold for every city | ✅ | ⚠ | ✅ |
| **G3** Fork-exec from controller tick | ✅ | ✅ | ⚠ extra process per rotation | SDK | S | Transparent | ✅ | ✅ via subprocess events | ✅ |

## 5. Recommendation

**Adopt F2: an SDK-only hybrid of A1 (RotatingFileRecorder, size-only, default-OFF) plus B1 (`gc events rotate` CLI).** Skip C1 unless a follow-up shows that inline-on-write rotation overshoots the size budget too far during write bursts. Defer A3/B2 (content-aware compaction) indefinitely — that's a different design that fails Bitter Lesson on its current shape and is partially superseded by the `gc-zykvg` payload-delta work attacking the same 42% `bead.updated` slice at write-time.

### 5.1 Why F2 over the alternatives

**Why SDK over pack (rules out D1, D2, D3, D4, F1, E1, E2).** The `events.jsonl` file is SDK infrastructure: every recorder constructor in the codebase writes to `$cityPath/.gc/events.jsonl`, derived from `citylayout.RuntimeRoot`, with no config knob to relocate it. Per `engdocs/architecture/controller.md:203`, every controller operation must function "with only the controller process running. No user-configured agent role is required." Pack-side scheduling fails this test the moment the user removes their maintenance pack — and the upstream evidence is unequivocal that pack-shipped maintenance defaults are operator-hostile. Issue [#1275](https://github.com/gastownhall/gascity/issues/1275) demonstrates this directly: every default gascity install today emits `ESCALATION: JSONL push failed [HIGH]` every 45 minutes after `gc start`, because `mol-dog-jsonl` ships default-on with a script that pushes to an unconfigured remote and swallows stderr until the third consecutive failure. jwp23 on [#1118](https://github.com/gastownhall/gascity/issues/1118) further notes that `gc reload` reverts edits to pack files — so even sophisticated operators can't safely patch a pack-side rotation. Putting `events.jsonl` rotation in the pack adds the same failure modes to a load-bearing infrastructure file.

**Why size-only over content-aware (rules out A3, B2).** Pure size-based rotation passes all three legs of the Primitive Test. *Atomicity*: the FileRecorder mutex + `flock` already serialize writes; rotation is an additional step inside the same critical section. *Bitter Lesson*: "split when 1 GB" is a definition, not a heuristic — a smarter model can't improve "split when 1 GB." *ZFC*: rename + prune are pure transport. Content-aware compaction fails Bitter Lesson immediately because *which events are safe to discard* is exactly the kind of judgment a smarter model would make better from a prompt with more context. The honest application of `engdocs/contributors/primitive-test.md`:

> Rotation that includes heuristics like "discard events older than 72 h if disk < 80% full" would *fail* Bitter Lesson — that's judgment a smarter model would do better from a prompt. Pure size-based splitting *passes*.

**Why A1 (inline) over C1 (controller tick).** Three reasons. First, **precedent**: `cmd/gc/session_reconciler_trace_store.go:370-374` already rotates inline on the write path inside `AppendBatch` (`if s.currentBytes >= sessionReconcilerTraceMaxSegmentBytes || s.currentBatches >= sessionReconcilerTraceMaxBatches { s.rotateSegment(now) }`). The shape is in-tree. Second, **bound tightness**: an inline check fires the moment a write crosses the threshold, so the size budget is always respected within one event's worth of slack. C1's tick-based check (default 30 s) lets a high-write burst overshoot by potentially hundreds of MB. Third, **simplicity**: A1 needs no new tracker plumbing — the controller doesn't need to know rotation exists. The recorder owns its own lifecycle.

**Why also B1 (rules in the operator-facing CLI even though A1 already auto-rotates).** Three orthogonal use cases. (a) **Forced rotation for one-shot operator runs**: rotate before snapshotting an install for a bug report, before a `gc dolt cleanup`, etc. (b) **Scripted scheduling as a fallback**: a sophisticated operator can wire `gc events rotate --max-size 50M --keep 10` into launchd or cron, mirroring jwp23's existing workaround pattern but without leaving the SDK contract. (c) **Bug-reporting parity with the maintainer-suggested fix**: the upstream issue body asked for it explicitly; saying yes to that ask is cheap and high-signal.

**Why default-OFF rather than default-ON.** This deserves an explicit defense. The #1275 cautionary tale is that Gas City has burned operators by shipping maintenance defaults that fail loudly on day-1. Default-OFF means an existing install that pulls this change continues to behave bit-for-bit identically until the operator opts in (`max_size = "100M"` in `[events]`). Once we have field evidence from a second release that A1 is benign — no operator surprises, no events lost, watch path stable across rotation — promote to default-ON in a follow-up. We can also flip the doctor `FixHint` to nudge operators toward the config as soon as A1 ships, even before the default flips. This sequencing is borrowed from the `[daemon].wisp_gc_interval` pattern: shipped optional, promoted later.

### 5.2 Engdocs precedents this leans on

- **Wisp-GC nil-guard pattern** (`cmd/gc/wisp_gc.go:14-42`) — `newWispGC(interval, ttl)` returns `nil` when either is zero; the controller nil-guards before calling. We mirror it for the rotation knob: `NewRotatingFileRecorder(path, maxSize, keepFiles, stderr)` returns a plain `*FileRecorder` when `maxSize == 0`. No nil-guard at the call site is needed because the wrapper's API surface is identical.
- **Trace-segment rotation** (`cmd/gc/session_reconciler_trace_store.go:370-452`) — in-tree precedent for "rotate when the file crosses a size threshold inside the same write critical section." The differences are intentional: trace store hardcodes `sessionReconcilerTraceMaxSegmentBytes = 16 << 20` because trace files are short-lived and tick-bounded; events.jsonl is long-lived and load-varying, so its threshold belongs in TOML.
- **Best-effort recording** (`engdocs/architecture/event-bus.md:180-181`) — `Record` errors are logged to stderr, never returned. Rotation must inherit this: a failed rename or prune logs to the same stderr (the FileRecorder's existing `r.stderr`), never returns from `Record`, never blocks the caller.
- **Provider abstraction** (`engdocs/architecture/event-bus.md:55-69`) — events providers already compose (`FileRecorder`, `Fake`, `exec.Provider`). RotatingFileRecorder is the next composition layer wrapping FileRecorder, the same way Multiplexer wraps multiple Providers without owning durability.
- **Config layering** (`engdocs/architecture/config.md` `[daemon]` patterns) — `interval` and `ttl` keys live in `[daemon]`, are duration-typed, and absent-means-disabled. `[events] max_size` and `[events] keep_files` follow the same pattern: size-typed (string parseable as bytes), absent means disabled.
- **`engdocs/design/external-messaging-fabric.md` retention defaults** — the messaging fabric retains closed binding beads for 30 days, deliveries for 7, group sessions for 90. The pattern is "ship a default, document it, allow override." Events rotation defaults follow the same shape.
- **`engdocs/design/idle-session-sleep.md` durable fingerprint** — the idle-sleep machinery survives controller restart by encoding fingerprints. Rotation similarly recovers global max seq across all sibling files at construction time, so a fresh process never re-issues a seq from a rotated file.

### 5.3 What this design explicitly does not do

- **Does not fix `/v0/city/{name}/events` HTTP timeouts.** That's a separate root cause in `internal/events/multiplexer.go`'s serial fan-out, separately tracked in [#1487](https://github.com/gastownhall/gascity/issues/1487). The pitch is honest about this: jwp23's "rotation will relieve operational pressure" hypothesis on #1118 was partially falsified by direct measurement. Rotation is still warranted on the four other grounds in §1.3.
- **Does not redesign the event bus.** No changes to `Provider`, `Recorder`, `TailProvider`, `Watcher` interfaces. RotatingFileRecorder satisfies them as-is.
- **Does not address `bead.updated` payload bloat.** That's [`gc-zykvg`](#references) — adjacent and complementary. Rotation bounds the file; payload-deltas reduce per-event size. They compose multiplicatively.
- **Does not introduce content-aware compaction.** Deferred per the Primitive Test. If a future need is real, it lives in a follow-up design that earns its keep against Bitter Lesson.
- **Does not promote rotation to default-ON in v1.** That's a follow-up after one release of field evidence.

## 6. Implementation sketch

### 6.1 Files to touch

| File | Change | Size |
|---|---|---|
| `internal/events/rotating.go` | NEW — `RotatingFileRecorder` type, `NewRotatingFileRecorder` constructor, rotation helpers (`rotateNowLocked`, `renameSequence`, `pruneOldFiles`, `readGlobalMaxSeq`, `rotatingWatcher` for cross-rotation watch continuity) | ~250 lines |
| `internal/events/rotating_test.go` | NEW — rotation-specific unit tests (size threshold trigger, rename ordering, prune, cross-rotation seq monotonicity, cross-rotation `List`/`ListTail` merge, `Watch` continuity over rotation, multi-process rename race via `t.TempDir()` flock) | ~400 lines |
| `internal/events/conformance_test.go` | EXTEND — add `TestRotatingFileRecorderConformance` factory invoking `NewRotatingFileRecorder(path, smallMaxSize, 5, &stderr)`. Forces every existing conformance test to also hold over a recorder that rotates mid-test. | ~30 lines |
| `internal/events/eventstest/conformance.go` | EXTEND — add `RunRotationTests` suite (size-monotonicity, cross-file Watch, restart recovery). Public so any future provider can opt in. | ~150 lines |
| `internal/events/recorder.go` | NO CHANGE | — |
| `internal/events/multiplexer.go` | NO CHANGE — multiplexer is read-only over Providers; A1 satisfies `Provider` | — |
| `internal/config/config.go` | EXTEND `EventsConfig`: `MaxSize string`, `KeepFiles int`. Add `MaxSizeBytes() (int64, error)` accessor that parses `"100M"`, `"1G"` etc. | ~30 lines |
| `cmd/gc/providers.go` | EXTEND the FileRecorder factory: when `cfg.Events.MaxSize` parses to >0, return `NewRotatingFileRecorder(path, bytes, cfg.Events.KeepFiles, stderr)`; otherwise return `NewFileRecorder(path, stderr)`. | ~20 lines |
| `cmd/gc/cmd_events.go` | NEW subcommand `gc events rotate`. Routes through the existing `apiClient()` to a new POST endpoint on the API control plane. Flags: `--force`, `--max-size`, `--keep`, `--out json|text`. | ~80 lines |
| `internal/api/...` | NEW Huma-registered endpoint `POST /v0/city/{name}/events/rotate` with typed request/response. Per `engdocs/contributors/huma-usage.md`, the endpoint must be Huma-registered and the OpenAPI spec regenerated. | ~80 lines |
| `internal/doctor/checks_semantic.go` | UPDATE `EventLogSizeCheck.Run` `FixHint`: include `"or set [events] max_size in city.toml to enable automatic rotation; gc events rotate forces rotation now"`. | 1 line |

Total: ~1100 lines, mostly tests and a single new recorder type.

### 6.2 Type sketch

```go
// internal/events/rotating.go

// RotatingFileRecorder wraps FileRecorder and rotates the active file when
// it exceeds maxSize. Reads merge across active and retained sibling files.
//
// Disabled at construction time when maxSize is zero — falls back to a
// plain *FileRecorder so callers cannot tell the difference.
type RotatingFileRecorder struct {
    mu        sync.Mutex          // guards active + rotation
    active    *FileRecorder       // currently open
    path      string              // base path, e.g. .gc/events.jsonl
    maxSize   int64               // 0 = disabled (caller should use NewFileRecorder directly)
    keepFiles int                 // siblings retained after rotation (≥1)
    stderr    io.Writer
    closed    bool
}

// NewRotatingFileRecorder returns a rotating recorder, OR a plain
// FileRecorder if maxSize ≤ 0. This lets callers always call this
// constructor without nil-guarding.
//
// Caller must close the returned Provider.
func NewRotatingFileRecorder(path string, maxSize int64, keepFiles int, stderr io.Writer) (events.Provider, error) {
    if maxSize <= 0 {
        return NewFileRecorder(path, stderr)
    }
    if keepFiles < 1 {
        keepFiles = 1
    }

    // Recover global max seq across active + sibling files so a fresh
    // process never re-issues a seq from a rotated file.
    maxSeq, err := readGlobalMaxSeq(path, keepFiles)
    if err != nil {
        return nil, err
    }
    active, err := NewFileRecorder(path, stderr)
    if err != nil {
        return nil, err
    }
    if active.seq < maxSeq {
        active.seq = maxSeq
    }
    return &RotatingFileRecorder{
        active:    active,
        path:      path,
        maxSize:   maxSize,
        keepFiles: keepFiles,
        stderr:    stderr,
    }, nil
}

func (r *RotatingFileRecorder) Record(e events.Event) {
    r.mu.Lock()
    defer r.mu.Unlock()
    if r.closed {
        return
    }
    r.active.Record(e)

    // Best-effort size check. Stat error → log + skip; rotation error →
    // log + continue (caller's operation must not fail).
    info, err := os.Stat(r.path)
    if err != nil {
        fmt.Fprintf(r.stderr, "events rotate stat: %v\n", err)
        return
    }
    if info.Size() <= r.maxSize {
        return
    }
    if err := r.rotateNowLocked(); err != nil {
        fmt.Fprintf(r.stderr, "events rotate: %v\n", err)
    }
}

// rotateNowLocked must be called with r.mu held.
//
// 1. Close the active recorder (releases flock, syncs via *os.File.Close)
// 2. Numbered shift: .keepFiles → discard, .keepFiles-1 → .keepFiles, …, .1 → .2, base → .1
// 3. Reopen base as a fresh active recorder, carrying r.active.seq forward
// 4. Emit "events.rotated" so the rotation appears in the event stream itself
//    (closing the loop: the last event in .jsonl.1 is "I rotated to here")
func (r *RotatingFileRecorder) rotateNowLocked() error {
    // …implementation per the §6.3 invariants…
}

// Read paths merge across rotated siblings so consumers see one logical stream.
func (r *RotatingFileRecorder) List(filter events.Filter) ([]events.Event, error)        { /* … */ }
func (r *RotatingFileRecorder) ListTail(filter events.Filter, n int) ([]events.Event, error) { /* … */ }
func (r *RotatingFileRecorder) LatestSeq() (uint64, error)                                { /* … */ }
func (r *RotatingFileRecorder) Watch(ctx context.Context, afterSeq uint64) (events.Watcher, error) { /* … */ }
func (r *RotatingFileRecorder) Close() error                                              { /* … */ }
```

### 6.3 Invariants the tests must enforce

These are the Primitive-Test atomicity tests applied to the failure modes specific to file rotation:

1. **Seq monotonicity across rotation.** A consumer that read `Seq=N` from `.jsonl.1` and resumes via `Watch(ctx, N)` against the active file must see `N+1` next, never less.
2. **No event lost during rotation.** Every event that returned from `Record()` must appear in either the active file or a retained sibling. Test pattern: 10 goroutines × 1000 events with maxSize tuned to force ~3 rotations; assert exactly 10 000 events total across all sibling files.
3. **`Watch` continuity.** A long-running watcher started before rotation must yield events that were written into `.jsonl.1` after the watcher attached but before the rotation rename. Test pattern: attach watcher with `afterSeq=0`, write 100, rotate, write 100 more; watcher must yield 200 in monotonic order.
4. **Multi-process rotation race.** Two processes flock-open the same path; only one decides to rotate; the loser's next `Record()` reopens fresh active. Test pattern: two `*FileRecorder`s in the same `t.TempDir()`; both write past threshold; assert no event lost, no duplicate seq.
5. **`Close()` idempotence under rotation.** Calling `Close()` mid-rotation must not corrupt the rename ladder.
6. **Restart recovery.** Process A writes 1000, rotates (.1 has 600, active has 400), exits. Process B starts; `LatestSeq()` returns 1000 not 400.
7. **Disabled path is bit-for-bit identical.** `NewRotatingFileRecorder(p, 0, 0, e)` must be observationally indistinguishable from `NewFileRecorder(p, e)` — including the same conformance suite results.

### 6.4 TDD sequence

Write each test, watch it fail, then implement. Order optimized so each step is locally proveable:

1. RED: `TestRotatingFileRecorderDisabledPathMatchesFileRecorder` — empty maxSize returns *FileRecorder, all conformance tests pass on the wrapped result.
2. GREEN: implement constructor that delegates when maxSize == 0.
3. RED: `TestRotateOnSizeThreshold` — write past threshold, assert sibling appears at `.1` with the right contents.
4. GREEN: implement `rotateNowLocked` (close + rename + reopen, no prune yet).
5. RED: `TestRotateRenameLadder` — multiple rotations produce `.1, .2, .3, …` in order.
6. GREEN: implement `renameSequence` (shift loop).
7. RED: `TestRotatePrune` — keepFiles=3, do 5 rotations, assert only `.1, .2, .3` remain.
8. GREEN: implement `pruneOldFiles`.
9. RED: `TestSeqMonotonicityAcrossRotation`.
10. GREEN: implement `readGlobalMaxSeq` (scan siblings + active for max seq) and carry it into the active recorder on construct + after rotation.
11. RED: `TestListMergesAcrossSiblings`.
12. GREEN: implement read-side merge.
13. RED: `TestWatchContinuityAcrossRotation`.
14. GREEN: implement `rotatingWatcher` that detects rotation (file inode change via stat) and reattaches.
15. RED: `TestMultiProcessRotationRace`.
16. GREEN: confirm flock+stat-after-write semantics handle the race; add fix if not.
17. RED: `TestRecordNeverPanicsOnRotationFailure` — point keepFiles into a non-writable dir; assert errors go to stderr only.
18. GREEN: ensure errors are wrapped + logged + dropped.
19. RED: extend conformance suite — `RunProviderTests` and `RunConcurrencyTests` against the rotating factory, with maxSize tuned to force rotation mid-test.
20. GREEN: pass the full suite.
21. CLI: implement `gc events rotate` against a new Huma endpoint; e2e test with `httptest.Server`.
22. Doctor: update FixHint, add unit test for the new wording.

### 6.5 TOML schema additions

```toml
# city.toml
[events]
provider = ""              # existing — unchanged
max_size = ""              # NEW — e.g. "100M". Empty/zero = rotation disabled.
keep_files = 5             # NEW — sibling files retained after rotation. Default 5.
```

`MaxSize` is a string so we get the `"100M"`/`"1G"` ergonomics already present in human-facing config sections; parse via a new `EventsConfig.MaxSizeBytes()` accessor that returns `(int64, error)` and follows the existing `time.Duration` accessor pattern in `[daemon]`.

### 6.6 Endpoint shape (Huma-registered)

```go
// internal/api/events_rotate.go (new file)

type RotateEventsRequest struct {
    Body struct {
        Force       bool   `json:"force,omitempty"`
        MaxSizeStr  string `json:"max_size,omitempty"`   // override config for this call
        KeepFiles   int    `json:"keep_files,omitempty"`
    } `contentType:"application/json"`
}

type RotateEventsResponse struct {
    Body struct {
        Rotated   bool   `json:"rotated"`
        FromBytes int64  `json:"from_bytes"`
        ToBytes   int64  `json:"to_bytes"`
        ArchivedTo string `json:"archived_to,omitempty"` // e.g. ".gc/events.jsonl.1"
        Pruned    []string `json:"pruned,omitempty"`    // siblings dropped
    } `contentType:"application/json"`
}
```

Per `engdocs/contributors/huma-usage.md`, the OpenAPI spec must regenerate (`make openapi`); the dashboard build runs `make dashboard-check` to confirm typed-wire downstream compatibility.

## 7. Open questions

These are the questions the corpus could not resolve unambiguously. Each one is a small decision the Overseer + crew can settle quickly during review.

1. **Default `max_size` once we promote to default-ON.** Aligning with the doctor's existing 100 MB warning is the obvious choice (operators already understand this number). But the warning fires *before* rotation kicks in, which is awkward: we'd want the rotate-trigger to be *below* the warning so the warning state never persists. Proposed: `max_size = "75M"`, leave doctor at 100 MB. Tiebreaker question for review.
2. **Default `keep_files`.** 3 (≤4× the active size on disk) vs 5 (≤6×) vs 10 (~10×). With 75 MB active, 5 retained = ~525 MB worst case, vs the current unbounded growth at >100 MB/day. Proposed: 5.
3. **`max_size` units.** `"100M"` (current convention in similar fields) vs `100_000_000` (raw bytes) vs `time.Duration`-style typed. Proposed: human strings parsed into bytes.
4. **Should rotated siblings be gzip-compressed?** ~70% size reduction on JSONL is plausible. Adds a dependency on `compress/gzip` (stdlib) and complicates `ReadFiltered`/`ReadFilteredTail` over siblings. Proposed: defer to a follow-up; ship uncompressed first.
5. **Do we emit an `events.rotated` event into the new active file at the moment of rotation?** Yes for forensic clarity (it shows in `events.jsonl` itself: "rotation happened here, see .jsonl.1 for prior history"). Proposed: yes. Question: payload shape (`from_bytes`, `archive_path`, `seq_at_rotation`)?
6. **Multi-process flock semantics during rotation.** The supervisor process and the city controller both write to the same `events.jsonl` (per `cmd/gc/cmd_supervisor.go`). Two flock-holders can each independently decide to rotate. Proposed handling: after `flock(LOCK_EX)`, re-stat — if size now ≤ `maxSize`, the other process already rotated and we don't. Need test coverage in `TestMultiProcessRotationRace`.
7. **`gc events rotate` from a remote operator.** The CLI talks to the API control plane (per `engdocs/architecture/api-control-plane.md`'s "object model at the center"). For multi-city installs, what's the addressing? Proposed: `gc events rotate --city <name>`; default current city.
8. **Should `gc doctor --fix` invoke rotation?** Adds discoverability but couples doctor to write-path concerns it doesn't currently own. Proposed: no for v1; add a follow-up bead if operator demand emerges.
9. **Promotion timeline.** Ship default-OFF in release N, observe one release of field data, promote to default-ON in N+1 (mirroring how `[daemon].wisp_gc_interval` shipped). Confirm sequencing.

## 8. Risks

### 8.1 Implementation risks

- **Watch continuity is the trickiest piece.** The current `fileWatcher` (recorder.go:147-211) tracks a byte offset into the open file. After a rename, that offset points into `.jsonl.1` — correct in spirit, but the watcher must detect the rename (stat → inode change) and either continue reading from `.jsonl.1` or re-attach to the new active file. Mitigation: extensive `TestWatchContinuityAcrossRotation` coverage; consider a small abstraction (`rotatingWatcher`) that owns this complexity rather than baking it into `fileWatcher`.

- **Multi-process race during rename.** `os.Rename` is atomic on the same filesystem on macOS/Linux, but the *decision* to rotate isn't. Two processes can each `flock`, write past threshold, decide to rotate; the second's rename moves a file that's already been moved. Mitigation: re-check size after acquiring `flock` (the existing `ReadLatestSeq` re-read pattern is a precedent); test with `TestMultiProcessRotationRace`.

- **Crash mid-rotation.** Process dies between `os.Rename` and `os.OpenFile`. Next process construction recovers `seq` via `readGlobalMaxSeq` (scans active + siblings) and opens a fresh active file. Worst case: a brief gap in monotonic timestamps, no events lost. Mitigation: explicit `TestCrashMidRotation` (kill -9 simulation in test infrastructure).

- **Rename across filesystems.** If `.gc/` and `/tmp/` straddle filesystems on a weird setup, `os.Rename` falls back to copy+remove with non-atomic semantics. Mitigation: rotation happens within `.gc/` (same parent dir as the file); test with `t.TempDir()` which is on the same filesystem.

- **Reader-while-rotating.** A `gc events --follow` process holds the file open; rotation renames it. Linux/macOS: the open fd remains valid against the renamed inode, so the follower keeps reading the now-`.1` file until EOF, then must re-open the new active file. Mitigation: covered by `TestWatchContinuityAcrossRotation`; same logic the watcher needs.

### 8.2 Operational risks

- **"Where did my events go?"** Operators who don't know about `keep_files` may report bugs about apparent event loss after pruning. Mitigation: doctor `FixHint` mentions the behavior; release notes call it out; default `keep_files = 5` (≥1 day of evidence at current write rates).

- **Default-ON regret.** If we flip the default in a future release and a corner case manifests, every fresh install hits it. Mitigation: deliberate default-OFF in v1; one full release of field telemetry before promoting; the change to default-ON is a one-line config-default change, easily reverted.

- **Disk-pressure regression.** Five 100 MB siblings = ~500 MB worst case, vs the current "unbounded but no automatic deletion." Some operators may have monitoring tuned to "no automatic cleanup happens" and find the new behavior surprising. Mitigation: opt-in.

- **Compaction expectations.** Operators who read this design and want compaction (drop heartbeats) will be disappointed. Mitigation: §5.3 calls out that compaction is deferred and partially superseded by `gc-zykvg`; the doctor message could optionally mention payload-bloat as a related-but-separate concern.

### 8.3 Architectural risks

- **Convergence with the trace-store rotation pattern.** Once both subsystems rotate, a tempting refactor is "extract a generic `RotatingFile` primitive." We deliberately don't. The two subsystems differ enough (trace-store is short-lived per-tick, segment-bounded by structural constants; events.jsonl is long-lived, threshold-tunable) that the abstraction would over-fit. Mitigation: leave them parallel for two releases, revisit only if a third file-lifecycle subsystem appears.

- **Promoting rotation to a "primitive."** It is *not* a primitive (`engdocs/contributors/primitive-test.md`); it is derived from FileRecorder + Config + Controller. The implementation should be a wrapper, not a new interface. Resist the pull to add `Provider.Rotate() error` to the `events.Provider` interface — that would force every provider (Fake, exec, future stores) to think about rotation when only the file-backed one needs to.

## 9. References

### 9.1 Engdocs cited

- `engdocs/architecture/event-bus.md` (lines 154-157, 180-181, 310-318, 391-394, 396-400, 410-413) — FileRecorder description, best-effort recording invariant, append-only invariant, Known Limitations gap call-out, ReadFiltered scan note, exec-Watch lifetime caveat.
- `engdocs/architecture/nine-concepts.md` (Layer 0-1 primitives, Layering Invariants 159-174, Primitive Test triad) — placement of Event Bus, derived-mechanism pattern, primitive criteria.
- `engdocs/architecture/controller.md` (lines 9-16 summary, 59-95 scope, 196-198 + 203 SDK self-sufficiency invariant) — controller-loop scope, no-role-names invariant.
- `engdocs/architecture/health-patrol.md` (lines 50-60 nil-guard composition, 99-127 controller-loop pattern, 242-244 SDK self-sufficiency) — analogous controller-driven composition pattern.
- `engdocs/architecture/config.md` (TOML duration/threshold patterns, `[daemon]` section conventions) — config schema precedent.
- `engdocs/architecture/glossary.md` (lines 159-162 ZFC definition) — rule we're defending.
- `engdocs/architecture/api-control-plane.md` — "object model at the center" rule for new HTTP endpoints.
- `engdocs/architecture/orders.md` — order-vs-patrol scheduling shape.
- `engdocs/architecture/dispatch.md` — agent-pool dispatch semantics (relevant to ruling out pack-side options).
- `engdocs/contributors/primitive-test.md` (full document, lines 9-64 verbatim) — the decision framework applied to each option.
- `engdocs/contributors/huma-usage.md` — typed-wire endpoint construction.
- `engdocs/contributors/codebase-map.md` — package boundaries.
- `engdocs/contributors/reconciler-debugging.md` (line 65) — events.jsonl is load-bearing for forensics.
- `engdocs/design/external-messaging-fabric.md` — retention defaults precedent (30/7/90 days).
- `engdocs/design/idle-session-sleep.md` — durable fingerprint precedent for re-arm behavior.
- `engdocs/design/session-lifecycle-domain-cleanup-plan.md` — centralized-transition-writers precedent.
- `engdocs/design/machine-wide-supervisor-v0.md` — Bitter Lesson invocation precedent.
- `engdocs/design/agent-pools.md` — ZFC-compliance precedent (user-owned scaling decisions).
- `engdocs/design/index.md` — design-document status conventions.

### 9.2 Code cited

- `internal/events/recorder.go:20-27` (FileRecorder fields), `:32-53` (constructor), `:57-94` (Record), `:96-112` (List/ListTail/LatestSeq), `:115-132` (Watch), `:136-144` (Close), `:147-211` (fileWatcher).
- `internal/events/multiplexer.go:27-30` (type), `:76-92` (ListAll), `:97-129` (ListTail), `:133-147` (LatestCursor), `:149-213` (Watch attach loop).
- `internal/events/events.go:110-114` (Recorder), `:120-136` (Provider), `:140-142` (TailProvider), `:144-156` (Watcher).
- `internal/events/conformance_test.go:12-26` (FileRecorder factory pattern).
- `internal/doctor/checks_semantic.go:139-184` (`EventLogSizeCheck`).
- `cmd/gc/cmd_events.go:110-183` (current `gc events` cobra command).
- `cmd/gc/wisp_gc.go:14-42` (nil-guard tracker pattern).
- `cmd/gc/controller.go:226` (controllerLoop).
- `cmd/gc/session_reconciler_trace_store.go:24-25` (hardcoded constants — contrast example), `:344-437` (AppendBatch with rotation check), `:439-452` (`rotateSegment`).
- `cmd/gc/providers.go` (FileRecorder construction call site).

### 9.3 Upstream issues

- [#1118](https://github.com/gastownhall/gascity/issues/1118) — primary ask. Reporter + jwp23 comment. No maintainer comment.
- [#1275](https://github.com/gastownhall/gascity/issues/1275) — `mol-dog-jsonl` broken-by-default. Cautionary tale for pack-side defaults.
- [#1487](https://github.com/gastownhall/gascity/issues/1487) — events HTTP timeout. Our A3Ackerman comment empirically falsifies the "rotation fixes timeouts" hypothesis. Separate root cause in multiplexer fan-out.
- [#1557](https://github.com/gastownhall/gascity/issues/1557) — `mol-dog-compactor.compact` has no executor. Cautionary tale for shipping a formula without a runner.
- [#1150](https://github.com/gastownhall/gascity/pull/1150) (open PR) — `feat(supervisor): Dolt store maintenance loop + CLI (ADR 0002)`. Closest precedent for the "SDK + CLI bundle" shape we propose.

### 9.4 Local beads

- `gc-n0mqg` (P2) — the bug bead this design will resolve.
- `gc-juwa3` (P2) — the work bead for this design.
- `gc-zykvg` (P3) — adjacent: payload-deltas on `bead.updated` events. Composes multiplicatively with rotation; out of scope here.
- `gc-j50tc` (P2) — adjacent: missing `mol-dog-compactor` formula on the pack side.
- `gc-smq3z` (P1, in_progress) — adjacent: dashboard `/events` fanout hang. Same root cause as upstream #1487 (multiplexer fan-out).
- `gc-icpur` (P2) — `gc-flp1` layer 4: max_active_sessions distinguishes crew vs polecat. Unrelated to this design.
- `gc-6ed5z` (P2) — `gc-flp1` layer 5: structured event on every session-lifecycle action. Unrelated but reinforces "events.jsonl is load-bearing for forensics."

### 9.5 Workaround precedents

- Issue #1118 (azanar): "Custom order + script that truncates to last 10 k lines when file exceeds 50 MB."
- Issue #1118 (jwp23): "`~/Library/LaunchAgents/` plist + script in `scripts/`, not in the maintenance pack, because edits to pack files get reverted by `gc reload`."

---

*End of design pitch. Reviewers: please tag with status `Accepted` or comment with concrete redirections; the next step is an implementation bead off this proposal.*
