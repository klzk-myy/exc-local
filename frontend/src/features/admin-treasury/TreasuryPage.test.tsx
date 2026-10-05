/**
 * Treasury console tests (Phase-10.5 Task 10.5.3.8) — wire-level
 * assertions for nostro coverage/replenishment, recon breaks,
 * client-money certifications and the collateral schedule PUT.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import TreasuryPage from './TreasuryPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BASE = {
  'GET /api/v1/admin/funding/nostro': {
    body: {
      currencies: [
        {
          currency: 'USD',
          nostro_total: '5000000.00',
          confirmed_due: '6000000.00',
          queued: 3,
          deficit: true,
          accounts: [],
        },
      ],
    },
  },
  'GET /api/v1/admin/nostro-accounts': {
    body: {
      accounts: [
        {
          id: 1,
          currency: 'USD',
          bank_name: 'Corr Bank',
          iban: 'US111',
          role: 'NOSTRO',
          balance: '5000000.00',
          status: 'ACTIVE',
        },
      ],
    },
  },
  'GET /api/v1/admin/funding/nostro/replenishments': {
    body: {
      items: [
        {
          id: 9,
          currency: 'USD',
          amount: '1000000.00',
          source_nostro_id: 1,
          target_nostro_id: 2,
          status: 'PENDING_APPROVAL',
          requested_by: 5,
          created_at: '2026-01-02T00:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/swift-messages': { body: { items: [] } },
  'GET /api/v1/admin/nostro-reconciliation': {
    body: {
      date: '2026-01-02T00:00:00Z',
      runs: [],
      breaks: [
        {
          id: 3,
          run_id: 1,
          nostro_account_id: 1,
          category: 'MISSING_MOVEMENT',
          status: 'OPEN',
          detected_at: '2026-01-02T01:00:00Z',
        },
      ],
      accounts_reconciled: 1,
      open_breaks: 1,
      threshold_breaches: 0,
    },
  },
  'GET /api/v1/admin/pb-reconciliation': {
    body: {
      prime_broker_id: 0,
      date: '2026-01-02',
      runs: [
        {
          ID: 1,
          Source: 'AFFIRMATION',
          TradesScanned: 120,
          AutoMatched: 118,
          BreaksDetected: 2,
        },
      ],
      open_breaks: [],
      auto_match_rate_pct: '98.3',
    },
  },
  'GET /api/v1/admin/client-money/audits': {
    body: {
      audits: [
        {
          id: 4,
          engagement_year: 2026,
          auditor_firm: 'Assurance LLP',
          scope: 'SAFEGUARDING',
          period_start: '2026-01-01T00:00:00Z',
          period_end: '2026-12-31T00:00:00Z',
          status: 'FIELDWORK',
          independence_confirmed: true,
        },
      ],
    },
  },
  'GET /api/v1/admin/client-money/certifications': { body: { certifications: [] } },
  'GET /api/v1/admin/treasury/own-funds': {
    body: {
      own_funds: [
        {
          id: 1,
          line_kind: 'TIER1_CAPITAL',
          currency: 'USD',
          balance: '25000000.00',
          reconciliation_status: 'RECONCILED',
        },
      ],
      treasury_controls: {
        discretionary_outflows_frozen: false,
        lp_capacity_blocked: false,
      },
    },
  },
  'GET /api/v1/admin/treasury/contingent-capital': { body: { commitments: [] } },
  'GET /api/v1/admin/insurance-fund': {
    body: {
      balances: [{ currency: 'USD', balance: '1200000.00', depletion_threshold: '200000.00' }],
      transactions: { data: [], next_cursor: '', limit: 100 },
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

describe('TreasuryPage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<TreasuryPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all four panels with coverage and deficit surfaced', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock(BASE);
    renderApp(<TreasuryPage />);
    const nostro = await waitFor(() => region('Nostro operations'));
    await waitFor(() => expect(within(nostro).getByText('DEFICIT')).toBeInTheDocument());
    expect(region('Reconciliation')).toBeInTheDocument();
    expect(region('Client money')).toBeInTheDocument();
    expect(region('Treasury')).toBeInTheDocument();
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('requests a replenishment then decides it under four-eyes', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/funding/nostro/replenishments': {
        status: 201,
        body: {
          id: 10,
          currency: 'USD',
          amount: '500.00',
          source_nostro_id: 1,
          target_nostro_id: 2,
          status: 'PENDING_APPROVAL',
          created_at: '2026-01-02T00:00:00Z',
        },
      },
      'POST /api/v1/admin/funding/nostro/replenishments/9/decide': {
        body: {
          id: 9,
          currency: 'USD',
          amount: '1000000.00',
          source_nostro_id: 1,
          target_nostro_id: 2,
          status: 'EXECUTED',
          created_at: '2026-01-02T00:00:00Z',
        },
      },
    });
    renderApp(<TreasuryPage />);
    const user = userEvent.setup();
    const nostro = await waitFor(() => region('Nostro operations'));

    await waitFor(() => expect(within(nostro).getByText('Corr Bank')).toBeInTheDocument());
    const reqForm = within(nostro).getByRole('form', { name: 'Request replenishment' });
    await user.clear(within(reqForm).getByLabelText('Amount'));
    await user.type(within(reqForm).getByLabelText('Amount'), '500');
    await user.type(within(reqForm).getByLabelText('Source nostro id'), '1');
    await user.type(within(reqForm).getByLabelText('Target nostro id'), '2');
    await user.click(within(reqForm).getByRole('button', { name: /Request/ }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/funding/nostro/replenishments'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        currency: 'USD',
        amount: '500',
        source_nostro_id: 1,
        target_nostro_id: 2,
      });
    });
    await waitFor(() =>
      expect(
        within(nostro).getByText(/Replenishment #10 opened — PENDING_APPROVAL/),
      ).toBeInTheDocument(),
    );

    await user.click(within(nostro).getByRole('button', { name: 'Decide…' }));
    await user.click(within(nostro).getByRole('button', { name: 'Approve & execute' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/9/decide'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({ action: 'APPROVE' });
    });
  });

  it('renders the nostro recon report and resolves a break', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/nostro-reconciliation/breaks/3/resolve': { body: { break: { id: 3 } } },
    });
    renderApp(<TreasuryPage />);
    const user = userEvent.setup();
    const recon = await waitFor(() => region('Reconciliation'));
    await waitFor(() => expect(within(recon).getByText('MISSING_MOVEMENT')).toBeInTheDocument());
    expect(within(recon).getByText(/1 accounts reconciled/)).toBeInTheDocument();
    // PB recon (PascalCase wire) renders alongside.
    await waitFor(() =>
      expect(within(recon).getByText(/auto-match rate 98\.3/)).toBeInTheDocument(),
    );

    await user.click(within(recon).getByRole('button', { name: 'Investigate' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/breaks/3/resolve'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        action: 'INVESTIGATE',
      });
    });
  });

  it('issues a segregation certification with a distinct approver', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/client-money/certifications': {
        status: 201,
        body: { certification: { id: 1 } },
      },
    });
    renderApp(<TreasuryPage />);
    const user = userEvent.setup();
    const cm = await waitFor(() => region('Client money'));
    await waitFor(() => expect(within(cm).getByText('Assurance LLP')).toBeInTheDocument());
    const form = within(cm).getByRole('form', { name: 'Issue certification' });
    await user.type(within(form).getByLabelText('Audit id'), '4');
    await user.type(within(form).getByLabelText('Evidence pack id'), '7');
    await user.type(within(form).getByLabelText('Statement'), 'Segregated per CASS');
    await user.type(within(form).getByLabelText('Signatory name'), 'CFO');
    await user.type(within(form).getByLabelText('Published until'), '2026-12-31');
    await user.type(within(form).getByLabelText('Approver id'), '88');
    await user.click(within(form).getByRole('button', { name: 'Issue (4-eyes)' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/client-money/certifications'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        audit_id: 4,
        evidence_pack_id: 7,
        approver_id: 88,
        published_until: '2026-12-31',
      });
    });
  });

  it('puts the collateral schedule row with X-Admin-Env', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE,
      'PUT /api/v1/admin/collateral-schedule': { body: { rows: [] } },
    });
    renderApp(<TreasuryPage />);
    const user = userEvent.setup();
    const t = await waitFor(() => region('Treasury'));
    const form = within(t).getByRole('form', { name: 'Update collateral row' });
    await user.clear(within(form).getByLabelText('Haircut %'));
    await user.type(within(form).getByLabelText('Haircut %'), '2.5');
    await user.click(within(form).getByRole('button', { name: 'Upsert row (PUT)' }));
    await waitFor(() => {
      const put = calls.find((c) => c.method === 'PUT' && c.url.includes('collateral-schedule'));
      expect(put).toBeDefined();
      expect(new Headers(put?.init?.headers).get('X-Admin-Env')).toBe('dev');
      expect(JSON.parse(put?.init?.body as string)).toMatchObject({
        rows: [{ currency: 'USD', eligible: true, haircut_pct: '2.5' }],
      });
    });
  });
});
