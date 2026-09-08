import { describe, expect, it } from 'vitest';
import type { AttentionRegistryEntry } from 'gas-city-dashboard-shared';
import { selectWhosWaiting, type WhosWaitingPending, type WhosWaitingSession } from './whosWaiting';

// Wall clock for every case below: 2026-09-08T20:00:00Z, i.e. 13:00 -0700.
const NOW_MS = Date.parse('2026-09-08T20:00:00Z');

function entry(overrides: Partial<AttentionRegistryEntry> = {}): AttentionRegistryEntry {
  return {
    seat: 'qcore/archer',
    runtime: 'claude',
    event_id: 'a22fb8d9',
    reason: 'question',
    state: 'waiting_user',
    since: '2026-09-08T12:50:49-0700',
    summary: 'Claude is waiting for your input',
    session_id: 'sess-archer',
    ...overrides,
  };
}

function session(overrides: Partial<WhosWaitingSession> = {}): WhosWaitingSession {
  return { name: 'qcore/archer', sessionKey: 'sess-archer', state: 'active', ...overrides };
}

function select(
  entries: AttentionRegistryEntry[],
  sessions: WhosWaitingSession[],
  pending: WhosWaitingPending[] = [],
) {
  return selectWhosWaiting({ entries, sessions, pending, nowMs: NOW_MS });
}

describe('liveness verification', () => {
  it('keeps an entry whose session_id matches a live session key', () => {
    const model = select([entry()], [session()]);
    expect(model.droppedStale).toBe(0);
    expect(model.idle.map((r) => r.seat)).toEqual(['qcore/archer']);
    expect(model.idle[0]?.unverified).toBe(false);
  });

  it('drops an entry with no session at all, and only counts it', () => {
    const model = select([entry()], []);
    expect(model.blocked).toEqual([]);
    expect(model.idle).toEqual([]);
    expect(model.droppedStale).toBe(1);
  });

  it('drops an entry whose seat cycled to a new session key', () => {
    // Same seat name, different runtime session: the wait that wrote this entry
    // is over even though a session by that name is still running.
    const model = select([entry()], [session({ sessionKey: 'sess-archer-NEXT' })]);
    expect(model.droppedStale).toBe(1);
    expect(model.idle).toEqual([]);
  });

  it('drops an entry backed only by a closed session', () => {
    const model = select([entry()], [session({ state: 'closed' })]);
    expect(model.droppedStale).toBe(1);
  });

  it('drops a blocked-tier entry too — staleness is checked before tiering', () => {
    const model = select([entry({ reason: 'permission' })], []);
    expect(model.blocked).toEqual([]);
    expect(model.droppedStale).toBe(1);
  });

  it('falls back to a seat-name join when the source redacts session keys', () => {
    // The typed /v0 session list does not expose session_key, so the pane
    // supplies name + state only.
    const model = select([entry()], [{ name: 'qcore/archer', state: 'active' }]);
    expect(model.droppedStale).toBe(0);
    expect(model.idle).toHaveLength(1);
  });

  it('does not match a keyless session with a different name', () => {
    const model = select([entry()], [{ name: 'qcore/pam', state: 'active' }]);
    expect(model.droppedStale).toBe(1);
  });
});

describe('interactive-* seats', () => {
  const interactive = entry({ seat: 'interactive-abc123', session_id: 'sess-interactive' });

  it('keeps a young interactive seat with no session, flagged unverified', () => {
    const model = select([interactive], []);
    expect(model.droppedStale).toBe(0);
    expect(model.idle).toHaveLength(1);
    expect(model.idle[0]?.unverified).toBe(true);
  });

  it('drops an interactive seat older than 48h', () => {
    const stale = entry({
      seat: 'interactive-abc123',
      since: '2026-09-05T12:50:49-0700', // ~3 days back
    });
    const model = select([stale], []);
    expect(model.droppedStale).toBe(1);
    expect(model.idle).toEqual([]);
  });

  it('drops an interactive seat with an unparseable since', () => {
    const model = select([entry({ seat: 'interactive-abc123', since: 'not-a-date' })], []);
    expect(model.droppedStale).toBe(1);
  });

  it('does not flag an interactive seat that does have a live session', () => {
    const model = select(
      [interactive],
      [session({ name: 'interactive-abc123', sessionKey: 'sess-interactive' })],
    );
    expect(model.idle[0]?.unverified).toBe(false);
  });
});

describe('tiering', () => {
  it('puts reason=permission in blocked', () => {
    const model = select([entry({ reason: 'permission' })], [session()]);
    expect(model.blocked.map((r) => r.reasonLabel)).toEqual(['permission request']);
    expect(model.idle).toEqual([]);
  });

  it('puts reason=blocked in blocked', () => {
    const model = select([entry({ reason: 'blocked' })], [session()]);
    expect(model.blocked.map((r) => r.reasonLabel)).toEqual(['blocked']);
  });

  it('puts an omp question in blocked — omp questions are real questions', () => {
    const model = select([entry({ runtime: 'omp' })], [session()]);
    expect(model.blocked.map((r) => r.reasonLabel)).toEqual(['question']);
    expect(model.idle).toEqual([]);
  });

  it('puts a claude question in idle and labels it honestly', () => {
    const model = select([entry()], [session()]);
    expect(model.blocked).toEqual([]);
    expect(model.idle.map((r) => r.reasonLabel)).toEqual(['idle at prompt']);
  });

  it('promotes a claude question to blocked when a pending probe confirms it', () => {
    const model = select(
      [entry()],
      [session()],
      [{ seat: 'qcore/archer', prompt: 'Run tests?\nmore lines', toolName: 'Bash' }],
    );
    expect(model.idle).toEqual([]);
    expect(model.blocked).toHaveLength(1);
    expect(model.blocked[0]?.reasonLabel).toBe('approval prompt');
    expect(model.blocked[0]?.detail).toBe('Bash: Run tests?');
  });

  it('ignores a pending probe for a different seat', () => {
    const model = select([entry()], [session()], [{ seat: 'qcore/pam', prompt: 'Run tests?' }]);
    expect(model.idle).toHaveLength(1);
    expect(model.idle[0]?.detail).toBeUndefined();
  });

  it('treats an unrecognized reason as blocked and shows the hook’s own word', () => {
    const model = select([entry({ reason: 'quarantined' })], [session()]);
    expect(model.blocked.map((r) => r.reasonLabel)).toEqual(['quarantined']);
  });
});

describe('probe detail', () => {
  it('uses the tool name alone when the probe carried no prompt', () => {
    const model = select([entry()], [session()], [{ seat: 'qcore/archer', toolName: 'Edit' }]);
    expect(model.blocked[0]?.detail).toBe('Edit');
  });

  it('uses the prompt alone when the probe carried no tool name', () => {
    const model = select([entry()], [session()], [{ seat: 'qcore/archer', prompt: 'Proceed?' }]);
    expect(model.blocked[0]?.detail).toBe('Proceed?');
  });

  it('omits detail entirely when the probe carried neither', () => {
    const model = select([entry()], [session()], [{ seat: 'qcore/archer' }]);
    expect(model.blocked[0]?.detail).toBeUndefined();
    // Still blocked: the probe's existence is the signal, not its text.
    expect(model.blocked).toHaveLength(1);
  });
});

describe('elapsed and timestamp formats', () => {
  it('reads the offset-style (-0700) timestamp the claude hook writes', () => {
    // 12:50:49 -0700 is 19:50:49Z; NOW is 20:00:00Z, so ~9m -> rounds to 1h.
    const model = select([entry({ since: '2026-09-08T12:50:49-0700' })], [session()]);
    expect(model.idle[0]?.elapsedMs).toBe(NOW_MS - Date.parse('2026-09-08T19:50:49Z'));
    expect(model.idle[0]?.elapsedLabel).toBe('1h');
  });

  it('reads the Z-style timestamp the omp hook writes', () => {
    const model = select([entry({ runtime: 'omp', since: '2026-09-08T14:00:00Z' })], [session()]);
    expect(model.blocked[0]?.elapsedMs).toBe(6 * 60 * 60 * 1000);
    expect(model.blocked[0]?.elapsedLabel).toBe('6h');
  });

  it('treats the two formats as the same instant', () => {
    const offsetForm = select([entry({ since: '2026-09-08T12:50:49-0700' })], [session()]);
    const zForm = select([entry({ since: '2026-09-08T19:50:49Z' })], [session()]);
    expect(offsetForm.idle[0]?.elapsedMs).toBe(zForm.idle[0]?.elapsedMs);
  });

  it('nulls the elapsed fields on an unparseable timestamp rather than guessing', () => {
    const model = select([entry({ since: 'whenever' })], [session()]);
    expect(model.idle[0]?.elapsedMs).toBeNull();
    expect(model.idle[0]?.elapsedLabel).toBeNull();
  });
});

describe('ordering', () => {
  it('sorts each tier by since ascending — longest waiting first', () => {
    const entries = [
      entry({ seat: 'b', session_id: 'kb', since: '2026-09-08T19:55:00Z' }),
      entry({ seat: 'a', session_id: 'ka', since: '2026-09-08T10:00:00Z' }),
      entry({ seat: 'c', session_id: 'kc', since: '2026-09-08T19:00:00Z' }),
      entry({ seat: 'p', session_id: 'kp', reason: 'permission', since: '2026-09-08T19:30:00Z' }),
      entry({ seat: 'q', session_id: 'kq', reason: 'permission', since: '2026-09-08T11:00:00Z' }),
    ];
    const sessions = entries.map((e) => session({ name: e.seat, sessionKey: e.session_id }));
    const model = select(entries, sessions);
    expect(model.idle.map((r) => r.seat)).toEqual(['a', 'c', 'b']);
    expect(model.blocked.map((r) => r.seat)).toEqual(['q', 'p']);
  });

  it('sorts an unparseable timestamp last, then by seat', () => {
    const entries = [
      entry({ seat: 'z', session_id: 'kz', since: 'nope' }),
      entry({ seat: 'a', session_id: 'ka', since: 'nope' }),
      entry({ seat: 'm', session_id: 'km', since: '2026-09-08T19:00:00Z' }),
    ];
    const sessions = entries.map((e) => session({ name: e.seat, sessionKey: e.session_id }));
    expect(select(entries, sessions).idle.map((r) => r.seat)).toEqual(['m', 'a', 'z']);
  });
});

describe('row projection', () => {
  it('carries seat, runtime, raw reason, since and summary through', () => {
    const model = select([entry({ runtime: 'omp', reason: 'blocked' })], [session()]);
    const row = model.blocked[0];
    expect(row).toMatchObject({
      key: 'qcore/archer',
      seat: 'qcore/archer',
      runtime: 'omp',
      tier: 'blocked',
      reason: 'blocked',
      since: '2026-09-08T12:50:49-0700',
      summary: 'Claude is waiting for your input',
      unverified: false,
    });
  });

  it('omits an empty summary rather than rendering a blank line', () => {
    const model = select([entry({ summary: '   ' })], [session()]);
    expect(model.idle[0]?.summary).toBeUndefined();
  });

  it('drops a seatless entry as stale — it can never be addressed', () => {
    const model = select([entry({ seat: '' })], [session()]);
    expect(model.droppedStale).toBe(1);
    expect(model.idle).toEqual([]);
  });

  it('returns empty tiers and a zero count for an empty registry', () => {
    const model = select([], [session()]);
    expect(model).toEqual({ blocked: [], idle: [], droppedStale: 0 });
  });
});
