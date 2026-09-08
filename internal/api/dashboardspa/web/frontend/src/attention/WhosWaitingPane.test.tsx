import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { invalidate } from '../api/cache';
import { NowProvider } from '../contexts/NowContext';
import { resetSupervisorApiForTests } from '../supervisor/client';
import { assertAtMostOneMark } from '../test/assertions/oneMarkRule';
import { WhosWaitingPane } from './WhosWaitingPane';

// Renders the real component against a stubbed fetch, so the BFF decoder, the
// supervisor client, the pure selector, and the paint are all exercised
// together — the seam this pane actually fails at is the wire, not the markup.
//
// The claim under test is the evidence contract: a row's words must never
// outrun what was actually probed.

interface StubOptions {
  attention?: unknown;
  attentionStatus?: number;
  /**
   * How the per-session pending probe answers.
   *  'pending'     — supported, with a live approval dialog (the confirming case)
   *  'none'        — supported, nothing pending right now
   *  'unsupported' — 200 { supported: false }, as an omp/ACP session answers
   *  'error'       — the probe request fails outright
   */
  pending?: 'pending' | 'none' | 'unsupported' | 'error';
}

const ATTENTION_URL = '/api/city/test-city/attention';
const SESSIONS_URL = '/v0/city/test-city/sessions?limit=1000';
const AGENTS_URL = '/v0/city/test-city/agents';

function defaultAttention() {
  return {
    entries: [
      {
        seat: 'qcore/archer',
        runtime: 'claude',
        event_id: 'e1',
        reason: 'question',
        state: 'waiting_user',
        since: '2026-09-08T12:50:49-0700',
        summary: 'Claude is waiting for your input',
        session_id: 'sess-archer',
      },
      {
        seat: 'katya',
        runtime: 'omp',
        event_id: 'e2',
        reason: 'permission',
        state: 'waiting_user',
        since: '2026-09-08T10:00:00Z',
        summary: 'approve a write outside the worktree',
        session_id: 'sess-katya',
      },
      {
        seat: 'qcore/ghost',
        runtime: 'claude',
        event_id: 'e3',
        reason: 'question',
        state: 'waiting_user',
        since: '2026-09-08T09:00:00Z',
        summary: 'Claude is waiting for your input',
        session_id: 'sess-ghost',
      },
    ],
    skippedMalformed: 1,
    readAt: '2026-09-08T20:00:00Z',
  };
}

function sessionRow(alias: string) {
  return {
    id: `gc-${alias.replace(/\W/g, '')}`,
    session_name: alias.replace(/\W/g, '_'),
    alias,
    state: 'active',
    template: alias,
    provider: 'claude-5',
    running: true,
    attached: false,
    created_at: '2026-09-08T00:00:00Z',
    title: alias,
  };
}

function pendingResponse(mode: StubOptions['pending']): Response {
  if (mode === 'error') return jsonResponse({ error: 'probe blew up' }, { status: 500 });
  if (mode === 'unsupported') return jsonResponse({ supported: false });
  if (mode === 'none') return jsonResponse({ supported: true });
  return jsonResponse({
    supported: true,
    pending: {
      kind: 'approval',
      request_id: 'req-1',
      prompt: 'Run the migration?\nsecond line',
      metadata: { tool_name: 'Bash' },
    },
  });
}

function stubFetch(options: StubOptions = {}) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const raw =
        input instanceof Request
          ? input.url
          : input instanceof URL
            ? input.toString()
            : String(input);
      const url = raw.startsWith(window.location.origin)
        ? raw.slice(window.location.origin.length)
        : raw;

      if (url === ATTENTION_URL) {
        return jsonResponse(options.attention ?? defaultAttention(), {
          status: options.attentionStatus ?? 200,
        });
      }
      if (url === SESSIONS_URL) {
        return jsonResponse({
          items: [sessionRow('qcore/archer'), sessionRow('katya')],
          total: 2,
        });
      }
      if (url === AGENTS_URL) {
        return jsonResponse({
          items: [
            {
              name: 'qcore/archer',
              available: true,
              running: true,
              suspended: false,
              state: 'active',
              pack_derived: false,
              session: { name: 'qcore_archer', attached: false },
            },
          ],
          total: 1,
        });
      }
      if (url.includes('/pending')) {
        return pendingResponse(options.pending);
      }
      throw new Error(`unexpected fetch: ${url}`);
    }),
  );
  resetSupervisorApiForTests();
}

function jsonResponse(payload: unknown, init?: ResponseInit): Response {
  return new Response(JSON.stringify(payload), {
    status: init?.status ?? 200,
    headers: { 'content-type': 'application/json' },
  });
}

function renderPane() {
  return render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <NowProvider intervalMs={1_000_000}>
        <WhosWaitingPane />
      </NowProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  invalidate('attention:registry:test-city');
  invalidate('attention:live-seats:test-city');
});

afterEach(() => {
  cleanup();
  resetSupervisorApiForTests();
  vi.unstubAllGlobals();
});

describe('WhosWaitingPane', () => {
  it('confirms only the probed seat, reports the rest, and accounts for what it hid', async () => {
    stubFetch();
    const { container } = renderPane();

    // qcore/archer: a claude question the pending probe independently confirmed.
    const confirmed = await screen.findByTestId('whos-waiting-confirmed');
    expect(within(confirmed).getByRole('heading', { name: 'Needs you (confirmed)' })).toBeDefined();
    expect(
      within(confirmed)
        .getAllByRole('link')
        .map((l) => l.textContent),
    ).toEqual(['qcore/archer']);
    // The probe's own words, not the hook's generic summary.
    expect(confirmed.textContent).toContain('Bash: Run the migration?');
    expect(confirmed.textContent).not.toContain('unconfirmed');

    // katya: an omp permission request with no probe of any kind. It is a
    // REPORT, and it says so — it never reaches the confirmed section.
    const unconfirmed = screen.getByTestId('whos-waiting-unconfirmed');
    expect(
      within(unconfirmed).getByRole('heading', { name: 'Reported waiting (unconfirmed)' }),
    ).toBeDefined();
    expect(
      within(unconfirmed)
        .getAllByRole('link')
        .map((l) => l.textContent),
    ).toEqual(['katya']);
    // Wording, not wall clock: `useNow` is the real clock here, so the age
    // phrase itself is not a stable assertion.
    expect(unconfirmed.textContent).toMatch(
      /reported permission request — as of \d+[hd] ago, unconfirmed/,
    );

    const katyaLink = screen.getByRole('link', { name: 'katya' });
    expect(katyaLink.getAttribute('href')).toBe('/agents/katya');
    const archerLink = screen.getByRole('link', { name: 'qcore/archer' });
    expect(archerLink.getAttribute('href')).toBe('/agents/qcore%2Farcher');

    // qcore/ghost has no live session by name: counted as unverifiable, and
    // never described as stale — only the reaper can say that.
    expect(screen.queryByRole('link', { name: 'qcore/ghost' })).toBeNull();
    expect(screen.getByTestId('whos-waiting-footnote').textContent).toBe(
      '1 entry with no live session (unverifiable) · 1 unreadable file',
    );
    expect(screen.getByTestId('whos-waiting-footnote').textContent).not.toContain('stale');

    assertAtMostOneMark(container);
  });

  it('does not confirm anything when the probe answers supported+none', async () => {
    // The tmux probe sees approval markers only, so its negative is not
    // evidence that the seat is fine.
    stubFetch({ pending: 'none' });
    renderPane();

    const unconfirmed = await screen.findByTestId('whos-waiting-unconfirmed');
    expect(screen.queryByTestId('whos-waiting-confirmed')).toBeNull();
    expect(unconfirmed.textContent).toContain('qcore/archer');
    expect(unconfirmed.textContent).toContain('reported idle at prompt');
    expect(unconfirmed.textContent).toContain('unconfirmed');
    expect(unconfirmed.textContent).not.toContain('approval prompt');
  });

  it('does not confirm anything when the runtime has no pane to probe', async () => {
    stubFetch({ pending: 'unsupported' });
    renderPane();

    const unconfirmed = await screen.findByTestId('whos-waiting-unconfirmed');
    expect(screen.queryByTestId('whos-waiting-confirmed')).toBeNull();
    // Both seats are reports; the unsupported probe took nothing else down.
    expect(
      within(unconfirmed)
        .getAllByRole('link')
        .map((l) => l.textContent),
    ).toEqual(['katya', 'qcore/archer']);
  });

  it('puts a seat whose probe errored in the collapsed unknown group, with the reason', async () => {
    stubFetch({ pending: 'error' });
    renderPane();

    const unknown = await screen.findByTestId('whos-waiting-unknown');
    expect(unknown.textContent).toContain('Unknown (1)');
    expect(unknown.textContent).toContain('qcore/archer');
    expect(unknown.textContent).toContain('unknown: probe failed');
    // The failed probe costs exactly that one seat.
    expect(screen.getByTestId('whos-waiting-unconfirmed').textContent).toContain('katya');
    expect(screen.queryByTestId('whos-waiting-confirmed')).toBeNull();
  });

  it('shows one quiet line, and no headings, when nobody is waiting', async () => {
    stubFetch({ attention: { entries: [], skippedMalformed: 0, readAt: '2026-09-08T20:00:00Z' } });
    renderPane();

    const pane = await screen.findByTestId('whos-waiting');
    expect(pane.textContent).toBe('No seat is waiting on you.');
    expect(screen.queryByRole('heading')).toBeNull();
    expect(screen.queryByTestId('whos-waiting-footnote')).toBeNull();
  });

  it('still accounts for hidden entries when every row was dropped', async () => {
    stubFetch({
      attention: {
        entries: [
          {
            seat: 'qcore/ghost',
            runtime: 'claude',
            event_id: 'e1',
            reason: 'question',
            state: 'waiting_user',
            since: '2026-09-08T09:00:00Z',
            summary: 'Claude is waiting for your input',
            session_id: 'sess-ghost',
          },
        ],
        skippedMalformed: 0,
        readAt: '2026-09-08T20:00:00Z',
      },
    });
    renderPane();

    await waitFor(() => {
      expect(screen.getByTestId('whos-waiting-footnote').textContent).toBe(
        '1 entry with no live session (unverifiable)',
      );
    });
  });

  it('renders nothing at all while the registry read is unresolved', async () => {
    stubFetch({ attentionStatus: 503, attention: { error: 'unavailable' } });
    const { container } = renderPane();
    // A pane that cannot read the registry must not assert that nobody is
    // waiting; it says nothing.
    await waitFor(() => {
      expect(screen.queryByTestId('whos-waiting')).toBeNull();
    });
    expect(container.textContent).toBe('');
  });
});
