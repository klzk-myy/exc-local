import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import { parseAnnouncement, parseFeeSchedule, parseSystemStatus } from './api';
import { AnnouncementsPanel, FeeSchedulePanel, SolvencyPanel, SystemInfoPanel } from './panels';
import { ExportJobsPanel } from './ExportJobsPanel';
import { TcaPanel } from './TcaPanel';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const STUB_501 = {
  status: 501,
  body: { type: 'error', error: 'NOT_IMPLEMENTED', message: 'stub', status: 501 },
};

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
  signInForTests();
});

describe('wire narrowing', () => {
  it('parseFeeSchedule reads the AccountFees payload', () => {
    const f = parseFeeSchedule({
      account_id: 42,
      tier_id: 3,
      tier_name: 'Standard',
      maker_bps: '2.0',
      taker_bps: '4.0',
      effective_maker_bps: '1.5',
      effective_taker_bps: '4.0',
      promo_active: true,
      promo: { until: '2026-10-01T00:00:00Z', maker_bps: '1.5' },
    });
    expect(f.tierName).toBe('Standard');
    expect(f.effectiveMakerBps).toBe('1.5');
    expect(f.promoActive).toBe(true);
  });
  it('parseAnnouncement / parseSystemStatus narrow correctly', () => {
    expect(parseAnnouncement({ id: 7, title: 'T', body: 'B', category: 'INCIDENT' })).toMatchObject(
      {
        id: '7',
        title: 'T',
      },
    );
    expect(parseAnnouncement({ body: 'x' })).toBeNull();
    const s = parseSystemStatus({
      status: 'operational',
      mode: 'Normal',
      components: [{ name: 'engine', state: 'operational', critical: true, latency_ms: 0.4 }],
      source: 'aggregator',
    });
    expect(s.components[0]?.name).toBe('engine');
    expect(s.mode).toBe('Normal');
  });
});

describe('FeeSchedulePanel', () => {
  it('renders the live fee payload', async () => {
    installFetchMock({
      'GET /api/v1/fees': {
        status: 200,
        body: {
          tier_name: 'Standard',
          maker_bps: '2',
          taker_bps: '4',
          effective_maker_bps: '2',
          effective_taker_bps: '4',
          promo_active: false,
        },
      },
    });
    renderApp(<FeeSchedulePanel />);
    await waitFor(() => {
      expect(screen.getByText(/Standard/)).toBeInTheDocument();
    });
    expect(screen.getAllByText('2').length).toBeGreaterThan(0);
  });
});

describe('SolvencyPanel', () => {
  it('renders the honest unavailable state on 501 — no fabricated roots', async () => {
    installFetchMock({ 'GET /api/v1/public/proof-of-reserves/daily-root': STUB_501 });
    renderApp(<SolvencyPanel />);
    await waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent('Proof of reserves');
    });
    expect(screen.queryByText(/Merkle/)).toBeInTheDocument(); // only inside the unavailable note
  });
});

describe('SystemInfoPanel', () => {
  it('renders component health from the live endpoint', async () => {
    installFetchMock({
      'GET /api/v1/system/status': {
        status: 200,
        body: {
          status: 'operational',
          mode: 'Normal',
          components: [{ name: 'gateway', state: 'operational', critical: true, latency_ms: 1.2 }],
          source: 'aggregator',
          updated_at: '2026-09-28T00:00:00Z',
        },
      },
    });
    renderApp(<SystemInfoPanel />);
    await waitFor(() => {
      expect(screen.getByText(/gateway/)).toBeInTheDocument();
    });
    expect(screen.getAllByText('operational').length).toBeGreaterThan(0);
  });
});

describe('AnnouncementsPanel', () => {
  it('renders, grades by category, and dismiss persists to storage', async () => {
    installFetchMock({
      'GET /api/v1/announcements': {
        status: 200,
        body: {
          data: [
            {
              id: 1,
              title: 'Maint window',
              body: 'Sun 21:00 UTC',
              category: 'MAINTENANCE',
              status: 'PUBLISHED',
            },
            {
              id: 2,
              title: 'Incident',
              body: 'Degraded fills',
              category: 'INCIDENT',
              status: 'PUBLISHED',
            },
          ],
        },
      },
    });
    renderApp(<AnnouncementsPanel />);
    await waitFor(() => {
      expect(screen.getByText('Maint window')).toBeInTheDocument();
    });
    expect(screen.getByText('INCIDENT')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /Dismiss announcement Maint window/ }));
    expect(screen.queryByText('Maint window')).not.toBeInTheDocument();
    expect(window.localStorage.getItem('exc.dismissed-announcements.v1')).toContain('"1"');
  });
});

describe('ExportJobsPanel', () => {
  it('renders owner jobs with status/expiry and a live download link', async () => {
    const calls = installFetchMock({
      'GET /api/v1/export-jobs': {
        status: 200,
        body: {
          data: [
            {
              id: 5,
              kind: 'trades',
              symbol: 'EUR/USD',
              format: 'csv',
              status: 'COMPLETED',
              row_count: 4200,
              expires_at: new Date(Date.now() + 3600_000).toISOString(),
              download_url: '/api/v1/export-jobs/5/download',
            },
            {
              id: 6,
              kind: 'klines',
              symbol: 'USD/JPY',
              format: 'parquet',
              status: 'QUEUED',
            },
          ],
        },
      },
      'GET /api/v1/export-jobs/6': {
        status: 200,
        body: { data: { id: 6, kind: 'klines', status: 'RUNNING' } },
      },
    });
    renderApp(<ExportJobsPanel />);
    expect(await screen.findByText('#5')).toBeInTheDocument();
    expect(screen.getByText('COMPLETED')).toBeInTheDocument();
    expect(screen.getByText('QUEUED')).toBeInTheDocument();
    expect(screen.getByText(/expires in/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Download' })).toBeInTheDocument();
    // QUEUED row exposes per-job refresh — GET /export-jobs/{id}.
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'GET' && c.url.includes('/export-jobs/6'))).toBe(true);
    });
  });
});

describe('TcaPanel', () => {
  it('fetches the claims-account report and renders buckets', async () => {
    const calls = installFetchMock({
      'GET /api/v1/reports/tca/1001': {
        status: 200,
        body: {
          account_id: 1001,
          period: 'daily',
          buckets: [
            {
              bucket_start: '2026-10-01T00:00:00Z',
              symbol: 'EUR/USD',
              fills: 42,
              avg_slip_arrival_bps: '0.4',
              avg_slip_vwap_bps: '-0.1',
              avg_price_improvement_delta: '0.02',
            },
          ],
        },
      },
    });
    renderApp(<TcaPanel />);
    expect(await screen.findByText('EUR/USD')).toBeInTheDocument();
    expect(screen.getByText('42')).toBeInTheDocument();
    expect(screen.getByText('0.4')).toBeInTheDocument();
    expect(calls.some((c) => c.url.includes('period=daily'))).toBe(true);
    await userEvent.selectOptions(screen.getByLabelText('Instrument class'), 'SPOT');
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('instrument_class=SPOT'))).toBe(true);
    });
  });
});
