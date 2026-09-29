/**
 * Smart defaults service (Task 10.3.29 item 4).
 *
 * Context-aware, NON-BINDING pre-fills — the user always sees and can
 * override them. Pure functions; consumers recompute when the market
 * context changes (symbol switch, mark move >0.1%).
 */
import { addDecimal, divDecimal, mulDecimal, roundToStep } from './decimal';
import type { InstrumentMeta } from './instruments';

/** BBO top-of-book quote (bbo@ channel payload shape). */
export interface BboQuote {
  bid?: string | null;
  ask?: string | null;
}

/** Limit-order default price: current BBO bid (SELL) / ask (BUY),
 * tick-rounded per the instrument. Null without a usable quote. */
export function defaultLimitPrice(
  meta: InstrumentMeta | undefined,
  side: 'BUY' | 'SELL',
  bbo: BboQuote | null | undefined,
): string | null {
  const raw = side === 'BUY' ? bbo?.ask : bbo?.bid;
  if (raw === null || raw === undefined || raw === '') return null;
  if (meta === undefined) return raw;
  return roundToStep(raw, meta.tickSize, 'nearest') ?? raw;
}

/** Market-order default quantity: max affordable from free margin at
 * mark × leverage, lot-rounded DOWN (never overspends). */
export function defaultMarketQty(
  meta: InstrumentMeta | undefined,
  freeMargin: string,
  markPrice: string,
  leverage: string,
): string | null {
  const notional = mulDecimal(freeMargin, leverage);
  if (notional === null) return null;
  const qty = divDecimal(notional, markPrice, 12, 'down');
  if (qty === null) return null;
  if (meta === undefined) return qty;
  return roundToStep(qty, meta.lotSize, 'down') ?? qty;
}

/** Stop-loss default distance: the instrument's typical daily range —
 * the 20-day ATR supplied by Phase-23 analytics (parameter so the
 * function stays pure; callers pass null → no default rather than a
 * fabricated distance). */
export function defaultStopDistance(meta: InstrumentMeta, atr20: string | null): string | null {
  if (atr20 === null || atr20 === '') return null;
  return roundToStep(atr20, meta.tickSize, 'nearest') ?? atr20;
}

/** Stop-loss default price: entry ∓ ATR distance (BUY stops below). */
export function defaultStopPrice(
  meta: InstrumentMeta,
  side: 'BUY' | 'SELL',
  entryPrice: string,
  atr20: string | null,
): string | null {
  const dist = defaultStopDistance(meta, atr20);
  if (dist === null) return null;
  const signed = side === 'BUY' ? `-${dist}` : dist;
  const p = addDecimal(entryPrice, signed);
  if (p === null) return null;
  return roundToStep(p, meta.tickSize, 'nearest') ?? p;
}

/** Beneficiary default: last-used beneficiary per currency (callers
 * persist the choice in workspace prefs/local state). */
export function defaultBeneficiary<T extends { currency: string }>(
  beneficiaries: readonly T[],
  currency: string,
  lastUsedId: string | null,
): T | undefined {
  const forCurrency = beneficiaries.filter((b) => b.currency === currency);
  if (forCurrency.length === 0) return undefined;
  return forCurrency.find((b) => (b as { id?: string }).id === lastUsedId) ?? forCurrency[0];
}

/** Transfer defaults: `from` = currently selected sub-account; `to` =
 * master when the context is a sub-account ('' when already on master —
 * the caller picks a sub-account explicitly, never a hidden default). */
export function defaultTransferAccounts(
  currentAccountId: string,
  masterAccountId: string,
): { from: string; to: string } {
  return {
    from: currentAccountId,
    to: currentAccountId === masterAccountId ? '' : masterAccountId,
  };
}
