import type { AttentionRegistryEntry } from 'gas-city-dashboard-shared';
import { elapsedSince, formatElapsed } from './elapsed';

// ga-s0fn27 — "who's waiting on you", the operator-facing read of the attention
// registry (<cityRoot>/.gc/runtime/attention/, served by the BFF).
//
// This module is the whole judgement layer, and it is pure so every rule below
// is a unit test rather than a thing you have to reproduce in a browser.
//
// Two problems it exists to solve:
//
//  1. A registry entry is a CLAIM, not a fact. The hook writes it and the hook
//     is supposed to clear it, but a killed session, a crashed hook, or a
//     cycled seat leaves the entry behind. So an entry is only shown when a
//     LIVE session still backs it; everything else is counted, not listed.
//
//  2. `reason: "question"` means two different things per runtime. On omp it is
//     a real question the agent asked. On claude it is the generic 60-second
//     idle notification — the session went quiet at its prompt, which is NOT
//     the same as being blocked on you. Calling that "a question" trains the
//     operator to ignore the pane, so it gets its own honest tier and its own
//     honest words ("idle at prompt"). It is promoted to blocked only when the
//     supervisor's pending probe independently confirms a live approval dialog.

/** Seats named `interactive-*` are ad-hoc human terminals with no session row. */
const INTERACTIVE_SEAT_PREFIX = 'interactive-';

/**
 * How long an unverifiable `interactive-*` entry is trusted. These seats have no
 * session to join against, so age is the only liveness signal available; past
 * this the entry is assumed abandoned.
 */
const UNVERIFIED_MAX_AGE_MS = 48 * 60 * 60 * 1000;

/** A session state that means the session is gone. */
const CLOSED_SESSION_STATE = 'closed';

/**
 * One live session, as the pane can see it.
 *
 * `sessionKey` is the runtime's own session id (`gc session list`'s
 * `session_key`), which is exactly what the hooks stamp as `session_id`. The
 * typed /v0 session list redacts it (`filterMetadata` in handler_sessions.go),
 * so a caller reading the supervisor API supplies `name` + `state` only; a
 * caller that can see keys (the CLI shape, and this module's tests) supplies
 * them and gets the stronger join. See `matchesSession`.
 */
export interface WhosWaitingSession {
  /** Seat/alias the session runs as, e.g. "qcore/archer". */
  readonly name: string;
  /** The runtime session id, when the source exposes it. */
  readonly sessionKey?: string;
  /** Supervisor session state; 'closed' means not live. */
  readonly state: string;
}

/**
 * A supervisor pending-interaction probe. These exist only for claude-in-tmux
 * approval dialogs — omp/ACP sessions report unsupported — so an absent probe
 * is never evidence that a seat is fine, only that we could not confirm it.
 */
export interface WhosWaitingPending {
  /** Seat the probe belongs to (AgentPendingInteraction.agentName). */
  readonly seat: string;
  /** The dialog's prompt text, when the probe carried one. */
  readonly prompt?: string;
  /** The tool the dialog is asking to run (PendingInteraction.metadata.tool_name). */
  readonly toolName?: string;
}

/**
 * `blocked` — the operator's move, now. `idle` — quiet at its prompt, worth a
 * look, not an interrupt.
 */
export type WhosWaitingTier = 'blocked' | 'idle';

export interface WhosWaitingRow {
  /** Stable list key. Seats are unique in the registry (one file per seat). */
  readonly key: string;
  readonly seat: string;
  readonly runtime: string;
  readonly tier: WhosWaitingTier;
  /** The raw registry reason, kept for callers that want to branch on it. */
  readonly reason: string;
  /** Operator-facing phrasing of the reason, honest about what is known. */
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
  /** True when no live session could confirm this row (interactive-* only). */
  readonly unverified: boolean;
}

export interface WhosWaitingModel {
  /** Blocked on you, oldest wait first. */
  readonly blocked: readonly WhosWaitingRow[];
  /** Idle at their prompt, oldest wait first. */
  readonly idle: readonly WhosWaitingRow[];
  /** Entries dropped because nothing live backed them. */
  readonly droppedStale: number;
}

export interface WhosWaitingInput {
  readonly entries: readonly AttentionRegistryEntry[];
  readonly sessions: readonly WhosWaitingSession[];
  readonly pending: readonly WhosWaitingPending[];
  readonly nowMs: number;
}

/**
 * Classify the registry into the two tiers the pane renders.
 *
 * Order of operations is deliberate: verify liveness FIRST, then tier. A stale
 * entry must not be able to reach the blocked tier just because its reason word
 * is urgent.
 */
export function selectWhosWaiting(input: WhosWaitingInput): WhosWaitingModel {
  const { entries, sessions, pending, nowMs } = input;
  const pendingBySeat = new Map(pending.map((probe) => [probe.seat, probe]));

  const blocked: WhosWaitingRow[] = [];
  const idle: WhosWaitingRow[] = [];
  let droppedStale = 0;

  for (const entry of entries) {
    const seat = entry.seat ?? '';
    if (seat.length === 0) {
      droppedStale += 1;
      continue;
    }
    const live = sessions.some((session) => matchesSession(session, entry));
    let unverified = false;
    if (!live) {
      // An `interactive-*` seat is a human terminal, not a managed agent: there
      // is no session row to join against, ever. Age is the only liveness
      // signal left, so trust it for a bounded window and SAY that the row is
      // unverified rather than presenting a guess as a confirmed fact.
      if (!seat.startsWith(INTERACTIVE_SEAT_PREFIX)) {
        droppedStale += 1;
        continue;
      }
      const ageMs = elapsedSince(entry.since, nowMs);
      if (ageMs === null || ageMs >= UNVERIFIED_MAX_AGE_MS) {
        droppedStale += 1;
        continue;
      }
      unverified = true;
    }

    const probe = pendingBySeat.get(seat);
    const tier = tierFor(entry, probe !== undefined);
    const row = buildRow(entry, tier, probe, unverified, nowMs);
    if (tier === 'blocked') {
      blocked.push(row);
    } else {
      idle.push(row);
    }
  }

  return {
    blocked: blocked.sort(bySinceAscending),
    idle: idle.sort(bySinceAscending),
    droppedStale,
  };
}

/**
 * Whether a session backs an entry.
 *
 * A closed session backs nothing. Otherwise the strong join is key equality —
 * that is what distinguishes "the seat is still in the wait that wrote this
 * entry" from "the seat cycled and a new session took its name". When the
 * source cannot expose keys (the typed /v0 list redacts `session_key`), fall
 * back to the seat name: a name match is weaker, but the alternative is a pane
 * that drops every row and shows the operator nothing at all.
 */
function matchesSession(session: WhosWaitingSession, entry: AttentionRegistryEntry): boolean {
  if (session.state === CLOSED_SESSION_STATE) return false;
  const key = session.sessionKey ?? '';
  if (key.length > 0) return key === entry.session_id;
  return session.name === entry.seat;
}

/**
 * The tier rules, in one place:
 *   permission                       → blocked (it is literally asking you)
 *   blocked                          → blocked
 *   omp + question                   → blocked (omp questions are real)
 *   claude + question + live probe   → blocked (the probe confirms a dialog)
 *   claude + question                → idle    (the 60s idle notification)
 *
 * Anything else — an unknown reason from a future hook — is treated as blocked:
 * a reason this code does not understand is a reason not to hide it.
 */
function tierFor(entry: AttentionRegistryEntry, hasPending: boolean): WhosWaitingTier {
  if (entry.reason !== 'question') return 'blocked';
  if (entry.runtime !== 'claude') return 'blocked';
  return hasPending ? 'blocked' : 'idle';
}

function reasonLabelFor(
  entry: AttentionRegistryEntry,
  tier: WhosWaitingTier,
  hasPending: boolean,
): string {
  if (entry.reason === 'permission') return 'permission request';
  if (entry.reason === 'blocked') return 'blocked';
  if (entry.reason === 'question') {
    if (tier === 'idle') return 'idle at prompt';
    return hasPending ? 'approval prompt' : 'question';
  }
  // Unknown reason: show the word the hook wrote rather than inventing one.
  return entry.reason.length > 0 ? entry.reason : 'waiting';
}

function buildRow(
  entry: AttentionRegistryEntry,
  tier: WhosWaitingTier,
  probe: WhosWaitingPending | undefined,
  unverified: boolean,
  nowMs: number,
): WhosWaitingRow {
  const elapsedMs = elapsedSince(entry.since, nowMs);
  const row: {
    key: string;
    seat: string;
    runtime: string;
    tier: WhosWaitingTier;
    reason: string;
    reasonLabel: string;
    since: string;
    elapsedMs: number | null;
    elapsedLabel: string | null;
    summary?: string;
    detail?: string;
    unverified: boolean;
  } = {
    key: entry.seat,
    seat: entry.seat,
    runtime: entry.runtime ?? '',
    tier,
    reason: entry.reason ?? '',
    reasonLabel: reasonLabelFor(entry, tier, probe !== undefined),
    since: entry.since ?? '',
    elapsedMs,
    elapsedLabel: elapsedMs === null ? null : formatElapsed(elapsedMs),
    unverified,
  };
  const summary = (entry.summary ?? '').trim();
  if (summary.length > 0) row.summary = summary;
  const detail = probeDetail(probe);
  if (detail !== undefined) row.detail = detail;
  return row;
}

/** The probe's own words: "<tool_name>: <first prompt line>", either part optional. */
function probeDetail(probe: WhosWaitingPending | undefined): string | undefined {
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
