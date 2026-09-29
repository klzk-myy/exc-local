/**
 * Bulk / paste input helpers (Task 10.3.29 item 9).
 *
 *   - `parseCsvOrders` — CSV order import for POST /api/v1/orders/batch
 *     (Task 5.3.32: ≤10 submit rows). Per-row validation against the
 *     shared order rules — invalid rows are excluded, never silently
 *     submitted.
 *   - `parseBeneficiaryLines` — paste multi-line beneficiary records
 *     (Phase-11 Task 11.3.7 registry) with per-row IBAN-ish checks.
 *   - `parseScaledLadder` — price/quantity ladder for scaled orders
 *     (POST /api/v1/orders/scaled).
 *
 * Every parser returns {rows, summary} — the caller renders a pre-submit
 * table with per-row status and total notional before confirmation.
 */
import { addDecimal, mulDecimal } from './decimal';
import {
  ORDER_SIDES,
  ORDER_TYPES,
  TIME_IN_FORCE,
  validateField,
  RULE_SYMBOL,
  RULE_QUANTITY,
  RULE_PRICE,
} from './validation';

export interface BulkRow<T> {
  /** 1-based source line number. */
  line: number;
  raw: string;
  value: T | null;
  /** Per-row validation errors — empty means the row is submittable. */
  errors: string[];
  status: 'ok' | 'invalid';
}

export interface BulkParseResult<T> {
  rows: BulkRow<T>[];
  okCount: number;
  invalidCount: number;
  /** True when the row count exceeds the caller-supplied cap. */
  overLimit: boolean;
}

/** Minimal CSV tokenizer — quoted fields with "" escapes; no newlines
 * inside quotes (import payloads are line-oriented). */
export function splitCsvLine(line: string): string[] {
  const out: string[] = [];
  let cur = '';
  let inQuotes = false;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (ch === undefined) break;
    if (inQuotes) {
      if (ch === '"') {
        if (line[i + 1] === '"') {
          cur += '"';
          i++;
        } else {
          inQuotes = false;
        }
      } else {
        cur += ch;
      }
    } else if (ch === '"') {
      inQuotes = true;
    } else if (ch === ',') {
      out.push(cur);
      cur = '';
    } else {
      cur += ch;
    }
  }
  out.push(cur);
  return out.map((s) => s.trim());
}

function lines(text: string): { line: number; raw: string }[] {
  return text
    .split(/\r?\n/)
    .map((raw, i) => ({ line: i + 1, raw: raw.trim() }))
    .filter((l) => l.raw !== '');
}

function isHeader(cells: readonly string[], expected: readonly string[]): boolean {
  return cells.length >= expected.length && expected.every((h, i) => cells[i]?.toLowerCase() === h);
}

// ---------------------------------------------------------------------------
// CSV order import — columns: symbol,side,type,qty,price,tif
// ---------------------------------------------------------------------------

export interface CsvOrder {
  symbol: string;
  side: string;
  type: string;
  quantity: string;
  price: string | null;
  time_in_force: string;
}

export const CSV_ORDER_COLUMNS = ['symbol', 'side', 'type', 'qty', 'price', 'tif'] as const;

export function parseCsvOrders(text: string, maxRows = 10): BulkParseResult<CsvOrder> {
  let rows = lines(text);
  // Optional header row is skipped, not validated.
  const first = rows[0];
  if (first && isHeader(splitCsvLine(first.raw), CSV_ORDER_COLUMNS)) {
    rows = rows.slice(1);
  }
  const parsed: BulkRow<CsvOrder>[] = rows.map((l) => {
    const cells = splitCsvLine(l.raw);
    const errors: string[] = [];
    if (cells.length < 3 || cells.length > 6) {
      errors.push(`expected ${CSV_ORDER_COLUMNS.join(',')} columns (got ${cells.length})`);
    }
    const [symbol = '', side = '', type = '', qty = '', price = '', tif = ''] = cells;
    const symbolU = symbol.toUpperCase();
    const sideU = side.toUpperCase();
    const typeU = type.toUpperCase();
    const tifU = tif === '' ? 'GTC' : tif.toUpperCase();

    const symErr = validateField(RULE_SYMBOL, symbolU);
    if (symErr) errors.push(symErr.message);
    if (!ORDER_SIDES.includes(sideU as (typeof ORDER_SIDES)[number])) {
      errors.push(`side must be BUY or SELL (got "${side}")`);
    }
    if (!ORDER_TYPES.includes(typeU as (typeof ORDER_TYPES)[number])) {
      errors.push(`type must be one of ${ORDER_TYPES.join('|')} (got "${type}")`);
    }
    const qtyErr = validateField(RULE_QUANTITY, qty);
    if (qtyErr) errors.push(`qty: ${qtyErr.message}`);
    if ((typeU === 'LIMIT' || typeU === 'ICEBERG' || typeU === 'STOP_LIMIT') && price === '') {
      errors.push('price required for LIMIT-family rows');
    }
    if (price !== '') {
      const pErr = validateField(RULE_PRICE, price);
      if (pErr) errors.push(`price: ${pErr.message}`);
    }
    if (!TIME_IN_FORCE.includes(tifU as (typeof TIME_IN_FORCE)[number])) {
      errors.push(`tif must be one of ${TIME_IN_FORCE.join('|')} (got "${tif}")`);
    }

    return {
      line: l.line,
      raw: l.raw,
      value:
        errors.length === 0
          ? {
              symbol: symbolU,
              side: sideU,
              type: typeU,
              quantity: qty,
              price: price === '' ? null : price,
              time_in_force: tifU,
            }
          : null,
      errors,
      status: errors.length === 0 ? 'ok' : 'invalid',
    };
  });
  return {
    rows: parsed,
    okCount: parsed.filter((r) => r.status === 'ok').length,
    invalidCount: parsed.filter((r) => r.status === 'invalid').length,
    overLimit: parsed.length > maxRows,
  };
}

/** Estimated total notional over the VALID rows (qty×price where priced;
 * MARKET rows contribute qty only — callers annotate the column). */
export function csvOrderNotional(rows: readonly BulkRow<CsvOrder>[]): string | null {
  let total: string | null = '0';
  for (const r of rows) {
    if (r.status !== 'ok' || r.value === null) continue;
    if (r.value.price === null) continue;
    const n = mulDecimal(r.value.quantity, r.value.price);
    if (n === null || total === null) {
      total = null;
      continue;
    }
    total = addDecimal(total, n);
  }
  return total;
}

// ---------------------------------------------------------------------------
// Beneficiary paste — "name, ibanOrAccount, bankCodeOrSwift[, currency]"
// ---------------------------------------------------------------------------

export interface BeneficiaryRow {
  name: string;
  accountRef: string;
  bankCode: string;
  currency: string | null;
}

/** Loose structural checks only — server-side beneficiary validation is
 * authoritative (IBAN checksum lives in Phase-11, not duplicated here). */
export function parseBeneficiaryLines(text: string, maxRows = 50): BulkParseResult<BeneficiaryRow> {
  const rows = lines(text);
  const parsed: BulkRow<BeneficiaryRow>[] = rows.map((l) => {
    const cells = l.raw.includes(',')
      ? splitCsvLine(l.raw)
      : l.raw.split(/\t+/).map((s) => s.trim());
    const errors: string[] = [];
    const [name = '', accountRef = '', bankCode = '', currency = ''] = cells;
    if (cells.length < 3) errors.push('expected: name, account/IBAN, bank code');
    if (name === '') errors.push('name is required');
    if (accountRef === '') errors.push('account reference is required');
    else if (!/^[A-Z0-9]{8,34}$/i.test(accountRef.replace(/\s+/g, ''))) {
      errors.push('account reference must be 8–34 alphanumeric chars');
    }
    if (bankCode === '') errors.push('bank code is required');
    if (currency !== '' && !/^[A-Za-z]{3}$/.test(currency)) {
      errors.push('currency must be a 3-letter ISO code');
    }
    return {
      line: l.line,
      raw: l.raw,
      value:
        errors.length === 0
          ? {
              name,
              accountRef: accountRef.replace(/\s+/g, '').toUpperCase(),
              bankCode: bankCode.toUpperCase(),
              currency: currency === '' ? null : currency.toUpperCase(),
            }
          : null,
      errors,
      status: errors.length === 0 ? 'ok' : 'invalid',
    };
  });
  return {
    rows: parsed,
    okCount: parsed.filter((r) => r.status === 'ok').length,
    invalidCount: parsed.filter((r) => r.status === 'invalid').length,
    overLimit: parsed.length > maxRows,
  };
}

// ---------------------------------------------------------------------------
// Scaled-order ladder — "price, qty" per line
// ---------------------------------------------------------------------------

export interface ScaledLeg {
  price: string;
  quantity: string;
}

export function parseScaledLadder(text: string, maxRows = 50): BulkParseResult<ScaledLeg> {
  const rows = lines(text);
  const parsed: BulkRow<ScaledLeg>[] = rows.map((l) => {
    const cells = l.raw.includes(',') ? splitCsvLine(l.raw) : l.raw.split(/\s+/);
    const errors: string[] = [];
    const [price = '', qty = ''] = cells;
    if (cells.length !== 2) errors.push('expected: price, quantity');
    const pErr = validateField(RULE_PRICE, price);
    if (pErr) errors.push(`price: ${pErr.message}`);
    const qErr = validateField(RULE_QUANTITY, qty);
    if (qErr) errors.push(`qty: ${qErr.message}`);
    return {
      line: l.line,
      raw: l.raw,
      value: errors.length === 0 ? { price, quantity: qty } : null,
      errors,
      status: errors.length === 0 ? 'ok' : 'invalid',
    };
  });
  return {
    rows: parsed,
    okCount: parsed.filter((r) => r.status === 'ok').length,
    invalidCount: parsed.filter((r) => r.status === 'invalid').length,
    overLimit: parsed.length > maxRows,
  };
}
