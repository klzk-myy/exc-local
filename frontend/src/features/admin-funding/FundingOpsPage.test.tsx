/**
 * Funding-ops console tests (Phase-10.5 Task 10.5.3.7) — wire-level
 * assertions for deposit ingest/review, withdrawal four-eyes,
 * quarantine resolve, bank-account verify and the ops-alerts rail.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import FundingOpsPage from './FundingOpsPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const SUSPENSE = {
  id: 21,
  bank_tx_id: 'TX-88',
  account_id: 1001,
  rail: 'SWIFT',
  currency: 'USD',
  amount: '15000.00',
  originator_name: 'Mystery Sender',
  name_match_score: 0.41,
  unmatched_reason: 'NAME_MISMATCH',
  gl_account: '2150_SUSPENSE_DEPOSITS_USD',
  quarantine_status: 'OPEN',
  quarantined_at: '2026-01-02T03:00:00Z',
  sla_expires_at: '2026-01-04T03:00:00Z',
};

const BENEFICIARY = {
  bank_account_id: 5,
  account_id: 1001,
  currency: 'USD',
  iban: 'US123',
  bank_name: 'First Bank',
  beneficiary_name: 'Alice',
  rail: 'WIRE',
  status: 'PENDING_VERIFICATION',
  created_at: '2026-01-01T00:00:00Z',
};

const BASE = {
  'GET /api/v1/admin/funding/ops-alerts': {
    body: {
      items: [
        {
          id: 1,
          code: 'DEPOSIT_REVIEW_BREACH',
          severity: 'P1',
          funding_transaction_id: 44,
          account_id: 1001,
          summary: 'review deadline breached',
          status: 'OPEN',
          created_at: '2026-01-02T04:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/funding/quarantine': { body: { items: [SUSPENSE], total: 1 } },
  'GET /api/v1/admin/funding/bank-accounts': { body: { bank_accounts: [BENEFICIARY] } },
};

const region = (name: string) => screen.getByRole('region', { name });

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

describe('FundingOpsPage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<FundingOpsPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all five panels and stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock(BASE);
    renderApp(<FundingOpsPage />);
    await waitFor(() => expect(region('Funding alerts')).toBeInTheDocument());
    expect(region('Deposit operations')).toBeInTheDocument();
    expect(region('Withdrawal approvals')).toBeInTheDocument();
    expect(region('Quarantine')).toBeInTheDocument();
    expect(region('Bank accounts')).toBeInTheDocument();
    await waitFor(() =>
      expect(
        within(region('Funding alerts')).getByText('DEPOSIT_REVIEW_BREACH'),
      ).toBeInTheDocument(),
    );
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('ingests a detected deposit and renders the review tier', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/funding/deposits': {
        status: 201,
        body: {
          deposit_id: 77,
          status: 'PENDING_REVIEW',
          currency: 'USD',
          amount: '60000.00',
          usd_amount: '60000.00',
          review_tier: 'PENDING_REVIEW',
          confirmations: 1,
          flags: ['VELOCITY'],
        },
      },
    });
    renderApp(<FundingOpsPage />);
    const user = userEvent.setup();
    const dep = await waitFor(() => region('Deposit operations'));
    const form = within(dep).getByRole('form', { name: 'Ingest detected deposit' });
    await user.type(within(form).getByLabelText('Account id'), '1001');
    await user.type(within(form).getByLabelText('Amount'), '60000');
    await user.type(within(form).getByLabelText('Reference'), 'WIRE-1');
    await user.click(within(form).getByRole('button', { name: 'Ingest' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.endsWith('/funding/deposits'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        account_id: 1001,
        amount: '60000',
        reference: 'WIRE-1',
      });
    });
    await waitFor(() => expect(within(dep).getByText('#77')).toBeInTheDocument());
    expect(within(dep).getAllByText('PENDING REVIEW').length).toBeGreaterThanOrEqual(1);
    expect(within(dep).getByText(/VELOCITY/)).toBeInTheDocument();
  });

  it('submits the four-eyes deposit review body', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/funding/deposits/77/review': {
        body: {
          deposit_id: 77,
          status: 'CONFIRMED',
          currency: 'USD',
          amount: '60000.00',
          confirmations: 2,
        },
      },
    });
    renderApp(<FundingOpsPage />);
    const user = userEvent.setup();
    const dep = await waitFor(() => region('Deposit operations'));
    const form = within(dep).getByRole('form', { name: 'Review deposit' });
    await user.type(within(form).getByLabelText('Review deposit id'), '77');
    await user.type(within(form).getByLabelText('Approver id'), '88');
    await user.click(within(form).getByRole('button', { name: 'Submit review' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/deposits/77/review'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        action: 'APPROVE',
        approver_id: 88,
      });
    });
  });

  it('approves a withdrawal under four-eyes', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/withdrawals/33/approve': {
        body: {
          withdrawal_id: 33,
          status: 'APPROVED',
          currency: 'USD',
          amount: '75000.00',
          review_tier: 'PENDING_REVIEW',
        },
      },
    });
    renderApp(<FundingOpsPage />);
    const user = userEvent.setup();
    const wd = await waitFor(() => region('Withdrawal approvals'));
    await user.type(within(wd).getByLabelText('Withdrawal id'), '33');
    await user.type(within(wd).getByLabelText(/Approver id/), '88');
    await user.click(within(wd).getByRole('button', { name: 'Approve' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/withdrawals/33/approve'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({ approver_id: 88 });
    });
    await waitFor(() => expect(within(wd).getByText('#33')).toBeInTheDocument());
  });

  it('resolves a quarantine row and verifies a beneficiary', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/funding/quarantine/21/resolve': { body: { status: 'RESOLVED' } },
      'POST /api/v1/admin/funding/bank-accounts/5/verify': {
        body: { ...BENEFICIARY, status: 'VERIFIED' },
      },
    });
    renderApp(<FundingOpsPage />);
    const user = userEvent.setup();

    const q = await waitFor(() => region('Quarantine'));
    await waitFor(() => expect(within(q).getByText('TX-88')).toBeInTheDocument());
    expect(within(q).getByText('NAME_MISMATCH')).toBeInTheDocument();
    await user.click(within(q).getByRole('button', { name: 'Resolve…' }));
    const qForm = within(q).getByRole('form', { name: 'Resolve quarantine' });
    await user.type(within(qForm).getByLabelText('Approver id'), '88');
    await user.click(within(qForm).getByRole('button', { name: 'Apply resolution' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/quarantine/21/resolve'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        action: 'RELEASE_TO_CLIENT',
        approver_id: 88,
      });
    });

    const ba = region('Bank accounts');
    await waitFor(() => expect(within(ba).getByText('Alice')).toBeInTheDocument());
    await user.click(within(ba).getByRole('button', { name: 'Verify…' }));
    const bForm = within(ba).getByRole('form', { name: 'verify beneficiary' });
    await user.type(within(bForm).getByLabelText('Approver id'), '88');
    await user.click(within(bForm).getByRole('button', { name: 'Verify (4-eyes)' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/bank-accounts/5/verify'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        approver_id: 88,
        method: 'BANK_STATEMENT',
      });
    });
  });

  it('surfaces the quarantine disposition for an inbound wire', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/funding/inbound-wires': {
        status: 202,
        body: {
          disposition: 'QUARANTINED',
          reason: 'NAME_MISMATCH',
          suspense: { id: 99 },
          name_match_score: 0.31,
        },
      },
    });
    renderApp(<FundingOpsPage />);
    const user = userEvent.setup();
    const dep = await waitFor(() => region('Deposit operations'));
    const form = within(dep).getByRole('form', { name: 'Register inbound wire' });
    await user.type(within(form).getByLabelText('Bank tx id'), 'TX-99');
    await user.type(within(form).getByLabelText('Wire amount'), '500');
    await user.click(within(form).getByRole('button', { name: 'Register wire' }));
    await waitFor(() => expect(within(dep).getByText(/suspense #99/)).toBeInTheDocument());
    const post = calls.find((c) => c.method === 'POST' && c.url.includes('/inbound-wires'));
    expect(JSON.parse(post?.init?.body as string)).toMatchObject({ bank_tx_id: 'TX-99' });
  });
});
