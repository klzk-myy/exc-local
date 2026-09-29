import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import { SessionList } from './SessionList';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const SESSIONS = {
  data: [
    {
      id: 's-1',
      device: 'Firefox on macOS',
      ip: '203.0.113.5',
      geo_city: 'Zurich',
      geo_country: 'CH',
      created_at: '2026-01-01T08:00:00Z',
      last_active_at: '2026-01-02T09:00:00Z',
      expires_at: '2026-01-09T08:00:00Z',
      current: true,
    },
    {
      id: 's-2',
      device: 'Safari on iPhone',
      ip: '198.51.100.7',
      created_at: '2026-01-01T07:00:00Z',
      last_active_at: '2026-01-01T19:00:00Z',
      expires_at: '2026-01-08T07:00:00Z',
    },
  ],
};

beforeEach(() => {
  resetSessionForTests();
  signInForTests();
});

describe('SessionList', () => {
  it('renders sessions with a current marker and revokes another session after confirm', async () => {
    const calls = installFetchMock({
      'GET /api/v1/account/sessions': { body: SESSIONS },
      'DELETE /api/v1/account/sessions/s-2': { status: 200, body: {} },
    });
    renderApp(<SessionList />);
    expect(await screen.findByText('Firefox on macOS')).toBeInTheDocument();
    expect(screen.getByText('current')).toBeInTheDocument();
    expect(screen.getByText('Safari on iPhone')).toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Revoke' }));
    await user.click(await screen.findByRole('button', { name: 'Revoke session' }));
    await waitFor(() => {
      expect(
        calls.some((c) => c.method === 'DELETE' && c.url.includes('/account/sessions/s-2')),
      ).toBe(true);
    });
  });
});
