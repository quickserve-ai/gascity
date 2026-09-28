import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';
import {
  SupervisorMailUnconfirmedError,
  createSupervisorApi,
  resetSupervisorApiForTests,
  setSupervisorApiForTests,
} from './client';
import { replySupervisorMail, sendSupervisorMail } from './mailWrites';

// ga-nee27h: the supervisor answers a send/reply whose write could not be read
// back with 202. That must reach the UI as an error naming the message ID, not
// as a successful send.
function respondWith(status: number, id: string) {
  return vi.fn(
    async (_input: RequestInfo | URL) =>
      new Response(
        JSON.stringify({
          id,
          from: 'human',
          to: 'mayor',
          subject: 'status',
          body: 'all green',
          created_at: '0001-01-01T00:00:00Z',
          read: false,
        }),
        { status, headers: { 'content-type': 'application/json' } },
      ),
  );
}

function useApiWithFetch(fetchSpy: ReturnType<typeof respondWith>) {
  setSupervisorApiForTests(
    createSupervisorApi({ baseUrl: 'http://gc-supervisor.test', fetch: fetchSpy as typeof fetch }),
  );
}

describe('supervisor mail writes', () => {
  beforeEach(() => {
    setActiveCity('test-city');
  });
  afterEach(() => {
    resetSupervisorApiForTests();
  });

  it('rejects an unconfirmed (202) send with an error naming the message ID', async () => {
    const fetchSpy = respondWith(202, 'gc-unc-1');
    useApiWithFetch(fetchSpy);

    const sent = sendSupervisorMail({ to: 'mayor', subject: 'status', body: 'all green' }, 'human');
    await expect(sent).rejects.toBeInstanceOf(SupervisorMailUnconfirmedError);
    await expect(sent).rejects.toMatchObject({
      status: 202,
      code: 'mail_unconfirmed',
      messageId: 'gc-unc-1',
    });
    await expect(sent).rejects.toThrow(/gc-unc-1.*before resending/);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
  });

  it('rejects an unconfirmed (202) reply with an error naming the message ID', async () => {
    useApiWithFetch(respondWith(202, 'gc-unc-2'));

    const replied = replySupervisorMail({ id: 'gc-orig' }, { body: 'ack' }, 'human');
    await expect(replied).rejects.toMatchObject({
      status: 202,
      code: 'mail_unconfirmed',
      messageId: 'gc-unc-2',
    });
  });

  it('resolves a confirmed (201) send', async () => {
    useApiWithFetch(respondWith(201, 'gc-ok-1'));

    await expect(
      sendSupervisorMail({ to: 'mayor', subject: 'status', body: 'all green' }, 'human'),
    ).resolves.toBeUndefined();
  });
});
