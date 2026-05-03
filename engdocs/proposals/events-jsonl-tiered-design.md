# events.jsonl: tiered compaction + rotation

**Status**: Proposal
**Filed**: 2026-05-03
**Resolves**: gastownhall/gascity#1118 (events.jsonl unbounded growth)
**Companion full design**: `engdocs/proposals/events-jsonl-rotation-design.md` (582-line research-grade survey)

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

### Tier 1 — Drop `cache-reconcile` events at write time

```go
// internal/events/recorder.go
func (r *FileRecorder) Record(e Event) {
    if e.Actor == "cache-reconcile" {
        return  // bookkeeping, not domain event
    }
    // ... existing append path ...
}
```

**Why**: deterministic, single-actor filter, no judgment. The cache-reconcile actor is internal bookkeeping; its events embed full bead payloads (~1.1KB) but are byte-equivalent to a `bd show`. They serve no consumer. Dropping them at write time recovers ~67% of file growth on the qlandia install. Passes ZFC (no role-name reasoning, just metadata filter). Passes Bitter Lesson (it's a definition: "this actor's events are not domain events", not "decide which events are noise"). One-line change + one test.

**Risk**: any consumer that relies on cache-reconcile events for triggering would break. Audit before merge — none expected (the cache layer maintains its own state from beads, doesn't subscribe to its own events).

### Tier 2 — Collapse paired patrol-order events

`order.fired` + `order.completed` for recurring patrol orders (`gate-sweep`, `cross-rig-deps`, `orphan-sweep`, `spawn-storm-detect`) appear ~21,000 times in our window, perfectly paired. Each pair is informationally equivalent to "patrol X ran, succeeded" — useful in aggregate (frequency, failure-rate), useless individually.

Collapse window-N pairs to one summary event: `order.batch_completed{name: "gate-sweep", count: 2699, success: 2698, last_ts: ...}`. Recovers ~7% additional, and improves the signal-to-noise of the file even more dramatically (real anomalies like `session.idle_killed` become much easier to find by eye/grep).

Less urgent than Tier 1; ship after T1 is in.

### Tier 3 — Size-based rotation (the polecat's F2)

`RotatingFileRecorder` wrapping `FileRecorder`, size-only, default-OFF, atomic rename to `events.jsonl.<timestamp>`. Plus `gc events rotate` CLI for manual + maintenance-pack callers. Full design in the companion doc; precedent in-tree at `cmd/gc/session_reconciler_trace_store.go:rotateSegment`.

After Tiers 1+2, rotation triggers ~3-4× less often. Still needed as the absolute file-size cap (Tier 1+2 reduce growth rate; rotation bounds peak size).

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
