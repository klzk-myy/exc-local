/**
 * Calculator tests (Task 10.3.8) — pure math plus the page surface:
 * live-mark defaulting, stale badge, JPY pip convention, leverage cap.
 */
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { installFetchMock, renderApp } from '@/test/accountMocks';
import { dec } from '@/lib/decimal/decimal';
import { connectWs, makeWsHarness, pushEvent, ackSub } from '@/lib/trading/testkit';
import { useMarketStore } from '@/lib/trading/marketStore';

import { computePositionCalc, effectiveLeverage } from './calc';
import CalculatorPage from './CalculatorPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const INSTRUMENTS = {
  data: [
    {
      symbol: 'EUR/USD',
      base_currency: 'EUR',
      quote_currency: 'USD',
      tick_size: '0.00001',
      lot_size: '1000',
      min_order_qty: '1000',
      max_leverage: 30,
      status: 'ACTIVE',
    },
    {
      symbol: 'USD/JPY',
      base_currency: 'USD',
      quote_currency: 'JPY',
      tick_size: '0.001',
      lot_size: '1000',
      min_order_qty: '1000',
      max_leverage: 30,
      status: 'ACTIVE',
    },
  ],
};

describe('computePositionCalc', () => {
  it('computes notional, margin, pip value, liquidation estimate', () => {
    const r = computePositionCalc({
      symbol: 'EUR/USD',
      side: 'LONG',
      quantity: dec('100000'),
      leverage: dec('30'),
      entryPrice: dec('1.10'),
      maintenanceBps: dec('50'),
    });
    expect(r).toBeDefined();
    expect(r?.notional.toString()).toBe('110000');
    expect(r?.marginRequired.toFixed(2)).toBe('3666.67');
    expect(r?.pipSize.toString()).toBe('0.0001');
    expect(r?.pipValueQuote.toString()).toBe('10'); // $10/pip per standard lot
    expect(r?.liquidation).toBeDefined();
    expect(r?.liquidation?.lt(dec('1.10'))).toBe(true); // long liq below entry
  });

  it('honors the JPY pip convention', () => {
    const r = computePositionCalc({
      symbol: 'USD/JPY',
      side: 'SHORT',
      quantity: dec('100000'),
      leverage: dec('20'),
      entryPrice: dec('150'),
      maintenanceBps: dec('50'),
    });
    expect(r?.pipSize.toString()).toBe('0.01');
    expect(r?.pipValueQuote.toString()).toBe('1000'); // ¥1000/pip
    expect(r?.liquidation?.gt(dec('150'))).toBe(true); // short liq above entry
  });

  it('returns undefined until inputs parse positive — never zeros', () => {
    expect(
      computePositionCalc({
        symbol: 'EUR/USD',
        side: 'LONG',
        quantity: undefined,
        leverage: dec('30'),
        entryPrice: dec('1.1'),
        maintenanceBps: dec('50'),
      }),
    ).toBeUndefined();
    expect(
      computePositionCalc({
        symbol: 'EUR/USD',
        side: 'LONG',
        quantity: dec('0'),
        leverage: dec('30'),
        entryPrice: dec('1.1'),
        maintenanceBps: dec('50'),
      }),
    ).toBeUndefined();
  });
});

describe('effectiveLeverage', () => {
  const inst = { maxLeverage: 30 } as Parameters<typeof effectiveLeverage>[1];
  it('clamps to the instrument cap', () => {
    expect(effectiveLeverage(dec('100'), inst)?.toString()).toBe('30');
    expect(effectiveLeverage(dec('10'), inst)?.toString()).toBe('10');
  });
});

describe('CalculatorPage', () => {
  function setupFetch() {
    installFetchMock({ 'GET /api/v1/instruments': { body: INSTRUMENTS } });
  }

  beforeEach(() => {
    // marketStore is module-global — a BBO pushed by an earlier test would
    // otherwise leak a live mid into the next render.
    useMarketStore.setState({ bbo: {}, books: {} });
  });

  it('computes outputs from typed inputs with the live mark badge', async () => {
    const h = makeWsHarness();
    setupFetch();
    renderApp(<CalculatorPage client={h.client} />);
    const sock = await connectWs(h);
    ackSub(sock, 'bbo@EUR/USD');
    pushEvent(sock, 'bbo@EUR/USD', 1, {
      symbol: 'EUR/USD',
      bid: '1.0999',
      ask: '1.1001',
      ts_ms: 10,
    });

    // live mark badge + notional for the default 100,000 qty at 30:1
    expect(await screen.findByText('live mark')).toBeInTheDocument();
    expect(await screen.findByText(/110,000(\.00)? USD/)).toBeInTheDocument();
    expect(screen.getByText(/3,666\.67 USD/)).toBeInTheDocument();
    expect(screen.getByText(/10\.00 USD \/ pip/)).toBeInTheDocument();
  });

  it('shows "mark unavailable" and still computes from a manual entry', async () => {
    const h = makeWsHarness();
    setupFetch();
    renderApp(<CalculatorPage client={h.client} />);
    // DISCONNECTED stub → no bbo data
    expect(await screen.findByText('mark unavailable')).toBeInTheDocument();
    const entry = screen.getByLabelText('Entry price');
    await userEvent.type(entry, '1.2000');
    expect(await screen.findByText(/120,000(\.00)? USD/)).toBeInTheDocument();
    expect(screen.getByText(/manual entry 1\.20000/)).toBeInTheDocument();
  });

  it('labels the JPY pair pip convention', async () => {
    const h = makeWsHarness();
    setupFetch();
    renderApp(<CalculatorPage client={h.client} />);
    // wait for the instruments query to populate the options
    await screen.findByRole('option', { name: 'USD/JPY' });
    await userEvent.selectOptions(screen.getByLabelText('Pair'), 'USD/JPY');
    await userEvent.type(screen.getByLabelText('Entry price'), '150');
    expect(await screen.findByText(/Pip size \(JPY pair\)/)).toBeInTheDocument();
    expect(screen.getByText('0.01')).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// Task-8 supplemental calculators — P&L + swap/rollover.
// ---------------------------------------------------------------------------

describe('computePnL', () => {
  it('computes signed pips + quote P&L', async () => {
    const { computePnL } = await import('./calc');
    // LONG 1000 EUR/USD 1.1000 → 1.1050: +50 pips, +50 USD.
    const r = computePnL('EUR/USD', 'LONG', dec('1000'), dec('1.1000'), dec('1.1050'));
    expect(r?.pnlQuote.toDisplay(2)).toBe('5.00');
    expect(r?.pips.toDisplay(0)).toBe('50'); // 0.0050 ÷ pip 0.0001
    // SHORT flips the sign.
    const s = computePnL('EUR/USD', 'SHORT', dec('1000'), dec('1.1000'), dec('1.1050'));
    expect(s?.pnlQuote.toDisplay(2)).toBe('-5.00');
  });
});

describe('computeSwap', () => {
  it('applies qty × points × days with the markup leg separate', async () => {
    const { computeSwap } = await import('./calc');
    // 1000 base units × 0.5 pts × 3 days = 1500 quote; 10bps markup = 1.50.
    const r = computeSwap('EUR/USD', 'LONG', dec('1000'), dec('0.5'), dec('10'), 3);
    expect(r?.interbankQuote.toDisplay(2)).toBe('1,500.00');
    expect(r?.markupQuote.toDisplay(2)).toBe('-1.50');
    expect(r?.totalQuote.toDisplay(2)).toBe('1,498.50');
  });

  it('refuses days < 1', async () => {
    const { computeSwap } = await import('./calc');
    expect(computeSwap('EUR/USD', 'LONG', dec('1'), dec('0.5'), dec('0'), 0)).toBeUndefined();
  });
});

describe('SwapSection', () => {
  it('renders the published sheet and projects a charge', async () => {
    installFetchMock({
      'GET /api/v1/instruments': { body: INSTRUMENTS },
      'GET /api/v1/instruments/EUR%2FUSD/swap-rates': {
        body: {
          data: [
            {
              effective_date: '2026-01-02',
              long_points: '0.5',
              short_points: '-0.7',
              long_markup_bps: '10',
              short_markup_bps: '10',
              triple: false,
            },
          ],
        },
      },
    });
    const harness = makeWsHarness();
    try {
      renderApp(<CalculatorPage client={harness.client} />);
      expect(await screen.findByText('Overnight financing')).toBeInTheDocument();
      await screen.findByText(/Sheet 2026-01-02/);
      // 1000 × 0.5 × 1d = 500 − 0.50 markup = 499.50
      expect(await screen.findByTestId('swap-result')).toHaveTextContent('499.50');
    } finally {
      harness.client.stop();
    }
  });
});
