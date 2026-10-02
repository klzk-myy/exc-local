/**
 * PAMM pools (Phase-14 Task 14.3.8) — browse, detail, statement, invest,
 * redeem, manager create. Routes live:
 *   GET  /pamm/pools                 ACTIVE discovery (keyset ?after=)
 *   GET  /pamm/pools/{id}            detail + totals + my_allocation
 *   GET  /pamm/pools/{id}/statement  sub-ledger movements (keyset)
 *   POST /pamm/pools                 manager creates pool {name,currency,min_investment}
 *   POST /pamm/pools/{id}/invest     {amount} — PAMM_INVEST transfer
 *   POST /pamm/pools/{id}/redeem     {amount} — PAMM_REDEEM transfer
 *
 * Pool/Allocation marshal Go field names (no json tags server-side);
 * statement + summary rows carry snake_case tags.
 */
import type { ApiClient } from '@/lib/api';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;

export interface PammPool {
  poolId: number;
  managerAccountId: number;
  name: string;
  currency: string;
  minInvestment: string;
  status: string;
  createdAt: string;
}

export interface PammAllocation {
  allocationId: number;
  invested: string;
  status: string;
}

export interface PammPoolDetail {
  pool: PammPool;
  investorCount: number;
  totalInvested: string;
  myAllocation: PammAllocation | null;
}

export interface StatementEntry {
  entryId: number;
  txnType: string;
  direction: string;
  amount: string;
  currency: string;
  narrative: string;
  postedAt: string;
}

export function parsePool(v: unknown): PammPool | null {
  if (!isRecord(v)) return null;
  const poolId = num(v['PoolID']) ?? num(v['pool_id']);
  const name = str(v['Name']) ?? str(v['name']);
  if (poolId === undefined || !name) return null;
  return {
    poolId,
    managerAccountId: num(v['ManagerAccountID']) ?? num(v['manager_account_id']) ?? 0,
    name,
    currency: str(v['Currency']) ?? str(v['currency']) ?? '',
    minInvestment: str(v['MinInvestment']) ?? str(v['min_investment']) ?? '0',
    status: str(v['Status']) ?? str(v['status']) ?? '',
    createdAt: str(v['CreatedAt']) ?? str(v['created_at']) ?? '',
  };
}

export function parseAllocation(v: unknown): PammAllocation | null {
  if (!isRecord(v)) return null;
  const id = num(v['AllocationID']) ?? num(v['allocation_id']);
  const invested = str(v['Invested']) ?? str(v['invested']);
  if (id === undefined || invested === undefined) return null;
  return { allocationId: id, invested, status: str(v['Status']) ?? str(v['status']) ?? '' };
}

export function parsePoolDetail(v: unknown): PammPoolDetail | null {
  if (!isRecord(v)) return null;
  const pool = parsePool(v['pool']);
  if (!pool) return null;
  const total =
    str(v['total_invested']) ??
    (isRecord(v['total_invested']) ? str(v['total_invested']['value']) : undefined) ??
    '0';
  return {
    pool,
    investorCount: num(v['investor_count']) ?? 0,
    totalInvested: total,
    myAllocation: parseAllocation(v['my_allocation']),
  };
}

export function parseEntry(v: unknown): StatementEntry | null {
  if (!isRecord(v)) return null;
  const entryId = num(v['entry_id']);
  const txn = str(v['txn_type']);
  if (entryId === undefined || !txn) return null;
  return {
    entryId,
    txnType: txn,
    direction: str(v['direction']) ?? '',
    amount: str(v['amount']) ?? '0',
    currency: str(v['currency']) ?? '',
    narrative: str(v['narrative']) ?? '',
    postedAt: str(v['posted_at']) ?? '',
  };
}

export async function listPools(api: ApiClient): Promise<PammPool[]> {
  const res = await api.get<unknown>('/pamm/pools');
  const raw = isRecord(res) && Array.isArray(res['pools']) ? res['pools'] : [];
  return raw.map(parsePool).filter((p): p is PammPool => p !== null);
}

export async function poolDetail(api: ApiClient, poolId: number): Promise<PammPoolDetail | null> {
  return parsePoolDetail(await api.get<unknown>(`/pamm/pools/${poolId}`));
}

export async function poolStatement(api: ApiClient, poolId: number): Promise<StatementEntry[]> {
  const res = await api.get<unknown>(`/pamm/pools/${poolId}/statement`);
  const raw = isRecord(res) && Array.isArray(res['entries']) ? res['entries'] : [];
  return raw.map(parseEntry).filter((e): e is StatementEntry => e !== null);
}

export async function createPool(
  api: ApiClient,
  input: { name: string; currency: string; minInvestment: string },
): Promise<PammPool | null> {
  const res = await api.post<unknown>('/pamm/pools', {
    name: input.name,
    currency: input.currency,
    min_investment: input.minInvestment,
  });
  return parsePool(res);
}

export async function invest(api: ApiClient, poolId: number, amount: string): Promise<void> {
  await api.post(`/pamm/pools/${poolId}/invest`, { amount });
}

export async function redeem(api: ApiClient, poolId: number, amount: string): Promise<void> {
  await api.post(`/pamm/pools/${poolId}/redeem`, { amount });
}
