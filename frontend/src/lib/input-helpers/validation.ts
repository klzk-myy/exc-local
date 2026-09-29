/**
 * Field-level validation framework (Task 10.3.29 item 1).
 *
 * Two tiers:
 *   1. Route contracts — GENERATED from docs/openapi/openapi.json
 *      (generated/route-contracts.ts; regenerate via
 *      `npm run gen:validators`). Never hand-edit; drift is a defect.
 *   2. Field rules — the canonical structural rules below mirror the
 *      server-side validators with provenance comments:
 *        services/internal/orders/validate.go   (side/TIF/type/STP/
 *                                                price/qty/filter rules)
 *        services/internal/orders/types.go      (enum vocabularies)
 *        services/internal/accounts/countdown.go (countdown_ms bounds)
 *        Phase-16 Task 16.3.19                  (grid-bot params)
 *        Phase-14 Task 14.3.14                  (copy-follow params)
 *        services/internal/api/tax.go           (tax method enum)
 *      The published OpenAPI document carries no requestBody schemas —
 *      when it gains them, gen-validators emits them and this registry
 *      shrinks. Nothing here may contradict a server rule; where a bound
 *      is unknown the rule is left unsatisfied (client never invents).
 *
 * `useInputHelper(route)` (useInputHelper.ts) binds a route contract to
 * the field-rule set declared for it in `ROUTE_FIELD_BINDINGS`.
 */
import { cmpDecimal, isDecimal, isMultipleOf, isPositive, isZero } from './decimal';

// ---------------------------------------------------------------------------
// Canonical vocabularies (mirroring services/internal/orders/types.go)
// ---------------------------------------------------------------------------

export const ORDER_SIDES = ['BUY', 'SELL'] as const;
/** Wire-expressible types (validate.go validWireType); extended §6.1
 * types are Phase-16 surfaces submitted via their own routes. */
export const ORDER_TYPES = ['LIMIT', 'MARKET', 'STOP', 'STOP_LIMIT', 'ICEBERG'] as const;
export const ORDER_TYPES_EXTENDED = [
  'LIMIT',
  'MARKET',
  'STOP',
  'STOP_LIMIT',
  'ICEBERG',
  'TWAP',
  'VWAP',
  'TRAILING_STOP',
  'BRACKET',
  'OCO',
  'SPREAD',
  'SCALE',
  'PEG',
  'FIXING',
  'MOO',
  'MOC',
] as const;
export const TIME_IN_FORCE = ['GTC', 'IOC', 'FOK', 'GTD', 'DAY'] as const;
export const STP_MODES = [
  'CANCEL_NEWEST',
  'CANCEL_OLDEST',
  'CANCEL_BOTH',
  'DECREMENT',
  'NONE',
] as const;
export const ORDER_STATUSES = [
  'PENDING',
  'RESERVED',
  'ACTIVE',
  'PARTIALLY_FILLED',
  'FILLED',
  'CANCELLED',
  'REJECTED',
  'EXPIRED',
] as const;
/** Open (cancellable) statuses — mirrors OpenStatuses in types.go. */
export const OPEN_ORDER_STATUSES = ['PENDING', 'RESERVED', 'ACTIVE', 'PARTIALLY_FILLED'] as const;
export const TAX_METHODS = ['FIFO', 'LIFO', 'HIFO', 'AVG_COST'] as const;
export const GRID_MODES = ['ARITHMETIC', 'GEOMETRIC'] as const;
export const COPY_SAFETY_MODES = ['FULL', 'HALF_RISK'] as const;
export const INSTRUMENT_TYPES = ['SPOT', 'FORWARD', 'SWAP', 'NDF', 'OPTION'] as const;

/** Dead-man countdown bounds (accounts/countdown.go Min/MaxCountdownMs). */
export const COUNTDOWN_MIN_MS = 1000;
export const COUNTDOWN_MAX_MS = 300000;
/** Grid-bot bounds (Phase-16 Task 16.3.19: grid_count 5–200, ≤5 live). */
export const GRID_COUNT_MIN = 5;
export const GRID_COUNT_MAX = 200;
export const GRID_MAX_CONCURRENT = 5;
/** Batch-order cap (Task 5.3.32: ≤10 submit / ≤20 cancel). */
export const BATCH_SUBMIT_MAX = 10;
export const BATCH_CANCEL_MAX = 20;
/** client_order_id length cap (validate.go). */
export const CLIENT_ORDER_ID_MAX = 64;
/** Symbol form — canonical FX pair "EUR/USD" (spec §10.5 charset). */
export const SYMBOL_RE = /^[A-Z]{3}\/[A-Z]{3}$/;

// ---------------------------------------------------------------------------
// Field rules
// ---------------------------------------------------------------------------

export interface FieldRule {
  /** Canonical wire name (e.g. 'symbol', 'countdown_ms'). */
  readonly name: string;
  readonly label?: string;
  readonly required?: boolean;
  /** Scalar type the field must parse as. */
  readonly kind?: 'string' | 'decimal' | 'integer' | 'boolean' | 'rfc3339';
  readonly enum?: readonly string[];
  /** String length caps. */
  readonly maxLength?: number;
  readonly minLength?: number;
  readonly pattern?: RegExp;
  /** Decimal bounds (inclusive, string-compared via fixed-point). */
  readonly min?: string;
  readonly max?: string;
  /** Integer bounds (inclusive). */
  readonly minInt?: number;
  readonly maxInt?: number;
  /** Must be strictly positive decimal. */
  readonly positive?: boolean;
  /** Human-readable hint rendered with the error. */
  readonly hint?: string;
}

export interface FieldError {
  field: string;
  message: string;
}

export type FieldValues = Record<string, string | undefined>;

function label(rule: FieldRule): string {
  return rule.label ?? rule.name.replace(/_/g, ' ');
}

/** Validate one value against a rule. Returns null when valid. */
export function validateField(rule: FieldRule, raw: string | undefined): FieldError | null {
  const name = label(rule);
  const value = raw?.trim() ?? '';
  if (value === '') {
    if (rule.required) return { field: rule.name, message: `${name} is required` };
    return null;
  }
  if (rule.enum && !rule.enum.includes(value)) {
    return { field: rule.name, message: `${name} must be one of ${rule.enum.join(', ')}` };
  }
  if (rule.maxLength !== undefined && value.length > rule.maxLength) {
    return { field: rule.name, message: `${name} exceeds ${rule.maxLength} characters` };
  }
  if (rule.minLength !== undefined && value.length < rule.minLength) {
    return { field: rule.name, message: `${name} must be at least ${rule.minLength} characters` };
  }
  if (rule.pattern && !rule.pattern.test(value)) {
    return {
      field: rule.name,
      message: `${name} has invalid format${rule.hint ? ` (${rule.hint})` : ''}`,
    };
  }
  switch (rule.kind) {
    case 'decimal': {
      if (!isDecimal(value)) {
        return { field: rule.name, message: `${name} must be a decimal number` };
      }
      if (rule.positive && !isPositive(value)) {
        return { field: rule.name, message: `${name} must be positive` };
      }
      if (rule.min !== undefined && cmpDecimal(value, rule.min) === -1) {
        return { field: rule.name, message: `${name} below minimum ${rule.min}` };
      }
      if (rule.max !== undefined && cmpDecimal(value, rule.max) === 1) {
        return { field: rule.name, message: `${name} exceeds maximum ${rule.max}` };
      }
      break;
    }
    case 'integer': {
      if (!/^[+-]?\d+$/.test(value)) {
        return { field: rule.name, message: `${name} must be an integer` };
      }
      const n = Number(value);
      if (rule.minInt !== undefined && n < rule.minInt) {
        return { field: rule.name, message: `${name} below minimum ${rule.minInt}` };
      }
      if (rule.maxInt !== undefined && n > rule.maxInt) {
        return { field: rule.name, message: `${name} exceeds maximum ${rule.maxInt}` };
      }
      break;
    }
    case 'rfc3339': {
      if (Number.isNaN(Date.parse(value))) {
        return { field: rule.name, message: `${name} must be an RFC3339 timestamp` };
      }
      break;
    }
    default:
      break;
  }
  return null;
}

/** Validate a record of raw values against a rule set. */
export function validateRecord(rules: readonly FieldRule[], values: FieldValues): FieldError[] {
  const errors: FieldError[] = [];
  for (const rule of rules) {
    const err = validateField(rule, values[rule.name]);
    if (err) errors.push(err);
  }
  return errors;
}

// ---------------------------------------------------------------------------
// Shared rule atoms (provenance in comments)
// ---------------------------------------------------------------------------

export const RULE_SYMBOL: FieldRule = {
  name: 'symbol',
  label: 'Symbol',
  required: true,
  kind: 'string',
  pattern: SYMBOL_RE,
  hint: 'e.g. EUR/USD',
};
export const RULE_SIDE: FieldRule = {
  name: 'side',
  label: 'Side',
  required: true,
  kind: 'string',
  enum: ORDER_SIDES,
};
export const RULE_ORDER_TYPE: FieldRule = {
  name: 'type',
  label: 'Order type',
  required: true,
  kind: 'string',
  enum: ORDER_TYPES,
};
export const RULE_TIF: FieldRule = {
  name: 'time_in_force',
  label: 'Time in force',
  kind: 'string',
  enum: TIME_IN_FORCE,
};
export const RULE_QUANTITY: FieldRule = {
  name: 'quantity',
  label: 'Quantity',
  kind: 'decimal',
  positive: true,
};
export const RULE_PRICE: FieldRule = {
  name: 'price',
  label: 'Price',
  kind: 'decimal',
  positive: true,
};
export const RULE_STOP_PRICE: FieldRule = {
  name: 'stop_price',
  label: 'Stop price',
  kind: 'decimal',
  positive: true,
};
export const RULE_CLIENT_ORDER_ID: FieldRule = {
  name: 'client_order_id',
  label: 'Client order ID',
  kind: 'string',
  maxLength: CLIENT_ORDER_ID_MAX,
};
export const RULE_STP_MODE: FieldRule = {
  name: 'stp_mode',
  label: 'Self-trade prevention',
  kind: 'string',
  enum: STP_MODES,
};
export const RULE_GTD_EXPIRY: FieldRule = {
  name: 'gtd_expiry',
  label: 'GTD expiry',
  kind: 'rfc3339',
};
/** countdown_ms 1000–300000 (accounts/countdown.go). */
export const RULE_COUNTDOWN_MS: FieldRule = {
  name: 'countdown_ms',
  label: 'Countdown (ms)',
  required: true,
  kind: 'integer',
  minInt: COUNTDOWN_MIN_MS,
  maxInt: COUNTDOWN_MAX_MS,
};

// ---------------------------------------------------------------------------
// Route → field-rule bindings. Only routes with declared request bodies
// on the order/automation surface get bindings; `useInputHelper` on an
// unbound route still returns the contract + path-param validation.
// ---------------------------------------------------------------------------

export const ROUTE_FIELD_BINDINGS: Readonly<Record<string, readonly FieldRule[]>> = {
  // POST /api/v1/orders — SubmitRequest (orders/validate.go). Cross-field
  // rules (MARKET qty XOR quote_quantity, price-required-per-type) live in
  // validateOrderSubmit below — they are relational, not per-field.
  'POST /api/v1/orders': [
    RULE_SYMBOL,
    RULE_SIDE,
    RULE_ORDER_TYPE,
    RULE_TIF,
    RULE_QUANTITY,
    RULE_PRICE,
    RULE_STOP_PRICE,
    RULE_CLIENT_ORDER_ID,
    RULE_STP_MODE,
    RULE_GTD_EXPIRY,
  ],
  'POST /api/v1/orders/test': [
    RULE_SYMBOL,
    RULE_SIDE,
    RULE_ORDER_TYPE,
    RULE_TIF,
    RULE_QUANTITY,
    RULE_PRICE,
    RULE_STOP_PRICE,
  ],
  'PUT /api/v1/orders/{id}': [RULE_PRICE, RULE_QUANTITY, RULE_STOP_PRICE, RULE_TIF],
  'PUT /api/v1/orders/{id}/amend/keep-priority': [RULE_QUANTITY],
  'POST /api/v1/orders/countdown-cancel-all': [RULE_COUNTDOWN_MS],
  // Grid bot (Phase-16 Task 16.3.19).
  'POST /api/v1/bots/grid': [
    RULE_SYMBOL,
    { name: 'upper_price', label: 'Upper price', required: true, kind: 'decimal', positive: true },
    { name: 'lower_price', label: 'Lower price', required: true, kind: 'decimal', positive: true },
    {
      name: 'grid_count',
      label: 'Grid count',
      required: true,
      kind: 'integer',
      minInt: GRID_COUNT_MIN,
      maxInt: GRID_COUNT_MAX,
    },
    {
      name: 'total_investment',
      label: 'Total investment',
      required: true,
      kind: 'decimal',
      positive: true,
    },
    { name: 'mode', label: 'Grid mode', kind: 'string', enum: GRID_MODES },
    { name: 'take_profit_price', label: 'Take-profit price', kind: 'decimal', positive: true },
    { name: 'stop_loss_price', label: 'Stop-loss price', kind: 'decimal', positive: true },
  ],
  // Copy follow (Phase-14 Task 14.3.14).
  'POST /api/v1/copy/follows': [
    { name: 'strategy_id', label: 'Strategy', required: true, kind: 'integer', minInt: 1 },
    {
      name: 'allocation',
      label: 'Allocation',
      required: true,
      kind: 'decimal',
      positive: true,
    },
    { name: 'safety_mode', label: 'Safety mode', kind: 'string', enum: COPY_SAFETY_MODES },
    { name: 'stop_loss_cap', label: 'Stop-loss cap', kind: 'decimal', positive: true },
    { name: 'max_copy_qty', label: 'Max per-trade copy size', kind: 'decimal', positive: true },
  ],
  'GET /api/v1/tax/report': [
    {
      name: 'year',
      label: 'Tax year',
      required: true,
      kind: 'integer',
      minInt: 1970,
      maxInt: 9999,
    },
    { name: 'method', label: 'Cost-basis method', kind: 'string', enum: TAX_METHODS },
  ],
  'GET /api/v1/account/tax-report': [
    {
      name: 'year',
      label: 'Tax year',
      required: true,
      kind: 'integer',
      minInt: 1970,
      maxInt: 9999,
    },
    { name: 'method', label: 'Cost-basis method', kind: 'string', enum: TAX_METHODS },
  ],
};

/**
 * Cross-field rules for order submit/dry-run — the relational half of
 * ValidateSubmit (§22.1): exactly one of quantity|quote_quantity on
 * MARKET; quote_quantity only on MARKET; price required for
 * LIMIT/ICEBERG; stop_price for STOP/STOP_LIMIT; GTD needs gtd_expiry.
 * Instrument filter rules (tick/lot/min_notional/bands) are checked by
 * format.ts/instrument-aware consumers, not here — they need metadata.
 */
export function validateOrderSubmit(values: FieldValues): FieldError[] {
  const errors: FieldError[] = [];
  const type = values['type']?.trim() ?? '';
  const qty = values['quantity']?.trim() ?? '';
  const quoteQty = values['quote_quantity']?.trim() ?? '';
  const price = values['price']?.trim() ?? '';
  const stop = values['stop_price']?.trim() ?? '';
  const tif = values['time_in_force']?.trim() ?? '';
  const gtd = values['gtd_expiry']?.trim() ?? '';
  const icebergQty = values['iceberg_visible_qty']?.trim() ?? '';

  if (type === 'MARKET') {
    if ((qty === '') === (quoteQty === '')) {
      errors.push({
        field: 'quantity',
        message: 'Market orders require exactly one of quantity or quote_quantity',
      });
    }
    if (quoteQty !== '' && !isPositive(quoteQty)) {
      errors.push({ field: 'quote_quantity', message: 'quote_quantity must be positive' });
    }
  } else {
    if (quoteQty !== '') {
      errors.push({
        field: 'quote_quantity',
        message: 'quote_quantity is only valid on MARKET orders',
      });
    }
    if (type !== '' && (qty === '' || !isPositive(qty))) {
      errors.push({ field: 'quantity', message: 'quantity must be positive' });
    }
  }
  if ((type === 'LIMIT' || type === 'ICEBERG') && (price === '' || !isPositive(price))) {
    errors.push({ field: 'price', message: `price required for ${type} orders` });
  }
  if (type === 'STOP' && (stop === '' || !isPositive(stop))) {
    errors.push({ field: 'stop_price', message: 'stop_price required for STOP orders' });
  }
  if (
    type === 'STOP_LIMIT' &&
    (stop === '' || !isPositive(stop) || price === '' || !isPositive(price))
  ) {
    errors.push({ field: 'stop_price', message: 'price and stop_price required for STOP_LIMIT' });
  }
  if (icebergQty !== '' && type !== 'ICEBERG') {
    errors.push({
      field: 'iceberg_visible_qty',
      message: 'iceberg_visible_qty is only valid on ICEBERG orders',
    });
  }
  if (tif === 'GTD' && gtd === '') {
    errors.push({ field: 'gtd_expiry', message: 'GTD requires gtd_expiry' });
  }
  return errors;
}

/** Instrument-aware field checks (mirrors validateFilters): lot/tick
 * multiples, min/max qty, min/max price, min notional vs eval price.
 * All comparisons are fixed-point; unknown metadata fields are skipped
 * (never fail closed on absent reference data — the server re-checks). */
export function validateAgainstInstrument(
  meta: {
    tickSize: string;
    lotSize: string;
    minOrderQty: string;
    maxOrderQty: string;
    minNotional: string;
    minPrice: string | null;
    maxPrice: string | null;
  },
  values: FieldValues,
): FieldError[] {
  const errors: FieldError[] = [];
  const qty = values['quantity']?.trim() ?? '';
  const price = values['price']?.trim() ?? '';
  if (qty !== '' && isDecimal(qty)) {
    if (isPositive(meta.minOrderQty) && cmpDecimal(qty, meta.minOrderQty) === -1) {
      errors.push({
        field: 'quantity',
        message: `quantity ${qty} below min_order_qty ${meta.minOrderQty}`,
      });
    }
    if (isPositive(meta.maxOrderQty) && cmpDecimal(qty, meta.maxOrderQty) === 1) {
      errors.push({
        field: 'quantity',
        message: `quantity ${qty} exceeds max_order_qty ${meta.maxOrderQty}`,
      });
    }
    if (isPositive(meta.lotSize) && !isMultipleOf(qty, meta.lotSize)) {
      errors.push({
        field: 'quantity',
        message: `quantity ${qty} is not a multiple of lot_size ${meta.lotSize}`,
      });
    }
  }
  if (price !== '' && isDecimal(price)) {
    if (meta.minPrice !== null && cmpDecimal(price, meta.minPrice) === -1) {
      errors.push({ field: 'price', message: `price ${price} below min_price ${meta.minPrice}` });
    }
    if (meta.maxPrice !== null && cmpDecimal(price, meta.maxPrice) === 1) {
      errors.push({ field: 'price', message: `price ${price} above max_price ${meta.maxPrice}` });
    }
    if (isPositive(meta.tickSize) && !isMultipleOf(price, meta.tickSize)) {
      errors.push({
        field: 'price',
        message: `price ${price} is not a multiple of tick_size ${meta.tickSize}`,
      });
    }
  }
  return errors;
}

/** True when a value is a nonzero decimal — small helper for filter UIs. */
export function isNonZeroDecimal(raw: string): boolean {
  return isDecimal(raw) && !isZero(raw);
}
