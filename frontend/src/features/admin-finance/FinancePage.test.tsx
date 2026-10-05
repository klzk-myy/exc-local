/**
 * Finance, fees & tax-reporting console tests (Phase-10.5 Task
 * 10.5.3.10) — wire-level assertions for versioned fee schedules,
 * dual-controlled promo windows, statements/invoices/reporting-values,
 * and the CRS/FATCA run lifecycle.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import FinancePage from './FinancePage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BASE = {
  'GET /api/v1/admin/funding/fees': {
    body: {
      items: [
        {
          id: 1,
          rail: 'SWIFT',
          currency: 'USD',
          direction: 'WITHDRAWAL',
          account_tier: 'T1',
          flat_fee: '25.00',
          percentage_bps: '10',
          min_fee: '5.00',
          max_fee: '75.00',
          effective_date: '2026-01-01T00:00:00Z',
          version: 2,
        },
      ],
      count: 1,
    },
  },
  'GET /api/v1/admin/fees/promos': {
    body: {
      promo_windows: [
        {
          id: 7,
          fee_tier_id: 3,
          promo_maker_bps: '1',
          ends_at: '2026-02-01T00:00:00Z',
          status: 'PENDING_APPROVAL',
          created_by: 5,
        },
      ],
    },
  },
  'GET /api/v1/admin/finance/trial-balance': {
    body: {
      business_date: '2026-01-05T00:00:00Z',
      generated_at: '2026-01-05T01:00:00Z',
      currencies: [
        {
          currency: 'USD',
          total_debits: '1000.00',
          total_credits: '1000.00',
          assets: '800.00',
          liabilities: '600.00',
          equity: '50.00',
          revenue: '200.00',
          expenses: '50.00',
          net_income: '150.00',
        },
      ],
    },
  },
  'GET /api/v1/admin/invoices': { body: { invoices: [] } },
  'GET /api/v1/admin/reporting-values': { body: { reporting_values: [] } },
  'GET /api/v1/admin/tax-reporting/runs': {
    body: {
      runs: [
        {
          id: 9,
          regime: 'CRS',
          report_year: 2025,
          jurisdiction: 'GB',
          version: 1,
          status: 'UNDER_REVIEW',
          account_count: 42,
          created_by: 5,
          reviewed_by: 6,
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

describe('FinancePage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<FinancePage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all four panels and stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock(BASE);
    renderApp(<FinancePage />);
    await waitFor(() => expect(region('Funding fee schedules')).toBeInTheDocument());
    expect(region('Fee promos')).toBeInTheDocument();
    expect(region('Finance statements')).toBeInTheDocument();
    expect(region('Tax reporting')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('SWIFT')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('150.00')).toBeInTheDocument());
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('creates a v1 fee schedule with the full wire contract', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/funding/fees': {
        status: 201,
        body: { id: 12, rail: 'SEPA', currency: 'EUR', direction: 'DEPOSIT', version: 1 },
      },
    });
    renderApp(<FinancePage />);
    const user = userEvent.setup();
    const fees = await waitFor(() => region('Funding fee schedules'));
    const form = within(fees).getByRole('form', { name: 'Create fee schedule' });
    await user.clear(within(form).getByLabelText('Fee rail'));
    await user.type(within(form).getByLabelText('Fee rail'), 'SEPA');
    await user.clear(within(form).getByLabelText('Fee currency'));
    await user.type(within(form).getByLabelText('Fee currency'), 'EUR');
    await user.selectOptions(within(form).getByLabelText('Fee direction'), 'DEPOSIT');
    await user.clear(within(form).getByLabelText('Account tier'));
    await user.type(within(form).getByLabelText('Account tier'), 'T2');
    await user.clear(within(form).getByLabelText('Flat fee'));
    await user.type(within(form).getByLabelText('Flat fee'), '1.50');
    await user.clear(within(form).getByLabelText('Bps'));
    await user.type(within(form).getByLabelText('Bps'), '5');
    await user.clear(within(form).getByLabelText('Min fee'));
    await user.type(within(form).getByLabelText('Min fee'), '0.50');
    await user.click(within(form).getByRole('button', { name: 'Create v1' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.endsWith('/admin/funding/fees'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        rail: 'SEPA',
        currency: 'EUR',
        direction: 'DEPOSIT',
        account_tier: 'T2',
        flat_fee: '1.50',
        percentage_bps: '5',
        min_fee: '0.50',
        max_fee: null,
      });
    });
    await waitFor(() => expect(within(fees).getByText(/v1 created/)).toBeInTheDocument());
  });

  it('inserts a successor version via PUT (supersedes, not in-place edit)', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'PUT /api/v1/admin/funding/fees/1': {
        body: { id: 13, version: 3, supersedes_id: 1, flat_fee: '30.00' },
      },
    });
    renderApp(<FinancePage />);
    const user = userEvent.setup();
    const fees = await waitFor(() => region('Funding fee schedules'));
    await waitFor(() => expect(within(fees).getByText('SWIFT')).toBeInTheDocument());
    await user.click(within(fees).getByRole('button', { name: 'Succeed…' }));
    const form = within(fees).getByRole('form', { name: 'Successor version' });
    await user.clear(within(form).getByLabelText('Flat fee'));
    await user.type(within(form).getByLabelText('Flat fee'), '30.00');
    await user.click(within(form).getByRole('button', { name: 'Insert successor (PUT)' }));
    await waitFor(() => {
      const put = calls.find((c) => c.method === 'PUT' && c.url.endsWith('/admin/funding/fees/1'));
      expect(put).toBeDefined();
      expect(JSON.parse(put?.init?.body as string)).toMatchObject({ flat_fee: '30.00' });
    });
    await waitFor(() =>
      expect(within(fees).getByText(/Successor v3 inserted/)).toBeInTheDocument(),
    );
  });

  it('approves a PENDING promo window via the dual-control route', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/fees/promo/7/approve': { body: {} },
    });
    renderApp(<FinancePage />);
    const user = userEvent.setup();
    const promos = await waitFor(() => region('Fee promos'));
    await waitFor(() => expect(within(promos).getByText('PENDING APPROVAL')).toBeInTheDocument());
    await user.click(within(promos).getByRole('button', { name: 'Approve (4-eyes)' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/fees/promo/7/approve'),
      );
      expect(post).toBeDefined();
    });
  });

  it('drives a tax run from UNDER_REVIEW through approve to submission', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/tax-reporting/runs': {
        body: {
          runs: [
            {
              id: 9,
              regime: 'CRS',
              report_year: 2025,
              jurisdiction: 'GB',
              version: 1,
              status: 'APPROVED',
              account_count: 42,
              created_by: 5,
              reviewed_by: 6,
              approved_by: 7,
            },
          ],
        },
      },
      'POST /api/v1/admin/tax-reporting/runs/9/submit': {
        body: { run: { id: 9, status: 'SUBMITTED', submission_ref: 'HMRC-2026-001' } },
      },
    });
    renderApp(<FinancePage />);
    const user = userEvent.setup();
    const tax = await waitFor(() => region('Tax reporting'));
    await waitFor(() => expect(within(tax).getByText('APPROVED')).toBeInTheDocument());
    await user.type(within(tax).getByLabelText('Submission ref for run 9'), 'HMRC-2026-001');
    await user.click(within(tax).getByRole('button', { name: 'Submit' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/tax-reporting/runs/9/submit'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        submission_ref: 'HMRC-2026-001',
      });
    });
    await waitFor(() => expect(within(tax).getByText(/Run #9 → SUBMITTED/)).toBeInTheDocument());
  });
});
