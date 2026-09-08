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

interface StubOptions {
  attention?: unknown;
  attentionStatus?: number;
  /** Reject every /pending probe, as an omp-only city does. */
  pendingUnsupported?: boolean;
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
        if (options.pendingUnsupported === true) {
          return jsonResponse({ error: 'unsupported' }, { status: 501 });
        }
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
  it('renders the two tiers, links each seat, and accounts for what it hid', async () => {
    stubFetch();
    const { container } = renderPane();

    const blocked = await screen.findByTestId('whos-waiting-blocked');
    expect(within(blocked).getByRole('heading', { name: 'Blocked on you' })).toBeDefined();

    // katya: an omp permission request. qcore/archer: a claude question the
    // pending probe confirmed, so it is promoted out of the idle tier.
    await waitFor(() => {
      const names = within(screen.getByTestId('whos-waiting-blocked'))
        .getAllByRole('link')
        .map((link) => link.textContent);
      expect(names).toEqual(['katya', 'qcore/archer']);
    });

    const katyaLink = screen.getByRole('link', { name: 'katya' });
    expect(katyaLink.getAttribute('href')).toBe('/agents/katya');
    const archerLink = screen.getByRole('link', { name: 'qcore/archer' });
    expect(archerLink.getAttribute('href')).toBe('/agents/qcore%2Farcher');

    // The probe's own words, not the hook's generic summary.
    expect(screen.getByTestId('whos-waiting-blocked').textContent).toContain(
      'Bash: Run the migration?',
    );
    expect(screen.getByTestId('whos-waiting-blocked').textContent).toContain('permission request');

    // qcore/ghost has no live session: dropped, and counted alongside the
    // registry's unreadable file.
    expect(screen.queryByRole('link', { name: 'qcore/ghost' })).toBeNull();
    expect(screen.getByTestId('whos-waiting-footnote').textContent).toBe(
      '1 stale entry hidden · 1 unreadable file',
    );

    assertAtMostOneMark(container);
  });

  it('labels an unconfirmed claude question honestly and puts it in its own tier', async () => {
    stubFetch({ pendingUnsupported: true });
    renderPane();

    const idle = await screen.findByTestId('whos-waiting-idle');
    expect(within(idle).getByRole('heading', { name: 'Waiting at prompt' })).toBeDefined();
    expect(idle.textContent).toContain('qcore/archer');
    expect(idle.textContent).toContain('idle at prompt');
    // Never dressed up as an urgent question.
    expect(idle.textContent).not.toContain('approval prompt');
    // The probe failing must not take the rest of the pane down with it.
    expect(screen.getByTestId('whos-waiting-blocked').textContent).toContain('katya');
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
      expect(screen.getByTestId('whos-waiting-footnote').textContent).toBe('1 stale entry hidden');
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
