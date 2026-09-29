import { beforeEach, describe, expect, it } from 'vitest';

import { alertSatisfied, evaluateAlerts, useRateAlertStore, type RateAlert } from './rateAlerts';
import { useWatchlistStore } from './watchlist';

function alert(over: Partial<RateAlert> = {}): RateAlert {
  return {
    id: 'a1',
    symbol: 'EUR/USD',
    direction: 'above',
    targetPrice: '1.1000',
    createdAtMs: 1000,
    ...over,
  };
}

beforeEach(() => {
  window.localStorage.clear();
  useRateAlertStore.setState({ alerts: {} });
  useWatchlistStore.setState({ lists: {} });
});

describe('watchlist store', () => {
  it('adds, dedupes, removes, and persists per account', () => {
    const s = useWatchlistStore.getState();
    s.add(7, 'EUR/USD');
    s.add(7, 'EUR/USD');
    s.add(7, 'USD/JPY');
    expect(useWatchlistStore.getState().lists['7']).toEqual(['EUR/USD', 'USD/JPY']);
    // namespaced under a per-account key
    expect(window.localStorage.getItem('exc.watchlist.v1.7')).toContain('USD/JPY');
    s.remove(7, 'EUR/USD');
    expect(useWatchlistStore.getState().lists['7']).toEqual(['USD/JPY']);
  });

  it('separates lists per account and anon fallback', () => {
    const s = useWatchlistStore.getState();
    s.add(1, 'EUR/USD');
    s.add(2, 'USD/JPY');
    s.add(null, 'GBP/USD');
    expect(useWatchlistStore.getState().lists['1']).toEqual(['EUR/USD']);
    expect(useWatchlistStore.getState().lists['2']).toEqual(['USD/JPY']);
    expect(useWatchlistStore.getState().lists['anon']).toEqual(['GBP/USD']);
  });

  it('toggle adds when absent and removes when present', () => {
    const s = useWatchlistStore.getState();
    s.toggle(1, 'EUR/USD');
    s.toggle(1, 'EUR/USD');
    expect(useWatchlistStore.getState().lists['1']).toEqual([]);
  });
});

describe('rate-alert evaluation', () => {
  it('fires above/below thresholds on the observed price', () => {
    expect(alertSatisfied(alert({ direction: 'above', targetPrice: '1.10' }), 1.1)).toBe(true);
    expect(alertSatisfied(alert({ direction: 'above', targetPrice: '1.10' }), 1.0999)).toBe(false);
    expect(alertSatisfied(alert({ direction: 'below', targetPrice: '1.10' }), 1.1)).toBe(true);
    expect(alertSatisfied(alert({ direction: 'below', targetPrice: '1.10' }), 1.1001)).toBe(false);
  });

  it('skips already-triggered and unpriced alerts', () => {
    const fired = evaluateAlerts(
      [
        alert({ id: 'open' }),
        alert({ id: 'done', triggeredAtMs: 2000 }),
        alert({ id: 'nopx', symbol: 'GBP/USD' }),
      ],
      { 'EUR/USD': 1.2 },
    );
    expect(fired.map((f) => f.alert.id)).toEqual(['open']);
  });

  it('markTriggered makes an alert one-shot and persists it', () => {
    const s = useRateAlertStore.getState();
    s.addAlert(9, alert({ id: 'a1' }));
    s.markTriggered(9, 'a1', '1.1001', 5000);
    const list = useRateAlertStore.getState().alerts['9'];
    expect(list?.[0]?.triggeredAtMs).toBe(5000);
    expect(list?.[0]?.triggeredPrice).toBe('1.1001');
    expect(evaluateAlerts(list ?? [], { 'EUR/USD': 1.5 }).map((f) => f.alert.id)).toEqual([]);
    expect(window.localStorage.getItem('exc.ratealerts.v1.9')).toContain('1.1001');
  });
});
