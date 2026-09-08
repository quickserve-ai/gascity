import { useMemo } from 'react';
import { Link } from 'react-router-dom';
import type { SessionResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import { api } from '../api/client';
import { getActiveCity } from '../api/cityBase';
import { useNow } from '../contexts/NowContext';
import { useCachedData } from '../hooks/useCachedData';
import { listAgentPendingInteractions } from '../supervisor/agentPending';
import { supervisorApi } from '../supervisor/client';
import {
  selectWhosWaiting,
  type WhosWaitingPending,
  type WhosWaitingRow,
  type WhosWaitingSession,
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

interface LiveSeatFacts {
  readonly sessions: readonly WhosWaitingSession[];
  readonly pending: readonly WhosWaitingPending[];
}

const EMPTY_FACTS: LiveSeatFacts = { sessions: [], pending: [] };

/**
 * The live half of the join: which sessions exist, and which of them the
 * supervisor can independently confirm are sitting on an approval dialog.
 *
 * The pending probe covers claude-in-tmux only — omp/ACP sessions answer
 * "unsupported" — so a probe failure is expected operating condition, not an
 * error worth showing anyone. It degrades to "no confirmations", which costs at
 * most a tier: a claude question stays in "Waiting at prompt" instead of being
 * promoted. The session list is the load-bearing read and is allowed to throw;
 * without it every row would be dropped as stale, which would be a lie.
 */
export async function fetchLiveSeatFacts(cityName: string | null): Promise<LiveSeatFacts> {
  if (cityName === null) return EMPTY_FACTS;
  const sessionList = await supervisorApi().listSessions(cityName);
  const sessionItems = sessionList.items ?? [];
  const facts: { sessions: WhosWaitingSession[]; pending: WhosWaitingPending[] } = {
    sessions: sessionItems.map(toWhosWaitingSession),
    pending: [],
  };
  try {
    const agentList = await supervisorApi().listAgents(cityName);
    const probes = await listAgentPendingInteractions(agentList.items ?? [], sessionItems);
    facts.pending = probes.map((probe) => {
      const row: { seat: string; prompt?: string; toolName?: string } = { seat: probe.agentName };
      if (probe.pending.prompt !== undefined) row.prompt = probe.pending.prompt;
      const toolName = probe.pending.metadata?.['tool_name'];
      if (toolName !== undefined) row.toolName = toolName;
      return row;
    });
  } catch {
    facts.pending = [];
  }
  return facts;
}

/**
 * A session as the registry names it. The hooks stamp the seat (GC_AGENT), which
 * is the session's alias; `session_name` is the tmux-safe mangling of it and is
 * the fallback when no alias is set.
 *
 * `sessionKey` is absent on purpose: the typed /v0 list redacts it
 * (filterMetadata in handler_sessions.go), so the selector's seat-name fallback
 * is what does the matching here. Should the field ever be exposed, populating
 * it is the only change needed for the stronger key join.
 */
function toWhosWaitingSession(session: SessionResponse): WhosWaitingSession {
  return { name: session.alias ?? session.session_name, state: session.state };
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
        pending: facts.pending,
        nowMs,
      }),
    [registryData, facts, nowMs],
  );

  // Say nothing until the registry has actually been read. A pane that renders
  // "nobody is waiting" while its own fetch is in flight (or has failed) states
  // as fact the one thing it does not know.
  if (registryData === undefined) return null;

  const skipped = registryData.skippedMalformed;
  const footnote = footnoteFor(model.droppedStale, skipped);

  if (model.blocked.length === 0 && model.idle.length === 0) {
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
      <WhosWaitingSection title="Blocked on you" testId="blocked" rows={model.blocked} />
      <WhosWaitingSection title="Waiting at prompt" testId="idle" rows={model.idle} />
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

function WhosWaitingRowItem({ row }: { row: WhosWaitingRow }) {
  // The probe's own words beat the hook's generic summary when we have them.
  const note = row.detail ?? row.summary;
  return (
    <li className="text-body text-fg flex items-baseline gap-3" data-testid="whos-waiting-row">
      <Link
        to={`/agents/${encodeURIComponent(row.seat)}`}
        className="font-medium hover:text-fg focus-mark"
      >
        {row.seat}
      </Link>
      <span className="text-label uppercase tracking-wider text-fg-muted">{row.reasonLabel}</span>
      {row.elapsedLabel !== null && (
        <span className="text-label text-fg-muted">{row.elapsedLabel}</span>
      )}
      {row.unverified && (
        <span
          className="text-label uppercase tracking-wider text-fg-muted"
          title="No live session confirms this entry; it is shown because it is recent."
        >
          unverified
        </span>
      )}
      {note !== undefined && <span className="text-body text-fg-muted">{note}</span>}
    </li>
  );
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
 */
function footnoteFor(droppedStale: number, skippedMalformed: number): string | null {
  const parts: string[] = [];
  if (droppedStale > 0) {
    parts.push(`${droppedStale} stale ${plural(droppedStale, 'entry', 'entries')} hidden`);
  }
  if (skippedMalformed > 0) {
    parts.push(`${skippedMalformed} unreadable ${plural(skippedMalformed, 'file', 'files')}`);
  }
  return parts.length === 0 ? null : parts.join(' · ');
}

function plural(count: number, one: string, many: string): string {
  return count === 1 ? one : many;
}
