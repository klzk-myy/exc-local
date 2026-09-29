/**
 * Percentage sizing math (Task 10.3.11) — pure Dec functions so the
 * slider component stays declarative and every clamp has a unit test.
 *
 *   raw qty = freeMargin × pct% × leverage ÷ price
 *
 * then lot-quantized and clamped against the instrument filters
 * (min/max qty, min notional). Every clamp reports a human reason —
 * the slider surfaces it ("clamped: below min notional") rather than
 * silently shrinking the ticket.
 */
import { Dec, dec } from '@/lib/decimal/decimal';
import type { QtyConstraints } from '@/lib/trading/fx';

export const SIZE_PRESETS = [10, 25, 50, 75, 100] as const;

export interface SizingResult {
  /** Final quantity after lot quantization + filter clamps. */
  qty: Dec;
  /** Unclamped margin-implied size (before filters). */
  rawQty: Dec;
  /** Margin the final qty consumes: qty × price ÷ leverage. */
  marginUsed: Dec;
  /** Notional in quote currency: qty × price. */
  notional: Dec;
  /** Why the value differs from rawQty — surfaced next to the field. */
  clampReason: string | null;
  /** True when inputs were insufficient to size at all. */
  unavailable: boolean;
}

export function sizeByPercent(
  pct: number,
  freeMargin: Dec,
  leverage: Dec,
  price: Dec | undefined,
  c: QtyConstraints,
): SizingResult {
  const unavailableResult: SizingResult = {
    qty: Dec.ZERO,
    rawQty: Dec.ZERO,
    marginUsed: Dec.ZERO,
    notional: Dec.ZERO,
    clampReason: null,
    unavailable: true,
  };
  if (
    pct <= 0 ||
    !price ||
    !price.isPositive() ||
    !leverage.isPositive() ||
    !freeMargin.isPositive()
  ) {
    return unavailableResult;
  }
  const raw = freeMargin.mul(dec(pct)).div(dec(100)).mul(leverage).div(price);
  let qty = raw.quantizeTo(c.step, 'down');
  let reason: string | null = null;

  if (c.maxQty.isPositive() && qty.gt(c.maxQty)) {
    qty = c.maxQty;
    reason = 'capped at max order qty';
  }
  const notional = () => qty.mul(price);
  if (c.minQty.isPositive() && qty.lt(c.minQty)) {
    qty = Dec.ZERO;
    reason = 'below minimum order qty';
  } else if (c.minNotional.isPositive() && notional().lt(c.minNotional)) {
    qty = Dec.ZERO;
    reason = 'below minimum notional';
  }
  if (qty.isZero() && reason === null && !raw.isZero()) reason = 'rounds below lot size';

  return {
    qty,
    rawQty: raw,
    marginUsed: qty.isZero() ? Dec.ZERO : qty.mul(price).div(leverage),
    notional: qty.mul(price),
    clampReason: reason,
    unavailable: false,
  };
}
