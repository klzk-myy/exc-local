import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import HistoryExplorerPage from './HistoryExplorerPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
});

describe('HistoryExplorerPage', () => {
  it('renders the trades tape with access-tier and delayed badges, then pages via cursor', async () => {
    const calls = installFetchMock({
      'GET /api/v1/history/trades/*': {
        handler: (url) => {
          const hasCursor = url.includes('cursor=');
          return {
            status: 200,
            body: {
              data: hasCursor
                ? [
                    {
                      trade_id: 1,
                      price: '1.0810',
                      quantity: '5000',
                      side: 'SELL',
                      time: '2026-10-01T09:00:00Z',
                    },
                  ]
                : [
                    {
                      trade_id: 2,
                      price: '1.0812',
                      quantity: '10000',
                      side: 'BUY',
                      time: '2026-10-01T10:00:00Z',
                    },
                  ],
              next_cursor: hasCursor ? null : 'cur-2',
              access_tier: 'free',
              delayed: true,
              total: 2,
            },
          };
        },
      },
    });
    renderApp(<HistoryExplorerPage />);
    expect(await screen.findByText('1.0812')).toBeInTheDocument();
    expect(screen.getByText('tier free')).toBeInTheDocument();
    expect(screen.getByText('delayed tape')).toBeInTheDocument();
    expect(screen.getByText('total 2')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /Load more/ }));
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('cursor=cur-2'))).toBe(true);
    });
    expect(await screen.findByText('1.0810')).toBeInTheDocument();
  });

  it('klines sends the persisted interval label', async () => {
    const calls = installFetchMock({
      'GET /api/v1/history/klines/*': {
        status: 200,
        body: {
          data: [
            {
              open_time_ms: 1759305600000,
              open: '1.08',
              high: '1.09',
              low: '1.07',
              close: '1.085',
              volume: '1200',
              trade_count: 34,
              closed: true,
            },
          ],
          access_tier: 'free',
        },
      },
    });
    renderApp(<HistoryExplorerPage />);
    await userEvent.selectOptions(screen.getByLabelText('Dataset'), 'klines');
    await userEvent.selectOptions(screen.getByLabelText('Interval'), '4h');
    fireEvent.click(screen.getByRole('button', { name: 'Load' }));
    await waitFor(() => {
      expect(
        calls.some((c) => c.url.includes('/history/klines/') && c.url.includes('interval=4h')),
      ).toBe(true);
    });
    expect(await screen.findByText('1.085')).toBeInTheDocument();
  });

  it('swap-rates renders the triple-roll badge and needs no symbol', async () => {
    installFetchMock({
      'GET /api/v1/history/swap-rates': {
        status: 200,
        body: {
          data: [
            {
              symbol: 'EUR/USD',
              effective_date: '2026-10-01',
              long_points: '-1.2',
              short_points: '0.8',
              days_applied: 3,
              triple: true,
              accrual_count: 3,
              source: 'ECB',
            },
          ],
          access_tier: 'premium',
        },
      },
    });
    renderApp(<HistoryExplorerPage />);
    await userEvent.selectOptions(screen.getByLabelText('Dataset'), 'swap-rates');
    expect(await screen.findByText('2026-10-01')).toBeInTheDocument();
    expect(screen.getByText('×3')).toBeInTheDocument();
    expect(screen.getByText('tier premium')).toBeInTheDocument();
  });

  it('block-trades render bust/correction lineage', async () => {
    installFetchMock({
      'GET /api/v1/history/block-trades/*': {
        status: 200,
        body: {
          data: [
            {
              entry_id: 9,
              kind: 'CORRECTION',
              block_trade_id: 55,
              price: '1.09',
              quantity: '5000000',
              notional_usd: '5450000',
              pub_ts: '2026-10-01T10:00:00Z',
              exec_ts: '2026-10-01T09:58:00Z',
              delay_ms: 900000,
              supersedes: 54,
            },
          ],
          access_tier: 'free',
        },
      },
    });
    renderApp(<HistoryExplorerPage />);
    await userEvent.selectOptions(screen.getByLabelText('Dataset'), 'block-trades');
    expect(await screen.findByText('CORRECTION')).toBeInTheDocument();
    expect(screen.getByText(/supersedes #54/)).toBeInTheDocument();
    expect(screen.getByText('900000ms')).toBeInTheDocument();
  });

  it('queues an async export and surfaces the job id', async () => {
    signInForTests();
    const calls = installFetchMock({
      'GET /api/v1/history/trades/*': {
        handler: (url) =>
          url.includes('/export')
            ? { status: 202, body: { job: { id: 77, kind: 'trades', status: 'QUEUED' } } }
            : { status: 200, body: { data: [], access_tier: 'free' } },
      },
    });
    renderApp(<HistoryExplorerPage />);
    await screen.findByRole('button', { name: 'Export CSV' });
    fireEvent.click(screen.getByRole('button', { name: 'Export CSV' }));
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('/export') && c.url.includes('async=1'))).toBe(true);
    });
    expect(await screen.findByText(/Export job #77 queued/)).toBeInTheDocument();
    expect(calls.find((c) => c.url.includes('/export'))?.url).toContain('format=csv');
  });
});
