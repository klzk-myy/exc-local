import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import SupportPage from './SupportPage';
import TicketDetailPage from './TicketDetailPage';
import { urgentAllowed } from './api';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const TICKET = {
  ticket_id: 42,
  account_id: 1001,
  type: 'SUPPORT',
  category: 'FUNDING',
  priority: 'NORMAL',
  subject: 'Wire not credited',
  body: 'Sent yesterday.',
  status: 'OPEN',
  sla_due_at: new Date(Date.now() + 4 * 3600_000).toISOString(),
  created_at: '2026-01-01T09:00:00Z',
  updated_at: '2026-01-02T09:00:00Z',
};

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
  signInForTests();
});

function renderSupport() {
  return renderApp(
    <Routes>
      <Route path="/support" element={<SupportPage />} />
      <Route path="/support/tickets/:id" element={<TicketDetailPage />} />
      <Route path="/login" element={<div>login</div>} />
    </Routes>,
    '/support',
  );
}

describe('SupportPage', () => {
  it('lists tickets with SLA countdown and filters server-side', async () => {
    const calls = installFetchMock({
      'GET /api/v1/support/tickets*': { body: { data: [TICKET], next_cursor: '', total: 1 } },
    });
    renderSupport();
    expect(await screen.findByText('Wire not credited')).toBeInTheDocument();
    expect(screen.getByText(/ack due in/)).toBeInTheDocument();
    const user = userEvent.setup();
    await user.selectOptions(screen.getByLabelText(/^Status/), 'RESOLVED');
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('status=RESOLVED'))).toBe(true);
    });
  });

  it('shows the staff-queue link only for Support Agent', () => {
    installFetchMock({ 'GET /api/v1/support/tickets*': { body: { data: [] } } });
    renderSupport();
    expect(screen.queryByText(/Staff queue/)).not.toBeInTheDocument();
  });

  it('submits a ticket and navigates to the thread', async () => {
    const calls = installFetchMock({
      'GET /api/v1/support/tickets*': { body: { data: [] } },
      'POST /api/v1/support/tickets': { status: 201, body: TICKET },
      'GET /api/v1/support/tickets/42': { body: { ticket: TICKET, notes: [] } },
    });
    renderSupport();
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'New ticket' }));
    await user.type(screen.getByLabelText(/^Subject/), 'Wire not credited');
    await user.type(screen.getByLabelText(/Describe the issue/), 'Sent yesterday.');
    await user.click(screen.getByRole('button', { name: 'Submit ticket' }));
    await waitFor(() => {
      const c = calls.find((x) => x.method === 'POST' && x.url.includes('/support/tickets'));
      expect(JSON.parse(c?.init?.body as string)).toMatchObject({
        category: 'TECHNICAL',
        subject: 'Wire not credited',
        priority: 'NORMAL',
        origin_channel: 'web',
      });
    });
    expect(await screen.findByText('Your report')).toBeInTheDocument();
  });

  it('requires justification for URGENT on a sub-T2 account', async () => {
    installFetchMock({ 'GET /api/v1/support/tickets*': { body: { data: [] } } });
    renderSupport();
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'New ticket' }));
    await user.selectOptions(screen.getByLabelText(/^Priority/), 'URGENT');
    expect(await screen.findByLabelText(/Financial-impact justification/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Submit ticket' })).toBeDisabled();
  });

  it('shows MiFID complaint wording for the COMPLAINT category', async () => {
    installFetchMock({ 'GET /api/v1/support/tickets*': { body: { data: [] } } });
    renderSupport();
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'New ticket' }));
    // the form's Category select comes after the list filter's
    const catSelects = screen.getAllByLabelText(/^Category/);
    await user.selectOptions(catSelects[catSelects.length - 1]!, 'COMPLAINT');
    expect((await screen.findAllByText(/8 business hours/)).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/final response within/).length).toBeGreaterThan(0);
  });
});

describe('TicketDetailPage', () => {
  it('renders the thread and a reopen action on RESOLVED tickets', async () => {
    const resolved = { ...TICKET, status: 'RESOLVED', acknowledged_at: '2026-01-01T10:00:00Z' };
    const calls = installFetchMock({
      'GET /api/v1/support/tickets/42': {
        body: {
          ticket: resolved,
          notes: [
            {
              note_id: 1,
              ticket_id: 42,
              author_kind: 'ADMIN',
              body: 'Credited now.',
              created_at: '2026-01-02T08:00:00Z',
            },
            {
              note_id: 2,
              ticket_id: 42,
              author_kind: 'CLIENT',
              body: 'Thanks!',
              created_at: '2026-01-02T09:00:00Z',
            },
          ],
        },
      },
      'POST /api/v1/support/tickets/42/reopen': { body: {} },
    });
    renderApp(
      <Routes>
        <Route path="/support/tickets/:id" element={<TicketDetailPage />} />
      </Routes>,
      '/support/tickets/42',
    );
    expect(await screen.findByText('Credited now.')).toBeInTheDocument();
    expect(screen.getByText('acknowledged', { exact: false })).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Reopen this ticket' }));
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('/reopen'))).toBe(true);
    });
  });
});

describe('urgentAllowed', () => {
  it('T2+ always, lower tiers need justification', () => {
    expect(urgentAllowed('T2', '')).toBe(true);
    expect(urgentAllowed('INSTITUTIONAL', '')).toBe(true);
    expect(urgentAllowed('T1', '')).toBe(false);
    expect(urgentAllowed('T1', 'withdrawal stuck')).toBe(true);
    expect(urgentAllowed(null, '')).toBe(false);
  });
});
