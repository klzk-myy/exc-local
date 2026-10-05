/**
 * Reg-reporting desk tests (Phase-10.5 Task 10.5.3.13) — wire-level
 * assertions for break dispositions, corrected resubmission, the
 * RTS27/28 generate→publish pipeline, EMIR events, and the compliance
 * export contract.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import RegReportingPage from './RegReportingPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BASE = {
  'GET /api/v1/admin/regreporting/queue': {
    body: {
      breaks: [
        {
          break_id: 12,
          event_id: 40,
          uti: 'UTI-100',
          regime: 'EMIR',
          break_type: 'PAIR_MISMATCH',
          status: 'OPEN',
          detected_by: 'reconciler',
          sla_due_at: '2026-01-06T00:00:00Z',
          detected_at: '2026-01-05T00:00:00Z',
        },
      ],
      transport: [],
    },
  },
  'GET /api/v1/admin/regreporting/submissions': {
    body: {
      submissions: [
        {
          report_submission_id: 30,
          event_id: 40,
          regime: 'EMIR',
          destination: 'ARM',
          attempt: 1,
          status: 'NACKED',
          error_code: 'VAL-99',
        },
      ],
    },
  },
  'GET /api/v1/admin/bestexec/rts27': { body: { reports: [] } },
  'GET /api/v1/admin/bestexec/rts28': { body: { reports: [] } },
  'GET /api/v1/admin/emir-report': {
    body: {
      events: [
        {
          event_id: 40,
          uti: 'UTI-100',
          regime: 'EMIR',
          action_type: 'NEW',
          event_type: 'TRADE',
          report_seq: 1,
          instrument_code: 'EURUSD',
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

describe('RegReportingPage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<RegReportingPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all three panels, queue + EMIR events, stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock(BASE);
    renderApp(<RegReportingPage />);
    await waitFor(() => expect(region('Reg submissions queue')).toBeInTheDocument());
    expect(region('Best execution reports')).toBeInTheDocument();
    expect(region('Regime reports')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('PAIR_MISMATCH')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('NACKED')).toBeInTheDocument());
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('resolves a break with the RESOLVED|WONT_FIX disposition contract', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/regreporting/breaks/12/resolve': {
        body: { break_id: 12, resolution: 'RESOLVED', applied: true },
      },
    });
    renderApp(<RegReportingPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Reg submissions queue'));
    const form = within(panel).getByRole('form', { name: 'Resolve break' });
    await user.type(within(form).getByLabelText('Break id'), '12');
    await user.type(within(form).getByLabelText('Break notes'), 'counterparty confirmed');
    await user.click(within(form).getByRole('button', { name: 'Resolve' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/breaks/12/resolve'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        resolution: 'RESOLVED',
        notes: 'counterparty confirmed',
      });
    });
    await waitFor(() => expect(within(panel).getByText(/Break #12 RESOLVED/)).toBeInTheDocument());
  });

  it('resubmits a NACKED submission with corrections (new row, not overwrite)', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/regreporting/submissions/30/resubmit': {
        status: 201,
        body: { submission: { report_submission_id: 31, status: 'PENDING' } },
      },
    });
    renderApp(<RegReportingPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Reg submissions queue'));
    const form = within(panel).getByRole('form', { name: 'Resubmit submission' });
    await user.type(within(form).getByLabelText('Submission id'), '30');
    await user.clear(within(form).getByLabelText('Corrections JSON'));
    await user.click(within(form).getByLabelText('Corrections JSON'));
    await user.paste('{"price":"1.0850"}');
    await user.click(within(form).getByRole('button', { name: 'Resubmit (corrected)' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/submissions/30/resubmit'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        corrections: { price: '1.0850' },
      });
    });
    await waitFor(() =>
      expect(within(panel).getByText(/Corrected submission #31 created/)).toBeInTheDocument(),
    );
  });

  it('runs the RTS27 pipeline: materialize → generate → publish', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/bestexec/rts27': {
        body: {
          reports: [
            {
              id: 7,
              quarter_start: '2026-01-01T00:00:00Z',
              instrument_class: 'FX_SPOT',
              version: 1,
              status: 'DRAFT',
              days_covered: 63,
            },
          ],
        },
      },
      'POST /api/v1/admin/bestexec/rts27/materialize': { body: {} },
      'POST /api/v1/admin/bestexec/rts27/generate': { status: 201, body: { reports: [{}] } },
      'POST /api/v1/admin/bestexec/rts27/7/publish': { body: { report: { id: 7 } } },
    });
    renderApp(<RegReportingPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Best execution reports'));
    await waitFor(() => expect(within(panel).getByText('FX_SPOT')).toBeInTheDocument());
    await user.type(within(panel).getByLabelText('Materialize day'), '2026-01-05');
    await user.click(within(panel).getByRole('button', { name: 'Materialize day' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/rts27/materialize'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({ day: '2026-01-05' });
    });
    await user.type(within(panel).getByLabelText('Quarter date'), '2026-02-15');
    await user.click(within(panel).getByRole('button', { name: 'Generate quarter' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/rts27/generate'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({ quarter: '2026-02-15' });
    });
    await user.click(within(panel).getByRole('button', { name: 'Publish' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/rts27/7/publish'));
      expect(post).toBeDefined();
    });
  });

  it('runs the compliance export with type/from/to params', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/compliance-report': { body: { rows: [], type: 'EMIR' } },
    });
    renderApp(<RegReportingPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Regime reports'));
    await waitFor(() => expect(within(panel).getByText('UTI-100')).toBeInTheDocument());
    const form = within(panel).getByRole('form', { name: 'Compliance export' });
    await user.selectOptions(within(form).getByLabelText('Export type'), 'EMIR');
    await user.type(within(form).getByLabelText('Export from'), '2026-01-01');
    await user.click(within(form).getByRole('button', { name: 'Export' }));
    await waitFor(() => {
      const get = calls.find((c) => c.method === 'GET' && c.url.includes('/compliance-report'));
      expect(get).toBeDefined();
      expect(get?.url).toContain('type=EMIR');
      expect(get?.url).toContain('from=2026-01-01');
    });
  });
});
