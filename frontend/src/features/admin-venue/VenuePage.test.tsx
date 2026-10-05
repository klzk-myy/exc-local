/**
 * Venue-governance console tests (Phase-10.5 Task 10.5.3.14) —
 * wire-level assertions for member admission actions, the rulebook
 * lifecycle, case transitions, and the launch gate surface.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import VenuePage from './VenuePage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BASE = {
  'GET /api/v1/admin/venue/members': {
    body: {
      members: [
        {
          member_id: 5,
          legal_name: 'Alpha Liquidity LLC',
          lei: 'LEIALPHA0000000001',
          access_model: 'DEA',
          approved_products: [],
          approved_ports: [],
          due_diligence_status: 'COMPLETED',
          admission_decision: 'APPROVED',
          suspended: false,
          created_at: '2026-01-01T00:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/venue/rulebooks': {
    body: {
      rulebooks: [
        {
          rulebook_id: 3,
          kind: 'RULEBOOK',
          scope_key: 'GLOBAL',
          version: '2026.1',
          body_ref: 'doc://rb-2026-1',
          status: 'DRAFT',
          requires_regulator_approval: true,
          regulator_status: 'PENDING',
        },
      ],
    },
  },
  'GET /api/v1/admin/venue/interventions': { body: { interventions: [] } },
  'GET /api/v1/admin/venue/cases': {
    body: {
      cases: [
        {
          case_id: 9,
          case_ref: 'CASE-2026-009',
          kind: 'INVESTIGATION',
          subject: ' layering pattern',
          status: 'OPEN',
          opened_at: '2026-01-02T00:00:00Z',
        },
      ],
    },
  },
  'GET /api/v1/admin/venue/conflicts': { body: { conflicts: [] } },
  'GET /api/v1/admin/venue/self-assessments': { body: { assessments: [] } },
  'GET /api/v1/admin/venue/cco-reports': { body: { reports: [] } },
  'GET /api/v1/admin/venue/launch-prerequisites': {
    body: {
      prerequisites: [
        {
          prereq_id: 1,
          kind: 'LICENSING',
          scope: 'GLOBAL',
          required: true,
          status: 'EVIDENCED',
          description: 'MTF license',
          evidence_ref: 'lic://mtf-1',
        },
      ],
    },
  },
  'GET /api/v1/admin/venue/launch-gate': {
    body: {
      launch_gate: {
        ready: false,
        missing: [
          {
            prereq_id: 2,
            kind: 'BOARD_CCO',
            scope: 'GLOBAL',
            required: true,
            status: 'PENDING',
            description: 'sign-off',
          },
        ],
        evaluated_at: '2026-01-05T00:00:00Z',
      },
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

describe('VenuePage', () => {
  it('denies non-admin sessions before any API call', () => {
    signInForTests({ roles: [] });
    const calls = installFetchMock({});
    renderApp(<VenuePage />);
    expect(screen.getByText(/Admin access required/i)).toBeInTheDocument();
    expect(calls.filter((c) => c.url.includes('/admin/'))).toHaveLength(0);
  });

  it('renders all four panels + launch gate, stamps X-Admin-Env', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock(BASE);
    renderApp(<VenuePage />);
    await waitFor(() => expect(region('Venue members')).toBeInTheDocument());
    expect(region('Venue rulebooks')).toBeInTheDocument();
    expect(region('Venue oversight')).toBeInTheDocument();
    expect(region('Venue assurance')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('Alpha Liquidity LLC')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('BLOCKED')).toBeInTheDocument());
    for (const c of calls.filter((x) => x.url.includes('/admin/'))) {
      expect(new Headers(c.init?.headers).get('X-Admin-Env')).toBe('dev');
    }
  });

  it('records an admission decision on the member lifecycle', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/venue/members/5': {
        body: {
          member: {
            member_id: 5,
            legal_name: 'Alpha Liquidity LLC',
            lei: 'LEIALPHA0000000001',
            access_model: 'DEA',
            due_diligence_status: 'COMPLETED',
            admission_decision: 'PENDING',
            suspended: false,
            approved_products: [],
            approved_ports: [],
            created_at: '2026-01-01T00:00:00Z',
          },
          events: [
            {
              event_id: 1,
              member_id: 5,
              event_type: 'DD_COMPLETED',
              detail: {},
              created_at: '2026-01-03T00:00:00Z',
            },
          ],
          reviews: [],
        },
      },
      'POST /api/v1/admin/venue/members/5/decision': {
        body: { member: { member_id: 5, admission_decision: 'APPROVED' } },
      },
    });
    renderApp(<VenuePage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Venue members'));
    await waitFor(() => expect(within(panel).getByText('Alpha Liquidity LLC')).toBeInTheDocument());
    await user.click(within(panel).getByText('Alpha Liquidity LLC'));
    await waitFor(() => expect(within(panel).getByText(/DD_COMPLETED/)).toBeInTheDocument());
    const form = within(panel).getByRole('form', { name: 'Member action' });
    await user.selectOptions(within(form).getByLabelText('Lifecycle action'), 'decision');
    await user.type(within(form).getByLabelText('Member action reason'), 'dd clean');
    await user.type(within(form).getByLabelText('Member action annual_review_due'), '2027-01-01');
    await user.click(within(form).getByRole('button', { name: 'Apply' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/members/5/decision'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        approve: true,
        reason: 'dd clean',
        annual_review_due: '2027-01-01',
      });
    });
  });

  it('records a regulator filing on a rulebook (DRAFT → FILED)', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/venue/rulebooks/3': {
        body: {
          rulebook: {
            rulebook_id: 3,
            kind: 'RULEBOOK',
            scope_key: 'GLOBAL',
            version: '2026.1',
            body_ref: 'doc://rb-2026-1',
            status: 'DRAFT',
            requires_regulator_approval: true,
            regulator_status: 'PENDING',
          },
          notices: [],
          acknowledgements: [],
        },
      },
      'POST /api/v1/admin/venue/rulebooks/3/file': {
        body: { rulebook: { rulebook_id: 3, status: 'FILED' } },
      },
    });
    renderApp(<VenuePage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Venue rulebooks'));
    await waitFor(() => expect(within(panel).getByText('2026.1')).toBeInTheDocument());
    await user.click(within(panel).getByText('2026.1'));
    const form = await waitFor(() => within(panel).getByRole('form', { name: 'Rulebook action' }));
    await user.selectOptions(within(form).getByLabelText('Rulebook lifecycle action'), 'file');
    await user.type(within(form).getByLabelText('Rulebook action filing_ref'), 'FCA-2026-77');
    await user.click(within(form).getByRole('button', { name: 'Apply' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/rulebooks/3/file'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        filing_ref: 'FCA-2026-77',
      });
    });
  });

  it('only offers legal case transitions and requires outcome on terminal', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'GET /api/v1/admin/venue/cases/9': {
        body: {
          case: {
            case_id: 9,
            case_ref: 'CASE-2026-009',
            kind: 'INVESTIGATION',
            subject: 'layering pattern',
            status: 'OPEN',
            opened_at: '2026-01-02T00:00:00Z',
          },
          evidence: [],
        },
      },
      'POST /api/v1/admin/venue/cases/9/transition': {
        body: { case: { case_id: 9, status: 'DISMISSED' } },
      },
    });
    renderApp(<VenuePage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Venue oversight'));
    await waitFor(() => expect(within(panel).getByText('CASE-2026-009')).toBeInTheDocument());
    await user.click(within(panel).getByText('CASE-2026-009'));
    const form = await waitFor(() => within(panel).getByRole('form', { name: 'Case action' }));
    await user.selectOptions(within(form).getByLabelText('Case action kind'), 'transition');
    const statusSel = within(form).getByLabelText('Transition status');
    // OPEN → only INVESTIGATING|DISMISSED are legal
    expect(
      within(statusSel)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['select…', 'INVESTIGATING', 'DISMISSED']);
    await user.selectOptions(statusSel, 'DISMISSED');
    // DISMISSED is terminal → outcome field required
    await user.type(within(form).getByLabelText('Oversight outcome'), 'no evidence');
    await user.click(within(form).getByRole('button', { name: 'Apply' }));
    await waitFor(() => {
      const post = calls.find((c) => c.method === 'POST' && c.url.includes('/cases/9/transition'));
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        status: 'DISMISSED',
        outcome: 'no evidence',
      });
    });
  });

  it('evidences a launch prerequisite', async () => {
    signInForTests({ roles: ['Compliance Officer'] });
    const calls = installFetchMock({
      ...BASE,
      'POST /api/v1/admin/venue/launch-prerequisites': {
        body: { prerequisite: { prereq_id: 2 } },
      },
    });
    renderApp(<VenuePage />);
    const user = userEvent.setup();
    const panel = await waitFor(() => region('Venue assurance'));
    await waitFor(() => expect(within(panel).getByText('BLOCKED')).toBeInTheDocument());
    const form = within(panel).getByRole('form', { name: 'Evidence prerequisite' });
    await user.selectOptions(within(form).getByLabelText('Prereq kind'), 'BOARD_CCO');
    await user.type(within(form).getByLabelText('Prereq evidenceRef'), 'board://minutes-2026-01');
    await user.click(within(form).getByRole('button', { name: 'Evidence' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/launch-prerequisites'),
      );
      expect(post).toBeDefined();
      expect(JSON.parse(post?.init?.body as string)).toMatchObject({
        kind: 'BOARD_CCO',
        evidence_ref: 'board://minutes-2026-01',
      });
    });
  });
});
