import { useMemo } from 'react';
import { Link } from 'react-router-dom';
import type { SessionResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import { api } from '../api/client';
import { getActiveCity } from '../api/cityBase';
import { useNow } from '../contexts/NowContext';
import { useCachedData } from '../hooks/useCachedData';
import { probeAgentPendingInteractions, type AgentPendingProbe } from '../supervisor/agentPending';
import { supervisorApi } from '../supervisor/client';
import {
  selectWhosWaiting,
  type WhosWaitingProbe,
  type WhosWaitingRow,
  type WhosWaitingSession,
  type WhosWaitingUnknownReason,
} from './whosWaiting';

// ga-s0fn27 — "who's waiting on you" on the operator's home.
//
// Deliberately NOT registered as an attention contributor: the badge/contributor
// registry already counts the same agents through selectAgentsNeedingYou, and
// feeding this in would double-count every needs-you seat in the nav badge. This
// pane is a detail view of the attention registry, not a second source of truth
// for the badge number.
//
// All of the judgement lives in whosWaiting.ts; this component is fetch + paint.
// The one rule the paint itself carries: a row's words must match its evidence.
// Confirmed rows state what is happening. Unconfirmed rows are prefixed
// "reported" and suffixed "unconfirmed". Unknown rows are collapsed behind a
// count and name the reason they are unknown. Nothing is ever rendered as a
// verdict the data cannot support.

interface LiveSeatFacts {
  readonly sessions: readonly WhosWaitingSession[];
  readonly probes: readonly WhosWaitingProbe[];
}

const EMPTY_FACTS: LiveSeatFacts = { sessions: [], probes: [] };

/**
 * The live half of the join: which sessions exist by name, and what each seat's
 * pending probe actually answered.
 *
 * The probe outcomes are carried through un-flattened
 * (`probeAgentPendingInteractions`, not `listAgentPendingInteractions`) because
 * the selector must be able to tell "supported, nothing pending" from "the
 * runtime has no pane to read" from "the probe blew up". A seat's failed probe
 * makes that ONE seat unknown; it does not silence the pane.
 *
 * The session list is the load-bearing read and is allowed to throw; without it
 * every row would be counted as having no live session, which would be a lie.
 * The agent list failing only costs probes — every row then reads as reported
 * and unconfirmed, which is exactly what it would be.
 *
 * The city-wide pending aggregate is deliberately NOT used here: it omits
 * unsupported and failed probes (so absence there is unreadable), and its
 * SessionID is the session bead id rather than the runtime key.
 */
export async function fetchLiveSeatFacts(cityName: string | null): Promise<LiveSeatFacts> {
  if (cityName === null) return EMPTY_FACTS;
  const sessionList = await supervisorApi().listSessions(cityName);
  const sessionItems = sessionList.items ?? [];
  const facts: { sessions: WhosWaitingSession[]; probes: WhosWaitingProbe[] } = {
    sessions: sessionItems.map(toWhosWaitingSession),
    probes: [],
  };
  try {
    const agentList = await supervisorApi().listAgents(cityName);
    const probed = await probeAgentPendingInteractions(agentList.items ?? [], sessionItems);
    facts.probes = probed.map(toWhosWaitingProbe);
  } catch {
    // Could not enumerate agents at all: no seat was probed. "Not probed" and
    // "probed and told nothing" are the same for the selector — unconfirmed.
    facts.probes = [];
  }
  return facts;
}

/**
 * A session as the registry names it. The hooks stamp the seat (GC_AGENT), which
 * is the session's alias; `session_name` is the tmux-safe mangling of it and is
 * the fallback when no alias is set.
 *
 * Name and state only: the typed /v0 list redacts `session_key` (filterMetadata
 * in handler_sessions.go), so this join can never be more than "a session by
 * that name is live".
 */
function toWhosWaitingSession(session: SessionResponse): WhosWaitingSession {
  return { name: session.alias ?? session.session_name, state: session.state };
}

/** Carry the probe outcome across verbatim; only `pending` gains detail. */
function toWhosWaitingProbe(probe: AgentPendingProbe): WhosWaitingProbe {
  if (probe.outcome === 'pending' && probe.pending !== undefined) {
    const row: { seat: string; outcome: 'pending'; prompt?: string; toolName?: string } = {
      seat: probe.agentName,
      outcome: 'pending',
    };
    if (probe.pending.prompt !== undefined) row.prompt = probe.pending.prompt;
    const toolName = probe.pending.metadata?.['tool_name'];
    if (toolName !== undefined) row.toolName = toolName;
    return row;
  }
  if (probe.outcome === 'error') return { seat: probe.agentName, outcome: 'error' };
  if (probe.outcome === 'unsupported') return { seat: probe.agentName, outcome: 'unsupported' };
  return { seat: probe.agentName, outcome: 'none' };
}

export function WhosWaitingPane() {
  const cityName = getActiveCity();
  const cacheSuffix = cityName ?? 'no-city';
  const registry = useCachedData(`attention:registry:${cacheSuffix}`, () =>
    api.attentionRegistry(),
  );
  const live = useCachedData(`attention:live-seats:${cacheSuffix}`, () =>
    fetchLiveSeatFacts(cityName),
  );
  const nowMs = useNow();

  const registryData = registry.data;
  const facts = live.data ?? EMPTY_FACTS;
  const model = useMemo(
    () =>
      selectWhosWaiting({
        entries: registryData?.entries ?? [],
        sessions: facts.sessions,
        probes: facts.probes,
        nowMs,
      }),
    [registryData, facts, nowMs],
  );

  // Say nothing until the registry has actually been read. A pane that renders
  // "nobody is waiting" while its own fetch is in flight (or has failed) states
  // as fact the one thing it does not know.
  if (registryData === undefined) return null;

  const skipped = registryData.skippedMalformed;
  const footnote = footnoteFor(model.droppedUnverifiable, skipped);
  const empty =
    model.confirmed.length === 0 && model.unconfirmed.length === 0 && model.unknown.length === 0;

  if (empty) {
    return (
      <section aria-label="Who’s waiting" data-testid="whos-waiting">
        <p className="text-body text-fg-muted">No seat is waiting on you.</p>
        {footnote !== null && <Footnote text={footnote} />}
      </section>
    );
  }

  return (
    <section aria-labelledby="whos-waiting-title" className="space-y-3" data-testid="whos-waiting">
      <h2 id="whos-waiting-title" className="text-headline font-semibold text-fg">
        Who’s waiting
      </h2>
      <WhosWaitingSection title="Needs you (confirmed)" testId="confirmed" rows={model.confirmed} />
      <WhosWaitingSection
        title="Reported waiting (unconfirmed)"
        testId="unconfirmed"
        rows={model.unconfirmed}
      />
      <UnknownSection rows={model.unknown} />
      {footnote !== null && <Footnote text={footnote} />}
    </section>
  );
}

function WhosWaitingSection({
  title,
  testId,
  rows,
}: {
  title: string;
  testId: string;
  rows: readonly WhosWaitingRow[];
}) {
  if (rows.length === 0) return null;
  return (
    <div className="space-y-2" data-testid={`whos-waiting-${testId}`}>
      <h3 className="text-label uppercase tracking-wider text-fg-muted">{title}</h3>
      <ul className="space-y-2">
        {rows.map((row) => (
          <WhosWaitingRowItem key={row.key} row={row} />
        ))}
      </ul>
    </div>
  );
}

/**
 * The rows we could not establish anything about. Collapsed and muted on
 * purpose: they must never read as "idle" or as a clean state, but they are also
 * not something to act on, so they do not take space from the rows that are.
 */
function UnknownSection({ rows }: { rows: readonly WhosWaitingRow[] }) {
  if (rows.length === 0) return null;
  return (
    <details className="text-fg-muted" data-testid="whos-waiting-unknown">
      <summary className="text-label uppercase tracking-wider text-fg-muted">
        Unknown ({rows.length})
      </summary>
      <ul className="space-y-2 pt-2">
        {rows.map((row) => (
          <WhosWaitingRowItem key={row.key} row={row} />
        ))}
      </ul>
    </details>
  );
}

function WhosWaitingRowItem({ row }: { row: WhosWaitingRow }) {
  // The probe's own words beat the hook's generic summary when we have them.
  const note = row.detail ?? row.summary;
  return (
    <li className="text-body text-fg flex items-baseline gap-3" data-testid="whos-waiting-row">
      <SeatLink seat={row.seat} />
      <span className="text-label uppercase tracking-wider text-fg-muted">{claimText(row)}</span>
      {row.verification === 'confirmed' && row.elapsedLabel !== null && (
        <span className="text-label text-fg-muted">{row.elapsedLabel}</span>
      )}
      {note !== undefined && <span className="text-body text-fg-muted">{note}</span>}
    </li>
  );
}

/** A seatless entry has nothing to link to; say so rather than link nowhere. */
function SeatLink({ seat }: { seat: string }) {
  if (seat.length === 0) {
    return <span className="font-medium text-fg-muted">(no seat)</span>;
  }
  return (
    <Link
      to={`/agents/${encodeURIComponent(seat)}`}
      className="font-medium hover:text-fg focus-mark"
    >
      {seat}
    </Link>
  );
}

/**
 * The row's words, matched to its evidence.
 *
 *   confirmed   → "approval prompt"                       (what the probe saw)
 *   unconfirmed → "reported X — as of 3h ago, unconfirmed" (what a hook wrote)
 *   unknown     → "reported X — unknown: probe failed"     (and why)
 */
function claimText(row: WhosWaitingRow): string {
  if (row.verification === 'confirmed') return row.reasonLabel;
  const age = row.elapsedLabel === null ? 'age unknown' : `as of ${row.elapsedLabel} ago`;
  if (row.verification === 'unknown') {
    return `reported ${row.reasonLabel} — ${age}, unknown: ${unknownReasonText(row.unknownReason)}`;
  }
  return `reported ${row.reasonLabel} — ${age}, unconfirmed`;
}

function unknownReasonText(reason: WhosWaitingUnknownReason | undefined): string {
  switch (reason) {
    case 'probe-error':
      return 'probe failed';
    case 'identity':
      return 'entry does not identify a session';
    case 'retention':
      return 'unmanaged terminal, liveness not observable';
    case undefined:
      return 'no reason recorded';
  }
}

function Footnote({ text }: { text: string }) {
  return (
    <p className="text-label text-fg-muted" data-testid="whos-waiting-footnote">
      {text}
    </p>
  );
}

/**
 * One muted line accounting for what the pane did NOT show, so a short list is
 * never mistaken for a complete one. Null when there is nothing to account for.
 *
 * The dropped entries are called unverifiable, not stale: from a browser, a seat
 * with no live session row by name is a seat we cannot check. Only the
 * server-side reaper, which can see session keys, confirms staleness.
 */
function footnoteFor(droppedUnverifiable: number, skippedMalformed: number): string | null {
  const parts: string[] = [];
  if (droppedUnverifiable > 0) {
    parts.push(
      `${droppedUnverifiable} ${plural(droppedUnverifiable, 'entry', 'entries')} with no live session (unverifiable)`,
    );
  }
  if (skippedMalformed > 0) {
    parts.push(`${skippedMalformed} unreadable ${plural(skippedMalformed, 'file', 'files')}`);
  }
  return parts.length === 0 ? null : parts.join(' · ');
}

function plural(count: number, one: string, many: string): string {
  return count === 1 ? one : many;
}
