/**
 * Admin instrument-lifecycle wire seam (Phase-15 Task 15.3.2 surface —
 * spec §7.1/§7.2; the management panel is Phase-10 Task 10.3.5).
 *
 * Live routes (services/internal/api/admin_instruments.go over
 * internal/admin.InstrumentService — field names mirror the Go JSON
 * tags, nothing is fabricated):
 *
 *   GET  /api/v1/admin/instruments                  list incl. non-ACTIVE — Risk Manager+
 *   POST /api/v1/admin/instruments                  create →DRAFT — Super Admin, dual-control
 *   PUT  /api/v1/admin/instruments/{id}             param edit (DRAFT/ACTIVE) — Risk Manager+
 *   POST /api/v1/admin/instruments/{id}/activate    DRAFT→ACTIVE — Risk Manager+
 *   POST /api/v1/admin/instruments/{id}/restrict    →RESTRICTED — Risk Manager+
 *   POST /api/v1/admin/instruments/{id}/cancel-only →CANCEL_ONLY — RM|Compliance (service gate)
 *   POST /api/v1/admin/instruments/{id}/suspend     →SUSPENDED — Compliance Officer+
 *   POST /api/v1/admin/instruments/{id}/halt        →HALTED — Risk Manager+
 *   POST /api/v1/admin/instruments/{id}/resume      →ACTIVE — Risk Manager+, dual-control
 *   POST /api/v1/admin/instruments/{id}/delist      →DELISTED — Super Admin, dual-control
 *
 * Four-eyes ops (create/resume/delist) return HTTP 202 with
 * {status:"PENDING", dual_control_id, operation, required_approver,
 * expires_at} — surfaced as the 'pending' variant of
 * InstrumentMutationResult so the UI renders "submitted for approval",
 * never an error.
 *
 * BoundAdminApi (src/lib/env) exposes get/post only; the PUT update is
 * issued on the raw ApiClient with the X-Admin-Env header stamped by
 * hand — the env-binding contract holds identically.
 */
import type { ApiClient } from '@/lib/api/client';
import { malformed } from '@/lib/admin/api';
import { ADMIN_ENV_HEADER, type AdminEnv, type BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;

// ---------------------------------------------------------------------------
// Instrument projection (admin.Instrument — the lifecycle view; fields
// absent from instrumentAdminCols simply aren't emitted and stay
// undefined here).
// ---------------------------------------------------------------------------

export interface AdminInstrument {
  id: number;
  symbol: string;
  baseCurrency: string;
  quoteCurrency: string;
  /** SPOT | FORWARD | SWAP | NDF | OPTION (open string — forward-safe). */
  instrumentType: string;
  /** DRAFT | ACTIVE | CANCEL_ONLY | SUSPENDED | HALTED | RESTRICTED | DELISTED. */
  status: string;
  tickSize: string;
  lotSize: string;
  minOrderQty: string;
  maxOrderQty: string;
  settlementCycle: number;
  maxLeverage: number;
  stateEnteredAt?: string;
  graceDeadline?: string;
  /** CANCEL_ONLY_WINDOW | DELIST_NOTICE | CLOSE_ONLY when a grace applies. */
  graceKind?: string;
}

export function parseAdminInstrument(v: unknown): AdminInstrument | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const symbol = str(v['symbol']);
  const status = str(v['status']);
  if (id === undefined || symbol === undefined || status === undefined) return null;
  return {
    id,
    symbol,
    status,
    baseCurrency: str(v['base_currency']) ?? '',
    quoteCurrency: str(v['quote_currency']) ?? '',
    instrumentType: str(v['instrument_type']) ?? '',
    tickSize: str(v['tick_size']) ?? '',
    lotSize: str(v['lot_size']) ?? '',
    minOrderQty: str(v['min_order_qty']) ?? '',
    maxOrderQty: str(v['max_order_qty']) ?? '',
    settlementCycle: num(v['settlement_cycle']) ?? 0,
    maxLeverage: num(v['max_leverage']) ?? 0,
    stateEnteredAt: str(v['state_entered_at']),
    graceDeadline: str(v['grace_deadline']),
    graceKind: str(v['grace_kind']),
  };
}

// ---------------------------------------------------------------------------
// Dual-control pending acknowledgement (writePendingDual — HTTP 202)
// ---------------------------------------------------------------------------

export interface DualControlPending {
  dualControlId: number;
  operation: string;
  /** §8.2 role name the second approver must hold. */
  requiredApprover: string;
  /** Unix seconds. */
  expiresAt?: number;
}

function parseDualPending(v: unknown): DualControlPending | null {
  if (!isRecord(v) || v['status'] !== 'PENDING') return null;
  const dualControlId = num(v['dual_control_id']);
  if (dualControlId === undefined) return null;
  return {
    dualControlId,
    operation: str(v['operation']) ?? '',
    requiredApprover: str(v['required_approver']) ?? '',
    expiresAt: num(v['expires_at']),
  };
}

/** A lifecycle/create call either applied immediately (200 Instrument)
 * or was accepted into the four-eyes queue (202 PENDING). */
export type InstrumentMutationResult =
  | { kind: 'applied'; instrument: AdminInstrument }
  | { kind: 'pending'; pending: DualControlPending };

function parseMutationResult(v: unknown): InstrumentMutationResult | null {
  const pending = parseDualPending(v);
  if (pending !== null) return { kind: 'pending', pending };
  const instrument = parseAdminInstrument(v);
  if (instrument !== null) return { kind: 'applied', instrument };
  return null;
}

// ---------------------------------------------------------------------------
// Fetchers / mutations
// ---------------------------------------------------------------------------

/** GET /api/v1/admin/instruments — {instruments: [...]} incl. DRAFT and
 * control states. Route gate: Risk Manager+ (server-side). */
export async function listAdminInstruments(api: BoundAdminApi): Promise<AdminInstrument[]> {
  const raw = await api.get<unknown>('/admin/instruments');
  if (!isRecord(raw) || !Array.isArray(raw['instruments'])) throw malformed('instruments');
  const out: AdminInstrument[] = [];
  for (const it of raw['instruments']) {
    const p = parseAdminInstrument(it);
    if (p !== null) out.push(p);
  }
  return out;
}

/** POST /admin/instruments payload (admin.InstrumentCreate). The server
 * uppercases symbol/currencies/type and validates decimal strings. */
export interface AdminInstrumentCreateInput {
  symbol: string;
  baseCurrency: string;
  quoteCurrency: string;
  instrumentType: string; // SPOT | FORWARD | SWAP | NDF | OPTION
  tickSize: string;
  lotSize: string;
  minOrderQty: string;
  maxOrderQty: string;
  /** Optional — server defaults "2.00" / "5.00". */
  priceBandPctUp?: string;
  priceBandPctDown?: string;
  settlementCycle: number; // 0 same-day | 1 T+1 | 2 T+2
  maxLeverage: number;
  reason?: string;
}

/** POST /api/v1/admin/instruments — Super Admin + §7.2 four-eyes;
 * resolves 'pending' with the dual-control request. */
export async function createAdminInstrument(
  api: BoundAdminApi,
  input: AdminInstrumentCreateInput,
): Promise<InstrumentMutationResult> {
  const raw = await api.post<unknown>('/admin/instruments', {
    symbol: input.symbol,
    base_currency: input.baseCurrency,
    quote_currency: input.quoteCurrency,
    instrument_type: input.instrumentType,
    tick_size: input.tickSize,
    lot_size: input.lotSize,
    min_order_qty: input.minOrderQty,
    max_order_qty: input.maxOrderQty,
    price_band_pct_up: input.priceBandPctUp,
    price_band_pct_down: input.priceBandPctDown,
    settlement_cycle: input.settlementCycle,
    max_leverage: input.maxLeverage,
    reason: input.reason,
  });
  const res = parseMutationResult(raw);
  if (res === null) throw malformed('instrument create');
  return res;
}

/** PUT /admin/instruments/{id} payload (admin.InstrumentUpdate) —
 * omitted fields stay unchanged server-side. */
export interface AdminInstrumentUpdateInput {
  tickSize?: string;
  lotSize?: string;
  minOrderQty?: string;
  maxOrderQty?: string;
  minNotional?: string;
  minPrice?: string;
  maxPrice?: string;
  priceBandPctUp?: string;
  priceBandPctDown?: string;
  maxSpreadPips?: string;
  maxOpenOrders?: number;
  maxAlgoOrders?: number;
  maxLeverage?: number;
  settlementCycle?: number;
  reason?: string;
}

/** PUT /api/v1/admin/instruments/{id} — parameter edit (DRAFT/ACTIVE
 * only, Risk Manager+). Issued on the raw client because BoundAdminApi
 * has no put(); the X-Admin-Env stamp is applied manually. */
export async function updateAdminInstrument(
  api: ApiClient,
  env: AdminEnv,
  id: number,
  input: AdminInstrumentUpdateInput,
): Promise<AdminInstrument> {
  const raw = await api.put<unknown>(
    `/admin/instruments/${id}`,
    {
      tick_size: input.tickSize,
      lot_size: input.lotSize,
      min_order_qty: input.minOrderQty,
      max_order_qty: input.maxOrderQty,
      min_notional: input.minNotional,
      min_price: input.minPrice,
      max_price: input.maxPrice,
      price_band_pct_up: input.priceBandPctUp,
      price_band_pct_down: input.priceBandPctDown,
      max_spread_pips: input.maxSpreadPips,
      max_open_orders: input.maxOpenOrders,
      max_algo_orders: input.maxAlgoOrders,
      max_leverage: input.maxLeverage,
      settlement_cycle: input.settlementCycle,
      reason: input.reason,
    },
    { headers: { [ADMIN_ENV_HEADER]: env } },
  );
  const inst = parseAdminInstrument(raw);
  if (inst === null) throw malformed('instrument update');
  return inst;
}

/** Lifecycle verbs — the REST path part after /instruments/{id}/. */
export type AdminInstrumentOp =
  'activate' | 'restrict' | 'cancel-only' | 'suspend' | 'halt' | 'resume' | 'delist';

/** admin.TransitionInput — reason is mandatory server-side for
 * restrict/cancel-only/suspend/halt/delist; skip_auction/auction steer
 * the reopening CALL on resume. */
export interface AdminInstrumentTransitionInput {
  reason?: string;
  skipAuction?: boolean;
  auction?: boolean;
}

/** POST /api/v1/admin/instruments/{id}/{op}. Single-approver ops return
 * the updated instrument; resume/delist resolve 'pending' (202). */
export async function transitionAdminInstrument(
  api: BoundAdminApi,
  id: number,
  op: AdminInstrumentOp,
  input: AdminInstrumentTransitionInput = {},
): Promise<InstrumentMutationResult> {
  const raw = await api.post<unknown>(`/admin/instruments/${id}/${op}`, {
    reason: input.reason,
    skip_auction: input.skipAuction,
    auction: input.auction,
  });
  const res = parseMutationResult(raw);
  if (res === null) throw malformed(`instrument ${op}`);
  return res;
}
