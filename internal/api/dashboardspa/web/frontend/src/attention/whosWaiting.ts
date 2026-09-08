import type { AttentionRegistryEntry } from 'gas-city-dashboard-shared';
import { elapsedSince, formatElapsed } from './elapsed';

// ga-s0fn27 — "who's waiting on you", the operator-facing read of the attention
// registry (<cityRoot>/.gc/runtime/attention/, served by the BFF).
//
// This module is the whole judgement layer, and it is pure so every rule below
// is a unit test rather than a thing you have to reproduce in a browser.
//
// THE RULE THIS MODULE EXISTS TO ENFORCE: never invent certainty from absence.
// A registry entry is a CLAIM written by a hook. Nothing the pane can read from
// a browser upgrades a claim to a fact except one thing — a supported pending
// probe that returns a live approval dialog for that seat. So every row carries
// two independent dimensions:
//
//   state        — WHAT the registry claims (blocked / idle at prompt).
//   verification — WHETHER anything fresh backs that claim
//                  (confirmed / unconfirmed / unknown).
//
// Why each of the three verification values has to exist:
//
//  1. A live session join proves only that A SESSION BY THAT NAME IS RUNNING.
//     The join is by NAME — the typed /v0 session list redacts `session_key`
//     (filterMetadata in handler_sessions.go) — so it cannot even tell "the seat
//     is still in the wait that wrote this entry" from "the seat cycled and a
//     new session took its name". It is certainly not a pending-request
//     predicate: the omp hook's clear can fail silently (the managed gc-hook
//     catches and ignores a failed clear, and a non-ask tool start does not
//     replace the record), so a seat can be mid-tool with a stale "question"
//     record. Liveness therefore buys `unconfirmed`, never `confirmed`.
//
//  2. The supervisor pending probe detects APPROVAL MARKERS ONLY — it greps the
//     tmux pane for "This command requires approval" / "Approve edits?" bound to
//     a tool header (internal/runtime/tmux/interaction.go). A real
//     AskUserQuestion shows no approval marker. Neither does a busy session. So
//     a probe that answers "no pending" is not evidence of anything; only its
//     POSITIVE answer confirms, and it confirms exactly one claim shape (an
//     approval/permission wait). Nothing today can confirm a question or
//     idleness, so no row reaches `confirmed` by any other path.
//
//  3. Probe outcomes must stay distinct. {supported+pending}, {supported+none},
//     {unsupported} and {error} mean four different things, and the flattening
//     helper (`listAgentPendingInteractions`) collapses the last three into
//     "absent". This module takes the un-flattened outcomes
//     (`probeAgentPendingInteractions`) precisely so an ERROR can be told from a
//     successful negative: an errored probe makes the row `unknown`, not idle.
//
//  4. Absence of a live session row is UNVERIFIABLE, not "confirmed stale".
//     Only the server-side reaper, which can see session keys, confirms
//     staleness. Those entries are dropped from the list but kept in a count
//     that says what it actually is.

/** Seats named `interactive-*` are ad-hoc human terminals with no session row. */
const INTERACTIVE_SEAT_PREFIX = 'interactive-';

/**
 * How long an `interactive-*` entry is retained. These seats have no session to
 * join against, ever, so nothing about their liveness is knowable from here.
 * This window is a RETENTION POLICY, not a verification: inside it the entry is
 * shown as `unknown`, outside it the entry is dropped.
 */
const INTERACTIVE_RETENTION_MS = 48 * 60 * 60 * 1000;

/** A session state that means the session is gone. */
const CLOSED_SESSION_STATE = 'closed';

/**
 * One live session, as the pane can see it.
 *
 * Name and state only, on purpose. The runtime's own session id
 * (`gc session list`'s `session_key`, which is what the hooks stamp as
 * `session_id`) is redacted by the typed /v0 list, so a key-equality join is not
 * available to this pane. Carrying an optional key field would only invite the
 * empty-vs-empty comparison that verifies nothing.
 */
export interface WhosWaitingSession {
  /** Seat/alias the session runs as, e.g. "qcore/archer". */
  readonly name: string;
  /** Supervisor session state; 'closed' means not live. */
  readonly state: string;
}

/**
 * What a per-session pending probe answered for one seat.
 *
 *  - `pending`     supported, and a live approval dialog was returned.
 *  - `none`        supported, and there is no approval dialog RIGHT NOW. Not a
 *                  clean bill of health: the probe cannot see questions.
 *  - `unsupported` the runtime has no pane to read (omp/ACP). Says nothing.
 *  - `error`       the probe failed. Says nothing, and we cannot tell it from a
 *                  negative, which is why it is kept distinct from `none`.
 *
 * A seat with no entry in the probe list was not probed at all (no agent row,
 * no session id, or the agent list itself failed) — also unconfirmed, never
 * unknown, because "we didn't ask" is a normal operating condition.
 */
export type WhosWaitingProbe =
  | {
      readonly seat: string;
      readonly outcome: 'pending';
      /** The dialog's prompt text, when the probe carried one. */
      readonly prompt?: string;
      /** The tool the dialog is asking to run (PendingInteraction.metadata.tool_name). */
      readonly toolName?: string;
    }
  | { readonly seat: string; readonly outcome: 'none' }
  | { readonly seat: string; readonly outcome: 'unsupported' }
  | { readonly seat: string; readonly outcome: 'error' };

/**
 * The registry's CLAIM about the seat, in the pane's own vocabulary.
 *
 * `blocked` — the claim is that the operator's move is required.
 * `idle`    — the claim is that the seat went quiet at its prompt (what the
 *             claude hook's 60-second notification actually reports).
 *
 * This is deliberately NOT the same field as the registry entry's own `state`
 * string ("waiting_user"); it is the pane's reading of `reason` + `runtime`.
 * It never encodes evidence — see `verification` for that.
 */
export type WhosWaitingState = 'blocked' | 'idle';

/**
 * How much fresh evidence backs the row's `state`.
 *
 * `confirmed`   — a supported pending probe returned a live approval dialog for
 *                 this seat. The only confirmable claim there is.
 * `unconfirmed` — a session by this seat's name is live, and nothing else is
 *                 known. Render as a report, never as a fact, and never count
 *                 it in an urgent total.
 * `unknown`     — we could not even establish that much (probe error,
 *                 incoherent identity, or an unmanaged terminal inside its
 *                 retention window).
 */
export type WhosWaitingVerification = 'confirmed' | 'unconfirmed' | 'unknown';

/** Why a row landed in `unknown`. Rendered, so the operator is told which. */
export type WhosWaitingUnknownReason =
  /** The entry does not identify a seat/session coherently. */
  | 'identity'
  /** The pending probe for this seat failed. */
  | 'probe-error'
  /** `interactive-*`: no session row exists to join against; inside retention. */
  | 'retention';

export interface WhosWaitingRow {
  /** Stable list key. Seats are unique in the registry (one file per seat). */
  readonly key: string;
  readonly seat: string;
  readonly runtime: string;
  /** The registry's claim. */
  readonly state: WhosWaitingState;
  /** The evidence behind that claim. */
  readonly verification: WhosWaitingVerification;
  /** Set iff `verification === 'unknown'`. */
  readonly unknownReason?: WhosWaitingUnknownReason;
  /** The raw registry reason, kept for callers that want to branch on it. */
  readonly reason: string;
  /** Operator-facing phrasing of the claim. Phrased as a claim, not a verdict. */
  readonly reasonLabel: string;
  /** The registry timestamp verbatim (offset-style or Z-style). */
  readonly since: string;
  /** Milliseconds waited, or null when `since` is unparseable or in the future. */
  readonly elapsedMs: number | null;
  /** Coarse age phrase ("3h", "5d"), or null when `elapsedMs` is null. */
  readonly elapsedLabel: string | null;
  /** The hook's own one-line summary, when it wrote one. */
  readonly summary?: string;
  /** Confirming detail from the pending probe (tool name and/or prompt line). */
  readonly detail?: string;
}

export interface WhosWaitingModel {
  /** Fresh probe evidence backs the claim. Oldest wait first. */
  readonly confirmed: readonly WhosWaitingRow[];
  /** Claimed by the registry, live by name, nothing more. Oldest wait first. */
  readonly unconfirmed: readonly WhosWaitingRow[];
  /** Could not be established either way. Oldest wait first. */
  readonly unknown: readonly WhosWaitingRow[];
  /**
   * Entries whose seat has NO live session row by name. Counted, not listed,
   * and NOT called stale: only the server-side reaper, which can see session
   * keys, can confirm staleness from here.
   */
  readonly droppedUnverifiable: number;
}

export interface WhosWaitingInput {
  readonly entries: readonly AttentionRegistryEntry[];
  readonly sessions: readonly WhosWaitingSession[];
  readonly probes: readonly WhosWaitingProbe[];
  readonly nowMs: number;
}

/**
 * Classify the registry into the three evidence states the pane renders.
 *
 * Precedence, in the order the code applies it:
 *
 *   1. empty seat                     → unknown('identity')
 *   2. no live session row by name    → interactive-* inside retention:
 *                                         unknown('retention');
 *                                       otherwise: droppedUnverifiable++
 *   3. probe outcome 'pending'        → confirmed (and the claim is upgraded to
 *                                       'blocked': the dialog IS the evidence)
 *   4. probe outcome 'error'          → unknown('probe-error')
 *   5. empty session_id on the entry  → unknown('identity')
 *   6. everything else                → unconfirmed
 *
 * Step 3 sits above 4 and 5 because a positive probe is direct evidence about
 * the seat itself; it does not depend on the entry's id fields being coherent.
 */
export function selectWhosWaiting(input: WhosWaitingInput): WhosWaitingModel {
  const { entries, sessions, probes, nowMs } = input;
  const probeBySeat = new Map(probes.map((probe) => [probe.seat, probe]));

  const confirmed: WhosWaitingRow[] = [];
  const unconfirmed: WhosWaitingRow[] = [];
  const unknown: WhosWaitingRow[] = [];
  let droppedUnverifiable = 0;

  entries.forEach((entry, index) => {
    const seat = (entry.seat ?? '').trim();

    // 1. An entry with no seat names nobody: it cannot be probed, cannot be
    // joined, and cannot be acted on. That is an incoherent record, not an idle
    // agent, so it is shown as unknown rather than quietly dropped.
    if (seat.length === 0) {
      unknown.push(buildRow(entry, `entry:${index}`, 'unknown', undefined, nowMs, 'identity'));
      return;
    }

    // 2. The name join. Passing it proves a session by that name is live and
    // nothing more; failing it proves nothing at all, which is why the failure
    // is a count with an honest name rather than a "stale" verdict.
    if (!hasLiveSession(sessions, seat)) {
      if (!seat.startsWith(INTERACTIVE_SEAT_PREFIX)) {
        droppedUnverifiable += 1;
        return;
      }
      // An `interactive-*` seat is a human terminal, not a managed agent: there
      // is no session row to join against, ever. Age is a retention policy, not
      // evidence — inside the window the row is unknown-liveness, outside it we
      // stop carrying it.
      const ageMs = elapsedSince(entry.since, nowMs);
      if (ageMs === null || ageMs >= INTERACTIVE_RETENTION_MS) {
        droppedUnverifiable += 1;
        return;
      }
      unknown.push(buildRow(entry, seat, 'unknown', undefined, nowMs, 'retention'));
      return;
    }

    const probe = probeBySeat.get(seat);

    // 3. The one confirmable claim.
    if (probe?.outcome === 'pending') {
      confirmed.push(buildRow(entry, seat, 'confirmed', probe, nowMs));
      return;
    }
    // 4. A failed probe is not a negative answer.
    if (probe?.outcome === 'error') {
      unknown.push(buildRow(entry, seat, 'unknown', undefined, nowMs, 'probe-error'));
      return;
    }
    // 5. No session id means any liveness comparison was empty against empty.
    if ((entry.session_id ?? '').trim().length === 0) {
      unknown.push(buildRow(entry, seat, 'unknown', undefined, nowMs, 'identity'));
      return;
    }
    // 6. A live name and a registry claim. That is a report, not a fact.
    unconfirmed.push(buildRow(entry, seat, 'unconfirmed', undefined, nowMs));
  });

  return {
    confirmed: confirmed.sort(bySinceAscending),
    unconfirmed: unconfirmed.sort(bySinceAscending),
    unknown: unknown.sort(bySinceAscending),
    droppedUnverifiable,
  };
}

/**
 * Whether a live session carries this seat's name.
 *
 * A closed session backs nothing. Everything else is a plain name match, which
 * is all the redacted /v0 list allows — see `WhosWaitingSession`. This is why no
 * row is ever `confirmed` on the strength of the join alone.
 */
function hasLiveSession(sessions: readonly WhosWaitingSession[], seat: string): boolean {
  return sessions.some(
    (session) => session.state !== CLOSED_SESSION_STATE && session.name === seat,
  );
}

/**
 * The registry's claim, read from `reason` + `runtime` ALONE — never from the
 * presence or absence of a probe:
 *
 *   permission | blocked        → blocked
 *   question + runtime !claude  → blocked (an omp question is a real question)
 *   question + runtime claude   → idle    (the claude hook's 60s notification)
 *   anything else               → blocked (an unknown reason is not a reason to
 *                                          hide the row)
 */
function stateFor(entry: AttentionRegistryEntry): WhosWaitingState {
  if (entry.reason !== 'question') return 'blocked';
  return entry.runtime === 'claude' ? 'idle' : 'blocked';
}

/**
 * How the claim is worded. `confirmed` rows say what the probe saw; every other
 * row says what the hook wrote, and the pane wraps it in "reported …".
 */
function reasonLabelFor(entry: AttentionRegistryEntry, confirmed: boolean): string {
  if (confirmed) return 'approval prompt';
  if (entry.reason === 'permission') return 'permission request';
  if (entry.reason === 'blocked') return 'blocked';
  if (entry.reason === 'question') {
    return entry.runtime === 'claude' ? 'idle at prompt' : 'question';
  }
  // Unknown reason: show the word the hook wrote rather than inventing one.
  return (entry.reason ?? '').length > 0 ? entry.reason : 'waiting';
}

function buildRow(
  entry: AttentionRegistryEntry,
  key: string,
  verification: WhosWaitingVerification,
  probe: Extract<WhosWaitingProbe, { outcome: 'pending' }> | undefined,
  nowMs: number,
  unknownReason?: WhosWaitingUnknownReason,
): WhosWaitingRow {
  const elapsedMs = elapsedSince(entry.since, nowMs);
  const confirmed = verification === 'confirmed';
  const row: {
    key: string;
    seat: string;
    runtime: string;
    state: WhosWaitingState;
    verification: WhosWaitingVerification;
    unknownReason?: WhosWaitingUnknownReason;
    reason: string;
    reasonLabel: string;
    since: string;
    elapsedMs: number | null;
    elapsedLabel: string | null;
    summary?: string;
    detail?: string;
  } = {
    key,
    seat: entry.seat ?? '',
    runtime: entry.runtime ?? '',
    // A confirmed approval dialog IS evidence that the operator's move is
    // required, so it upgrades the claim. Nothing else moves `state`.
    state: confirmed ? 'blocked' : stateFor(entry),
    verification,
    reason: entry.reason ?? '',
    reasonLabel: reasonLabelFor(entry, confirmed),
    since: entry.since ?? '',
    elapsedMs,
    elapsedLabel: elapsedMs === null ? null : formatElapsed(elapsedMs),
  };
  if (unknownReason !== undefined) row.unknownReason = unknownReason;
  const summary = (entry.summary ?? '').trim();
  if (summary.length > 0) row.summary = summary;
  const detail = probeDetail(probe);
  if (detail !== undefined) row.detail = detail;
  return row;
}

/** The probe's own words: "<tool_name>: <first prompt line>", either part optional. */
function probeDetail(
  probe: Extract<WhosWaitingProbe, { outcome: 'pending' }> | undefined,
): string | undefined {
  if (probe === undefined) return undefined;
  const tool = (probe.toolName ?? '').trim();
  const promptLine = (probe.prompt ?? '').split('\n', 1)[0]?.trim() ?? '';
  if (tool.length > 0 && promptLine.length > 0) return `${tool}: ${promptLine}`;
  if (tool.length > 0) return tool;
  if (promptLine.length > 0) return promptLine;
  return undefined;
}

/**
 * Oldest wait first. `since` is parsed rather than compared as a string because
 * the two hooks write different formats (offset-style and Z-style) that do not
 * sort lexically against each other. An unparseable stamp sorts last, then by
 * seat, so the order is total and stable.
 */
function bySinceAscending(a: WhosWaitingRow, b: WhosWaitingRow): number {
  const aMs = sinceMs(a.since);
  const bMs = sinceMs(b.since);
  if (aMs !== bMs) return aMs - bMs;
  return a.seat.localeCompare(b.seat);
}

function sinceMs(since: string): number {
  const parsed = Date.parse(since);
  return Number.isFinite(parsed) ? parsed : Number.POSITIVE_INFINITY;
}
