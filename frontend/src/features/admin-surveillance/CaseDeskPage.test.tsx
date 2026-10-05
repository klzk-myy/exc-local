/**
 * Case desk tests (Phase-10.5 Task 10.5.3.6) — the signal→case→
 * disposition→SAR path, dual-controlled comms retrieval, and the
 * X-Admin-Env / role-gate invariants.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import CaseDeskPage from './CaseDeskPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const CASE = {
  id: 9,
  case_ref: 'CASE-9',
  signal_id: 55,
  account_id: 1001,
  signal_type: 'LAYERING',
  symbol: 'EURUSD',
  severity: 'HIGH',
  status: 'OPEN',
  sla_deadline: '2026-01-05T00:00:00Z',
  sla_breached: false,
  opened_at: '2026-01-02T03:00:00Z',
};

const SAR = {
  id: 4,
  trigger_type: 'SURVEILLANCE',
  account_id: 1001,
  subject_ref: 'acct:1001',
  description: 'layering pattern',
  status: 'UNDER_REVIEW',
  source_ref: 'case-9',
  detected_at: '2026-01-02T03:00:00Z',
  filing_deadline: '2026-01-30T00:00:00Z',
};

const RECORDING = {
  recording_id: 12,
  account_id: 1001,
  channel: 'PHONE',
  direction: 'OUTBOUND',
  source: 'turret',
  started_at: '2026-01-02T10:00:00Z',
  ended_at: '2026-01-02T10:05:00Z',
  content_ref: 's3://worm/rec-12',
  sha256: 'abc123def4567890abcd',
  chain_hash: 'ffeeddccbbaa0011223344556677',
  retention_until: '2031-01-02T00:00:00Z',
  sealed: true,
};

const BASE = {
  'GET /api/v1/admin/surveillance/summary': {
    body: {
      month: '2026-01',
      opened: 5,
      closed: 3,
      escalated: 1,
      false_positives: 1,
      avg_disposition_hours: 22.5,
      accuracy_by_signal: { LAYERING: 0.8 },
      open_breached: 0,
    },
  },
  'GET /api/v1/admin/surveillance/cases': { body: { cases: [CASE] } },
  'GET /api/v1/admin/sar': { body: { reports: [SAR] } },
  'GET /api/v1/admin/ctr': { body: { reports: [] } },
  'GET /api/v1/admin/aml/program': {
    body: { program: { compliant: true, breaches: [], artifacts: [] } },
  },
  'GET /api/v1/admin/aml/artifacts': { body: { artifacts: [] } },
  'GET /api/v1/admin/aml/monitoring': { body: { events: [] } },
  'GET /api/v1/admin/sanctions/status': {
    body: { provider_gate: { state: 'OPEN' }, pending_screens: 0 },
  },
  'GET /api/v1/admin/comms-recordings': { body: { recordings: [RECORDING] } },
};

const region = (name: string) => screen.getByRole('region', { name });

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

describe('CaseDeskPage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<CaseDeskPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all five panels and stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock(BASE);
    renderApp(<CaseDeskPage />);
    await waitFor(() => expect(region('Surveillance cases')).toBeInTheDocument());
    expect(region('SAR and CTR')).toBeInTheDocument();
    expect(region('AML program')).toBeInTheDocument();
    expect(region('Sanctions ops')).toBeInTheDocument();
    expect(region('Comms recordings')).toBeInTheDocument();
    const adminCalls = calls.filter((c) => c.url.includes('/admin/'));
    expect(adminCalls.length).toBeGreaterThanOrEqual(8);
    for (const c of adminCalls) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('walks case → workspace → ESCALATE_SAR disposition', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/surveillance/cases/9': {
        body: {
          case: CASE,
          evidence: [
            {
              id: 1,
              case_id: 9,
              kind: 'NOTE',
              body: 'spoof cluster',
              added_by: 42,
              created_at: '2026-01-02T04:00:00Z',
            },
          ],
          linked_signals: [55, 56],
          order_audit_ids: [9001],
        },
      },
      'POST /api/v1/admin/surveillance/cases/9/disposition': {
        body: { case: { ...CASE, status: 'ESCALATED_SAR', escalated_sar_id: 4 } },
      },
    });
    renderApp(<CaseDeskPage />);
    const user = userEvent.setup();
    const surv = await waitFor(() => region('Surveillance cases'));
    await waitFor(() => expect(within(surv).getByText('LAYERING')).toBeInTheDocument());
    await user.click(within(surv).getByText('LAYERING'));

    await waitFor(() => expect(within(surv).getByText('spoof cluster')).toBeInTheDocument());
    expect(within(surv).getByText(/Linked signals: 55, 56/)).toBeInTheDocument();

    const form = within(surv).getByRole('form', { name: 'Dispose case' });
    await user.selectOptions(within(form).getByLabelText('Disposition'), 'ESCALATE_SAR');
    await user.type(within(form).getByLabelText('Disposition reason'), 'confirmed layering');
    await user.click(within(form).getByRole('button', { name: 'Record disposition' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/cases/9/disposition'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        disposition: 'ESCALATE_SAR',
        reason: 'confirmed layering',
      });
    });
    await waitFor(() => expect(within(surv).getByText(/ESCALATED_SAR/)).toBeInTheDocument());
  });

  it('approves a SAR under four-eyes and files with filing_ref', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/sar/4/approve': { body: { sar: { ...SAR, status: 'APPROVED' } } },
      'POST /api/v1/admin/sar/4/file': { body: { sar: { ...SAR, status: 'FILED' } } },
    });
    renderApp(<CaseDeskPage />);
    const user = userEvent.setup();
    const sar = await waitFor(() => region('SAR and CTR'));
    await waitFor(() => expect(within(sar).getByText('UNDER REVIEW')).toBeInTheDocument());
    await user.click(within(sar).getByText('acct:1001'));

    await user.click(within(sar).getByRole('button', { name: 'Approve (4-eyes)' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/sar/4/approve'));
      expect(post).toBeDefined();
    });
    await waitFor(() =>
      expect(within(sar).getByRole('button', { name: 'Mark filed' })).toBeInTheDocument(),
    );
    await user.type(within(sar).getByLabelText('Filing ref'), 'FINCEN-2026-9');
    await user.click(within(sar).getByRole('button', { name: 'Mark filed' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/sar/4/file'));
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        filing_ref: 'FINCEN-2026-9',
      });
    });
  });

  it('retrieves a comms recording under dual control', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/comms-recordings/12/retrieve': {
        body: { recording: RECORDING, body: 'call transcript text' },
      },
      'POST /api/v1/admin/comms-recordings/verify-day': {
        body: { day: '2026-01-02', chain_ok: true },
      },
    });
    renderApp(<CaseDeskPage />);
    const user = userEvent.setup();
    const comms = await waitFor(() => region('Comms recordings'));
    await waitFor(() => expect(within(comms).getByText('PHONE')).toBeInTheDocument());
    await user.click(within(comms).getByText('PHONE'));

    const form = within(comms).getByRole('form', { name: 'Retrieve recording' });
    await user.type(within(form).getByLabelText('Approver id'), '77');
    await user.type(within(form).getByLabelText('Justification'), 'case CASE-9 review');
    await user.click(within(form).getByRole('button', { name: 'Retrieve (dual-control)' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/comms-recordings/12/retrieve'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        approver_id: 77,
        justification: 'case CASE-9 review',
      });
    });
    await waitFor(() =>
      expect(within(comms).getByText('call transcript text')).toBeInTheDocument(),
    );

    await user.type(within(comms).getByLabelText('Verify day'), '2026-01-02');
    await user.click(within(comms).getByRole('button', { name: 'Verify chain' }));
    await waitFor(() => expect(within(comms).getByText(/chain INTACT/)).toBeInTheDocument());
  });
});
