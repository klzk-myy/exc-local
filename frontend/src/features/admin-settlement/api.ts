/**
 * admin-settlement wire seam (Phase-10.5 Task 10.5.3.9) — post-trade
 * operations: CLS PvP lifecycle, settlement exceptions, MT900/910
 * confirmations, allocation groups and chargebacks.
 *
 *   POST /api/v1/admin/settlement/cls/instructions            paired instruction
 *   POST …/cls/instructions/{ref}/{dispatch,amend,rescind,pay-in,finality,status}
 *   GET  /api/v1/admin/settlement-exceptions/{id}
 *   POST /api/v1/admin/settlement-exceptions/{id}/resolve     RETRY|REVERSE|MANUAL → 202 dual-control
 *   POST /api/v1/admin/settlement-confirmations               MT900/MT910 ingest
 *   POST /api/v1/admin/allocations/groups                     register group
 *   GET  /api/v1/admin/allocations/groups/{id}                evidence bundle
 *   POST …/groups/{id}/{fills,allocate,eligibility,submit}
 *   POST /api/v1/admin/allocations/{id}/{claim,reject,cancel,correct}
 *   POST /api/v1/admin/allocations/escalate                   T+0 sweep
 *   GET/POST /api/v1/admin/chargebacks[/{id}[/{submit,resolve}]]
 *
 * Wire note: ClsInstruction and SettlementException marshal PascalCase
 * (no json tags) — parsers dual-case via pick().
 */
import { malformed } from '@/lib/admin/api';
import type { BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const bool = (v: unknown): boolean | undefined => (typeof v === 'boolean' ? v : undefined);
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const pick = (r: Record<string, unknown>, snake: string, pascal: string): unknown =>
  r[snake] ?? r[pascal];

// ---------------------------------------------------------------------------
// CLS PvP (Task 24.3.8).
// ---------------------------------------------------------------------------

export interface ClsInstruction {
  id: number;
  instructionRef: string;
  counterpartyAccountId: number;
  memberBic: string;
  product: string;
  buyCurrency: string;
  buyAmount: string;
  sellCurrency: string;
  sellAmount: string;
  valueDate: string;
  status: string;
  rejectReason?: string;
  memberAckRef?: string;
}

export function parseClsInstruction(v: unknown): ClsInstruction {
  if (!isRecord(v)) throw malformed('CLS instruction');
  return {
    id: num(pick(v, 'id', 'ID')) ?? 0,
    instructionRef: str(pick(v, 'instruction_ref', 'InstructionRef')) ?? '',
    counterpartyAccountId: num(pick(v, 'counterparty_account_id', 'CounterpartyAccountID')) ?? 0,
    memberBic: str(pick(v, 'member_bic', 'MemberBIC')) ?? '',
    product: str(pick(v, 'product', 'Product')) ?? '',
    buyCurrency: str(pick(v, 'buy_currency', 'BuyCurrency')) ?? '',
    buyAmount: str(pick(v, 'buy_amount', 'BuyAmount')) ?? '0',
    sellCurrency: str(pick(v, 'sell_currency', 'SellCurrency')) ?? '',
    sellAmount: str(pick(v, 'sell_amount', 'SellAmount')) ?? '0',
    valueDate: str(pick(v, 'value_date', 'ValueDate')) ?? '',
    status: str(pick(v, 'status', 'Status')) ?? '',
    rejectReason: str(pick(v, 'reject_reason', 'RejectReason')),
    memberAckRef: str(pick(v, 'member_ack_ref', 'MemberAckRef')),
  };
}

export async function submitClsInstruction(
  api: BoundAdminApi,
  input: {
    instructionRef?: string;
    counterpartyAccountId: number;
    memberBic: string;
    product: string;
    buyCurrency: string;
    buyAmount: string;
    sellCurrency: string;
    sellAmount: string;
    valueDate: string;
    tradeId?: number;
  },
): Promise<ClsInstruction> {
  const raw = await api.post<unknown>('/admin/settlement/cls/instructions', {
    instruction_ref: input.instructionRef ?? '',
    counterparty_account_id: input.counterpartyAccountId,
    member_bic: input.memberBic,
    product: input.product,
    buy_currency: input.buyCurrency,
    buy_amount: input.buyAmount,
    sell_currency: input.sellCurrency,
    sell_amount: input.sellAmount,
    value_date: input.valueDate,
    trade_id: input.tradeId,
  });
  return parseClsInstruction(raw);
}

async function clsVerb(
  api: BoundAdminApi,
  ref: string,
  verb: string,
  body?: unknown,
): Promise<ClsInstruction> {
  const raw = await api.post<unknown>(
    `/admin/settlement/cls/instructions/${encodeURIComponent(ref)}/${verb}`,
    body ?? {},
  );
  return parseClsInstruction(raw);
}

export const clsDispatch = (api: BoundAdminApi, ref: string) => clsVerb(api, ref, 'dispatch');
export const clsPayIn = (api: BoundAdminApi, ref: string) => clsVerb(api, ref, 'pay-in');
export const clsRescind = (api: BoundAdminApi, ref: string, reason: string) =>
  clsVerb(api, ref, 'rescind', { reason });
export const clsAmend = (
  api: BoundAdminApi,
  ref: string,
  input: { buyAmount?: string; sellAmount?: string; valueDate?: string },
) =>
  clsVerb(api, ref, 'amend', {
    buy_amount: input.buyAmount,
    sell_amount: input.sellAmount,
    value_date: input.valueDate,
  });
export const clsFinality = (api: BoundAdminApi, ref: string, memberRef: string) =>
  clsVerb(api, ref, 'finality', { member_ref: memberRef, authenticated: true });
export const clsMemberStatus = (
  api: BoundAdminApi,
  ref: string,
  input: { to: string; memberRef?: string; detail?: string },
) =>
  clsVerb(api, ref, 'status', {
    to: input.to,
    member_ref: input.memberRef ?? '',
    detail: input.detail ?? '',
  });

// ---------------------------------------------------------------------------
// Settlement exceptions (Task 24.3.6) — resolve is dual-control → 202.
// ---------------------------------------------------------------------------

export interface SettlementException {
  id: number;
  instructionId?: number;
  tradeId?: number;
  accountId?: number;
  currency: string;
  amount: string;
  type: string;
  status: string;
  detectedBy: string;
  detail: string;
  action?: string;
  resolvedBy?: number;
  dualControlId?: number;
}

export async function fetchException(api: BoundAdminApi, id: number): Promise<SettlementException> {
  const raw = await api.get<unknown>(`/admin/settlement-exceptions/${id}`);
  if (!isRecord(raw) || !isRecord(raw['exception'])) throw malformed('exception');
  const e = raw['exception'];
  return {
    id: num(pick(e, 'id', 'ID')) ?? 0,
    instructionId: num(pick(e, 'instruction_id', 'InstructionID')),
    tradeId: num(pick(e, 'trade_id', 'TradeID')),
    accountId: num(pick(e, 'account_id', 'AccountID')),
    currency: str(pick(e, 'currency', 'Currency')) ?? '',
    amount: str(pick(e, 'amount', 'Amount')) ?? '0',
    type: str(pick(e, 'type', 'Type')) ?? '',
    status: str(pick(e, 'status', 'Status')) ?? '',
    detectedBy: str(pick(e, 'detected_by', 'DetectedBy')) ?? '',
    detail: str(pick(e, 'detail', 'Detail')) ?? '',
    action: str(pick(e, 'action', 'Action')),
    resolvedBy: num(pick(e, 'resolved_by', 'ResolvedBy')),
    dualControlId: num(pick(e, 'dual_control_id', 'DualControlID')),
  };
}

export interface ExceptionResolveResult {
  exceptionId: number;
  action: string;
  status: string;
  dualControlId?: number;
}

export async function resolveException(
  api: BoundAdminApi,
  id: number,
  input: { action: 'RETRY' | 'REVERSE' | 'MANUAL'; notes?: string },
): Promise<ExceptionResolveResult> {
  const raw = await api.post<unknown>(`/admin/settlement-exceptions/${id}/resolve`, {
    action: input.action,
    notes: input.notes ?? '',
  });
  if (!isRecord(raw)) throw malformed('resolve result');
  const dreq = isRecord(raw['dual_control_request']) ? raw['dual_control_request'] : {};
  return {
    exceptionId: num(raw['exception_id']) ?? id,
    action: str(raw['action']) ?? input.action,
    status: str(raw['status']) ?? 'PENDING',
    dualControlId: num(dreq['id']),
  };
}

// ---------------------------------------------------------------------------
// Settlement confirmations — manual MT900/910 intake (Task 24.3.3).
// ---------------------------------------------------------------------------

export async function ingestConfirmation(
  api: BoundAdminApi,
  input: {
    messageType: 'MT900' | 'MT910';
    reference: string;
    relatedReference?: string;
    currency: string;
    amount: string;
    valueDate?: string;
    nostroIban?: string;
    rawPayload?: string;
  },
): Promise<unknown> {
  return api.post<unknown>('/admin/settlement-confirmations', {
    message_type: input.messageType,
    reference: input.reference,
    related_reference: input.relatedReference ?? '',
    currency: input.currency,
    amount: input.amount,
    value_date: input.valueDate ?? '',
    nostro_iban: input.nostroIban ?? '',
    raw_payload: input.rawPayload ?? '',
  });
}

// ---------------------------------------------------------------------------
// Allocations (Tasks 24.3.10/.15).
// ---------------------------------------------------------------------------

export interface AllocationGroup {
  id: number;
  groupRef: string;
  managerAccountId: number;
  instrumentId: number;
  side: string;
  capacity: string;
  method: string;
  status: string;
  totalQty: string;
  allocatedQty: string;
  avgPrice: string;
  settlementLocked: boolean;
  escalatedAt?: string;
}

export function parseAllocationGroup(v: unknown): AllocationGroup {
  if (!isRecord(v)) throw malformed('allocation group');
  const g = isRecord(v['group']) ? v['group'] : v;
  return {
    id: num(g['id']) ?? 0,
    groupRef: str(g['group_ref']) ?? '',
    managerAccountId: num(g['manager_account_id']) ?? 0,
    instrumentId: num(g['instrument_id']) ?? 0,
    side: str(g['side']) ?? '',
    capacity: str(g['capacity']) ?? '',
    method: str(g['allocation_method']) ?? '',
    status: str(g['status']) ?? '',
    totalQty: str(g['total_qty']) ?? '0',
    allocatedQty: str(g['allocated_qty']) ?? '0',
    avgPrice: str(g['avg_price']) ?? '0',
    settlementLocked: bool(g['settlement_locked']) ?? false,
    escalatedAt: str(g['escalated_at']),
  };
}

export async function createAllocationGroup(
  api: BoundAdminApi,
  input: {
    groupRef: string;
    managerAccountId: number;
    instrumentId: number;
    side: string;
    capacity: string;
    method: string;
    eligibleAccountIds: number[];
  },
): Promise<AllocationGroup> {
  const raw = await api.post<unknown>('/admin/allocations/groups', {
    group_ref: input.groupRef,
    manager_account_id: input.managerAccountId,
    instrument_id: input.instrumentId,
    side: input.side,
    capacity: input.capacity,
    allocation_method: input.method,
    eligible_accounts: input.eligibleAccountIds.map((id) => ({ account_id: id })),
  });
  return parseAllocationGroup(raw);
}

export async function fetchAllocationGroup(
  api: BoundAdminApi,
  id: number,
): Promise<AllocationGroup> {
  const raw = await api.get<unknown>(`/admin/allocations/groups/${id}`);
  return parseAllocationGroup(raw);
}

export async function attachFills(
  api: BoundAdminApi,
  groupId: number,
  tradeIds: number[],
): Promise<AllocationGroup> {
  const raw = await api.post<unknown>(`/admin/allocations/groups/${groupId}/fills`, {
    trade_ids: tradeIds,
  });
  return parseAllocationGroup(raw);
}

export async function runAllocation(
  api: BoundAdminApi,
  groupId: number,
  legs: { accountId: number; quantity?: string; weight?: string }[],
): Promise<AllocationGroup> {
  const raw = await api.post<unknown>(`/admin/allocations/groups/${groupId}/allocate`, {
    legs: legs.map((l) => ({
      account_id: l.accountId,
      quantity: l.quantity ?? '',
      weight: l.weight ?? '',
    })),
  });
  return parseAllocationGroup(raw);
}

export async function flipEligibility(
  api: BoundAdminApi,
  groupId: number,
  input: { accountId: number; eligible: boolean },
): Promise<void> {
  await api.post(`/admin/allocations/groups/${groupId}/eligibility`, {
    account_id: input.accountId,
    eligible: input.eligible,
  });
}

export async function submitGroup(api: BoundAdminApi, groupId: number): Promise<AllocationGroup> {
  const raw = await api.post<unknown>(`/admin/allocations/groups/${groupId}/submit`, {});
  return parseAllocationGroup(raw);
}

export async function legAction(
  api: BoundAdminApi,
  allocId: number,
  verb: 'claim' | 'reject' | 'cancel' | 'correct',
  body: Record<string, unknown>,
): Promise<unknown> {
  return api.post<unknown>(`/admin/allocations/${allocId}/${verb}`, body);
}

export async function escalateAllocations(api: BoundAdminApi): Promise<unknown> {
  return api.post<unknown>('/admin/allocations/escalate', {});
}

// ---------------------------------------------------------------------------
// Chargebacks (Task 5.3.18).
// ---------------------------------------------------------------------------

export interface ChargebackRow {
  id: number;
  accountId: number;
  currency: string;
  amount: string;
  reason: string;
  status: string;
  cardNetwork?: string;
  openedAt: string;
}

export async function fetchChargebacks(
  api: BoundAdminApi,
  f: { status?: string; accountId?: number },
): Promise<ChargebackRow[]> {
  const q = new URLSearchParams();
  if (f.status) q.set('status', f.status);
  if (f.accountId !== undefined && f.accountId > 0) q.set('account_id', String(f.accountId));
  const suffix = q.size > 0 ? `?${q.toString()}` : '';
  const raw = await api.get<unknown>(`/admin/chargebacks${suffix}`);
  const items = isRecord(raw) ? (raw['data'] ?? raw['items']) : raw;
  return arr(items).flatMap((c) => {
    if (!isRecord(c)) return [];
    return [
      {
        id: num(c['id']) ?? 0,
        accountId: num(c['account_id']) ?? 0,
        currency: str(c['currency']) ?? '',
        amount: str(c['amount']) ?? '0',
        reason: str(c['reason']) ?? '',
        status: str(c['status']) ?? '',
        cardNetwork: str(c['card_network']),
        openedAt: str(c['opened_at']) ?? '',
      },
    ];
  });
}

export async function createChargeback(
  api: BoundAdminApi,
  input: {
    accountId: number;
    fundingTransactionId?: number;
    cardNetwork: string;
    currency: string;
    amount: string;
    reason: string;
    freezeAccount: boolean;
    approverUserId: number;
  },
): Promise<unknown> {
  return api.post<unknown>('/admin/chargebacks', {
    account_id: input.accountId,
    funding_transaction_id: input.fundingTransactionId,
    card_network: input.cardNetwork,
    currency: input.currency,
    amount: input.amount,
    reason: input.reason,
    freeze_account: input.freezeAccount,
    approver_user_id: input.approverUserId,
  });
}

export async function submitChargeback(api: BoundAdminApi, id: number): Promise<unknown> {
  return api.post<unknown>(`/admin/chargebacks/${id}/submit`, {});
}

export async function resolveChargeback(
  api: BoundAdminApi,
  id: number,
  input: { outcome: 'WON' | 'LOST'; note: string },
): Promise<unknown> {
  return api.post<unknown>(`/admin/chargebacks/${id}/resolve`, {
    outcome: input.outcome,
    note: input.note,
  });
}
