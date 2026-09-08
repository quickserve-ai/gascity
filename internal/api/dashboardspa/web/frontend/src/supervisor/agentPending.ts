import type {
  AgentResponse,
  PendingInteraction,
  RespondSessionResponse,
  SessionRespondInputBody,
  SessionResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow } from '../api/cityBase';
import { supervisorApi } from './client';

export interface AgentPendingInteraction {
  agentName: string;
  sessionId: string;
  sessionName: string;
  pending: PendingInteraction;
}

/**
 * Every agent that HAS a probe-able session, flattened to the seat that is
 * waiting on nothing but the probe's answer. Callers that need the answers
 * themselves use `probeAgentPendingInteractions`.
 */
export async function listAgentPendingInteractions(
  agents: readonly AgentResponse[],
  sessions: readonly SessionResponse[],
): Promise<AgentPendingInteraction[]> {
  const cityName = activeCityOrThrow('list agent pending interactions');
  const candidates = pendingProbeCandidates(agents, sessions);

  const pending = await Promise.all(
    candidates.map(async (candidate) => {
      const response = await supervisorApi().sessionPending(cityName, candidate.sessionId);
      if (response.pending === undefined) return null;
      return { ...candidate, pending: response.pending };
    }),
  );
  return pending.filter((item): item is AgentPendingInteraction => item !== null);
}

/**
 * The four outcomes a per-session pending probe can have. They are NOT
 * interchangeable, which is the whole reason this function exists alongside
 * `listAgentPendingInteractions`:
 *
 *  - `pending`     supported === true and a PendingInteraction came back.
 *  - `none`        supported === true, no interaction pending right now. This
 *                  is a successful negative for APPROVAL DIALOGS ONLY — the
 *                  tmux probe greps approval markers, so it cannot see a
 *                  question and cannot see a busy session.
 *  - `unsupported` supported === false: the runtime exposes no pane to read
 *                  (omp/ACP, or a session still being created). No information.
 *  - `error`       the request failed. No information, and crucially NOT the
 *                  same as `none`.
 *
 * `listAgentPendingInteractions` collapses the last three into "absent", which
 * makes an absent seat indistinguishable from a confirmed-fine one. Anything
 * that reasons about EVIDENCE (attention/whosWaiting.ts) must use this instead.
 */
export type AgentPendingProbeOutcome = 'pending' | 'none' | 'unsupported' | 'error';

export interface AgentPendingProbe {
  readonly agentName: string;
  readonly sessionId: string;
  readonly sessionName: string;
  readonly outcome: AgentPendingProbeOutcome;
  /** Present iff `outcome === 'pending'`. */
  readonly pending?: PendingInteraction;
}

/**
 * Probe every agent that has a session, keeping each seat's outcome distinct.
 *
 * One seat's failure never fails the batch: a probe that rejects becomes that
 * seat's `error` outcome, so the caller can say "unknown" about that seat and
 * still say something true about the others.
 */
export async function probeAgentPendingInteractions(
  agents: readonly AgentResponse[],
  sessions: readonly SessionResponse[],
): Promise<AgentPendingProbe[]> {
  const cityName = activeCityOrThrow('probe agent pending interactions');
  const candidates = pendingProbeCandidates(agents, sessions);

  return Promise.all(
    candidates.map(async (candidate): Promise<AgentPendingProbe> => {
      try {
        const response = await supervisorApi().sessionPending(cityName, candidate.sessionId);
        if (!response.supported) return { ...candidate, outcome: 'unsupported' };
        if (response.pending === undefined) return { ...candidate, outcome: 'none' };
        return { ...candidate, outcome: 'pending', pending: response.pending };
      } catch {
        return { ...candidate, outcome: 'error' };
      }
    }),
  );
}

interface PendingProbeCandidate {
  readonly agentName: string;
  readonly sessionId: string;
  readonly sessionName: string;
}

/** Agents whose session name resolves to a session id we can probe. */
function pendingProbeCandidates(
  agents: readonly AgentResponse[],
  sessions: readonly SessionResponse[],
): PendingProbeCandidate[] {
  const sessionIdsByName = sessionIdByName(sessions);
  return agents.flatMap((agent) => {
    const sessionName = agent.session?.name;
    if (sessionName === undefined) return [];
    const sessionId = sessionIdsByName.get(sessionName);
    if (sessionId === undefined) return [];
    return [{ agentName: agent.name, sessionId, sessionName }];
  });
}

export async function respondToAgentPendingInteraction(
  sessionId: string,
  body: SessionRespondInputBody,
): Promise<RespondSessionResponse> {
  const cityName = activeCityOrThrow('respond to agent pending interaction');
  return supervisorApi().respondSession(cityName, sessionId, body);
}

export function attachCommand(agentName: string): string {
  return `gc agent attach ${shellToken(agentName)}`;
}

function sessionIdByName(sessions: readonly SessionResponse[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const session of sessions) {
    if (session.session_name !== undefined) {
      out.set(session.session_name, session.id);
    }
  }
  return out;
}
function shellToken(value: string): string {
  if (/^[A-Za-z0-9_./:-]+$/.test(value)) return value;
  return `'${value.replaceAll("'", "'\\''")}'`;
}
