/**
 * ConsolesPanel — KYC queue renders pending submissions, reject submits
 * a reason; webhook DLQ tab lists dead letters and retransmits.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import AdminPage from './AdminPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

const BASE = {
  'GET /api/v1/system/status': { body: { status: 'operational', components: [] } },
  'GET /api/v1/admin/ops/health': { body: { mode: 'Normal' } },
  'GET /api/v1/admin/support/tickets': { body: { data: [], next_cursor: '', limit: 50, total: 0 } },
  'GET /api/v1/admin/dual-control': { body: { requests: [] } },
  'GET /api/v1/admin/audit-log': { body: { data: [], next_cursor: '', limit: 50, total: 0 } },
  'GET /api/v1/admin/instruments': { body: { instruments: [] } },
};

describe('ConsolesPanel — KYC queue', () => {
  it('renders pending submissions and approves', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/kyc/pending': {
        body: {
          submissions: [
            {
              id: 7,
              account_id: 1001,
              requested_tier: 'T1',
              status: 'PENDING_REVIEW',
              jurisdiction: 'GB',
              risk_score: 12,
              submitted_at: '2026-01-01T00:00:00Z',
              sla_due_at: '2026-01-02T00:00:00Z',
            },
          ],
        },
      },
      'POST /api/v1/admin/kyc/7/approve': { body: { status: 'APPROVED' } },
    });
    renderApp(<AdminPage />, '/admin');

    expect(await screen.findByText('1001')).toBeInTheDocument();
    expect(screen.getByText('PENDING REVIEW')).toBeInTheDocument();
    const approve = await screen.findByRole('button', { name: 'Approve' });
    await userEvent.setup().click(approve);
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'POST' && c.url.includes('/admin/kyc/7/approve'))).toBe(
        true,
      ),
    );
  });
});

describe('ConsolesPanel — webhook DLQ', () => {
  it('lists dead letters and retransmits', async () => {
    signInForTests({ roles: ['Super Admin'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/kyc/pending': { body: { submissions: [] } },
      'GET /api/v1/admin/webhooks/dead-letters': {
        body: {
          dead_letters: [
            {
              delivery_id: 'dl_9',
              event: 'order.filled',
              status: 'DEAD_LETTERED',
              attempts: 8,
              last_status_code: 500,
              last_error: 'endpoint 500',
            },
          ],
        },
      },
      'POST /api/v1/admin/webhooks/dead-letters/dl_9/retransmit': { body: { delivery: {} } },
    });
    renderApp(<AdminPage />, '/admin');

    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Webhook DLQ' }));
    expect(await screen.findByText('dl_9')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Retransmit' }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.method === 'POST' && c.url.includes('/admin/webhooks/dead-letters/dl_9/retransmit'),
        ),
      ).toBe(true),
    );
  });
});
