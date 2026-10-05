/**
 * Settlement console tests (Phase-10.5 Task 10.5.3.9) — wire-level
 * assertions for the CLS lifecycle, dual-control exception resolution,
 * allocation groups and the chargeback register.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import SettlementPage from './SettlementPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const CLS_PASCAL = {
  ID: 7,
  InstructionRef: 'CLS-77',
  CounterpartyAccountID: 42,
  MemberBIC: 'CLSBGSSS',
  Product: 'SPOT',
  BuyCurrency: 'EUR',
  BuyAmount: '1000.00',
  SellCurrency: 'USD',
  SellAmount: '1100.00',
  ValueDate: '2026-01-05T00:00:00Z',
  Status: 'VALIDATED',
};

const BASE = {
  'GET /api/v1/admin/chargebacks': {
    body: {
      data: [
        {
          id: 3,
          account_id: 1001,
          currency: 'USD',
          amount: '250.00',
          reason: 'fraud',
          status: 'OPEN',
          card_network: 'VISA',
          opened_at: '2026-01-02T00:00:00Z',
        },
      ],
      total: 1,
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

describe('SettlementPage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<SettlementPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all four panels and stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock(BASE);
    renderApp(<SettlementPage />);
    await waitFor(() => expect(region('CLS PvP')).toBeInTheDocument());
    expect(region('Settlement exceptions')).toBeInTheDocument();
    expect(region('Allocations')).toBeInTheDocument();
    const cb = region('Chargebacks');
    await waitFor(() => expect(within(cb).getByText('VISA')).toBeInTheDocument());
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('submits a paired CLS instruction then dispatches it', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/settlement/cls/instructions': {
        status: 201,
        body: CLS_PASCAL,
      },
      'POST /api/v1/admin/settlement/cls/instructions/CLS-77/dispatch': {
        body: { ...CLS_PASCAL, Status: 'MATCHED' },
      },
    });
    renderApp(<SettlementPage />);
    const user = userEvent.setup();
    const cls = await waitFor(() => region('CLS PvP'));
    const form = within(cls).getByRole('form', { name: 'Submit CLS instruction' });
    await user.type(within(form).getByLabelText('Counterparty account'), '42');
    await user.type(within(form).getByLabelText('Member BIC'), 'CLSBGSSS');
    await user.type(within(form).getByLabelText('Buy amount'), '1000');
    await user.type(within(form).getByLabelText('Sell amount'), '1100');
    await user.type(within(form).getByLabelText('Value date'), '2026-01-05');
    await user.click(within(form).getByRole('button', { name: 'Submit paired' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/settlement/cls/instructions'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        counterparty_account_id: 42,
        member_bic: 'CLSBGSSS',
        product: 'SPOT',
      });
    });
    // PascalCase wire renders — lifecycle strip highlights VALIDATED.
    await waitFor(() => expect(within(cls).getByText('CLS-77')).toBeInTheDocument());
    await user.click(within(cls).getByRole('button', { name: 'Dispatch' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/CLS-77/dispatch'));
      expect(post).toBeDefined();
    });
  });

  it('queues an exception resolution for four-eyes (202 pending, not executed)', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/settlement-exceptions/5': {
        body: {
          exception: {
            ID: 5,
            Currency: 'USD',
            Amount: '4200.00',
            Type: 'MISSING_SETTLEMENT',
            Status: 'OPEN',
            DetectedBy: 'recon-engine',
            Detail: 'leg never confirmed',
          },
        },
      },
      'POST /api/v1/admin/settlement-exceptions/5/resolve': {
        status: 202,
        body: {
          exception_id: 5,
          action: 'REVERSE',
          status: 'PENDING',
          dual_control_request: { id: 91 },
        },
      },
    });
    renderApp(<SettlementPage />);
    const user = userEvent.setup();
    const ex = await waitFor(() => region('Settlement exceptions'));
    await user.type(within(ex).getByLabelText('Exception id'), '5');
    await user.click(within(ex).getByRole('button', { name: 'Load' }));
    await waitFor(() => expect(within(ex).getByText('MISSING SETTLEMENT')).toBeInTheDocument());
    const rForm = within(ex).getByRole('form', { name: 'Resolve exception' });
    await user.selectOptions(within(rForm).getByLabelText('Resolution action'), 'REVERSE');
    await user.click(within(rForm).getByRole('button', { name: 'Submit for approval' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/settlement-exceptions/5/resolve'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({ action: 'REVERSE' });
    });
    await waitFor(() =>
      expect(
        within(ex).getByText(/queued for four-eyes approval \(request #91\)/),
      ).toBeInTheDocument(),
    );
  });

  it('registers an allocation group and submits it to settlement', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/allocations/groups': {
        status: 201,
        body: {
          id: 11,
          group_ref: 'GRP-9',
          manager_account_id: 500,
          instrument_id: 3,
          side: 'BUY',
          capacity: 'AGENCY',
          allocation_method: 'MANUAL',
          status: 'OPEN',
          total_qty: '0',
          allocated_qty: '0',
          avg_price: '0',
          settlement_locked: false,
        },
      },
      'POST /api/v1/admin/allocations/groups/11/submit': {
        body: {
          group: {
            id: 11,
            group_ref: 'GRP-9',
            status: 'SUBMITTED',
            settlement_locked: true,
            total_qty: '0',
            allocated_qty: '0',
            avg_price: '0',
          },
        },
      },
    });
    renderApp(<SettlementPage />);
    const user = userEvent.setup();
    const al = await waitFor(() => region('Allocations'));
    const form = within(al).getByRole('form', { name: 'Register group' });
    await user.type(within(form).getByLabelText('Group ref'), 'GRP-9');
    await user.type(within(form).getByLabelText('Manager account'), '500');
    await user.type(within(form).getByLabelText('Instrument id'), '3');
    await user.type(within(form).getByLabelText('Eligible accounts'), '501,502');
    await user.click(within(form).getByRole('button', { name: 'Register group' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.endsWith('/allocations/groups'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        group_ref: 'GRP-9',
        eligible_accounts: [{ account_id: 501 }, { account_id: 502 }],
      });
    });
    await waitFor(() => expect(within(al).getByText(/GRP-9/)).toBeInTheDocument());
    await user.click(within(al).getByRole('button', { name: 'Submit (lock)' }));
    await waitFor(() => expect(within(al).getByText('SETTLEMENT LOCKED')).toBeInTheDocument());
  });

  it('opens a chargeback under four-eyes then resolves it', async () => {
    signInForTests({ roles: ['Finance Ops'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/chargebacks': { status: 201, body: { id: 9 } },
      'POST /api/v1/admin/chargebacks/3/resolve': { body: { chargeback: { id: 3 } } },
    });
    renderApp(<SettlementPage />);
    const user = userEvent.setup();
    const cb = await waitFor(() => region('Chargebacks'));
    const form = within(cb).getByRole('form', { name: 'Open chargeback' });
    await user.type(within(form).getByLabelText('Chargeback account'), '1001');
    await user.type(within(form).getByLabelText('Chargeback amount'), '99.50');
    await user.type(within(form).getByLabelText('Chargeback reason'), 'unauthorized');
    await user.type(within(form).getByLabelText('Approver user id'), '88');
    await user.click(within(form).getByRole('button', { name: 'Open case' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.endsWith('/chargebacks'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        account_id: 1001,
        approver_user_id: 88,
        freeze_account: false,
      });
    });
    const rForm = within(cb).getByRole('form', { name: 'Resolve chargeback' });
    await user.type(within(rForm).getByLabelText('Resolve chargeback id'), '3');
    await user.type(within(rForm).getByLabelText('Resolution note'), 'evidence reviewed');
    await user.click(within(rForm).getByRole('button', { name: 'Resolve' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/chargebacks/3/resolve'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        outcome: 'WON',
        note: 'evidence reviewed',
      });
    });
  });
});
