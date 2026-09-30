/**
 * Order-entry draft validation (Task 10.3.3 item 2) — pure functions
 * composed on the Task 10.3.29 shared framework (`@/lib/input-helpers`):
 *
 *   - per-field rules come from the canonical route binding
 *     `ROUTE_FIELD_BINDINGS['POST /api/v1/orders']` (validateRecord —
 *     kind/required/positive mirroring orders/validate.go);
 *   - relational rules come from `validateOrderSubmit` (§22.1: MARKET
 *     qty XOR quote_quantity, price required for LIMIT, GTD expiry);
 *   - instrument filters (tick/lot multiples, min/max qty, min/max
 *     price) come from `validateAgainstInstrument`;
 *   - two checks stay local because the framework doesn't compose them:
 *     min_notional (cross-field qty × price) and "GTD expiry must be in
 *     the future" (RULE_GTD_EXPIRY is format-only).
 *
 * Raw input is canonicalized through `parseInputDecimal` first so the
 * validator sees the same value `buildRequest` puts on the wire (commas
 * and stray whitespace are accepted input, never a validation failure).
 * The server remains authoritative — client checks only keep obvious
 * rejects off the wire.
 */
import {
  ROUTE_FIELD_BINDINGS,
  cmpDecimal,
  isPositive,
  mulDecimal,
  validateAgainstInstrument,
  validateOrderSubmit,
  validateRecord,
  type FieldError,
} from '@/lib/input-helpers';
import { Dec } from '@/lib/market/decimal';
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

/** Canonical rule set for the ticket's submit route (Task 10.3.29). */
const ORDER_FIELD_RULES = ROUTE_FIELD_BINDINGS['POST /api/v1/orders'] ?? [];

/** Framework field names → DraftErrors keys. Other bound fields (symbol,
 * side, type, tif) are control-driven and can't fail here. */
const FIELD_MAP: Readonly<Record<string, DraftField>> = {
  quantity: 'quantity',
  price: 'price',
  gtd_expiry: 'gtdExpiry',
};

export function validateDraft(d: OrderDraft, inst?: Instrument): DraftErrors {
  const errors: DraftErrors = {};

  // Canonicalize before validating — the wire form (commas/whitespace
  // stripped) is what the framework rules should judge.
  const qty = parseInputDecimal(d.quantity);
  const price = d.type === 'LIMIT' ? parseInputDecimal(d.price) : null;
  const values = {
    symbol: d.symbol,
    side: d.side,
    type: d.type,
    quantity: qty !== null ? qty.toString() : d.quantity,
    price: d.type === 'LIMIT' ? (price !== null ? price.toString() : d.price) : '',
    time_in_force: d.timeInForce,
    gtd_expiry: d.timeInForce === 'GTD' ? d.gtdExpiry : '',
  };

  const collected: FieldError[] = [
    ...validateRecord(ORDER_FIELD_RULES, values),
    ...validateOrderSubmit(values),
  ];
  if (inst !== undefined) {
    collected.push(
      ...validateAgainstInstrument(
        {
          tickSize: inst.tickSize,
          lotSize: inst.lotSize,
          minOrderQty: inst.minOrderQty,
          maxOrderQty: inst.maxOrderQty,
          minNotional: inst.minNotional,
          minPrice: inst.minPrice ?? null,
          maxPrice: inst.maxPrice ?? null,
        },
        values,
      ),
    );
  }
  for (const err of collected) {
    const key = FIELD_MAP[err.field];
    if (key !== undefined && errors[key] === undefined) errors[key] = err.message;
  }

  // Local cross-field check the framework doesn't compose: notional =
  // qty × price ≥ min_notional (LIMIT only — MARKET has no price leg).
  if (
    inst !== undefined &&
    d.type === 'LIMIT' &&
    qty !== null &&
    qty.isPositive() &&
    price !== null &&
    price.isPositive() &&
    errors.quantity === undefined &&
    errors.price === undefined
  ) {
    const notional = mulDecimal(qty.toString(), price.toString());
    if (
      notional !== null &&
      isPositive(inst.minNotional) &&
      cmpDecimal(notional, inst.minNotional) === -1
    ) {
      errors.notional = `Order value below minimum notional ${inst.minNotional} ${inst.quoteCurrency}`;
    }
  }

  // Local temporal check — RULE_GTD_EXPIRY is format-only (rfc3339); the
  // "must be in the future" rule is relational to now.
  if (d.timeInForce === 'GTD' && errors.gtdExpiry === undefined) {
    const t = new Date(d.gtdExpiry);
    if (!Number.isNaN(t.getTime()) && t.getTime() <= Date.now()) {
      errors.gtdExpiry = 'Expiry must be in the future';
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
