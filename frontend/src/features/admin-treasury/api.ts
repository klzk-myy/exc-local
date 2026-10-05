/**
 * admin-treasury wire seam (Phase-10.5 Task 10.5.3.8) — house-money and
 * segregated-funds operations.
 *
 *   GET  /api/v1/admin/funding/nostro                       coverage per currency
 *   GET/POST /api/v1/admin/nostro-accounts                  registry
 *   GET/POST /api/v1/admin/funding/nostro/replenishments    reserve→operating
 *   POST /api/v1/admin/funding/nostro/replenishments/{id}/decide  four-eyes
 *   GET  /api/v1/admin/swift-messages[?from=&to=&type=&direction=]
 *   GET  /api/v1/admin/nostro-reconciliation?date=          report
 *   POST /api/v1/admin/nostro-reconciliation/run            {date?,account_id?}
 *   POST /api/v1/admin/nostro-reconciliation/breaks/{id}/resolve  INVESTIGATE|RESOLVE
 *   GET  /api/v1/admin/pb-reconciliation?pb_id=&date=       give-up recon
 *   GET/POST /api/v1/admin/client-money/audits              engagements
 *   POST /api/v1/admin/client-money/audits/{id}/evidence-pack
 *   GET/POST /api/v1/admin/client-money/certifications      four-eyes issuance
 *   GET  /api/v1/admin/treasury/own-funds                   ledger + freeze flags
 *   GET/POST /api/v1/admin/treasury/contingent-capital      waterfall register
 *   GET  /api/v1/admin/insurance-fund[?currency=&cursor=]   balances + txns
 *   PUT  /api/v1/admin/collateral-schedule                  {rows:[…]} (Risk Mgr)
 *
 * Note: PBReconRun/PBReconBreak marshal with Go field names (no json
 * tags on the structs) — parsers here accept both PascalCase and
 * snake_case keys.
 */
import { malformed } from '@/lib/admin/api';
import type { ApiClient } from '@/lib/api/client';
import { ADMIN_ENV_HEADER, type AdminEnv, type BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const bool = (v: unknown): boolean | undefined => (typeof v === 'boolean' ? v : undefined);
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
/** Dual-case lookup — PB recon rows marshal PascalCase, everything else snake_case. */
const pick = (r: Record<string, unknown>, snake: string, pascal: string): unknown =>
  r[snake] ?? r[pascal];
const envOpts = (env: AdminEnv) => ({ headers: { [ADMIN_ENV_HEADER]: env } });

// ---------------------------------------------------------------------------
// Nostro coverage + registry + replenishment + SWIFT journal.
// ---------------------------------------------------------------------------

export interface NostroBalanceRow {
  id: number;
  bankName: string;
  bankCode?: string;
  iban?: string;
  balance: string;
  status: string;
}

export interface NostroCoverageRow {
  currency: string;
  nostroTotal: string;
  confirmedDue: string;
  queued: number;
  deficit: boolean;
  accounts: NostroBalanceRow[];
}

export async function fetchNostroCoverage(api: BoundAdminApi): Promise<NostroCoverageRow[]> {
  const raw = await api.get<unknown>('/admin/funding/nostro');
  if (!isRecord(raw)) throw malformed('nostro coverage');
  return arr(raw['currencies']).map((c) => {
    if (!isRecord(c)) throw malformed('coverage row');
    return {
      currency: str(c['currency']) ?? '',
      nostroTotal: str(c['nostro_total']) ?? '0',
      confirmedDue: str(c['confirmed_due']) ?? '0',
      queued: num(c['queued']) ?? 0,
      deficit: bool(c['deficit']) ?? false,
      accounts: arr(c['accounts']).flatMap((a) => {
        if (!isRecord(a)) return [];
        return [
          {
            id: num(a['id']) ?? 0,
            bankName: str(a['bank_name']) ?? '',
            bankCode: str(a['bank_code']),
            iban: str(a['iban']),
            balance: str(a['balance']) ?? '0',
            status: str(a['status']) ?? '',
          },
        ];
      }),
    };
  });
}

export interface NostroAccount {
  id: number;
  currency: string;
  bankName: string;
  bankCode?: string;
  accountNumber?: string;
  iban?: string;
  role: string;
  balance: string;
  status: string;
}

export async function fetchNostroAccounts(
  api: BoundAdminApi,
  f: { currency?: string; role?: string; status?: string },
): Promise<NostroAccount[]> {
  const q = new URLSearchParams();
  if (f.currency) q.set('currency', f.currency);
  if (f.role) q.set('role', f.role);
  if (f.status) q.set('status', f.status);
  const suffix = q.size > 0 ? `?${q.toString()}` : '';
  const raw = await api.get<unknown>(`/admin/nostro-accounts${suffix}`);
  if (!isRecord(raw)) throw malformed('nostro accounts');
  return arr(raw['accounts']).flatMap((a) => {
    if (!isRecord(a)) return [];
    return [
      {
        id: num(a['id']) ?? 0,
        currency: str(a['currency']) ?? '',
        bankName: str(a['bank_name']) ?? '',
        bankCode: str(a['bank_code']),
        accountNumber: str(a['account_number']),
        iban: str(a['iban']),
        role: str(a['role']) ?? '',
        balance: str(a['balance']) ?? '0',
        status: str(a['status']) ?? '',
      },
    ];
  });
}

export async function createNostroAccount(
  api: BoundAdminApi,
  input: {
    currency: string;
    bankName: string;
    bankCode?: string;
    accountNumber?: string;
    iban?: string;
    role?: string;
  },
): Promise<void> {
  await api.post('/admin/nostro-accounts', {
    currency: input.currency,
    bank_name: input.bankName,
    bank_code: input.bankCode ?? '',
    account_number: input.accountNumber ?? '',
    iban: input.iban ?? '',
    role: input.role ?? '',
  });
}

export interface ReplenishmentRow {
  id: number;
  currency: string;
  amount: string;
  sourceNostroId: number;
  targetNostroId: number;
  status: string;
  requestedBy?: number;
  approvedBy?: number;
  decisionNote?: string;
  createdAt: string;
}

export function parseReplenishment(v: unknown): ReplenishmentRow {
  if (!isRecord(v)) throw malformed('replenishment');
  return {
    id: num(v['id']) ?? 0,
    currency: str(v['currency']) ?? '',
    amount: str(v['amount']) ?? '0',
    sourceNostroId: num(v['source_nostro_id']) ?? 0,
    targetNostroId: num(v['target_nostro_id']) ?? 0,
    status: str(v['status']) ?? '',
    requestedBy: num(v['requested_by']),
    approvedBy: num(v['approved_by']),
    decisionNote: str(v['decision_note']),
    createdAt: str(v['created_at']) ?? '',
  };
}

export async function fetchReplenishments(api: BoundAdminApi): Promise<ReplenishmentRow[]> {
  const raw = await api.get<unknown>('/admin/funding/nostro/replenishments?limit=50');
  if (!isRecord(raw)) throw malformed('replenishments');
  return arr(raw['items']).map(parseReplenishment);
}

export async function requestReplenishment(
  api: BoundAdminApi,
  input: { currency: string; amount: string; sourceNostroId: number; targetNostroId: number },
): Promise<ReplenishmentRow> {
  const raw = await api.post<unknown>('/admin/funding/nostro/replenishments', {
    currency: input.currency,
    amount: input.amount,
    source_nostro_id: input.sourceNostroId,
    target_nostro_id: input.targetNostroId,
  });
  return parseReplenishment(raw);
}

export async function decideReplenishment(
  api: BoundAdminApi,
  id: number,
  input: { action: 'APPROVE' | 'REJECT'; note?: string },
): Promise<ReplenishmentRow> {
  const raw = await api.post<unknown>(`/admin/funding/nostro/replenishments/${id}/decide`, {
    action: input.action,
    note: input.note ?? '',
  });
  return parseReplenishment(raw);
}

export interface SwiftMessage {
  id: number;
  messageType: string;
  reference: string;
  relatedReference?: string;
  direction: string;
  status: string;
  msgTimestamp: string;
}

export async function fetchSwiftMessages(
  api: BoundAdminApi,
  f: { type?: string; direction?: string; limit?: number },
): Promise<SwiftMessage[]> {
  const q = new URLSearchParams();
  if (f.type) q.set('type', f.type);
  if (f.direction) q.set('direction', f.direction);
  q.set('limit', String(f.limit ?? 100));
  const raw = await api.get<unknown>(`/admin/swift-messages?${q.toString()}`);
  const items = isRecord(raw) ? (raw['items'] ?? raw['messages']) : raw;
  return arr(items).flatMap((m) => {
    if (!isRecord(m)) return [];
    return [
      {
        id: num(m['id']) ?? 0,
        messageType: str(m['message_type']) ?? '',
        reference: str(m['reference']) ?? '',
        relatedReference: str(m['related_reference']),
        direction: str(m['direction']) ?? '',
        status: str(m['status']) ?? '',
        msgTimestamp: str(m['msg_timestamp']) ?? '',
      },
    ];
  });
}

// ---------------------------------------------------------------------------
// Nostro + PB reconciliation.
// ---------------------------------------------------------------------------

export interface NostroReconRun {
  id: number;
  nostroAccountId: number;
  reconDate: string;
  ourNet: string;
  statementNet: string;
  difference: string;
  thresholdBreach: boolean;
  breaksOpened: number;
  status: string;
}

export interface NostroReconBreak {
  id: number;
  category: string;
  swiftReference?: string;
  currency?: string;
  expectedAmount?: string;
  actualAmount?: string;
  difference?: string;
  status: string;
  assignedTo?: number;
  detectedAt: string;
}

export interface NostroReconReport {
  date: string;
  runs: NostroReconRun[];
  breaks: NostroReconBreak[];
  accountsReconciled: number;
  openBreaks: number;
  thresholdBreaches: number;
}

function parseReconReport(raw: unknown): NostroReconReport {
  if (!isRecord(raw)) throw malformed('nostro recon report');
  return {
    date: str(raw['date']) ?? '',
    runs: arr(raw['runs']).flatMap((r) => {
      if (!isRecord(r)) return [];
      return [
        {
          id: num(r['id']) ?? 0,
          nostroAccountId: num(r['nostro_account_id']) ?? 0,
          reconDate: str(r['recon_date']) ?? '',
          ourNet: str(r['our_net']) ?? '0',
          statementNet: str(r['statement_net']) ?? '0',
          difference: str(r['difference']) ?? '0',
          thresholdBreach: bool(r['threshold_breach']) ?? false,
          breaksOpened: num(r['breaks_opened']) ?? 0,
          status: str(r['status']) ?? '',
        },
      ];
    }),
    breaks: arr(raw['breaks']).flatMap((b) => {
      if (!isRecord(b)) return [];
      return [
        {
          id: num(b['id']) ?? 0,
          category: str(b['category']) ?? '',
          swiftReference: str(b['swift_reference']),
          currency: str(b['currency']),
          expectedAmount: str(b['expected_amount']),
          actualAmount: str(b['actual_amount']),
          difference: str(b['difference']),
          status: str(b['status']) ?? '',
          assignedTo: num(b['assigned_to']),
          detectedAt: str(b['detected_at']) ?? '',
        },
      ];
    }),
    accountsReconciled: num(raw['accounts_reconciled']) ?? 0,
    openBreaks: num(raw['open_breaks']) ?? 0,
    thresholdBreaches: num(raw['threshold_breaches']) ?? 0,
  };
}

export async function fetchNostroRecon(
  api: BoundAdminApi,
  date?: string,
): Promise<NostroReconReport> {
  const raw = await api.get<unknown>(
    `/admin/nostro-reconciliation${date !== undefined && date !== '' ? `?date=${date}` : ''}`,
  );
  return parseReconReport(raw);
}

export async function runNostroRecon(
  api: BoundAdminApi,
  input: { date?: string; accountId?: number },
): Promise<NostroReconReport> {
  const raw = await api.post<unknown>('/admin/nostro-reconciliation/run', {
    date: input.date ?? '',
    account_id: input.accountId,
  });
  return parseReconReport(raw);
}

export async function actOnBreak(
  api: BoundAdminApi,
  breakId: number,
  input: { action: 'INVESTIGATE' | 'RESOLVE'; notes?: string },
): Promise<void> {
  await api.post(`/admin/nostro-reconciliation/breaks/${breakId}/resolve`, {
    action: input.action,
    notes: input.notes ?? '',
  });
}

export interface PbReconRun {
  id: number;
  source: string;
  tradesScanned: number;
  autoMatched: number;
  breaksDetected: number;
}

export interface PbReconBreak {
  id: number;
  type: string;
  status: string;
  resolutionNote?: string;
}

export interface PbReconReport {
  primeBrokerId: number;
  date: string;
  runs: PbReconRun[];
  openBreaks: PbReconBreak[];
  autoMatchRatePct: string;
}

export async function fetchPbRecon(
  api: BoundAdminApi,
  f: { pbId?: number; date?: string },
): Promise<PbReconReport> {
  const q = new URLSearchParams();
  if (f.pbId !== undefined && f.pbId > 0) q.set('pb_id', String(f.pbId));
  if (f.date !== undefined && f.date !== '') q.set('date', f.date);
  const suffix = q.size > 0 ? `?${q.toString()}` : '';
  const raw = await api.get<unknown>(`/admin/pb-reconciliation${suffix}`);
  if (!isRecord(raw)) throw malformed('pb recon report');
  return {
    primeBrokerId: num(raw['prime_broker_id']) ?? 0,
    date: str(raw['date']) ?? '',
    // PBReconRun/Break have no json tags — PascalCase on the wire.
    runs: arr(raw['runs']).flatMap((r) => {
      if (!isRecord(r)) return [];
      return [
        {
          id: num(pick(r, 'id', 'ID')) ?? 0,
          source: str(pick(r, 'source', 'Source')) ?? '',
          tradesScanned: num(pick(r, 'trades_scanned', 'TradesScanned')) ?? 0,
          autoMatched: num(pick(r, 'auto_matched', 'AutoMatched')) ?? 0,
          breaksDetected: num(pick(r, 'breaks_detected', 'BreaksDetected')) ?? 0,
        },
      ];
    }),
    openBreaks: arr(raw['open_breaks']).flatMap((b) => {
      if (!isRecord(b)) return [];
      return [
        {
          id: num(pick(b, 'id', 'ID')) ?? 0,
          type: str(pick(b, 'type', 'Type')) ?? '',
          status: str(pick(b, 'status', 'Status')) ?? '',
          resolutionNote: str(pick(b, 'resolution_note', 'ResolutionNote')),
        },
      ];
    }),
    autoMatchRatePct: str(raw['auto_match_rate_pct']) ?? '',
  };
}

// ---------------------------------------------------------------------------
// Client money (Task 24.3.18).
// ---------------------------------------------------------------------------

export interface ClientMoneyAudit {
  id: number;
  engagementYear: number;
  auditorFirm: string;
  scope: string;
  periodStart: string;
  periodEnd: string;
  status: string;
  independenceConfirmed: boolean;
  findings?: unknown;
}

export async function fetchClientMoneyAudits(
  api: BoundAdminApi,
  status?: string,
): Promise<ClientMoneyAudit[]> {
  const raw = await api.get<unknown>(
    `/admin/client-money/audits${status !== undefined && status !== '' ? `?status=${status}` : ''}`,
  );
  if (!isRecord(raw)) throw malformed('audits');
  return arr(raw['audits']).flatMap((a) => {
    if (!isRecord(a)) return [];
    return [
      {
        id: num(a['id']) ?? 0,
        engagementYear: num(a['engagement_year']) ?? 0,
        auditorFirm: str(a['auditor_firm']) ?? '',
        scope: str(a['scope']) ?? '',
        periodStart: str(a['period_start']) ?? '',
        periodEnd: str(a['period_end']) ?? '',
        status: str(a['status']) ?? '',
        independenceConfirmed: bool(a['independence_confirmed']) ?? false,
        findings: a['findings'],
      },
    ];
  });
}

export async function createClientMoneyAudit(
  api: BoundAdminApi,
  input: {
    engagementYear: number;
    auditorFirm: string;
    scope: string;
    periodStart: string;
    periodEnd: string;
  },
): Promise<void> {
  await api.post('/admin/client-money/audits', {
    engagement_year: input.engagementYear,
    auditor_firm: input.auditorFirm,
    scope: input.scope,
    period_start: input.periodStart,
    period_end: input.periodEnd,
  });
}

export interface EvidencePack {
  id: number;
  auditId: number;
  packSha256: string;
  assembledAt: string;
}

export async function assembleEvidencePack(
  api: BoundAdminApi,
  auditId: number,
): Promise<EvidencePack> {
  const raw = await api.post<unknown>(`/admin/client-money/audits/${auditId}/evidence-pack`, {});
  if (!isRecord(raw) || !isRecord(raw['evidence_pack'])) throw malformed('evidence pack');
  const p = raw['evidence_pack'];
  return {
    id: num(p['id']) ?? 0,
    auditId: num(p['audit_id']) ?? 0,
    packSha256: str(p['pack_sha256']) ?? '',
    assembledAt: str(p['assembled_at']) ?? '',
  };
}

export interface SegregationCert {
  id: number;
  auditId: number;
  evidencePackId?: number;
  statement: string;
  packSha256: string;
  issuedBy: number;
  approvedBy: number;
  publishedUntil: string;
  status: string;
}

export async function fetchCertifications(api: BoundAdminApi): Promise<SegregationCert[]> {
  const raw = await api.get<unknown>('/admin/client-money/certifications');
  if (!isRecord(raw)) throw malformed('certifications');
  return arr(raw['certifications']).flatMap((c) => {
    if (!isRecord(c)) return [];
    return [
      {
        id: num(c['id']) ?? 0,
        auditId: num(c['audit_id']) ?? 0,
        evidencePackId: num(c['evidence_pack_id']),
        statement: str(c['statement']) ?? '',
        packSha256: str(c['pack_sha256']) ?? '',
        issuedBy: num(c['issued_by']) ?? 0,
        approvedBy: num(c['approved_by']) ?? 0,
        publishedUntil: str(c['published_until']) ?? '',
        status: str(c['status']) ?? '',
      },
    ];
  });
}

export async function issueCertification(
  api: BoundAdminApi,
  input: {
    auditId: number;
    evidencePackId: number;
    statement: string;
    signatory: string;
    publishedUntil: string;
    approverId: number;
  },
): Promise<void> {
  await api.post('/admin/client-money/certifications', {
    audit_id: input.auditId,
    evidence_pack_id: input.evidencePackId,
    statement: input.statement,
    signatories: [{ name: input.signatory }],
    published_until: input.publishedUntil,
    approver_id: input.approverId,
  });
}

// ---------------------------------------------------------------------------
// Treasury + insurance fund + collateral schedule.
// ---------------------------------------------------------------------------

export interface OwnFundsRow {
  id: number;
  lineKind: string;
  currency: string;
  balance: string;
  reconciliationStatus: string;
  statementRef?: string;
  statementBalance?: string;
  reconciledAt?: string;
}

export interface TreasuryControls {
  discretionaryOutflowsFrozen: boolean;
  lpCapacityBlocked: boolean;
  frozenAt?: string;
  freezeReason?: string;
}

export interface OwnFundsPayload {
  rows: OwnFundsRow[];
  controls: TreasuryControls | null;
}

export async function fetchOwnFunds(api: BoundAdminApi): Promise<OwnFundsPayload> {
  const raw = await api.get<unknown>('/admin/treasury/own-funds');
  if (!isRecord(raw)) throw malformed('own funds');
  const ctrl = isRecord(raw['treasury_controls']) ? raw['treasury_controls'] : null;
  return {
    rows: arr(raw['own_funds']).flatMap((f) => {
      if (!isRecord(f)) return [];
      return [
        {
          id: num(f['id']) ?? 0,
          lineKind: str(f['line_kind']) ?? '',
          currency: str(f['currency']) ?? '',
          balance: str(f['balance']) ?? '0',
          reconciliationStatus: str(f['reconciliation_status']) ?? '',
          statementRef: str(f['statement_ref']),
          statementBalance: str(f['statement_balance']),
          reconciledAt: str(f['reconciled_at']),
        },
      ];
    }),
    controls:
      ctrl === null
        ? null
        : {
            discretionaryOutflowsFrozen: bool(ctrl['discretionary_outflows_frozen']) ?? false,
            lpCapacityBlocked: bool(ctrl['lp_capacity_blocked']) ?? false,
            frozenAt: str(ctrl['frozen_at']),
            freezeReason: str(ctrl['freeze_reason']),
          },
  };
}

export interface Commitment {
  id: number;
  providerName: string;
  kind: string;
  prioritySeq: number;
  committedAmount: string;
  drawnAmount: string;
  currency: string;
  activationTrigger: string;
  agreementRef: string;
  status: string;
  expiresAt?: string;
}

export async function fetchCommitments(api: BoundAdminApi): Promise<Commitment[]> {
  const raw = await api.get<unknown>('/admin/treasury/contingent-capital');
  if (!isRecord(raw)) throw malformed('commitments');
  return arr(raw['commitments']).flatMap((c) => {
    if (!isRecord(c)) return [];
    return [
      {
        id: num(c['id']) ?? 0,
        providerName: str(c['provider_name']) ?? '',
        kind: str(c['commitment_kind']) ?? '',
        prioritySeq: num(c['priority_seq']) ?? 0,
        committedAmount: str(c['committed_amount']) ?? '0',
        drawnAmount: str(c['drawn_amount']) ?? '0',
        currency: str(c['currency']) ?? '',
        activationTrigger: str(c['activation_trigger']) ?? '',
        agreementRef: str(c['agreement_ref']) ?? '',
        status: str(c['status']) ?? '',
        expiresAt: str(c['expires_at']),
      },
    ];
  });
}

export async function recordCommitment(
  api: BoundAdminApi,
  input: {
    providerName: string;
    kind: string;
    prioritySeq: number;
    committedAmount: string;
    currency: string;
    activationTrigger: string;
    drawWindowDays: number;
    agreementRef: string;
  },
): Promise<void> {
  await api.post('/admin/treasury/contingent-capital', {
    provider_name: input.providerName,
    commitment_kind: input.kind,
    priority_seq: input.prioritySeq,
    committed_amount: input.committedAmount,
    currency: input.currency,
    activation_trigger: input.activationTrigger,
    draw_window_days: input.drawWindowDays,
    agreement_ref: input.agreementRef,
  });
}

export interface FundBalance {
  currency: string;
  balance: string;
  depletionThreshold?: string;
}

export interface FundTx {
  id: number;
  currency: string;
  direction: string;
  amount: string;
  reason: string;
  balanceAfter: string;
  createdAt: string;
}

export interface InsuranceFundPayload {
  balances: FundBalance[];
  transactions: FundTx[];
}

export async function fetchInsuranceFund(
  api: BoundAdminApi,
  currency?: string,
): Promise<InsuranceFundPayload> {
  const raw = await api.get<unknown>(
    `/admin/insurance-fund${currency !== undefined && currency !== '' ? `?currency=${currency}` : ''}`,
  );
  if (!isRecord(raw)) throw malformed('insurance fund');
  const txEnv = isRecord(raw['transactions']) ? raw['transactions'] : {};
  return {
    balances: arr(raw['balances']).flatMap((b) => {
      if (!isRecord(b)) return [];
      return [
        {
          currency: str(b['currency']) ?? '',
          balance: str(b['balance']) ?? '0',
          depletionThreshold: str(b['depletion_threshold']),
        },
      ];
    }),
    transactions: arr(txEnv['data']).flatMap((t) => {
      if (!isRecord(t)) return [];
      return [
        {
          id: num(t['id']) ?? 0,
          currency: str(t['currency']) ?? '',
          direction: str(t['direction']) ?? '',
          amount: str(t['amount']) ?? '0',
          reason: str(t['reason']) ?? '',
          balanceAfter: str(t['balance_after']) ?? '0',
          createdAt: str(t['created_at']) ?? '',
        },
      ];
    }),
  };
}

export interface CollateralRow {
  currency: string;
  eligible: boolean;
  haircutPct: string;
  maxConcentrationPct: string;
}

export async function putCollateralSchedule(
  api: ApiClient,
  env: AdminEnv,
  rows: CollateralRow[],
): Promise<void> {
  await api.put(
    '/admin/collateral-schedule',
    {
      rows: rows.map((r) => ({
        currency: r.currency,
        eligible: r.eligible,
        haircut_pct: r.haircutPct,
        max_concentration_pct: r.maxConcentrationPct,
      })),
    },
    envOpts(env),
  );
}
