/**
 * Order-entry draft validation (Task 10.3.3 item 2) — pure functions.
 *
 * Client-side checks mirror the server filters (spec §22.1) so obvious
 * rejects never reach the wire; the server remains authoritative:
 *   qty    > 0, ≥ min_order_qty, ≤ max_order_qty, multiple of lot_size
 *   price  required for LIMIT, > 0, multiple of tick_size,
 *          within min/max_price when the instrument declares them
 *   notional = qty × price ≥ min_notional (LIMIT only — MARKET has no
 *          price to multiply)
 *   GTD    requires a future expiry (§22.2 gtd_expiry, RFC3339)
 *   IOC/FOK are incompatible with GTD/DAY semantics server-side; the
 *          TIF selector itself constrains this (GTD expiry only shows
 *          for GTD).
 *
 * NOTE: Task 10.3.29 plans a shared `useInputHelper(route)` framework —
 * this module is the local implementation until that task lands; keep
 * rules pure so the swap is mechanical.
 */
import { Dec, decOrZero } from '@/lib/market/decimal';
import { parseInputDecimal } from '@/lib/market/format';
import type { Instrument, SubmitOrderRequest } from '@/lib/market/wire';
import type { OrderSide, TimeInForce } from '@/lib/market/wire';

export interface OrderDraft {
  symbol: string;
  side: OrderSide;
  type: 'LIMIT' | 'MARKET';
  /** Raw user input — parsed inside validate. */
  quantity: string;
  price: string;
  timeInForce: TimeInForce;
  /** `datetime-local` raw value — only meaningful when tif === 'GTD'. */
  gtdExpiry: string;
}

export type DraftField = 'quantity' | 'price' | 'gtdExpiry' | 'notional';
export type DraftErrors = Partial<Record<DraftField, string>>;

export const TIME_IN_FORCE_OPTIONS: readonly { value: TimeInForce; label: string }[] = [
  { value: 'GTC', label: 'GTC — Good-til-cancelled' },
  { value: 'DAY', label: 'DAY — expires at session close' },
  { value: 'GTD', label: 'GTD — good-til-date' },
  { value: 'IOC', label: 'IOC — immediate-or-cancel' },
  { value: 'FOK', label: 'FOK — fill-or-kill' },
];

export function validateDraft(d: OrderDraft, inst?: Instrument): DraftErrors {
  const errors: DraftErrors = {};

  const qty = parseInputDecimal(d.quantity);
  if (qty === null) {
    errors.quantity = 'Quantity is required';
  } else if (!qty.isPositive()) {
    errors.quantity = 'Quantity must be greater than 0';
  } else if (inst) {
    const minQty = decOrZero(inst.minOrderQty);
    const maxQty = decOrZero(inst.maxOrderQty);
    const lot = decOrZero(inst.lotSize);
    if (minQty.isPositive() && qty.lt(minQty)) {
      errors.quantity = `Minimum quantity is ${inst.minOrderQty}`;
    } else if (maxQty.isPositive() && qty.gt(maxQty)) {
      errors.quantity = `Maximum quantity is ${inst.maxOrderQty}`;
    } else if (lot.isPositive() && !qty.isMultipleOf(lot)) {
      errors.quantity = `Quantity must be a multiple of lot size ${inst.lotSize}`;
    }
  }

  if (d.type === 'LIMIT') {
    const price = parseInputDecimal(d.price);
    if (price === null) {
      errors.price = 'Price is required for limit orders';
    } else if (!price.isPositive()) {
      errors.price = 'Price must be greater than 0';
    } else if (inst) {
      const tick = decOrZero(inst.tickSize);
      const minP = decOrZero(inst.minPrice);
      const maxP = decOrZero(inst.maxPrice);
      if (tick.isPositive() && !price.isMultipleOf(tick)) {
        errors.price = `Price must be a multiple of tick size ${inst.tickSize}`;
      } else if (minP.isPositive() && price.lt(minP)) {
        errors.price = `Price below instrument minimum ${inst.minPrice}`;
      } else if (maxP.isPositive() && price.gt(maxP)) {
        errors.price = `Price above instrument maximum ${inst.maxPrice}`;
      }
      if (qty !== null && qty.isPositive() && errors.quantity === undefined) {
        const minNotional = decOrZero(inst.minNotional);
        if (minNotional.isPositive() && qty.mul(price).lt(minNotional)) {
          errors.notional = `Order value below minimum notional ${inst.minNotional} ${inst.quoteCurrency}`;
        }
      }
    }
  }

  if (d.timeInForce === 'GTD') {
    if (d.gtdExpiry.trim() === '') {
      errors.gtdExpiry = 'GTD orders require an expiry';
    } else {
      const t = new Date(d.gtdExpiry);
      if (Number.isNaN(t.getTime())) {
        errors.gtdExpiry = 'Expiry is not a valid date/time';
      } else if (t.getTime() <= Date.now()) {
        errors.gtdExpiry = 'Expiry must be in the future';
      }
    }
  }

  return errors;
}

export function hasErrors(e: DraftErrors): boolean {
  return Object.values(e).length > 0;
}

/** Draft → POST /api/v1/orders body (orders.ParseSubmit shape, §22.1).
 * Decimals are canonicalized (commas/whitespace stripped, trailing zeros
 * trimmed) so the wire never carries locale formatting. */
export function buildRequest(d: OrderDraft, clientOrderId: string): SubmitOrderRequest {
  const qty = parseInputDecimal(d.quantity) ?? Dec.ZERO;
  const req: SubmitOrderRequest = {
    symbol: d.symbol,
    side: d.side,
    type: d.type,
    time_in_force: d.timeInForce,
    client_order_id: clientOrderId,
    quantity: qty.toString(),
  };
  if (d.type === 'LIMIT') {
    const price = parseInputDecimal(d.price) ?? Dec.ZERO;
    req.price = price.toString();
  }
  if (d.timeInForce === 'GTD' && d.gtdExpiry.trim() !== '') {
    const t = new Date(d.gtdExpiry);
    if (!Number.isNaN(t.getTime())) req.gtd_expiry = t.toISOString();
  }
  return req;
}

/** Fresh client order id — `web-` prefix keeps UI-originated ids
 * distinguishable in ops tooling (§8.1 dedup key is exactly this). */
export function newClientOrderId(): string {
  const uuid =
    typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
      ? crypto.randomUUID()
      : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
  return `web-${uuid}`;
}
