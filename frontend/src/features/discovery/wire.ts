/**
 * Discovery-local frame parsers — bbo@/ticker@ event payloads
 * (services/internal/marketdata/bbo.go + ticker.go). Prices arrive as
 * decimal strings; alerts/discovery only need a display-precision number
 * (never fed back into order parameters).
 */

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const decNum = (v: unknown): number | undefined => {
  const s =
    typeof v === 'string' ? v : typeof v === 'number' && Number.isFinite(v) ? String(v) : undefined;
  if (s === undefined) return undefined;
  const n = Number(s);
  return Number.isFinite(n) ? n : undefined;
};

/** Mid-price from a `bbo@{symbol}` frame, or null when the frame lacks
 * a usable two-sided quote (halted book — never fabricate one). */
export function bboMid(frame: unknown): number | null {
  if (!isRecord(frame)) return null;
  const bid = decNum(frame['bid']);
  const ask = decNum(frame['ask']);
  if (bid === undefined || ask === undefined || bid <= 0 || ask <= 0) return null;
  return (bid + ask) / 2;
}

/** Last/close price from a `ticker@{symbol}` 24h frame. */
export function tickerClose(frame: unknown): number | null {
  if (!isRecord(frame)) return null;
  const close = decNum(frame['close']);
  return close !== undefined && close > 0 ? close : null;
}

/** Best observable price from any price-bearing frame (bbo mid → ticker
 * close → trades price). */
export function observablePrice(frame: unknown): number | null {
  return (
    bboMid(frame) ??
    tickerClose(frame) ??
    (isRecord(frame) ? (decNum(frame['price']) ?? null) : null)
  );
}
