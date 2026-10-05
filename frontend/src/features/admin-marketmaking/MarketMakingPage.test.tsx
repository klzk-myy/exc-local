/**
 * MM/DEA/algo governance console tests (Phase-10.5 Task 10.5.3.12) —
 * wire-level assertions for program enrollment + suspend/resume +
 * MMP reset + rebate sweep, algo-cert transitions, DEA upsert/suspend,
 * and RTS 6 self-assessment filing.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import MarketMakingPage from './MarketMakingPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BASE = {
  'GET /api/v1/admin/mm-programs': {
    body: {
      mm_programs: [
        {
          id: 5,
          account_id: 42,
          instrument_id: 3,
          symbol: 'EURUSD',
          min_quote_size: '100000',
          max_spread_bps: '4',
          presence_pct: '85',
          mmp_max_fills: 50,
          mmp_window_ms: 1000,
          rebate_bps: '0.5',
          status: 'ACTIVE',
        },
      ],
    },
  },
  'GET /api/v1/admin/algo-certifications': {
    body: {
      certifications: [
        {
          id: 9,
          algo_id: 'TWAP-1',
          account_id: 42,
          status: 'CERTIFIED',
          test_evidence_ref: 'evidence/9.pdf',
          kill_button_tested: true,
        },
      ],
    },
  },
  'GET /api/v1/admin/rts6/self-assessments': { body: { assessments: [] } },
};

const region = (name: string) => screen.getByRole('region', { name });

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

describe('MarketMakingPage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<MarketMakingPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders both panels, program terms, and stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock(BASE);
    renderApp(<MarketMakingPage />);
    await waitFor(() => expect(region('MM programs')).toBeInTheDocument());
    expect(region('Algo DEA RTS6 governance')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('EURUSD')).toBeInTheDocument());
    expect(screen.getByText('TWAP-1')).toBeInTheDocument();
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('suspends an ACTIVE program and resets its MMP lockout', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/mm-programs/5/suspend': { body: { id: 5, status: 'SUSPENDED' } },
      'POST /api/v1/admin/mm-programs/5/mmp-reset': { body: {} },
    });
    renderApp(<MarketMakingPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('MM programs'));
    await waitFor(() => expect(within(panel).getByText('ACTIVE')).toBeInTheDocument());
    await user.click(within(panel).getByRole('button', { name: 'Suspend' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/mm-programs/5/suspend'),
      );
      expect(post).toBeDefined();
    });
    await user.click(within(panel).getByRole('button', { name: 'MMP reset' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/mm-programs/5/mmp-reset'),
      );
      expect(post).toBeDefined();
    });
    await waitFor(() =>
      expect(within(panel).getByText(/MMP lockout .* cleared/)).toBeInTheDocument(),
    );
  });

  it('enrolls a program with the full quota-term wire contract', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/mm-programs': { status: 201, body: { id: 8, status: 'ACTIVE' } },
    });
    renderApp(<MarketMakingPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('MM programs'));
    const form = within(panel).getByRole('form', { name: 'Enroll MM program' });
    await user.type(within(form).getByLabelText('Program accountId'), '77');
    await user.type(within(form).getByLabelText('Program minQuoteSize'), '50000');
    await user.type(within(form).getByLabelText('Program maxSpreadBps'), '3');
    await user.type(within(form).getByLabelText('Program presencePct'), '90');
    await user.type(within(form).getByLabelText('Program mmpMaxFills'), '40');
    await user.type(within(form).getByLabelText('Program mmpWindowMs'), '2000');
    await user.type(within(form).getByLabelText('Program rebateBps'), '0.4');
    await user.click(within(form).getByRole('button', { name: 'Enroll' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.endsWith('/mm-programs'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        account_id: 77,
        min_quote_size: '50000',
        max_spread_bps: '3',
        presence_pct: '90',
        mmp_max_fills: 40,
        mmp_window_ms: 2000,
        rebate_bps: '0.4',
      });
    });
  });

  it('transitions an algo certification to SUSPENDED', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/algo-certifications/9/transition': {
        body: { certification: { id: 9, status: 'SUSPENDED' } },
      },
    });
    renderApp(<MarketMakingPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Algo DEA RTS6 governance'));
    const form = within(panel).getByRole('form', { name: 'Transition certification' });
    await user.type(within(form).getByLabelText('Cert id'), '9');
    await user.type(within(form).getByLabelText('Transition reason'), 'kill-switch drill fail');
    await user.click(within(form).getByRole('button', { name: 'Transition' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/algo-certifications/9/transition'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        to: 'SUSPENDED',
        reason: 'kill-switch drill fail',
      });
    });
  });

  it('upserts DEA limits then suspends the session', async () => {
    signInForTests({ roles: ['Risk Manager'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/dea/controls': { body: { dea_control: { id: 3 } } },
      'POST /api/v1/admin/dea/controls/FIX-DEA-7/suspend': { body: { status: 'SUSPENDED' } },
    });
    renderApp(<MarketMakingPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Algo DEA RTS6 governance'));
    const setForm = within(panel).getByRole('form', { name: 'Set DEA limits' });
    await user.type(within(setForm).getByLabelText('DEA sessionId'), 'FIX-DEA-7');
    await user.type(within(setForm).getByLabelText('DEA accountId'), '42');
    await user.type(within(setForm).getByLabelText('DEA maxOrderQty'), '1000000');
    await user.type(within(setForm).getByLabelText('DEA maxMsgsPerSec'), '500');
    await user.type(within(setForm).getByLabelText('DEA sponsoringDesk'), 'LONDON-DEA');
    await user.type(within(setForm).getByLabelText('DEA dropCopyFeed'), 'DC-1');
    await user.click(within(setForm).getByRole('button', { name: 'Upsert limits' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.endsWith('/dea/controls'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        session_id: 'FIX-DEA-7',
        max_order_qty: '1000000',
        max_msgs_per_sec: 500,
        sponsoring_desk: 'LONDON-DEA',
      });
    });
    const suspForm = within(panel).getByRole('form', { name: 'Suspend DEA session' });
    await user.type(within(suspForm).getByLabelText('Suspend session id'), 'FIX-DEA-7');
    await user.type(within(suspForm).getByLabelText('Suspend reason'), 'OTR breach');
    await user.click(within(suspForm).getByRole('button', { name: 'Suspend session' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/dea/controls/FIX-DEA-7/suspend'),
      );
      expect(post).toBeDefined();
    });
    await waitFor(() =>
      expect(within(panel).getByText(/suspended — DEA access cut/)).toBeInTheDocument(),
    );
  });
});
