/**
 * Compliance ops console tests (Phase-10.5 Task 10.5.3.5) — wire-level
 * assertions for screening, travel-rule cure, restricted lists,
 * pre-clearance decisions and the enforcement ladder, plus the
 * role-gate and X-Admin-Env invariants.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import CompliancePage from './CompliancePage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const TRAVEL_REC = {
  id: 7,
  transfer_id: 9001,
  direction: 'OUT',
  account_id: 1001,
  rail: 'SWIFT',
  currency: 'USD',
  amount: '25000.00',
  originator: { name: 'Alice', account_number: 'A-1' },
  beneficiary: {},
  status: 'MISSING_INFO',
  missing_fields: ['beneficiary.name', 'beneficiary.account_number'],
  hold_ref: 'H-77',
  created_at: '2026-01-02T03:00:00Z',
  updated_at: '2026-01-02T03:00:00Z',
};

const RESTRICTED = {
  id: 3,
  event_id: 'EVT-EARN-1',
  event_type: 'EARNINGS',
  instruments: ['EURUSD'],
  window_start: '2026-01-05T00:00:00Z',
  window_end: '2026-01-06T00:00:00Z',
  widen_minutes: 0,
  scope: 'ALL_STAFF',
  named_accounts: [],
  status: 'ACTIVE',
  reason: 'insider window',
  created_by: 42,
};

const CLEARANCE = {
  id: 11,
  request_id: 'PC-11',
  account_id: 2002,
  instrument: 'GBPUSD',
  side: 'BUY',
  reason: 'desk request',
  outcome: 'PENDING',
  expires_at: '2026-02-01T00:00:00Z',
  requested_by: 2002,
  created_at: '2026-01-02T03:00:00Z',
};

const BASE = {
  'GET /api/v1/admin/sanctions/status': {
    body: { provider_gate: { state: 'OPEN' }, pending_screens: 2, entries: 5000 },
  },
  'GET /api/v1/admin/travel-rule': { body: { records: [TRAVEL_REC] } },
  'GET /api/v1/admin/restricted-lists': { body: { restricted_lists: [RESTRICTED] } },
  'GET /api/v1/admin/pre-clearance': { body: { pre_clearances: [CLEARANCE] } },
  'GET /api/v1/admin/employee-dealing/audit': {
    body: {
      audit: [
        {
          id: 91,
          admin_user_id: 42,
          action: 'employee_dealing.decide',
          target_type: 'pre_clearance',
          created_at: '2026-01-02T04:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/enforcement': {
    body: {
      actions: [
        {
          action_id: 'EA-1',
          signal_id: 55,
          account_id: 1001,
          action: 'WARN',
          source: 'MANUAL',
          params: {},
          status: 'ACTIVE',
          actor_id: 42,
          note: 'first rung',
          created_at: '2026-01-02T03:00:00Z',
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

describe('CompliancePage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<CompliancePage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all five panels and stamps X-Admin-Env on every admin call', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock(BASE);
    renderApp(<CompliancePage />);
    await waitFor(() => expect(region('Screening')).toBeInTheDocument());
    expect(region('Travel rule')).toBeInTheDocument();
    expect(region('Restricted lists')).toBeInTheDocument();
    expect(region('Employee dealing')).toBeInTheDocument();
    expect(region('Enforcement')).toBeInTheDocument();
    const adminCalls = calls.filter((c) => c.url.includes('/admin/'));
    expect(adminCalls.length).toBeGreaterThanOrEqual(6);
    for (const c of adminCalls) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('screens an account and renders hits plus the provider gate', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/compliance/screening/accounts/1001': {
        body: {
          outcome: {
            account_id: 1001,
            sanction_hits: [{ list: 'OFAC', score: 0.99 }],
            quarantined: false,
            adverse_media_count: 0,
            screened_at: '2026-01-02T05:00:00Z',
          },
        },
      },
    });
    renderApp(<CompliancePage />);
    const user = userEvent.setup();
    const scr = await waitFor(() => region('Screening'));
    await waitFor(() => expect(within(scr).getByText('OPEN')).toBeInTheDocument());
    const scrForm = within(scr).getByRole('form', { name: 'Account screen' });
    await user.type(within(scrForm).getByLabelText('Account id'), '1001');
    await user.click(within(scrForm).getByRole('button', { name: 'Screen account' }));
    await waitFor(() => expect(within(scr).getByText('SANCTIONS HIT')).toBeInTheDocument());
    const post = calls.find((c) => c.url.includes('/screening/accounts/1001'));
    expect(post?.method).toBe('POST');
  });

  it('supplies missing travel-rule party data for a MISSING_INFO record', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/travel-rule/7/supply': {
        body: { record: { ...TRAVEL_REC, status: 'COMPLETE', missing_fields: [] } },
      },
    });
    renderApp(<CompliancePage />);
    const user = userEvent.setup();
    const tr = await waitFor(() => region('Travel rule'));
    await waitFor(() => expect(within(tr).getByText('MISSING_INFO')).toBeInTheDocument());
    expect(
      within(tr).getByText('beneficiary.name, beneficiary.account_number'),
    ).toBeInTheDocument();
    await user.click(within(tr).getByText('7'));
    const org = within(tr).getByRole('group', { name: 'Originator' });
    await user.type(within(org).getByLabelText('Name'), 'Bob');
    await user.click(within(tr).getByRole('button', { name: 'Supply missing info' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/travel-rule/7/supply'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        originator: { name: 'Bob' },
      });
    });
  });

  it('creates and retires a restricted list (retire behind confirmation)', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/restricted-lists': { body: { restricted_list: RESTRICTED } },
      'DELETE /api/v1/admin/restricted-lists': { body: { status: 'RETIRED' } },
    });
    renderApp(<CompliancePage />);
    const user = userEvent.setup();
    const rl = await waitFor(() => region('Restricted lists'));
    await waitFor(() => expect(within(rl).getByText('EVT-EARN-1')).toBeInTheDocument());

    const form = within(rl).getByRole('form', { name: 'Create restricted list' });
    await user.type(within(form).getByLabelText('Event id'), 'EVT-2');
    await user.type(within(form).getByLabelText('Event type'), 'CORP_ACTION');
    await user.type(within(form).getByLabelText(/Window start/), '2026-01-10T00:00:00Z');
    await user.type(within(form).getByLabelText(/Window end/), '2026-01-11T00:00:00Z');
    await user.type(within(form).getByLabelText('Reason'), 'board embargo');
    await user.click(within(form).getByRole('button', { name: 'Create restriction' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.endsWith('/restricted-lists'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        event_id: 'EVT-2',
        scope: 'ALL_STAFF',
      });
    });

    await user.click(within(rl).getByRole('button', { name: 'Retire…' }));
    await user.click(screen.getByRole('button', { name: 'Retire' }));
    await waitFor(() => {
      const del = calls.find((c) => c.method === 'DELETE' && c.url.includes('restricted-lists'));
      expect(del?.url).toContain('id=3');
    });
  });

  it('decides a pre-clearance request and files a new one', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/pre-clearance': { body: { pre_clearance: CLEARANCE } },
    });
    renderApp(<CompliancePage />);
    const user = userEvent.setup();
    const ed = await waitFor(() => region('Employee dealing'));
    await waitFor(() => expect(within(ed).getByText('PENDING')).toBeInTheDocument());
    expect(within(ed).getByText('employee_dealing.decide')).toBeInTheDocument();

    await user.click(within(ed).getByRole('button', { name: 'Decide…' }));
    await user.type(within(ed).getByLabelText('Decision note'), 'cleared by control room');
    await user.click(within(ed).getByRole('button', { name: 'Approve' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.endsWith('/pre-clearance'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        id: 11,
        approve: true,
        note: 'cleared by control room',
      });
    });
  });

  it('applies an enforcement action with the order-gate preview', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/enforcement/55': {
        body: { action: { action_id: 'EA-2', action: 'RESTRICT', status: 'ACTIVE' } },
      },
    });
    renderApp(<CompliancePage />);
    const user = userEvent.setup();
    const ef = await waitFor(() => region('Enforcement'));
    await waitFor(() => expect(within(ef).getAllByText('WARN').length).toBeGreaterThanOrEqual(2));
    expect(within(ef).getByLabelText('Order-gate effect preview')).toBeInTheDocument();

    await user.selectOptions(within(ef).getByLabelText('Action'), 'RESTRICT');
    expect(within(ef).getByLabelText('Order-gate effect preview')).toHaveTextContent(
      'New orders rejected',
    );
    await user.type(within(ef).getByLabelText('Signal id'), '55');
    await user.type(within(ef).getByLabelText('Note'), 'confirmed layering');
    await user.click(within(ef).getByRole('button', { name: 'Apply enforcement' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/enforcement/55'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        action: 'RESTRICT',
        note: 'confirmed layering',
      });
    });
  });
});
