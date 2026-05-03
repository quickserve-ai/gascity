# events.jsonl: retention strategy (pack-level chosen; upstream tiers deferred)

**Status**: **Decided 2026-05-03 — pack-level retention. Upstream-implementation tiers deferred for at least 1 week.** This document preserves both options for posterity.
**Filed**: 2026-05-03
**Companion full design**: `engdocs/proposals/events-jsonl-rotation-design.md` (582-line research-grade survey)
**Related upstream**: gastownhall/gascity#1118 (events.jsonl unbounded growth — the original ask)

---

## Decision summary (read this first)

**The chosen near-term fix is a pack-level truncate order in `packs/maintenance/`** that runs every 30min, checks if `events.jsonl` exceeds 500MB, and if so keeps only the last 50k lines. Crude, simple, sufficient.

**Why pack-level over an upstream SDK fix**:

1. **Retention is fundamentally a pack/operator concern.** Different packs emit events at different rates (a pack with no patrol orders barely writes; one with many patrols writes a lot). The "right" retention policy is therefore inherently pack-specific. Pushing the mechanism into the SDK introduces config surface that each pack would just override anyway.
2. **The bead store is the durable record**, not the events log. Beads + git history capture the system's authoritative state. The events log is operational telemetry — useful for live debugging, not for long-term audit. ~1 day of recent events is plenty.
3. **The upstream tiered design is over-engineered for the actual need.** T1 (drop noise) is appealing because the 67% cache-reconcile finding is dramatic, but if we just truncate the file periodically, the noise is bounded by retention not by emit-time filtering. Same operational outcome, less SDK surface area.
4. **Upstream isn't pulling for this.** Issue #1118 has been open since 2026-04-22 with two community comments (both ours/jwp23) and no maintainer engagement. The design proposals (T1/T2/T3) would be net-new SDK surface to land + maintain. Not worth the friction unless community pressure builds.
5. **Pack-level fix is reversible.** If we later decide an upstream mechanism is the right shape, we revert the pack order and ship the SDK change. Pack-level imposes zero lock-in.

**What got built (local pack)**:
- `packs/maintenance/orders/events-truncate.toml` — cooldown order, 30min interval
- `packs/maintenance/assets/scripts/events-truncate.sh` — `tail -n KEEP_LINES > tmp; cat tmp > events.jsonl` pattern (preserves inode for the controller's open `O_APPEND` fd)
- Configurable via env: `MAX_SIZE_MB` (default 500), `KEEP_LINES` (default 50000)

**What's deferred (upstream tiers)** — preserved below for reference. Re-evaluate in ~1 week if community engagement on #1118 changes.

---

---

## Problem

`events.jsonl` grows without bound. Two installations are running launchd/cron workarounds today; doctor warns at 100MB; jwp23's #1118 comment shows operational pressure on the controller HTTP path beyond ~178MB. Engdocs (`engdocs/architecture/event-bus.md`) explicitly acknowledges the gap.

We measured a real install (qlandia, 4 days uptime, multi-rig) and found the gap is bigger than just file size: **most of the file is bookkeeping noise from the bead cache layer that duplicates information already in `bd show`.**

| Slice | Count | % of events | % of bytes |
|---|---|---|---|
| `actor=cache-reconcile` events | 205,300 / 304,156 | **67.5%** | **~290 MB / 438 MB** |
| Paired `order.fired`/`order.completed` for patrol orders | 21,390 | 7.0% | ~4 MB |
| Domain events from named actors (mayor, crew, polecats, controller, human) | ~77,000 | 25% | ~144 MB |
| Lifecycle/anomaly signal (`session.idle_killed`, `session.draining`, `mail.sent`, etc.) | ~350 | <1% | <1 MB |

The cache-reconcile events: 100% have empty `message`, 72.4% are byte-identical duplicates of the prior cache-reconcile for the same bead, and 100% of the "novel" ones are derivable from `bd show` (the bead store is the source of truth — these are the cache layer's own sync notifications, not domain events).

## Recommendation: three tiers, sequenced

A single-mechanism design (rotation alone, per the polecat's F2 in the companion doc) bounds the file size but doesn't address that 67% of what we're rotating is information-free noise. A tiered design composes deterministic noise filtering with rotation; each tier is independently shippable and pays for itself.

### Tier 1 — Configurable actor-filter mechanism (drop bookkeeping events at write time)

**Mechanism (in SDK)**: `FileRecorder` consults a TOML-configured list of actors whose events are filtered before append.

```go
// internal/events/recorder.go
func (r *FileRecorder) Record(e Event) {
    if r.shouldDrop(e) {
        return
    }
    // ... existing append path ...
}

func (r *FileRecorder) shouldDrop(e Event) bool {
    return slices.Contains(r.dropActors, e.Actor)
}
```

**TOML schema (in SDK)**:
```toml
[events]
drop_actors = []   # default: empty (opt-in). Operators/packs add actors.
```

**Smart default (in vendored gastown pack at `examples/gastown/city.toml`)**:
```toml
[events]
drop_actors = ["cache-reconcile"]   # SDK's own bead-cache bookkeeping actor
```

**Why this shape (not hardcoded actor name)**: ZFC compliance. The SDK provides the *mechanism* (filter events by actor); the *policy* (which actors are noise) lives in operator config. Other packs that emit their own bookkeeping actors can use the same mechanism without an SDK change. The bundled gastown pack ships with `cache-reconcile` as a smart default because that's the SDK's own internal cache-layer actor and dropping it is essentially universal — but the SDK code never names it.

**Why ~67% recovery**: cache-reconcile events embed full bead payloads (~1.1KB avg) but are byte-equivalent to a `bd show`. They serve no consumer (the cache layer maintains state from beads, not from its own events).

**Risk**: any consumer subscribing to a filtered actor would lose events. Audit before merge for the default (cache-reconcile) — none expected. New entries to the list need similar audit.

### Tier 2 — Configurable paired-order collapse mechanism

**Mechanism (in SDK)**: `FileRecorder` consults a TOML-configured list of order names. When an `order.completed` event matches a configured name AND the most-recent `order.fired` for that name is in the same window, replace the pair with a single `order.batch_completed` summary carrying aggregate counts.

**TOML schema (in SDK)**:
```toml
[events.dedup]
collapse_paired_orders = []   # default: empty (opt-in)
collapse_window = "5m"        # window for batching identical patrol-order pairs
```

**Smart default (in vendored gastown pack `examples/gastown/city.toml`)**:
```toml
[events.dedup]
collapse_paired_orders = [
    "gate-sweep",          # maintenance pack
    "orphan-sweep",        # maintenance pack
    "cross-rig-deps",      # maintenance pack
    "spawn-storm-detect",  # maintenance pack
    "wisp-compact",        # maintenance pack
    "mol-dog-reaper",      # maintenance pack
    "prune-branches",      # maintenance pack
    "digest-generate",     # gastown pack
]
```

**Why config-driven (not hardcoded)**: order names are pack content. A pack with a different patrol set has different names. The SDK ships only the collapse *mechanism*; each pack contributes its patrol-order names to the bundled city.toml that ships with `gc init --from <pack>`. **No order name appears in Go.**

`order.fired` + `order.completed` events for recurring patrols appear ~21,000 times in our 4-day window, perfectly paired. Each pair is informationally equivalent to "patrol X ran, succeeded" — useful in aggregate, useless individually. Collapsing recovers ~7% additional, and improves signal-to-noise dramatically (anomalies like `session.idle_killed` become greppable instead of swimming in patrol noise).

Less urgent than Tier 1; ship after T1 is in.

### Tier 3 — Size-based rotation (the polecat's F2)

`RotatingFileRecorder` wrapping `FileRecorder`, size-only, default-OFF, atomic rename to `events.jsonl.<timestamp>`. Plus `gc events rotate` CLI for manual + maintenance-pack callers. Full design in the companion doc; precedent in-tree at `cmd/gc/session_reconciler_trace_store.go:rotateSegment`.

After Tiers 1+2, rotation triggers ~3-4× less often. Still needed as the absolute file-size cap (Tier 1+2 reduce growth rate; rotation bounds peak size).

## Smart defaults — where each pack ships its policy

The SDK provides the *mechanism* with empty-default lists. **Smart defaults live in each pack's bundled `city.toml` template** (the file `gc init --from <pack>` materializes as the operator's starting config).

| Pack | File | Sets defaults for |
|---|---|---|
| **`examples/gastown/`** (vendored gastown — what `gc init --from gastown` ships) | `examples/gastown/city.toml` | `[events] drop_actors = ["cache-reconcile"]` (SDK's own bookkeeping actor) and `[events.dedup] collapse_paired_orders = [<gastown+maintenance pack patrols>]` |
| **`examples/swarm/`**, **`examples/hyperscale/`**, **`examples/lifecycle/`** | each pack's `city.toml` | Their own respective patrol-order lists if they ship recurring patrols; otherwise leave empty |
| Custom downstream packs (e.g. `qlandia/packs/qlandia-crew/`) | operator's local `city.toml` | Add to the list (e.g. `xtm-sync` for our local pack) |

**Why this shape**: each pack KNOWS its own patrol orders (and any of its own internal-bookkeeping actors). The SDK doesn't enumerate them; the pack ships its self-aware default. New packs get the mechanism for free; their authors decide what to filter.

**Operator override path**: city.toml is operator-owned post-init. Operators can extend or redact the defaults at any time without re-init. Pack updates don't override operator edits.

## Sequencing

Tier 1 is the immediate win and unblocks the others. Recommended order:

1. **T1 first** — single-actor filter, ~50 LOC. Closes the 67% leak today.
2. **T3 second** — the polecat's full F2 design. Caps file size for the residual.
3. **T2 third** — patrol-order dedup. Polish; turns the events log from "telemetry stream" into "actually-useful audit trail".

T2 can ship before T3 if it's easier; the order is preference not dependency.

## What this design explicitly does NOT do

- Does not redesign event-bus interfaces (Provider/Recorder/Watcher unchanged).
- Does not address `bead.updated` payload bloat from named actors (separate angle: `gc-zykvg` proposes delta-only payloads).
- Does not fix `/v0/city/{name}/events` HTTP timeouts (separate root cause in multiplexer fan-out — see #1487).
- Does not introduce content-aware compaction with judgment ("which event is noise"). Both T1 and T2 are pattern-matching on metadata, not semantic filtering.
- Does not name any specific actor or order in Go code. T1 and T2 mechanisms are config-driven (TOML lists); SDK code is policy-free.

## Open questions

1. **T1 audit**: any consumer subscribing to `cache-reconcile` events? Best place to check: `internal/beads/cache_reconcile.go` (or wherever the actor is emitted from) — confirm no read path. If a consumer exists, change is safe iff that consumer is also internal cache layer (which would be a tight coupling but still recoverable).
2. **T3 default**: ship default-OFF and promote to default-ON after field validation, OR ship default-ON with a conservative threshold? (Polecat's design defaulted OFF; arguments for ON: jwp23's launchd workaround + our 408MB experience suggest most installs would benefit.)
3. **T2 window**: per-tick collapse, per-N-events, or time-bucketed (5min)? Trace store precedent uses per-segment.
4. **Threshold source**: TOML config (operationally tunable) vs hardcoded constant (trace store precedent). Polecat recommends TOML; events volume varies 100× across cities so this seems right.

## Risks

- **T1**: silent breakage of cache-reconcile consumers (mitigation: audit before merge; rollback is one-line revert).
- **T1+T2 reduce signal density of events.jsonl**: doctor warnings + size monitoring still apply, but a much smaller file is also a less reliable indicator of "controller activity". Compensating: add a counter event `events.dropped_by_filter{reason}` periodically so we can measure noise-drop rate.
- **T3**: rotation produces multiple files; readers (witness backoff, gc events --watch) must handle the rotation seam. Polecat's full design covers this.

## References

- Bug bead: `gc-n0mqg`
- Polecat's full design: `engdocs/proposals/events-jsonl-rotation-design.md`
- Adjacent: `gc-zykvg` (delta payloads), `gc-smq3z` (multiplexer fanout, #1487)
- Upstream: #1118 (the ask), #1275 (mol-dog-jsonl cautionary tale), #1557 (mol-dog-compactor), #1487 (HTTP timeouts)
- Code precedent: `cmd/gc/session_reconciler_trace_store.go:370-452` (in-tree inline-rotation pattern)
- Engdocs gap call-out: `engdocs/architecture/event-bus.md` ("No event retention or rotation")

---

## Appendix A — Empirical analysis

Sampled the qlandia install's full 438MB / 304k-line `events.jsonl` (4 days uptime, multi-rig: gascity / qcore / qwebsite + suspended xtm / beads).

### A.1 Event-type histogram

```
bead.updated      140,376  46.2%   ~255 MB     avg payload 1616B
bead.closed       106,573  35.0%   ~181 MB     avg payload 1494B
bead.created       33,385  11.0%    ~32 MB     avg payload  771B
order.fired        10,700   3.5%     ~2 MB
order.completed    10,690   3.5%     ~2 MB
session.woke        1,323   0.4%
session.stopped       711   0.2%
session.draining      107   0.0%
mail.sent              93   0.0%
mail.archived          75   0.0%
mail.read              36   0.0%
session.updated        25   0.0%
controller.started     12   0.0%
session.idle_killed    11   0.0%
city.ready             10   0.0%
[rest <10 each]
```

### A.2 Cache-reconcile signature

```
Total cache-reconcile events:     205,300  (67.5% of file)
  - With empty message:           205,300  (100.0%)
  - Duplicate of prior (per-bead): 148,703  (72.4%)  ← byte-identical to last
  - Distinct content:              56,597  (27.6%)  ← still derivable from bd show
Distinct beads touched:             6,495
```

Hash dedup ignored {`updated_at`, `synced_at`, `live_hash`, `wake_attempts`} (clock-driven fields that change on every reconcile without representing semantic change).

### A.3 Top per-bead cache-reconcile concentrations

Top 8 beads receiving cache-reconcile events:
```
hq-imj   13,509   gastown.boot
hq-5ml    9,688   gascity/control-dispatcher
hq-u53    8,567   control-dispatcher (city-level)
hq-6bon   7,767   qcore/control-dispatcher
hq-j7w    4,583   gastown.mayor
hq-k8i    4,429   ...
ql-d2s    4,049   gastown.boot session bead
hq-ad6x   3,854   qcore/syl (the destructively-evicted session from gc-flp1)
```

These are session beads that get `live_hash`/`wake_attempts` ticked constantly by the reconciler. The cache layer republishes the full bead each time.

### A.4 Top-N highest-frequency identical signatures

```
95,262   31.3%  bead.closed   actor=cache-reconcile  (empty message)
88,646   29.1%  bead.updated  actor=cache-reconcile  (empty message)
21,391    7.0%  bead.created  actor=cache-reconcile  (empty message)
10,700    3.5%  order.fired   actor=controller       (patrol orders)
10,690    3.5%  order.completed actor=controller     (patrol orders)
 4,103    1.3%  bead.updated  actor=gastown.mayor    msg=order:gate-sweep
 3,988    1.3%  bead.updated  actor=human            msg=order:gate-sweep
```

The top 7 signatures account for **78%** of the file. T1 alone (drop `cache-reconcile`) eliminates the top 3 = 67%.
