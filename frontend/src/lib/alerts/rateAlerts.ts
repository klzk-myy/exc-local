/**
 * Client-side rate alerts (Phase-10 Task 10.3.17 item: "Create rate
 * alerts (above/below threshold); evaluate client-side against live
 * ticks").
 *
 * IMPORTANT — the venue has no server-side alerting endpoint in the
 * Phase-05 route registry, so alerts are evaluated ONLY while this app
 * is open and subscribed to the symbol's price channel. They are
 * persisted in localStorage per account; a fired alert is marked
 * `triggeredAtMs` (one-shot — re-arm by creating a new alert).
 *
 * Evaluation uses the best-bid/ask when available, falling back to last
 * trade / mark price. `above` fires when the observable price ≥ target;
 * `below` when ≤ target.
 */
import { create } from 'zustand';

export type AlertDirection = 'above' | 'below';

export interface RateAlert {
  id: string;
  symbol: string;
  direction: AlertDirection;
  /** Decimal string at creation; evaluated as number (display-precision
   * thresholds — not order parameters, so float compare is fine here). */
  targetPrice: string;
  createdAtMs: number;
  triggeredAtMs?: number;
  /** The observed price at trigger time (display provenance). */
  triggeredPrice?: string;
  note?: string;
}

/** Symbol → best observable price (bid/ask mid or last). */
export type TickSnapshot = Record<string, number | undefined>;

const KEY_PREFIX = 'exc.ratealerts.v1.';
const keyOf = (accountId: number | null): string => `${KEY_PREFIX}${accountId ?? 'anon'}`;

function load(accountId: number | null): RateAlert[] {
  if (typeof window === 'undefined') return [];
  try {
    const raw = window.localStorage.getItem(keyOf(accountId));
    if (raw === null) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((a): a is RateAlert => {
      if (typeof a !== 'object' || a === null) return false;
      const r = a as Partial<RateAlert>;
      return (
        typeof r.id === 'string' &&
        typeof r.symbol === 'string' &&
        (r.direction === 'above' || r.direction === 'below') &&
        typeof r.targetPrice === 'string' &&
        typeof r.createdAtMs === 'number'
      );
    });
  } catch {
    return [];
  }
}

function save(accountId: number | null, alerts: RateAlert[]): void {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(keyOf(accountId), JSON.stringify(alerts));
  } catch {
    // storage full/blocked — in-memory alerts still evaluate this session
  }
}

interface RateAlertState {
  alerts: Record<string, RateAlert[]>;
  addAlert: (accountId: number | null, alert: RateAlert) => void;
  removeAlert: (accountId: number | null, id: string) => void;
  /** Mark an alert triggered (one-shot). */
  markTriggered: (accountId: number | null, id: string, price: string, nowMs: number) => void;
}

export const useRateAlertStore = create<RateAlertState>()((set, get) => {
  const write = (accountId: number | null, alerts: RateAlert[]) => {
    save(accountId, alerts);
    set((s) => ({ alerts: { ...s.alerts, [String(accountId ?? 'anon')]: alerts } }));
  };
  const listFor = (accountId: number | null): RateAlert[] =>
    get().alerts[String(accountId ?? 'anon')] ?? load(accountId);
  return {
    alerts: {},
    addAlert: (accountId, alert) => {
      write(accountId, [...listFor(accountId), alert]);
    },
    removeAlert: (accountId, id) => {
      write(
        accountId,
        listFor(accountId).filter((a) => a.id !== id),
      );
    },
    markTriggered: (accountId, id, price, nowMs) => {
      write(
        accountId,
        listFor(accountId).map((a) =>
          a.id === id && a.triggeredAtMs === undefined
            ? { ...a, triggeredAtMs: nowMs, triggeredPrice: price }
            : a,
        ),
      );
    },
  };
});

/** Reactive list accessor — hydrates from storage on first use. */
export function useRateAlerts(accountId: number | null): RateAlert[] {
  const key = String(accountId ?? 'anon');
  const list = useRateAlertStore((s) => s.alerts[key]);
  if (list === undefined) {
    const hydrated = load(accountId);
    queueMicrotask(() => {
      useRateAlertStore.setState((s) =>
        s.alerts[key] === undefined ? { alerts: { ...s.alerts, [key]: hydrated } } : s,
      );
    });
    return hydrated;
  }
  return list;
}

/** Pure predicate — has `price` satisfied the alert threshold? */
export function alertSatisfied(alert: RateAlert, price: number): boolean {
  const target = Number(alert.targetPrice);
  if (!Number.isFinite(target) || !Number.isFinite(price)) return false;
  return alert.direction === 'above' ? price >= target : price <= target;
}

/**
 * Evaluate a batch of alerts against the latest ticks. Returns the ids
 * that fired (not yet triggered + satisfied). Pure — the caller marks
 * them via `markTriggered` so the fired set is testable without a store.
 */
export function evaluateAlerts(
  alerts: readonly RateAlert[],
  ticks: TickSnapshot,
): { alert: RateAlert; price: number }[] {
  const fired: { alert: RateAlert; price: number }[] = [];
  for (const alert of alerts) {
    if (alert.triggeredAtMs !== undefined) continue;
    const price = ticks[alert.symbol];
    if (price === undefined) continue;
    if (alertSatisfied(alert, price)) fired.push({ alert, price });
  }
  return fired;
}
