import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../../api/cityBase';
import { ViewingAsProvider } from '../../contexts/ViewingAsContext';
import {
  createSupervisorApi,
  resetSupervisorApiForTests,
  setSupervisorApiForTests,
} from '../../supervisor/client';
import { ComposeModal } from './ComposeModal';

// ga-nee27h: a send the supervisor answers 202 (delivery unconfirmed) must not
// close the modal or clear the draft, and must tell the operator which message
// ID to check before resending.
describe('ComposeModal unconfirmed send', () => {
  beforeEach(() => {
    setActiveCity('test-city');
    const fetchSpy = vi.fn(
      async (_input: RequestInfo | URL) =>
        new Response(
          JSON.stringify({
            id: 'gc-unc-ui',
            from: 'human',
            to: 'mayor',
            subject: 'status',
            body: 'all green',
            created_at: '0001-01-01T00:00:00Z',
            read: false,
          }),
          { status: 202, headers: { 'content-type': 'application/json' } },
        ),
    );
    setSupervisorApiForTests(
      createSupervisorApi({
        baseUrl: 'http://gc-supervisor.test',
        fetch: fetchSpy as typeof fetch,
      }),
    );
  });

  afterEach(() => {
    cleanup();
    resetSupervisorApiForTests();
  });

  it('keeps the draft, does not report sent, and names the message ID', async () => {
    const onSent = vi.fn();
    render(
      <ViewingAsProvider>
        <ComposeModal open onClose={vi.fn()} onSent={onSent} />
      </ViewingAsProvider>,
    );

    fireEvent.change(screen.getByPlaceholderText(/mayor, mechanic/), {
      target: { value: 'mayor' },
    });
    const [subjectInput] = screen
      .getAllByRole('textbox')
      .filter((el) => (el as HTMLInputElement).maxLength === 200);
    fireEvent.change(subjectInput as HTMLElement, { target: { value: 'status' } });
    const bodyInput = screen
      .getAllByRole('textbox')
      .find((el) => el.tagName === 'TEXTAREA') as HTMLTextAreaElement;
    fireEvent.change(bodyInput, { target: { value: 'all green' } });

    fireEvent.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => {
      expect(screen.getByText(/delivery unconfirmed: message gc-unc-ui/)).toBeTruthy();
    });
    expect(onSent).not.toHaveBeenCalled();
    expect((subjectInput as HTMLInputElement).value).toBe('status');
    expect(bodyInput.value).toBe('all green');
  });
});
