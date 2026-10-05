/**
 * Conduct/governance/DORA console tests (Phase-10.5 Task 10.5.3.15) —
 * wire-level assertions for reg-change transitions, product-profile
 * dual-control submissions, FXGC verdicts, pack release, recert
 * decisions, and ICT provider register ops.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import ConductPage from './ConductPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BASE = {
  'GET /api/v1/admin/regulatory-changes': {
    body: {
      changes: [
        {
          change_id: 7,
          authority: 'FCA',
          instrument: 'PS26/1',
          title: 'Conduct rules update',
          status: 'RECEIVED',
          published_at: '2026-01-02T00:00:00Z',
          triage_due_at: '2026-01-16T00:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/execution-policies': {
    body: {
      policies: [{ id: 2, version: '2026.1', status: 'ACTIVE', body_ref: 'doc://exec-2026-1' }],
    },
  },
  'GET /api/v1/admin/product-profiles': {
    body: {
      profiles: [
        { profile_id: 1, code: 'STD', pricing_plan: 'FIXED', min_deposit: '0', status: 'ACTIVE' },
      ],
    },
  },
  'GET /api/v1/admin/product-target-markets': {
    body: {
      target_markets: [
        {
          id: 11,
          profile_id: 1,
          client_category: 'RETAIL',
          status: 'CURRENT',
          review_due_at: '2026-01-01T00:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/fx-global-code/assessments': {
    body: {
      runs: [
        {
          id: 4,
          code_version: 'v2025',
          period: '2025',
          status: 'OPEN',
          principles_total: 55,
          principles_adherent: 40,
          principles_partial: 3,
          principles_non: 1,
          principles_pending: 11,
        },
      ],
    },
  },
  'GET /api/v1/admin/governance-packs': {
    body: {
      governance_packs: [
        {
          pack_id: 9,
          kind: 'CEO_DAILY',
          period: '2026-01-05',
          status: 'GENERATED',
          content_hash: 'abcdef1234567890',
          generated_at: '2026-01-05T00:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/data-residency/policies': {
    body: {
      policies: [
        {
          jurisdiction_code: 'UK',
          home_region: 'eu-west-2',
          kms_key_id: 'kms-uk-1',
          s3_bucket: 'exc-uk',
          adequate: true,
          transfer_instrument: 'ADEQUACY',
        },
      ],
    },
  },
  'GET /api/v1/admin/data-residency/access-log': { body: { access_log: [] } },
  // Longer prefix first — installFetchMock matches startsWith in insertion order.
  'GET /api/v1/admin/ict-providers/due': {
    body: {
      alerts: [
        {
          Code: 'ICT_REVIEW_OVERDUE',
          ProviderID: 3,
          Name: 'CloudCo',
          Summary: 'review overdue',
          DueAt: '2026-01-01T00:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/ict-providers': {
    body: {
      ict_providers: [
        {
          ID: 3,
          Name: 'CloudCo',
          ICTService: 'Hosting',
          Concentration: 'HIGH',
          Status: 'ACTIVE',
          NextReviewAt: '2026-03-01T00:00:00Z',
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

describe('ConductPage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<ConductPage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all four panels, stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock(BASE);
    renderApp(<ConductPage />);
    await waitFor(() => expect(region('Reg changes')).toBeInTheDocument());
    expect(region('Conduct policies')).toBeInTheDocument();
    expect(region('FX Global Code')).toBeInTheDocument();
    expect(region('Governance and DORA')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('Conduct rules update')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText(/ICT_REVIEW_OVERDUE/)).toBeInTheDocument());
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('transitions a reg change with the action contract', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/regulatory-changes/7/impact': {
        body: {
          change: {
            change_id: 7,
            authority: 'FCA',
            instrument: 'PS26/1',
            title: 'Conduct rules update',
            status: 'RECEIVED',
            published_at: '2026-01-02T00:00:00Z',
            triage_due_at: '2026-01-16T00:00:00Z',
          },
          impacts: [
            {
              id: 21,
              change_id: 7,
              kind: 'SYSTEM',
              ref: 'matching-core',
              status: 'OPEN',
              recorded_by: 1,
              created_at: '2026-01-03T00:00:00Z',
              updated_at: '2026-01-03T00:00:00Z',
            },
          ],
          correspondence: [],
        },
      },
      'POST /api/v1/admin/regulatory-changes/7/transition': {
        body: { change: { change_id: 7, status: 'TRIAGED' } },
      },
    });
    renderApp(<ConductPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Reg changes'));
    await waitFor(() =>
      expect(within(panel).getByText('Conduct rules update')).toBeInTheDocument(),
    );
    await user.click(within(panel).getByText('Conduct rules update'));
    const form = await waitFor(() =>
      within(panel).getByRole('form', { name: 'Change transition' }),
    );
    await user.selectOptions(within(form).getByLabelText('Transition action'), 'triage');
    await user.type(within(form).getByLabelText('Transition owner'), '42');
    await user.click(within(form).getByRole('button', { name: 'Transition' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/regulatory-changes/7/transition'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        action: 'triage',
        owner: 42,
      });
    });
  });

  it('submits a product-profile change through dual-control (202 PENDING)', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/product-profiles': {
        status: 202,
        body: { request: { id: 77, status: 'PENDING' }, message: 'pending' },
      },
    });
    renderApp(<ConductPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Conduct policies'));
    const form = within(panel).getByRole('form', { name: 'Submit profile change' });
    await user.type(within(form).getByLabelText('Profile code'), 'PRO');
    await user.type(within(form).getByLabelText('Profile reason'), 'new tier');
    await user.click(within(form).getByRole('button', { name: 'Submit for approval' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.endsWith('/admin/product-profiles'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        reason: 'new tier',
        input: { code: 'PRO', pricing_plan: 'FIXED', min_deposit: '0' },
      });
    });
    await waitFor(() =>
      expect(within(panel).getByText(/request #77.*not yet applied/i)).toBeInTheDocument(),
    );
  });

  it('records an FXGC verdict with remediation on PARTIAL', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/fx-global-code/assessments/4': {
        body: { assessment: { id: 4, matrix: [] } },
      },
      'POST /api/v1/admin/fx-global-code/assessments/4/verdicts': {
        body: { principle: { principle_id: 12 } },
      },
    });
    renderApp(<ConductPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('FX Global Code'));
    await waitFor(() => expect(within(panel).getByText('v2025')).toBeInTheDocument());
    await user.click(within(panel).getByText('v2025'));
    const form = await waitFor(() =>
      within(panel).getByRole('form', { name: 'Principle verdict' }),
    );
    await user.type(within(form).getByLabelText('Principle id'), '12');
    await user.selectOptions(within(form).getByLabelText('Adherence status'), 'PARTIAL');
    await user.type(within(form).getByLabelText('Remediation ref'), 'ticket-99');
    await user.click(within(form).getByRole('button', { name: 'Record verdict' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/verdicts'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        principle_id: 12,
        adherence_status: 'PARTIAL',
        remediation_ref: 'ticket-99',
      });
    });
  });

  it('releases a governance pack with a distinct approver', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/governance-packs/9/release': {
        body: { pack_id: 9, status: 'RELEASED' },
      },
    });
    renderApp(<ConductPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Governance and DORA'));
    await waitFor(() => expect(within(panel).getByText('CEO_DAILY')).toBeInTheDocument());
    await user.type(within(panel).getByLabelText('Release pack id'), '9');
    await user.type(within(panel).getByLabelText('Release approver'), '55');
    await user.type(within(panel).getByLabelText('Release reason'), 'board review');
    await user.click(within(panel).getByRole('button', { name: 'Release (4-eyes)' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/release'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        approver_id: 55,
        reason: 'board review',
      });
    });
  });

  it('retires an ICT provider via DELETE', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'DELETE /api/v1/admin/ict-providers/3': { body: { status: 'RETIRED' } },
    });
    renderApp(<ConductPage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Governance and DORA'));
    await waitFor(() => expect(within(panel).getByText('CloudCo')).toBeInTheDocument());
    await user.click(within(panel).getByRole('button', { name: 'Retire' }));
    await waitFor(() => {
      const del = calls.find((c) => c.method === 'DELETE' && c.url.includes('/ict-providers/3'));
      expect(del).toBeDefined();
    });
  });
});
