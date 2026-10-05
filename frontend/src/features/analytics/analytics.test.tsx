import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import AnalyticsPage from './AnalyticsPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const STATS24H = {
  status: 200,
  body: {
    window: '24h',
    count: 1,
    server_time_ms: 1770000000000,
    data: [
      {
        symbol: 'EUR/USD',
        last: '1.0850',
        price_change_pct: '0.42',
        volume: '1200000',
        quote_volume: '1300000',
        trade_count: 88,
      },
    ],
  },
};

const BASE: Record<string, { status: number; body: unknown }> = {
  'GET /api/v1/stats/24h': STATS24H,
  'GET /api/v1/market/performance': {
    status: 200,
    body: {
      data: {
        status: 'held',
        held_age_ms: 90000,
        delay_ms: 300000,
        divergent_metrics: ['fill_rate_24h'],
        venue: { fill_rate_24h: '0.93', uptime_24h: 0.999 },
      },
    },
  },
  'GET /api/v1/analytics/volume*': { status: 200, body: { rows: [], count: 0 } },
  'GET /api/v1/analytics/stats': { status: 200, body: { fills: [], tiers: [] } },
  'GET /api/v1/analytics/open-interest*': {
    status: 200,
    body: {
      data: {
        symbol: 'EUR/USD',
        interval: '1h',
        current: {
          open_interest: '4200000',
          open_interest_notional: '4600000',
          positions: 41,
          stale: false,
          as_of_ms: 1,
        },
      },
    },
  },
  'GET /api/v1/analytics/long-short-ratio*': {
    status: 200,
    body: {
      data: {
        symbol: 'EUR/USD',
        period: '1h',
        delayed: true,
        delay_ms: 300000,
        as_of_ms: 1770000000000,
        points: [
          { bucket_start_ms: 1770000000000, suppressed: true },
          {
            bucket_start_ms: 1770003600000,
            accounts: 140,
            long_ratio: '0.62',
            short_ratio: '0.38',
            long_short_ratio: '1.63',
          },
        ],
      },
    },
  },
  'GET /api/v1/market/taker-volume*': { status: 200, body: { data: { points: [] } } },
  'GET /api/v1/market/positioning*': {
    status: 200,
    body: {
      data: {
        symbol: 'EUR/USD',
        delayed: true,
        delay_ms: 300000,
        accounts: 140,
        long_accounts: 90,
        short_accounts: 50,
        long_notional: '1000000',
        short_notional: '700000',
        gross_notional: '1700000',
      },
    },
  },
  'GET /api/v1/market/depth*': {
    status: 200,
    body: {
      symbol: 'EUR/USD',
      seq: 9,
      updated_at_ms: 1770000000000,
      bids: [{ price: '1.0849', quantity: '50000' }],
      asks: [{ price: '1.0851', quantity: '40000' }],
    },
  },
};

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
  signInForTests();
});

describe('AnalyticsPage', () => {
  it('renders 24h stats, performance held-status and delayed long/short with suppressed bucket', async () => {
    installFetchMock(BASE);
    renderApp(<AnalyticsPage />);
    expect(await screen.findByText('1.0850')).toBeInTheDocument();
    expect(await screen.findByText('held')).toBeInTheDocument();
    expect(screen.getByText(/divergent: fill_rate_24h/)).toBeInTheDocument();
    // suppressed cohort bucket renders explicitly, never silently dropped
    expect(await screen.findByText(/suppressed — cohort below floor/)).toBeInTheDocument();
    expect(screen.getByText('1.63')).toBeInTheDocument();
    expect(screen.getAllByText(/delayed/i).length).toBeGreaterThan(0);
  });

  it('L3 snapshot surfaces the 402/403 premium refusal verbatim', async () => {
    installFetchMock({
      ...BASE,
      'GET /api/v1/market-data/l3-snapshot/EUR%2FUSD': {
        status: 403,
        body: {
          type: 'error',
          error: 'TIER_REQUIRED',
          message: 'professional tier required',
          status: 403,
        },
      },
    });
    renderApp(<AnalyticsPage />);
    fireEvent.click(await screen.findByRole('button', { name: /Load L3 order-level snapshot/ }));
    expect((await screen.findAllByText(/professional tier/)).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/TIER_REQUIRED/).length).toBeGreaterThan(0);
  });

  it('renders account P&L rows when signed in', async () => {
    installFetchMock({
      ...BASE,
      'GET /api/v1/analytics/pnl': {
        status: 200,
        body: {
          rows: [
            {
              account_id: 1001,
              symbol: 'EUR/USD',
              day: '2026-10-06',
              realized: '120.50',
              unrealized: '-8.00',
              fees: '3.20',
              net: '109.30',
            },
          ],
        },
      },
    });
    renderApp(<AnalyticsPage />);
    expect(await screen.findByText('109.30')).toBeInTheDocument();
    expect(screen.getByText('My P&L analytics')).toBeInTheDocument();
  });

  it('hides the account P&L card when signed out', async () => {
    resetSessionForTests(); // signed out — /analytics/pnl must not fire
    const calls = installFetchMock(BASE);
    renderApp(<AnalyticsPage />);
    expect(await screen.findByText('1.0850')).toBeInTheDocument();
    expect(screen.queryByText('My P&L analytics')).not.toBeInTheDocument();
    await waitFor(() => {
      expect(calls.every((c) => !c.url.includes('/analytics/pnl'))).toBe(true);
    });
  });
});
