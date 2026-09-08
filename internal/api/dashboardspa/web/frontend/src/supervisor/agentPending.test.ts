import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { AgentResponse, SessionResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import { setActiveCity } from '../api/cityBase';
import type * as SupervisorClient from './client';
import { listAgentPendingInteractions, probeAgentPendingInteractions } from './agentPending';

// probeAgentPendingInteractions exists so a caller can tell FOUR probe outcomes
// apart. listAgentPendingInteractions collapses three of them into "absent",
// which is fine for a list of dialogs to answer and fatal for anything
// reasoning about evidence (attention/whosWaiting.ts).

const mockSupervisorApi = vi.hoisted(() => ({ sessionPending: vi.fn() }));

vi.mock('./client', async (importOriginal) => {
  const actual = await importOriginal<typeof SupervisorClient>();
  return { ...actual, supervisorApi: () => mockSupervisorApi };
});

function agent(name: string, sessionName: string | undefined): AgentResponse {
  const row: Record<string, unknown> = {
    name,
    available: true,
    running: true,
    suspended: false,
    state: 'active',
    pack_derived: false,
  };
  if (sessionName !== undefined) row['session'] = { name: sessionName, attached: false };
  return row as unknown as AgentResponse;
}

function session(id: string, sessionName: string): SessionResponse {
  return {
    id,
    session_name: sessionName,
    state: 'active',
    running: true,
    attached: false,
  } as unknown as SessionResponse;
}

const AGENTS = [agent('alpha', 'alpha'), agent('bravo', 'bravo'), agent('charlie', 'charlie')];
const SESSIONS = [session('gc-1', 'alpha'), session('gc-2', 'bravo'), session('gc-3', 'charlie')];

beforeEach(() => {
  setActiveCity('test-city');
  mockSupervisorApi.sessionPending.mockReset();
});

describe('probeAgentPendingInteractions', () => {
  it('keeps pending, none, unsupported and error distinct', async () => {
    mockSupervisorApi.sessionPending.mockImplementation(async (_city: string, id: string) => {
      if (id === 'gc-1') {
        return { supported: true, pending: { request_id: 'r1', kind: 'approval' } };
      }
      if (id === 'gc-2') return { supported: true };
      return { supported: false };
    });

    const probes = await probeAgentPendingInteractions(AGENTS, SESSIONS);
    expect(probes.map((p) => [p.agentName, p.outcome])).toEqual([
      ['alpha', 'pending'],
      ['bravo', 'none'],
      ['charlie', 'unsupported'],
    ]);
    expect(probes[0]?.pending).toMatchObject({ request_id: 'r1' });
    expect(probes[1]?.pending).toBeUndefined();
  });

  it('turns one seat’s failed probe into that seat’s error outcome, not a rejected batch', async () => {
    mockSupervisorApi.sessionPending.mockImplementation(async (_city: string, id: string) => {
      if (id === 'gc-2') throw new Error('probe blew up');
      return { supported: true };
    });

    const probes = await probeAgentPendingInteractions(AGENTS, SESSIONS);
    expect(probes.map((p) => [p.agentName, p.outcome])).toEqual([
      ['alpha', 'none'],
      ['bravo', 'error'],
      ['charlie', 'none'],
    ]);
  });

  it('never reports a supported+none seat as pending', async () => {
    mockSupervisorApi.sessionPending.mockResolvedValue({ supported: true });
    const probes = await probeAgentPendingInteractions(AGENTS, SESSIONS);
    expect(probes.every((p) => p.outcome === 'none')).toBe(true);
  });

  it('skips agents with no session and sessions with no agent — nothing to probe', async () => {
    mockSupervisorApi.sessionPending.mockResolvedValue({ supported: true });
    const probes = await probeAgentPendingInteractions(
      [agent('alpha', 'alpha'), agent('orphan', undefined), agent('ghost', 'no-such-session')],
      SESSIONS,
    );
    expect(probes.map((p) => p.agentName)).toEqual(['alpha']);
    expect(mockSupervisorApi.sessionPending).toHaveBeenCalledTimes(1);
    expect(mockSupervisorApi.sessionPending).toHaveBeenCalledWith('test-city', 'gc-1');
  });
});

describe('listAgentPendingInteractions (unchanged flattening behaviour)', () => {
  it('returns only the seats with a pending interaction', async () => {
    mockSupervisorApi.sessionPending.mockImplementation(async (_city: string, id: string) => {
      if (id === 'gc-1') {
        return { supported: true, pending: { request_id: 'r1', kind: 'approval' } };
      }
      return { supported: false };
    });

    const rows = await listAgentPendingInteractions(AGENTS, SESSIONS);
    expect(rows.map((r) => r.agentName)).toEqual(['alpha']);
  });
});
