/**
 * Instrument governance console tests (Phase-10.5 Task 10.5.3.11) —
 * wire-level assertions for listing reviews (APPROVE = 202 dual),
 * auction-calendar full-replace, schedule-override CRUD, and the
 * §13.12/§13.14 dual-controlled policy cells.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import InstrumentGovernancePage from './InstrumentGovernancePage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BASE = {
  'GET /api/v1/admin/listing-proposals': {
    body: {
      proposals: [
        {
          id: 4,
          proposer_id: 7,
          symbol: 'EURJPY',
          status: 'PENDING',
          reason: 'new cross demand',
          auto_checks: {},
          created_at: '2026-01-05T00:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/market-schedule': {
    body: {
      open_utc: '21:00',
      close_utc: '22:00',
      pre_open_utc: '20:30',
      version: 3,
      published_at: '2026-01-04T00:00:00Z',
      overrides: [{ id: 2, date: '2026-12-25', closed: true, reason: 'Christmas' }],
    },
  },
  'GET /api/v1/admin/entity-leverage-policy': {
    body: {
      policies: [
        {
          entity_code: 'EU',
          client_category: 'RETAIL',
          instrument_group: 'MAJOR',
          max_leverage: 30,
          effective_from: '2026-01-01T00:00:00Z',
        },
      ],
    },
  },
};

const region = (name: string) => screen.getByRole('region', { name });

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

describe('InstrumentGovernancePage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<InstrumentGovernancePage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all three panels and stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock(BASE);
    renderApp(<InstrumentGovernancePage />);
    await waitFor(() => expect(region('Listing proposals')).toBeInTheDocument());
    expect(region('Market schedule')).toBeInTheDocument();
    expect(region('Margin and leverage governance')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('EURJPY')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('Christmas')).toBeInTheDocument());
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('files an APPROVE review as a four-eyes request (202 pending, not listed)', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/listing-proposals/4/review': {
        status: 202,
        body: {
          status: 'PENDING',
          dual_control_id: 41,
          operation: 'instrument_listing',
          required_approver: 'Risk Manager',
          expires_at: 1736000400,
        },
      },
    });
    renderApp(<InstrumentGovernancePage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Listing proposals'));
    const form = within(panel).getByRole('form', { name: 'Review proposal' });
    await user.type(within(form).getByLabelText('Proposal id'), '4');
    await user.type(within(form).getByLabelText('Review note'), 'ref data checked');
    await user.click(within(form).getByRole('button', { name: 'Submit review' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/listing-proposals/4/review'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        action: 'APPROVE',
        note: 'ref data checked',
      });
    });
    await waitFor(() =>
      expect(within(panel).getByText(/queued for four-eyes — request #41/)).toBeInTheDocument(),
    );
  });

  it('creates then edits a schedule override (POST then PUT)', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/market-schedule/overrides': { status: 201, body: {} },
      'PUT /api/v1/admin/market-schedule/overrides/2': { body: {} },
    });
    renderApp(<InstrumentGovernancePage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Market schedule'));
    await waitFor(() => expect(within(panel).getByText('Christmas')).toBeInTheDocument());
    const form = within(panel).getByRole('form', { name: 'Create schedule override' });
    await user.type(within(form).getByLabelText('Override date'), '2026-01-01');
    await user.type(within(form).getByLabelText('Override reason'), 'New Year');
    await user.click(within(form).getByRole('button', { name: 'Add override' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/market-schedule/overrides'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        date: '2026-01-01',
        closed: true,
        reason: 'New Year',
      });
    });
    // Edit path: load row → PUT to the same id.
    await user.click(within(panel).getByRole('button', { name: 'Edit' }));
    await user.click(within(form).getByRole('button', { name: 'Update #2' }));
    await waitFor(() => {
      const put = calls.find(
        (c) => c.method === 'PUT' && c.url.endsWith('/market-schedule/overrides/2'),
      );
      expect(put).toBeDefined();
      expect(JSON.parse(put?.init?.body as string)).toMatchObject({ reason: 'Christmas' });
    });
  });

  it('files a margin param change and renders the approval-time gate honestly', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/margin-param-changes': {
        status: 202,
        body: {
          status: 'PENDING',
          dual_control_id: 88,
          operation: 'margin_param_change',
          required_approver: 'Risk Manager',
        },
      },
    });
    renderApp(<InstrumentGovernancePage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Margin and leverage governance'));
    await waitFor(() => expect(within(panel).getByText('30x')).toBeInTheDocument());
    const form = within(panel).getByRole('form', { name: 'Margin param change' });
    await user.type(within(form).getByLabelText('Parameter name'), 'auction_floor_pct');
    await user.type(within(form).getByLabelText('Proposed value'), '"0.97"');
    await user.type(within(form).getByLabelText('Validation run id'), '17');
    await user.type(within(form).getByLabelText('Change reason'), 'backtested');
    await user.click(within(form).getByRole('button', { name: 'File change (4-eyes)' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/margin-param-changes'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        parameter: 'auction_floor_pct',
        proposed_value: '0.97',
        run_id: 17,
      });
    });
    await waitFor(() =>
      expect(
        within(panel).getByText(/Param change queued — request #88.*MARGIN_MODEL_UNVALIDATED/),
      ).toBeInTheDocument(),
    );
  });

  it('replaces the auction calendar under dual control (full-replace PUT)', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/instruments/EURUSD/auction-calendar': {
        body: {
          symbol: 'EURUSD',
          entries: [
            {
              id: 1,
              auction_type: 'OPEN',
              trigger_time: '21:00',
              timezone: 'UTC',
              recurrence: 'MON-FRI',
              enabled: true,
            },
          ],
        },
      },
      'PUT /api/v1/admin/instruments/EURUSD/auction-calendar': {
        status: 202,
        body: {
          status: 'PENDING',
          dual_control_id: 52,
          operation: 'instrument_calendar',
          required_approver: 'Risk Manager',
        },
      },
    });
    renderApp(<InstrumentGovernancePage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Market schedule'));
    await user.click(within(panel).getByRole('button', { name: 'Load entries' }));
    await waitFor(() =>
      expect(within(panel).getByLabelText('Calendar entries JSON')).toBeInTheDocument(),
    );
    await user.type(within(panel).getByLabelText('Calendar replace reason'), 'add fixing call');
    await user.click(within(panel).getByRole('button', { name: 'Replace calendar (4-eyes)' }));
    await waitFor(() => {
      const put = calls.find(
        (c) => c.method === 'PUT' && c.url.includes('/EURUSD/auction-calendar'),
      );
      expect(put).toBeDefined();
      expect(JSON.parse(put?.init?.body as string)).toMatchObject({
        reason: 'add fixing call',
        entries: [{ auction_type: 'OPEN', trigger_time: '21:00' }],
      });
    });
    await waitFor(() => expect(within(panel).getByText(/request #52/)).toBeInTheDocument());
  });
});
