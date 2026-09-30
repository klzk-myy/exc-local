/**
 * Order-form → wire payload builder (Task 10.3.7).
 *
 * Covers the spec §6 taxonomy:
 *   base types  — LIMIT | MARKET | STOP | STOP_LIMIT | ICEBERG (POST /orders)
 *   composites  — BRACKET (parent→SL+TP OCO children, /orders/bracket) and
 *                 OCO (/orders/oco)
 *   algo        — TRAILING_STOP (distance in PIPS|PERCENTAGE|ABSOLUTE rides
 *                 algo_params, /orders/algo)
 *   TIF         — GTC | IOC | FOK | DAY | GTD (GTD requires the expiry
 *                 picker; §6.1 ties IOC/FOK to MARKET, DAY/GTD to resting
 *                 orders — enforced by TIF_FOR_TYPE)
 *
 * Pure builder returning either the ready body or a list of field errors —
 * never submits an invalid payload (fail-closed §2.7, validated client-side
 * first, server re-validates authoritatively).
 */
import { type Dec, tryDec } from '@/lib/decimal/decimal';
import { cmpDecimal, validateField } from '@/lib/input-helpers';
import { parseInput, splitPair } from '@/lib/trading/fx';
import type {
  BracketOrderBody,
  OcoOrderBody,
  SubmitOrderBody,
  AlgoOrderBody,
} from '@/lib/trading/api';

export type OrderKind =
  'LIMIT' | 'MARKET' | 'STOP' | 'STOP_LIMIT' | 'ICEBERG' | 'TRAILING_STOP' | 'BRACKET' | 'OCO';

export const ORDER_KINDS: readonly OrderKind[] = [
  'LIMIT',
  'MARKET',
  'STOP',
  'STOP_LIMIT',
  'ICEBERG',
  'TRAILING_STOP',
  'BRACKET',
  'OCO',
];

export type Tif = 'GTC' | 'IOC' | 'FOK' | 'GTD' | 'DAY';
export const ALL_TIFS: readonly Tif[] = ['GTC', 'IOC', 'FOK', 'DAY', 'GTD'];

/** §6.1 TIF legality per type. MARKET executes immediately (IOC-only is
 * implied — the wire omits TIF for MARKET); conditional types rest until
 * triggered so they take resting TIFs. */
export const TIF_FOR_KIND: Record<OrderKind, readonly Tif[]> = {
  LIMIT: ['GTC', 'IOC', 'FOK', 'DAY', 'GTD'],
  MARKET: [], // implicit IOC — no TIF sent
  STOP: ['GTC', 'GTD', 'DAY'],
  STOP_LIMIT: ['GTC', 'GTD', 'DAY'],
  ICEBERG: ['GTC', 'DAY', 'GTD'],
  TRAILING_STOP: ['GTC', 'GTD', 'DAY'],
  BRACKET: ['GTC', 'GTD', 'DAY'],
  OCO: ['GTC', 'GTD', 'DAY'],
};

export const TRIGGER_SOURCES = ['LAST_PRICE', 'MARK_PRICE', 'INDEX_PRICE'] as const;
export type TriggerSource = (typeof TRIGGER_SOURCES)[number];

export const TRAILING_UNITS = ['PIPS', 'PERCENTAGE', 'ABSOLUTE'] as const;
export type TrailingUnit = (typeof TRAILING_UNITS)[number];

export interface OrderFormState {
  symbol: string;
  side: 'BUY' | 'SELL';
  kind: OrderKind;
  tif: Tif;
  price: string;
  quantity: string;
  stopPrice: string;
  /** iceberg visible slice */
  visibleQty: string;
  /** GTD expiry — datetime-local input value (UTC wall clock). */
  gtdExpiry: string;
  postOnly: boolean;
  reduceOnly: boolean;
  triggerSource: TriggerSource;
  trailingDistance: string;
  trailingUnit: TrailingUnit;
  /** BRACKET children (absolute prices) / OCO second leg. */
  bracketStop: string;
  bracketTarget: string;
  ocoLimit: string;
}

export const EMPTY_FORM: OrderFormState = {
  symbol: '',
  side: 'BUY',
  kind: 'LIMIT',
  tif: 'GTC',
  price: '',
  quantity: '',
  stopPrice: '',
  visibleQty: '',
  gtdExpiry: '',
  postOnly: false,
  reduceOnly: false,
  triggerSource: 'LAST_PRICE',
  trailingDistance: '',
  trailingUnit: 'PIPS',
  bracketStop: '',
  bracketTarget: '',
  ocoLimit: '',
};

export type FieldErrors = Record<string, string>;

/** Datetime-local input → RFC3339 UTC (the input has no zone; treat it as
 * UTC since the venue clock is UTC, spec §6.7). */
export function gtdToRfc3339(local: string): string | undefined {
  if (local.trim() === '') return undefined;
  const ms = Date.parse(`${local}Z`); // input is "YYYY-MM-DDTHH:mm" UTC
  if (!Number.isFinite(ms)) return undefined;
  return new Date(ms).toISOString();
}

/** Task 10.3.29 — required positive-decimal fields run through the
 * shared rule engine (canonical required/decimal/positive vocabulary). */
function positive(field: string, raw: string, label: string, errors: FieldErrors): Dec | undefined {
  const err = validateField(
    { name: field, label, required: true, kind: 'decimal', positive: true },
    raw,
  );
  if (err !== null) {
    errors[field] = err.message;
    return undefined;
  }
  return parseInput(raw);
}

/**
 * Validate the form and produce the submission body + endpoint. Returns
 * `errors` non-empty when the payload can't be built — callers surface
 * them per-field.
 */
export function buildOrderPayload(form: OrderFormState): {
  endpoint: 'orders' | 'bracket' | 'oco' | 'algo';
  body?: SubmitOrderBody | BracketOrderBody | OcoOrderBody | AlgoOrderBody;
  errors: FieldErrors;
} {
  const errors: FieldErrors = {};
  if (splitPair(form.symbol) === null && !form.symbol.includes('/')) {
    errors['symbol'] = 'Select an instrument';
  }
  const qty = positive('quantity', form.quantity, 'Quantity', errors);

  const gtd = gtdToRfc3339(form.gtdExpiry);
  if (form.tif === 'GTD') {
    if (!gtd) {
      errors['gtdExpiry'] = 'GTD requires gtd_expiry'; // canonical §22.1 message
    } else if (Date.parse(gtd) <= Date.now()) {
      // Local temporal check — RULE_GTD_EXPIRY is format-only.
      errors['gtdExpiry'] = 'GTD expiry must be in the future';
    }
  }

  const flags: Pick<SubmitOrderBody, 'post_only' | 'reduce_only'> = {};
  if (form.postOnly) flags.post_only = true;
  if (form.reduceOnly) flags.reduce_only = true;

  switch (form.kind) {
    case 'LIMIT':
    case 'MARKET':
    case 'STOP':
    case 'STOP_LIMIT':
    case 'ICEBERG': {
      const body: SubmitOrderBody = {
        symbol: form.symbol,
        side: form.side,
        type: form.kind,
        ...flags,
      };
      if (form.kind !== 'MARKET') {
        body.time_in_force = form.tif;
        if (form.tif === 'GTD' && gtd) body.gtd_expiry = gtd;
      }
      if (form.kind !== 'MARKET' && form.kind !== 'STOP') {
        const p = positive('price', form.price, 'Price', errors);
        if (p) body.price = p.toString();
      }
      if (form.kind === 'STOP' || form.kind === 'STOP_LIMIT') {
        const sp = positive('stopPrice', form.stopPrice, 'Stop price', errors);
        if (sp) body.stop_price = sp.toString();
      }
      if (form.kind === 'ICEBERG') {
        const v = positive('visibleQty', form.visibleQty, 'Visible quantity', errors);
        if (v) {
          // Cross-field: visible slice must be strictly below total.
          if (qty && cmpDecimal(v.toString(), qty.toString()) !== -1) {
            errors['visibleQty'] = 'Visible quantity must be below total quantity';
          }
          body.iceberg_visible_qty = v.toString();
        }
      }
      if (qty) body.quantity = qty.toString();
      if (form.triggerSource !== 'LAST_PRICE') {
        body.algo_params = { trigger_source: form.triggerSource };
      }
      return { endpoint: 'orders', body, errors };
    }
    case 'TRAILING_STOP': {
      const dist = positive('trailingDistance', form.trailingDistance, 'Trailing distance', errors);
      const body: AlgoOrderBody = {
        symbol: form.symbol,
        side: form.side,
        algo_type: 'TRAILING_STOP',
        quantity: qty?.toString() ?? '',
        algo_params: {
          trailing_offset: dist?.toString() ?? '',
          trailing_unit: form.trailingUnit,
          trigger_source: form.triggerSource,
        },
      };
      return { endpoint: 'algo', body, errors };
    }
    case 'BRACKET': {
      const sl = positive('bracketStop', form.bracketStop, 'Stop-loss price', errors);
      const tp = positive('bracketTarget', form.bracketTarget, 'Take-profit price', errors);
      // Entry absent ⇒ market parent; present ⇒ limit parent.
      const entry = parseInput(form.price);
      const entryErr = validateField(
        { name: 'price', label: 'Entry price', kind: 'decimal' },
        form.price,
      );
      if (entryErr !== null) {
        errors['price'] = entryErr.message;
      }
      if (sl && tp && cmpDecimal(sl.toString(), tp.toString()) === 0) {
        errors['bracketTarget'] = 'Take-profit must differ from stop-loss';
      }
      // Sanity: for a BUY, SL below TP; for a SELL, reversed.
      if (sl && tp) {
        const slVsTp = cmpDecimal(sl.toString(), tp.toString());
        if (form.side === 'BUY' && slVsTp === 1)
          errors['bracketStop'] = 'Buy bracket: stop must sit below take-profit';
        if (form.side === 'SELL' && slVsTp === -1)
          errors['bracketStop'] = 'Sell bracket: stop must sit above take-profit';
      }
      const body: BracketOrderBody = {
        symbol: form.symbol,
        side: form.side,
        quantity: qty?.toString() ?? '',
        stop_price: sl?.toString() ?? '',
        take_profit_price: tp?.toString() ?? '',
        time_in_force: form.tif,
      };
      if (entry) body.entry_price = entry.toString();
      if (form.tif === 'GTD' && gtd) body.time_in_force = form.tif;
      return { endpoint: 'bracket', body, errors };
    }
    case 'OCO': {
      const lim = positive('ocoLimit', form.ocoLimit, 'Limit leg price', errors);
      const stp = positive('stopPrice', form.stopPrice, 'Stop leg trigger', errors);
      const body: OcoOrderBody = {
        symbol: form.symbol,
        side: form.side,
        quantity: qty?.toString() ?? '',
        price: lim?.toString() ?? '',
        stop_price: stp?.toString() ?? '',
        time_in_force: form.tif,
      };
      return { endpoint: 'oco', body, errors };
    }
  }
}

/** Formats a trailing distance into user-facing units for display. */
export function describeTrailing(distance: string, unit: TrailingUnit, symbol: string): string {
  const d = tryDec(distance);
  if (!d) return '—';
  const label =
    unit === 'PIPS' ? `pips` : unit === 'PERCENTAGE' ? '%' : (splitPair(symbol)?.quote ?? 'quote');
  return `${d.toString()} ${label}`;
}
