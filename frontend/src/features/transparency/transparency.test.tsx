import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp } from '@/test/accountMocks';

import SecurityPage from './SecurityPage';
import StatusPage from './StatusPage';
import TransparencyPage from './TransparencyPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
});

describe('StatusPage (no auth)', () => {
  it('renders session state, incidents and maintenance without sign-in', async () => {
    installFetchMock({
      'GET /api/v1/session/status': {
        body: {
          state: 'OPEN',
          market_open: true,
          consistent: true,
          shard_coverage: '8/8 shards OPEN',
          next_state: 'PRE_CLOSE',
          next_transition_at: '2026-10-02T22:00:00Z',
        },
      },
      'GET /api/v1/system/incidents': {
        body: {
          incidents: [
            {
              id: 3,
              title: 'Elevated order latency',
              severity: 'minor',
              status: 'RESOLVED',
              started_at: '2026-09-30T14:00:00Z',
              resolved_at: '2026-09-30T14:40:00Z',
              summary: 'p99 above SLO for 40m',
              postmortem_url: 'https://example.com/pm/3',
            },
          ],
        },
      },
      'GET /api/v1/maintenance/schedule': {
        body: {
          data: [
            {
              id: 2,
              title: 'Gateway restart',
              scope: 'GATEWAY',
              status: 'SCHEDULED',
              starts_at: '2026-10-04T02:00:00Z',
              ends_at: '2026-10-04T02:15:00Z',
            },
          ],
          count: 1,
        },
      },
    });
    renderApp(<StatusPage />);
    expect(await screen.findByText('OPEN')).toBeInTheDocument();
    expect(screen.getByText('8/8 shards OPEN')).toBeInTheDocument();
    expect(screen.getByText('YES')).toBeInTheDocument();
    expect(screen.getByText('Elevated order latency')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Postmortem' })).toHaveAttribute(
      'href',
      'https://example.com/pm/3',
    );
    expect(screen.getByText('Gateway restart')).toBeInTheDocument();
    expect(screen.getByText('GATEWAY')).toBeInTheDocument();
  });
});

describe('TransparencyPage (no auth)', () => {
  it('renders the venue document, leverage ceilings and RTS27 list', async () => {
    installFetchMock({
      'GET /api/v1/exchange-info': {
        body: {
          timezone: 'UTC',
          trading_hours: {
            type: '24/5',
            weekly_open_utc: 'Sunday 21:00',
            weekly_close_utc: 'Friday 22:00',
            daily_break_utc: '22:00–22:05',
          },
          symbols: [
            {
              symbol: 'EUR/USD',
              instrument_type: 'SPOT',
              status: 'TRADING',
              tick_size: '0.00001',
              lot_size: '100000',
              min_order_qty: '1000',
              max_leverage: 30,
              settlement_cycle: 2,
              order_types: ['LIMIT', 'MARKET'],
              permissions: { new_orders_allowed: true },
            },
          ],
          leverage_policies: [
            {
              entity_code: 'ESMA',
              client_category: 'RETAIL',
              instrument_group: 'FX_MAJOR',
              max_leverage: 30,
              effective_from: '2026-01-01',
            },
          ],
        },
      },
      'GET /api/v1/venue/best-execution/rts27': {
        body: {
          reports: [
            {
              id: 11,
              quarter_start: '2026-07-01T00:00:00Z',
              instrument_class: 'FX_MAJOR',
              version: 2,
              status: 'PUBLISHED',
              days_covered: 92,
              published_at: '2026-10-15T00:00:00Z',
            },
          ],
        },
      },
      'GET /api/v1/meta/pagination': {
        body: {
          envelope: {
            shape: '{data, next_cursor, limit, total}',
            cursor_order: '(created_at, id) DESC',
          },
          endpoints: [
            { path: '/api/v1/orders', default_limit: 100, max_limit: 1000, filterable: ['symbol'] },
          ],
        },
      },
      'GET /api/v1/meta/rate-limits': {
        body: {
          tiers: {
            public: { rate_per_sec: 10, burst_factor: 2, weight_per_min: 600, keyed_by: 'ip' },
          },
        },
      },
    });
    renderApp(<TransparencyPage />);
    expect(await screen.findByText('EUR/USD')).toBeInTheDocument();
    expect(screen.getByText(/24\/5/)).toBeInTheDocument();
    expect(screen.getByText('ESMA')).toBeInTheDocument();
    expect(screen.getByText('30:1')).toBeInTheDocument();
    expect(screen.getAllByText('FX_MAJOR').length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText('92')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'CSV' })).toBeInTheDocument();
    expect(screen.getByText('/api/v1/orders')).toBeInTheDocument();
    expect(screen.getByText('public')).toBeInTheDocument();
    // switch to RTS28 → refetches that list
    await userEvent.selectOptions(screen.getByLabelText('Report type'), 'rts28');
    await waitFor(() => {
      expect(screen.getByText(/No published reports yet/)).toBeInTheDocument();
    });
  });
});

describe('SecurityPage (no auth)', () => {
  it('loads the markdown policy and posts a disclosure', async () => {
    const calls = installFetchMock({
      'GET /api/v1/security/policy': {
        status: 200,
        body: '# VDP\n\nScope: all public endpoints.',
      },
      'POST /api/v1/security/disclosures': {
        status: 201,
        body: { report_id: 'VDP-2026-0042', status: 'RECEIVED', duplicate: false },
      },
    });
    renderApp(<SecurityPage />);
    expect(await screen.findByText(/Scope: all public endpoints/)).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Title'), 'IDOR on statements');
    await userEvent.type(screen.getByLabelText('Reporter handle'), 'sec-res-1');
    await userEvent.type(screen.getByLabelText('Reproduction steps'), '1) GET /x');
    fireEvent.click(screen.getByRole('button', { name: 'Submit report' }));
    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === 'POST' && c.url.includes('/security/disclosures'),
      );
      expect(post).toBeDefined();
      const body = JSON.parse(post?.init?.body as string) as Record<string, unknown>;
      expect(body['title']).toBe('IDOR on statements');
      expect(body['suggested_severity']).toBe('MEDIUM');
      expect(body['website']).toBe('');
    });
    expect(await screen.findByText(/report VDP-2026-0042/)).toBeInTheDocument();
  });
});
