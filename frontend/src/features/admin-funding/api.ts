/**
 * admin-funding wire seam (Phase-10.5 Task 10.5.3.7) — money-movement
 * review queues and their evidence.
 *
 *   POST /api/v1/admin/funding/deposits                    ingest detected
 *   POST /api/v1/admin/funding/deposits/{id}/confirm       2nd-source confirm
 *   POST /api/v1/admin/funding/deposits/{id}/review        {action,approver_id,note}
 *   POST /api/v1/admin/funding/inbound-wires               bank-side wire (202=quarantine)
 *   POST /api/v1/admin/funding/returns                     rail return
 *   POST /api/v1/admin/withdrawals/{id}/approve|reject     {approver_id,note}
 *   GET  /api/v1/admin/funding/quarantine[?status=&account_id=]
 *   POST /api/v1/admin/funding/quarantine/{id}/resolve     RELEASE_TO_CLIENT|RETURN_TO_SOURCE
 *   GET  /api/v1/admin/funding/bank-accounts[?status=]
 *   POST /api/v1/admin/funding/bank-accounts/{id}/verify   {approver_id,method}
 *   POST /api/v1/admin/funding/bank-accounts/{id}/reject   {reason}
 *   GET  /api/v1/admin/funding/ops-alerts[?limit=]
 *
 * There is deliberately no admin list endpoint for deposits or
 * withdrawals — the ops-alerts rail is the queue-discovery feed; the
 * deposit/withdrawal panels are id-driven action surfaces that render
 * the returned evidence (review tier, flags, confirmations).
 */
import { malformed } from '@/lib/admin/api';
import type { BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const strList = (v: unknown): string[] => arr(v).filter((x): x is string => typeof x === 'string');

// ---------------------------------------------------------------------------
// Deposit lifecycle — DepositResult is the evidence object.
// ---------------------------------------------------------------------------

export interface DepositResult {
  depositId: number;
  status: string;
  currency: string;
  amount: string;
  usdAmount?: string;
  reviewTier?: string;
  reviewDeadline?: string;
  confirmations: number;
  flags: string[];
}

export function parseDepositResult(v: unknown): DepositResult {
  if (!isRecord(v)) throw malformed('deposit result');
  return {
    depositId: num(v['deposit_id']) ?? 0,
    status: str(v['status']) ?? '',
    currency: str(v['currency']) ?? '',
    amount: str(v['amount']) ?? '',
    usdAmount: str(v['usd_amount']),
    reviewTier: str(v['review_tier']),
    reviewDeadline: str(v['review_deadline']),
    confirmations: num(v['confirmations']) ?? 0,
    flags: strList(v['flags']),
  };
}

export async function ingestDeposit(
  api: BoundAdminApi,
  input: {
    accountId: number;
    currency: string;
    amount: string;
    reference: string;
    bankMethod?: string;
    originatorName?: string;
    originatorAccount?: string;
    source: string;
  },
): Promise<DepositResult> {
  const raw = await api.post<unknown>('/admin/funding/deposits', {
    account_id: input.accountId,
    currency: input.currency,
    amount: input.amount,
    reference: input.reference,
    bank_method: input.bankMethod ?? '',
    originator_name: input.originatorName ?? '',
    originator_account: input.originatorAccount ?? '',
    source: input.source,
  });
  return parseDepositResult(raw);
}

export async function confirmDeposit(
  api: BoundAdminApi,
  id: number,
  input: { source: string; senderName?: string; senderAccount?: string },
): Promise<DepositResult> {
  const raw = await api.post<unknown>(`/admin/funding/deposits/${id}/confirm`, {
    source: input.source,
    sender_name: input.senderName ?? '',
    sender_account: input.senderAccount ?? '',
  });
  return parseDepositResult(raw);
}

export async function reviewDeposit(
  api: BoundAdminApi,
  id: number,
  input: { action: 'APPROVE' | 'REJECT'; approverId: number; note?: string },
): Promise<DepositResult> {
  const raw = await api.post<unknown>(`/admin/funding/deposits/${id}/review`, {
    action: input.action,
    approver_id: input.approverId,
    note: input.note ?? '',
  });
  return parseDepositResult(raw);
}

// ---------------------------------------------------------------------------
// Inbound wires + rail returns (Task 11.3.11).
// ---------------------------------------------------------------------------

export interface InboundWireResult {
  disposition: string;
  reason?: string;
  suspenseId?: number;
  depositId?: number;
  nameMatchScore?: number;
}

export async function registerInboundWire(
  api: BoundAdminApi,
  input: {
    bankTxId: string;
    rail: string;
    currency: string;
    amount: string;
    originatorName: string;
    originatorAccount: string;
    reference?: string;
    remittanceInfo?: string;
  },
): Promise<InboundWireResult> {
  const raw = await api.post<unknown>('/admin/funding/inbound-wires', {
    bank_tx_id: input.bankTxId,
    rail: input.rail,
    currency: input.currency,
    amount: input.amount,
    originator_name: input.originatorName,
    originator_account: input.originatorAccount,
    reference: input.reference ?? '',
    remittance_info: input.remittanceInfo ?? '',
  });
  if (!isRecord(raw)) throw malformed('inbound wire result');
  const suspense = isRecord(raw['suspense']) ? raw['suspense'] : null;
  const deposit = isRecord(raw['deposit']) ? raw['deposit'] : null;
  return {
    disposition: str(raw['disposition']) ?? '',
    reason: str(raw['reason']),
    suspenseId: suspense !== null ? num(suspense['id']) : undefined,
    depositId: deposit !== null ? num(deposit['id']) : undefined,
    nameMatchScore: num(raw['name_match_score']),
  };
}

export interface RailReturnResult {
  fundingStatus?: string;
  quarantined: boolean;
  mappingAction?: string;
}

export async function applyRailReturn(
  api: BoundAdminApi,
  input: { endToEndId: string; returnCode: string; reason?: string },
): Promise<RailReturnResult> {
  const raw = await api.post<unknown>('/admin/funding/returns', {
    end_to_end_id: input.endToEndId,
    return_code: input.returnCode,
    reason: input.reason ?? '',
  });
  if (!isRecord(raw)) throw malformed('rail return');
  const mapping = isRecord(raw['mapping']) ? raw['mapping'] : null;
  return {
    fundingStatus: str(raw['funding_status']),
    quarantined: raw['quarantined'] === true,
    mappingAction: mapping !== null ? str(mapping['action']) : undefined,
  };
}

// ---------------------------------------------------------------------------
// Withdrawal review (four-eyes) — WithdrawalResult evidence.
// ---------------------------------------------------------------------------

export interface WithdrawalResult {
  withdrawalId: number;
  status: string;
  currency: string;
  amount: string;
  usdAmount?: string;
  reviewTier?: string;
  flags: string[];
}

export async function reviewWithdrawal(
  api: BoundAdminApi,
  id: number,
  approve: boolean,
  input: { approverId: number; note?: string },
): Promise<WithdrawalResult> {
  const raw = await api.post<unknown>(
    `/admin/withdrawals/${id}/${approve ? 'approve' : 'reject'}`,
    { approver_id: input.approverId, note: input.note ?? '' },
  );
  if (!isRecord(raw)) throw malformed('withdrawal result');
  return {
    withdrawalId: num(raw['withdrawal_id']) ?? id,
    status: str(raw['status']) ?? '',
    currency: str(raw['currency']) ?? '',
    amount: str(raw['amount']) ?? '',
    usdAmount: str(raw['usd_amount']),
    reviewTier: str(raw['review_tier']),
    flags: strList(raw['flags']),
  };
}

// ---------------------------------------------------------------------------
// Quarantine journal (§5.46).
// ---------------------------------------------------------------------------

export interface SuspenseRow {
  id: number;
  bankTxId: string;
  accountId?: number;
  rail?: string;
  currency: string;
  amount: string;
  originatorName?: string;
  nameMatchScore?: number;
  unmatchedReason: string;
  glAccount: string;
  quarantineStatus: string;
  quarantinedAt: string;
  slaExpiresAt: string;
}

export async function fetchQuarantine(
  api: BoundAdminApi,
  filter: { status?: string; accountId?: number },
): Promise<SuspenseRow[]> {
  const raw = await api.get<unknown>('/admin/funding/quarantine', {
    status: filter.status === '' ? undefined : filter.status,
    account_id:
      filter.accountId === undefined || filter.accountId <= 0 ? undefined : filter.accountId,
    limit: 50,
  });
  if (!isRecord(raw) || !Array.isArray(raw['items'])) throw malformed('quarantine list');
  const out: SuspenseRow[] = [];
  for (const v of raw['items'] as unknown[]) {
    if (!isRecord(v) || num(v['id']) === undefined) continue;
    out.push({
      id: num(v['id']) ?? 0,
      bankTxId: str(v['bank_tx_id']) ?? '',
      accountId: num(v['account_id']),
      rail: str(v['rail']),
      currency: str(v['currency']) ?? '',
      amount:
        v['amount'] === undefined || v['amount'] === null
          ? ''
          : typeof v['amount'] === 'string'
            ? v['amount']
            : JSON.stringify(v['amount']),
      originatorName: str(v['originator_name']),
      nameMatchScore: num(v['name_match_score']),
      unmatchedReason: str(v['unmatched_reason']) ?? '',
      glAccount: str(v['gl_account']) ?? '',
      quarantineStatus: str(v['quarantine_status']) ?? '',
      quarantinedAt: str(v['quarantined_at']) ?? '',
      slaExpiresAt: str(v['sla_expires_at']) ?? '',
    });
  }
  return out;
}

export async function resolveQuarantine(
  api: BoundAdminApi,
  id: number,
  input: { action: 'RELEASE_TO_CLIENT' | 'RETURN_TO_SOURCE'; approverId: number; notes?: string },
): Promise<void> {
  await api.post(`/admin/funding/quarantine/${id}/resolve`, {
    action: input.action,
    approver_id: input.approverId,
    notes: input.notes ?? '',
  });
}

// ---------------------------------------------------------------------------
// Beneficiary bank accounts (Task 11.3.7).
// ---------------------------------------------------------------------------

export interface BankAccountRow {
  bankAccountId: number;
  accountId: number;
  currency: string;
  iban?: string;
  bankName: string;
  beneficiaryName: string;
  rail: string;
  status: string;
  verifiedAt?: string;
  unlockedAt?: string;
  rejectionReason?: string;
}

export async function fetchBankAccounts(
  api: BoundAdminApi,
  status?: string,
): Promise<BankAccountRow[]> {
  const raw = await api.get<unknown>('/admin/funding/bank-accounts', {
    status: status === '' ? undefined : status,
    limit: 100,
  });
  if (!isRecord(raw) || !Array.isArray(raw['bank_accounts'])) throw malformed('bank accounts');
  const out: BankAccountRow[] = [];
  for (const v of raw['bank_accounts'] as unknown[]) {
    if (!isRecord(v) || num(v['bank_account_id']) === undefined) continue;
    out.push({
      bankAccountId: num(v['bank_account_id']) ?? 0,
      accountId: num(v['account_id']) ?? 0,
      currency: str(v['currency']) ?? '',
      iban: str(v['iban']),
      bankName: str(v['bank_name']) ?? '',
      beneficiaryName: str(v['beneficiary_name']) ?? '',
      rail: str(v['rail']) ?? '',
      status: str(v['status']) ?? '',
      verifiedAt: str(v['verified_at']),
      unlockedAt: str(v['unlocked_at']),
      rejectionReason: str(v['rejection_reason']),
    });
  }
  return out;
}

export async function verifyBankAccount(
  api: BoundAdminApi,
  id: number,
  input: { approverId: number; method?: string },
): Promise<void> {
  await api.post(`/admin/funding/bank-accounts/${id}/verify`, {
    approver_id: input.approverId,
    method: input.method ?? '',
  });
}

export async function rejectBankAccount(
  api: BoundAdminApi,
  id: number,
  reason: string,
): Promise<void> {
  await api.post(`/admin/funding/bank-accounts/${id}/reject`, { reason });
}

// ---------------------------------------------------------------------------
// Ops-alerts rail — the queue-discovery feed (Task 11.3.6).
// ---------------------------------------------------------------------------

export interface OpsAlert {
  id: number;
  code: string;
  severity: string;
  fundingTransactionId?: number;
  accountId?: number;
  currency?: string;
  summary: string;
  status: string;
  createdAt: string;
}

export async function fetchOpsAlerts(api: BoundAdminApi): Promise<OpsAlert[]> {
  const raw = await api.get<unknown>('/admin/funding/ops-alerts', { limit: 50 });
  if (!isRecord(raw) || !Array.isArray(raw['items'])) throw malformed('ops alerts');
  const out: OpsAlert[] = [];
  for (const v of raw['items'] as unknown[]) {
    if (!isRecord(v) || num(v['id']) === undefined) continue;
    out.push({
      id: num(v['id']) ?? 0,
      code: str(v['code']) ?? '',
      severity: str(v['severity']) ?? '',
      fundingTransactionId: num(v['funding_transaction_id']),
      accountId: num(v['account_id']),
      currency: str(v['currency']),
      summary: str(v['summary']) ?? '',
      status: str(v['status']) ?? '',
      createdAt: str(v['created_at']) ?? '',
    });
  }
  return out;
}
