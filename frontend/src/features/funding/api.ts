/**
 * Funding & transfers API — Task 10.3.23.
 *
 * Live endpoints (services/internal/api + gateway route table):
 *   GET  /api/v1/deposits/{currency}          → {currency, account_id, reference, instructions[]}
 *   POST /api/v1/withdrawals                  {currency, amount, reference_account,
 *                                              bank_method, confirm_method} + Idempotency-Key
 *   POST /api/v1/withdrawals/{id}/confirm     {token, method}
 *   GET  /api/v1/funding                      ?type&currency&status&from&to&limit&cursor
 *   POST /api/v1/transfers                    {from_account_id,to_account_id,currency,amount}
 *                                             + Idempotency-Key (money-moving, §8.8)
 *   GET  /api/v1/transfers                    ?currency&direction&from&to&cursor
 *   POST /api/v1/funding/fee-estimate         {amount, currency, rail, direction}
 *                                             (Phase-11 Task 11.3.9 — live; error
 *                                             envelope still degrades gracefully)
 *   GET  /api/v1/account/balances             → {account_id, balances[]}
 */
import type { ApiClient } from '@/lib/api';

/** Canonical withdrawal confirmation window — exactly 15 minutes
 * (AGENTS.md canonical values; funding.WithdrawalService). */
export const CONFIRM_WINDOW_MS = 15 * 60 * 1000;

/** Canonical review tiers (spec; funding/funding.go):
 *   <$10K → AUTO · $10K–$50K → STANDARD · >$50K → PENDING_REVIEW +4h. */
export const REVIEW_TIERS = [
  { tier: 'AUTO', range: 'Under $10,000', note: 'processed automatically after confirmation' },
  { tier: 'STANDARD', range: '$10,000 – $50,000', note: 'standard compliance screen' },
  {
    tier: 'PENDING_REVIEW',
    range: 'Over $50,000',
    note: 'manual review — completed within 4 hours',
  },
] as const;

/** bank_method_enum + funding_type/status enums (migration 007). */
export const BANK_METHODS = [
  'SWIFT',
  'SEPA',
  'FEDNOW',
  'ACH',
  'CHAPS',
  'TARGET2',
  'WIRE',
  'INTERNAL',
] as const;
export const CONFIRM_METHODS = ['email', 'sms', 'push', '2fa_totp'] as const;
export const FUNDING_TYPES = [
  'DEPOSIT',
  'WITHDRAWAL',
  'ADJUSTMENT',
  'FEE',
  'FUNDING_RATE',
  'SETTLEMENT',
] as const;
export const FUNDING_STATUSES = [
  'PENDING',
  'CONFIRMED',
  'COMPLETED',
  'FAILED',
  'AUTO_CANCELLED',
  'PENDING_REVIEW',
] as const;

// ---------------------------------------------------------------------------
// Balances
// ---------------------------------------------------------------------------

export interface BalanceRow {
  currency: string;
  available: string;
  locked: string;
  total: string;
}

export async function balances(api: ApiClient): Promise<BalanceRow[]> {
  const res = await api.get<{ balances?: BalanceRow[] }>('/account/balances');
  return res.balances ?? [];
}

// ---------------------------------------------------------------------------
// Deposits
// ---------------------------------------------------------------------------

export interface NostroAccount {
  id: number;
  currency: string;
  bank_name: string;
  bank_code?: string;
  account_number?: string;
  iban?: string;
  status: string;
  beneficiary_name?: string;
  rail?: string;
}

export interface DepositInstructions {
  currency: string;
  accountId: number;
  /** Stable, account-scoped wire reference — quote verbatim on the transfer. */
  reference: string;
  instructions: NostroAccount[];
}

export async function depositInstructions(
  api: ApiClient,
  currency: string,
): Promise<DepositInstructions> {
  const res = await api.get<{
    currency?: string;
    account_id?: number;
    reference?: string;
    instructions?: NostroAccount[];
  }>(`/deposits/${encodeURIComponent(currency.toUpperCase())}`);
  return {
    currency: res.currency ?? currency.toUpperCase(),
    accountId: res.account_id ?? 0,
    reference: res.reference ?? '',
    instructions: res.instructions ?? [],
  };
}

// ---------------------------------------------------------------------------
// Withdrawals
// ---------------------------------------------------------------------------

export interface WithdrawalResult {
  withdrawal_id: number;
  status: string;
  currency: string;
  amount: string;
  usd_amount?: string;
  review_tier?: string;
  review_deadline?: string;
  /** Minted once at create — the UI must hand it to the user immediately. */
  confirm_token?: string;
  expires_at?: string;
  replayed?: boolean;
  dispatch_pending?: boolean;
}

export async function createWithdrawal(
  api: ApiClient,
  input: {
    currency: string;
    amount: string;
    referenceAccount: string;
    bankMethod?: string;
    confirmMethod?: string;
    idempotencyKey: string;
  },
): Promise<WithdrawalResult> {
  return api.post<WithdrawalResult>(
    '/withdrawals',
    {
      currency: input.currency,
      amount: input.amount,
      reference_account: input.referenceAccount,
      bank_method: input.bankMethod,
      confirm_method: input.confirmMethod,
    },
    { idempotencyKey: input.idempotencyKey },
  );
}

export async function confirmWithdrawal(
  api: ApiClient,
  withdrawalId: number,
  token: string,
  method?: string,
): Promise<WithdrawalResult> {
  return api.post<WithdrawalResult>(`/withdrawals/${withdrawalId}/confirm`, {
    token,
    method,
  });
}

// ---------------------------------------------------------------------------
// Unified funding history (deposits + withdrawals + fees)
// ---------------------------------------------------------------------------

export interface FundingTxRow {
  id: number;
  account_id: number;
  currency: string;
  type: string; // DEPOSIT | WITHDRAWAL | FEE | …
  amount: string;
  status: string;
  reference?: string;
  bank_method?: string;
  reference_account?: string;
  usd_amount?: string;
  review_tier?: string;
  review_deadline?: string;
  created_at: string;
  confirmed_at?: string;
  completed_at?: string;
}

export interface FundingFilter {
  type?: string;
  currency?: string;
  status?: string;
  from?: string;
  to?: string;
  limit?: number;
  cursor?: string;
}

export interface FundingEnvelope {
  data: FundingTxRow[];
  nextCursor: string;
  limit: number;
  total: number;
}

export async function fundingHistory(api: ApiClient, f: FundingFilter): Promise<FundingEnvelope> {
  const q = new URLSearchParams();
  if (f.type !== undefined && f.type !== '') q.set('type', f.type);
  if (f.currency !== undefined && f.currency !== '') q.set('currency', f.currency);
  if (f.status !== undefined && f.status !== '') q.set('status', f.status);
  if (f.from !== undefined && f.from !== '') q.set('from', f.from);
  if (f.to !== undefined && f.to !== '') q.set('to', f.to);
  if (f.limit !== undefined) q.set('limit', String(f.limit));
  if (f.cursor !== undefined && f.cursor !== '') q.set('cursor', f.cursor);
  const qs = q.toString();
  const res = await api.get<{
    data?: FundingTxRow[];
    next_cursor?: string;
    limit?: number;
    total?: number;
  }>(`/funding${qs === '' ? '' : `?${qs}`}`);
  return {
    data: res.data ?? [],
    nextCursor: res.next_cursor ?? '',
    limit: res.limit ?? 50,
    total: res.total ?? 0,
  };
}

// ---------------------------------------------------------------------------
// Internal transfers (Task 5.3.23 / Phase-11 11.3.x)
// ---------------------------------------------------------------------------

export interface TransferRow {
  id: number;
  account_id: number;
  from_account_id: number;
  to_account_id: number;
  currency: string;
  amount: string;
  status: string;
  actor?: string;
  journal_entry_id?: number;
  failure_reason?: string;
  created_at: string;
  completed_at?: string;
}

export interface TransferResult {
  transfer: TransferRow;
  replayed?: boolean;
  dispatch_pending?: boolean;
}

export async function createTransfer(
  api: ApiClient,
  input: {
    fromAccountId: number;
    toAccountId: number;
    currency: string;
    amount: string;
    idempotencyKey: string;
  },
): Promise<TransferResult> {
  return api.post<TransferResult>(
    '/transfers',
    {
      from_account_id: input.fromAccountId,
      to_account_id: input.toAccountId,
      currency: input.currency,
      amount: input.amount,
    },
    { idempotencyKey: input.idempotencyKey },
  );
}

export async function transferHistory(
  api: ApiClient,
  cursor?: string,
): Promise<{ data: TransferRow[]; nextCursor: string }> {
  const qs = cursor !== undefined && cursor !== '' ? `?cursor=${encodeURIComponent(cursor)}` : '';
  const res = await api.get<{ data?: TransferRow[]; next_cursor?: string } | TransferRow[]>(
    `/transfers${qs}`,
  );
  if (Array.isArray(res)) return { data: res, nextCursor: '' };
  return { data: res.data ?? [], nextCursor: res.next_cursor ?? '' };
}

// ---------------------------------------------------------------------------
// Fee estimator (Phase-11 Task 11.3.9 — live route; degrade on envelope error)
// ---------------------------------------------------------------------------

export interface FeeEstimate {
  amount: string;
  currency: string;
  rail: string;
  direction: string;
  /** Rail fee, in `fee_currency` (== `currency` unless the rail bills USD). */
  fee?: string;
  fee_currency?: string;
  /** Net amount credited/debited after the fee. */
  net_amount?: string;
  estimated_arrival?: string;
  /** Cut-off time before which the rail processes same business day. */
  cutoff_time?: string;
  review_tier?: string;
}

export async function feeEstimate(
  api: ApiClient,
  input: { amount: string; currency: string; rail: string; direction: string },
): Promise<FeeEstimate> {
  return api.post<FeeEstimate>('/funding/fee-estimate', input);
}

/** crypto.randomUUID with a Math.random fallback for jsdom. */
export function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) return crypto.randomUUID();
  return `idem-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
}

// ---------------------------------------------------------------------------
// Deposit intent (Task 11.3.3) — POST /api/v1/deposits
// ---------------------------------------------------------------------------

export interface DepositResult {
  deposit_id: number;
  status: string;
  currency: string;
  amount: string;
  usd_amount?: string;
  review_tier?: string;
  review_deadline?: string;
  confirmations?: number;
  flags?: string[];
  replayed?: boolean;
}

/** Client-declared inbound wire — Idempotency-Key required; replays
 * return the stored row (`replayed: true`, 200 instead of 201). */
export async function createDepositIntent(
  api: ApiClient,
  input: { currency: string; amount: string; reference?: string; bankMethod?: string },
): Promise<DepositResult> {
  return api.post<DepositResult>(
    '/deposits',
    {
      currency: input.currency,
      amount: input.amount,
      reference: input.reference,
      bank_method: input.bankMethod,
    },
    { idempotencyKey: newIdempotencyKey() },
  );
}

// ---------------------------------------------------------------------------
// Bank accounts / beneficiary registry (Task 11.3.7)
// ---------------------------------------------------------------------------

export interface BankAccount {
  bank_account_id: number;
  currency: string;
  iban?: string;
  account_number?: string;
  swift_bic?: string;
  bic_routing?: string;
  bank_name: string;
  beneficiary_name: string;
  rail: string;
  /** PENDING_VERIFICATION | VERIFIED | REJECTED — server-owned. */
  status: string;
  verification_method?: string;
  verified_at?: string;
  unlocked_at?: string;
  rejection_reason?: string;
  created_at?: string;
}

export interface BankAccountInput {
  currency: string;
  iban?: string;
  account_number?: string;
  swift_bic?: string;
  bic_routing?: string;
  bank_name: string;
  beneficiary_name: string;
  rail: string;
}

export async function bankAccounts(api: ApiClient): Promise<BankAccount[]> {
  const res = await api.get<{ bank_accounts?: BankAccount[] }>('/funding/bank-accounts');
  return res.bank_accounts ?? [];
}

export async function registerBankAccount(
  api: ApiClient,
  input: BankAccountInput,
): Promise<BankAccount> {
  return api.post<BankAccount>('/funding/bank-accounts', input);
}

/** Owner-scoped delete — query-form `?id=` (not `/{id}`). */
export async function deleteBankAccount(api: ApiClient, id: number): Promise<void> {
  await api.delete(`/funding/bank-accounts?id=${id}`);
}

// ---------------------------------------------------------------------------
// Rail capability matrix + selection preview (Task 11.3.1)
// ---------------------------------------------------------------------------

export interface RailCapability {
  rail: string;
  name: string;
  message_types?: string[];
  all_currencies?: boolean;
  currencies?: string[];
  cutoff_utc?: number;
  cutoff_label?: string;
  instant_capable?: boolean;
  max_amount?: string;
  cap_instant_only?: boolean;
  settlement_lag?: string;
  weekend_processing?: boolean;
}

export async function fundingRails(api: ApiClient): Promise<RailCapability[]> {
  const res = await api.get<{ rails?: RailCapability[] }>('/funding/rails');
  return res.rails ?? [];
}

export interface RailSelection {
  rail: string;
  capability?: RailCapability;
  value_date?: string;
  queued_next_day?: boolean;
  rejected_rails?: string[];
}

/** Fail-closed preview — BANKING_RAIL_UNAVAILABLE (503) /
 * RAIL_CUTOFF_EXCEEDED (422) surface as coded errors. */
export async function railSelection(
  api: ApiClient,
  input: { currency: string; amount: string; preferredRail?: string; requireSameDay?: boolean },
): Promise<RailSelection> {
  return api.post<RailSelection>('/funding/rail-selection', {
    currency: input.currency,
    amount: input.amount,
    preferred_rail: input.preferredRail,
    require_same_day: input.requireSameDay,
  });
}

// ---------------------------------------------------------------------------
// Withdrawal whitelist (Task 11.3.10)
// ---------------------------------------------------------------------------

export interface WhitelistView {
  mode: string; // ALLOW_ALL | WHITELIST_ONLY
  whitelist_only_enabled: boolean;
  timelock_until?: string;
  withdrawal_lock_until?: string;
  withdrawals_locked?: boolean;
  reenable_locked?: boolean;
  beneficiaries?: BankAccount[];
  updated_at?: string;
}

export async function withdrawalWhitelist(api: ApiClient): Promise<WhitelistView> {
  return api.get<WhitelistView>('/funding/withdrawal-whitelist');
}

/** Enable → WHITELIST_ONLY; disable → ALLOW_ALL + account-scoped 24h
 * egress lock (WHITELIST_CHANGE_LOCKED on premature re-enable). */
export async function setWhitelistMode(api: ApiClient, enable: boolean): Promise<WhitelistView> {
  return api.post<WhitelistView>(`/funding/withdrawal-whitelist/${enable ? 'enable' : 'disable'}`);
}

// ---------------------------------------------------------------------------
// Currency conversion (Task 11.3.9)
// ---------------------------------------------------------------------------

export interface ConversionRecord {
  id: number;
  direction: string; // DEPOSIT | WITHDRAWAL
  from_currency: string;
  to_currency: string;
  amount_from: string;
  mid_rate: string;
  spread_bps: string;
  rate_applied: string;
  amount_to: string;
  rate_source?: string;
  rate_valid_at?: string;
  funding_transaction_id?: number;
  created_at?: string;
}

export interface ConversionResult {
  converted: boolean;
  direction?: string;
  from_currency?: string;
  to_currency?: string;
  amount_from?: string;
  amount_to?: string;
  mid_rate?: string;
  spread_bps?: string;
  rate_applied?: string;
  rate_source?: string;
  rate_valid_at?: string;
  conversion?: ConversionRecord;
}

export async function convertFunds(
  api: ApiClient,
  input: { fromCurrency: string; toCurrency?: string; direction?: string; amount: string },
): Promise<ConversionResult> {
  return api.post<ConversionResult>('/funding/convert', {
    from_currency: input.fromCurrency,
    to_currency: input.toCurrency,
    direction: input.direction,
    amount: input.amount,
  });
}

export async function conversionHistory(api: ApiClient, limit = 50): Promise<ConversionRecord[]> {
  const res = await api.get<{ items?: ConversionRecord[] }>(`/funding/conversions?limit=${limit}`);
  return res.items ?? [];
}
