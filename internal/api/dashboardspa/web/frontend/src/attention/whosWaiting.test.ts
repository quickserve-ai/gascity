import { describe, expect, it } from 'vitest';
import type { AttentionRegistryEntry } from 'gas-city-dashboard-shared';
import { selectWhosWaiting, type WhosWaitingProbe, type WhosWaitingSession } from './whosWaiting';

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
  return { name: 'qcore/archer', state: 'active', ...overrides };
}

function select(
  entries: AttentionRegistryEntry[],
  sessions: WhosWaitingSession[],
  probes: WhosWaitingProbe[] = [],
) {
  return selectWhosWaiting({ entries, sessions, probes, nowMs: NOW_MS });
}

/** A supported probe that saw a live approval dialog. The only confirming fact. */
function approvalProbe(seat = 'qcore/archer', extra: { prompt?: string; toolName?: string } = {}) {
  return { seat, outcome: 'pending' as const, ...extra };
}

describe('the live-session join buys unconfirmed, never confirmed', () => {
  it('keeps a name-joined entry as a REPORT, not a fact', () => {
    const model = select([entry()], [session()]);
    expect(model.droppedUnverifiable).toBe(0);
    expect(model.confirmed).toEqual([]);
    expect(model.unconfirmed.map((r) => r.seat)).toEqual(['qcore/archer']);
    expect(model.unconfirmed[0]?.verification).toBe('unconfirmed');
  });

  it('does not confirm a permission claim just because the seat is live', () => {
    // The clear-on-resume path can fail silently, so a live seat plus a
    // historical "permission" record is not a pending request.
    const model = select([entry({ reason: 'permission' })], [session()]);
    expect(model.confirmed).toEqual([]);
    expect(model.unconfirmed[0]).toMatchObject({
      state: 'blocked',
      verification: 'unconfirmed',
      reasonLabel: 'permission request',
    });
  });

  it('counts an entry with no live session as unverifiable, and does not call it stale', () => {
    const model = select([entry()], []);
    expect(model.confirmed).toEqual([]);
    expect(model.unconfirmed).toEqual([]);
    expect(model.unknown).toEqual([]);
    expect(model.droppedUnverifiable).toBe(1);
  });

  it('counts a blocked-tier entry as unverifiable too — the join runs before the claim', () => {
    const model = select([entry({ reason: 'permission' })], []);
    expect(model.unconfirmed).toEqual([]);
    expect(model.droppedUnverifiable).toBe(1);
  });

  it('does not join a closed session', () => {
    const model = select([entry()], [session({ state: 'closed' })]);
    expect(model.droppedUnverifiable).toBe(1);
  });

  it('does not join a session with a different name', () => {
    const model = select([entry()], [session({ name: 'qcore/pam' })]);
    expect(model.droppedUnverifiable).toBe(1);
  });
});

describe('confirmed — the one confirmable claim', () => {
  it('confirms a claude question when a supported probe returns an approval dialog', () => {
    const model = select(
      [entry()],
      [session()],
      [approvalProbe('qcore/archer', { prompt: 'Run tests?\nmore lines', toolName: 'Bash' })],
    );
    expect(model.unconfirmed).toEqual([]);
    expect(model.confirmed).toHaveLength(1);
    expect(model.confirmed[0]).toMatchObject({
      verification: 'confirmed',
      // The dialog is evidence for the stronger claim, so it upgrades `state`.
      state: 'blocked',
      reasonLabel: 'approval prompt',
      detail: 'Bash: Run tests?',
    });
  });

  it('leaves a claude question unconfirmed when the probe answered supported+none', () => {
    // The tmux probe greps APPROVAL MARKERS only: a real AskUserQuestion shows
    // none, and so does a busy session. The negative proves nothing.
    const model = select([entry()], [session()], [{ seat: 'qcore/archer', outcome: 'none' }]);
    expect(model.confirmed).toEqual([]);
    expect(model.unknown).toEqual([]);
    expect(model.unconfirmed[0]).toMatchObject({
      verification: 'unconfirmed',
      state: 'idle',
      reasonLabel: 'idle at prompt',
    });
  });

  it('leaves a claude question unconfirmed when no probe ran for that seat at all', () => {
    const model = select([entry()], [session()]);
    expect(model.confirmed).toEqual([]);
    expect(model.unconfirmed).toHaveLength(1);
  });

  it('leaves a permission entry unconfirmed when its probe answered supported+none', () => {
    const model = select(
      [entry({ reason: 'permission' })],
      [session()],
      [{ seat: 'qcore/archer', outcome: 'none' }],
    );
    expect(model.confirmed).toEqual([]);
    expect(model.unconfirmed[0]?.verification).toBe('unconfirmed');
  });

  it('ignores a confirming probe belonging to a different seat', () => {
    const model = select([entry()], [session()], [approvalProbe('qcore/pam')]);
    expect(model.confirmed).toEqual([]);
    expect(model.unconfirmed[0]?.detail).toBeUndefined();
  });
});

describe('omp entries are never confirmable today', () => {
  it('renders an omp question as an unconfirmed blocked claim', () => {
    const model = select([entry({ runtime: 'omp' })], [session()]);
    expect(model.confirmed).toEqual([]);
    expect(model.unconfirmed[0]).toMatchObject({
      runtime: 'omp',
      state: 'blocked',
      verification: 'unconfirmed',
      reasonLabel: 'question',
    });
  });

  it('renders an omp permission request as unconfirmed even though its probe is unsupported', () => {
    const model = select(
      [entry({ runtime: 'omp', reason: 'permission' })],
      [session()],
      [{ seat: 'qcore/archer', outcome: 'unsupported' }],
    );
    expect(model.confirmed).toEqual([]);
    expect(model.unknown).toEqual([]);
    expect(model.unconfirmed[0]?.verification).toBe('unconfirmed');
  });

  it('keeps an omp blocked entry unconfirmed', () => {
    const model = select([entry({ runtime: 'omp', reason: 'blocked' })], [session()]);
    expect(model.unconfirmed[0]).toMatchObject({ state: 'blocked', reasonLabel: 'blocked' });
    expect(model.confirmed).toEqual([]);
  });
});

describe('unknown — the states we cannot establish either way', () => {
  it('marks a seat whose probe errored as unknown, not as a successful negative', () => {
    const model = select([entry()], [session()], [{ seat: 'qcore/archer', outcome: 'error' }]);
    expect(model.unconfirmed).toEqual([]);
    expect(model.confirmed).toEqual([]);
    expect(model.unknown).toHaveLength(1);
    expect(model.unknown[0]).toMatchObject({
      verification: 'unknown',
      unknownReason: 'probe-error',
    });
  });

  it('marks a seatless entry unknown rather than dropping it silently', () => {
    const model = select([entry({ seat: '' })], [session()]);
    expect(model.droppedUnverifiable).toBe(0);
    expect(model.unknown).toHaveLength(1);
    expect(model.unknown[0]).toMatchObject({
      seat: '',
      verification: 'unknown',
      unknownReason: 'identity',
    });
  });

  it('gives seatless entries distinct keys so several can render', () => {
    const model = select([entry({ seat: '' }), entry({ seat: '' })], [session()]);
    expect(model.unknown).toHaveLength(2);
    expect(new Set(model.unknown.map((r) => r.key)).size).toBe(2);
  });

  it('marks an entry with an empty session id unknown — empty vs empty verifies nothing', () => {
    const model = select([entry({ session_id: '' })], [session()]);
    expect(model.unconfirmed).toEqual([]);
    expect(model.unknown[0]).toMatchObject({
      verification: 'unknown',
      unknownReason: 'identity',
    });
  });

  it('still confirms an empty-session-id entry when its probe returns a live dialog', () => {
    // A positive probe is direct evidence about the SEAT; it does not depend on
    // the entry's id fields being coherent.
    const model = select(
      [entry({ session_id: '' })],
      [session()],
      [approvalProbe('qcore/archer', { toolName: 'Edit' })],
    );
    expect(model.unknown).toEqual([]);
    expect(model.confirmed[0]).toMatchObject({ verification: 'confirmed', detail: 'Edit' });
  });

  it('prefers the probe error over the identity gap when both apply', () => {
    const model = select(
      [entry({ session_id: '' })],
      [session()],
      [{ seat: 'qcore/archer', outcome: 'error' }],
    );
    expect(model.unknown[0]?.unknownReason).toBe('probe-error');
  });
});

describe('interactive-* seats — retention policy, not verification', () => {
  const interactive = entry({ seat: 'interactive-abc123', session_id: 'sess-interactive' });

  it('marks a young interactive seat unknown, never idle', () => {
    const model = select([interactive], []);
    expect(model.droppedUnverifiable).toBe(0);
    expect(model.unconfirmed).toEqual([]);
    expect(model.unknown).toHaveLength(1);
    expect(model.unknown[0]).toMatchObject({
      verification: 'unknown',
      unknownReason: 'retention',
    });
  });

  it('drops an interactive seat older than the 48h retention window', () => {
    const stale = entry({
      seat: 'interactive-abc123',
      since: '2026-09-05T12:50:49-0700', // ~3 days back
    });
    const model = select([stale], []);
    expect(model.droppedUnverifiable).toBe(1);
    expect(model.unknown).toEqual([]);
  });

  it('drops an interactive seat with an unparseable since', () => {
    const model = select([entry({ seat: 'interactive-abc123', since: 'not-a-date' })], []);
    expect(model.droppedUnverifiable).toBe(1);
    expect(model.unknown).toEqual([]);
  });

  it('treats an interactive seat that DOES have a live session like any other seat', () => {
    const model = select([interactive], [session({ name: 'interactive-abc123' })]);
    expect(model.unknown).toEqual([]);
    expect(model.unconfirmed[0]?.verification).toBe('unconfirmed');
  });
});

describe('the claim (`state`) is read from the registry alone', () => {
  it('reads permission as a blocked claim', () => {
    const model = select([entry({ reason: 'permission' })], [session()]);
    expect(model.unconfirmed[0]).toMatchObject({
      state: 'blocked',
      reasonLabel: 'permission request',
    });
  });

  it('reads blocked as a blocked claim', () => {
    const model = select([entry({ reason: 'blocked' })], [session()]);
    expect(model.unconfirmed[0]).toMatchObject({ state: 'blocked', reasonLabel: 'blocked' });
  });

  it('reads a claude question as an idle claim', () => {
    const model = select([entry()], [session()]);
    expect(model.unconfirmed[0]).toMatchObject({ state: 'idle', reasonLabel: 'idle at prompt' });
  });

  it('reads an unrecognized reason as a blocked claim and shows the hook’s own word', () => {
    const model = select([entry({ reason: 'quarantined' })], [session()]);
    expect(model.unconfirmed[0]).toMatchObject({ state: 'blocked', reasonLabel: 'quarantined' });
  });

  it('does not let a probe outcome of `none` change the claim', () => {
    const withProbe = select([entry()], [session()], [{ seat: 'qcore/archer', outcome: 'none' }]);
    const withoutProbe = select([entry()], [session()]);
    expect(withProbe.unconfirmed[0]?.state).toBe(withoutProbe.unconfirmed[0]?.state);
  });
});

describe('probe detail', () => {
  it('uses the tool name alone when the probe carried no prompt', () => {
    const model = select(
      [entry()],
      [session()],
      [approvalProbe('qcore/archer', { toolName: 'Edit' })],
    );
    expect(model.confirmed[0]?.detail).toBe('Edit');
  });

  it('uses the prompt alone when the probe carried no tool name', () => {
    const model = select(
      [entry()],
      [session()],
      [approvalProbe('qcore/archer', { prompt: 'Proceed?' })],
    );
    expect(model.confirmed[0]?.detail).toBe('Proceed?');
  });

  it('omits detail entirely when the probe carried neither, and still confirms', () => {
    const model = select([entry()], [session()], [approvalProbe()]);
    expect(model.confirmed).toHaveLength(1);
    expect(model.confirmed[0]?.detail).toBeUndefined();
  });
});

describe('elapsed and timestamp formats', () => {
  it('reads the offset-style (-0700) timestamp the claude hook writes', () => {
    // 12:50:49 -0700 is 19:50:49Z; NOW is 20:00:00Z, so ~9m -> rounds to 1h.
    const model = select([entry({ since: '2026-09-08T12:50:49-0700' })], [session()]);
    expect(model.unconfirmed[0]?.elapsedMs).toBe(NOW_MS - Date.parse('2026-09-08T19:50:49Z'));
    expect(model.unconfirmed[0]?.elapsedLabel).toBe('1h');
  });

  it('reads the Z-style timestamp the omp hook writes', () => {
    const model = select([entry({ runtime: 'omp', since: '2026-09-08T14:00:00Z' })], [session()]);
    expect(model.unconfirmed[0]?.elapsedMs).toBe(6 * 60 * 60 * 1000);
    expect(model.unconfirmed[0]?.elapsedLabel).toBe('6h');
  });

  it('treats the two formats as the same instant', () => {
    const offsetForm = select([entry({ since: '2026-09-08T12:50:49-0700' })], [session()]);
    const zForm = select([entry({ since: '2026-09-08T19:50:49Z' })], [session()]);
    expect(offsetForm.unconfirmed[0]?.elapsedMs).toBe(zForm.unconfirmed[0]?.elapsedMs);
  });

  it('nulls the elapsed fields on an unparseable timestamp rather than guessing', () => {
    const model = select([entry({ since: 'whenever' })], [session()]);
    expect(model.unconfirmed[0]?.elapsedMs).toBeNull();
    expect(model.unconfirmed[0]?.elapsedLabel).toBeNull();
  });
});

describe('ordering', () => {
  it('sorts each group by since ascending — longest waiting first', () => {
    const entries = [
      entry({ seat: 'b', session_id: 'kb', since: '2026-09-08T19:55:00Z' }),
      entry({ seat: 'a', session_id: 'ka', since: '2026-09-08T10:00:00Z' }),
      entry({ seat: 'c', session_id: 'kc', since: '2026-09-08T19:00:00Z' }),
      entry({ seat: 'p', session_id: 'kp', since: '2026-09-08T19:30:00Z' }),
      entry({ seat: 'q', session_id: 'kq', since: '2026-09-08T11:00:00Z' }),
    ];
    const sessions = entries.map((e) => session({ name: e.seat }));
    const model = select(entries, sessions, [approvalProbe('p'), approvalProbe('q')]);
    expect(model.unconfirmed.map((r) => r.seat)).toEqual(['a', 'c', 'b']);
    expect(model.confirmed.map((r) => r.seat)).toEqual(['q', 'p']);
  });

  it('sorts the unknown group oldest first too', () => {
    const entries = [
      entry({ seat: 'y', session_id: 'ky', since: '2026-09-08T19:00:00Z' }),
      entry({ seat: 'x', session_id: 'kx', since: '2026-09-08T10:00:00Z' }),
    ];
    const sessions = entries.map((e) => session({ name: e.seat }));
    const probes: WhosWaitingProbe[] = [
      { seat: 'x', outcome: 'error' },
      { seat: 'y', outcome: 'error' },
    ];
    expect(select(entries, sessions, probes).unknown.map((r) => r.seat)).toEqual(['x', 'y']);
  });

  it('sorts an unparseable timestamp last, then by seat', () => {
    const entries = [
      entry({ seat: 'z', session_id: 'kz', since: 'nope' }),
      entry({ seat: 'a', session_id: 'ka', since: 'nope' }),
      entry({ seat: 'm', session_id: 'km', since: '2026-09-08T19:00:00Z' }),
    ];
    const sessions = entries.map((e) => session({ name: e.seat }));
    expect(select(entries, sessions).unconfirmed.map((r) => r.seat)).toEqual(['m', 'a', 'z']);
  });
});

describe('row projection', () => {
  it('carries seat, runtime, raw reason, since and summary through', () => {
    const model = select([entry({ runtime: 'omp', reason: 'blocked' })], [session()]);
    expect(model.unconfirmed[0]).toMatchObject({
      key: 'qcore/archer',
      seat: 'qcore/archer',
      runtime: 'omp',
      state: 'blocked',
      verification: 'unconfirmed',
      reason: 'blocked',
      since: '2026-09-08T12:50:49-0700',
      summary: 'Claude is waiting for your input',
    });
  });

  it('omits unknownReason on rows that are not unknown', () => {
    const model = select([entry()], [session()]);
    expect(model.unconfirmed[0]?.unknownReason).toBeUndefined();
  });

  it('omits an empty summary rather than rendering a blank line', () => {
    const model = select([entry({ summary: '   ' })], [session()]);
    expect(model.unconfirmed[0]?.summary).toBeUndefined();
  });

  it('returns empty groups and a zero count for an empty registry', () => {
    const model = select([], [session()]);
    expect(model).toEqual({
      confirmed: [],
      unconfirmed: [],
      unknown: [],
      droppedUnverifiable: 0,
    });
  });
});
