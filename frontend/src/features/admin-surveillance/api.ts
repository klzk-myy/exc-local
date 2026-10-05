/**
 * admin-surveillance wire seam (Phase-10.5 Task 10.5.3.6) — the
 * Phase-17→21 signal-to-case pipeline plus SAR lifecycle, AML program
 * register, sanctions provider ops and MiFID II comms recordings.
 *
 *   GET  /api/v1/admin/surveillance/summary?month=YYYY-MM
 *   GET  /api/v1/admin/surveillance/cases[?status=&assignee=]
 *   GET  /api/v1/admin/surveillance/cases/{id}
 *   POST /api/v1/admin/surveillance/cases/{id}/assign      {assignee}
 *   POST /api/v1/admin/surveillance/cases/{id}/disposition {disposition,reason,action}
 *   POST /api/v1/admin/surveillance/cases/{id}/evidence    {kind,body,attachment_ref,sha256}
 *   GET  /api/v1/admin/sar[?status=]                       → {reports}
 *   POST /api/v1/admin/sar                                 → 201|200 {sar,created}
 *   GET  /api/v1/admin/sar/{id}                            → {sar}
 *   POST /api/v1/admin/sar/{id}/review|approve             {note}
 *   POST /api/v1/admin/sar/{id}/file                       {filing_ref}
 *   POST /api/v1/admin/sar/{id}/reject                     {reason}
 *   GET  /api/v1/admin/ctr[?status=]                       → {reports}
 *   GET  /api/v1/admin/aml/program|monitoring|artifacts
 *   POST /api/v1/admin/aml/artifacts                       → 201 {artifact}
 *   GET  /api/v1/admin/comms-recordings[?account_id=]      → {recordings}
 *   POST /api/v1/admin/comms-recordings/{id}/retrieve      dual-control
 *   POST /api/v1/admin/comms-recordings/verify-day         {day}
 *   POST /api/v1/admin/sanctions/refresh                   vendor pull
 *   POST /api/v1/admin/sanctions/queue/replay              pending drain
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
const numList = (v: unknown): number[] =>
  arr(v).filter((x): x is number => typeof x === 'number' && Number.isFinite(x));

// ---------------------------------------------------------------------------
// Surveillance cases (Task 21.3.21).
// ---------------------------------------------------------------------------

export const CASE_DISPOSITIONS = [
  'FALSE_POSITIVE',
  'ESCALATE_SAR',
  'ESCALATE_STR',
  'ESCALATE_ACTION',
] as const;

export interface SurveillanceCase {
  id: number;
  caseRef: string;
  signalId?: number;
  accountId?: number;
  signalType: string;
  symbol: string;
  severity: string;
  status: string;
  assignedTo?: number;
  slaDeadline: string;
  slaBreached: boolean;
  escalatedSarId?: number;
  openedAt: string;
  closedAt?: string;
}

export interface CaseEvidence {
  id: number;
  kind: string;
  body: string;
  attachmentRef?: string;
  sha256?: string;
  addedBy: number;
  createdAt: string;
}

export interface CaseWorkspace {
  case: SurveillanceCase;
  evidence: CaseEvidence[];
  linkedSignals: number[];
  orderAuditIds: number[];
}

export interface CaseMonthlySummary {
  month: string;
  opened: number;
  closed: number;
  escalated: number;
  falsePositives: number;
  avgDispositionHours: number;
  accuracyBySignal: Record<string, number>;
  openBreached: number;
}

function parseCase(v: unknown): SurveillanceCase | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    caseRef: str(v['case_ref']) ?? '',
    signalId: num(v['signal_id']),
    accountId: num(v['account_id']),
    signalType: str(v['signal_type']) ?? '',
    symbol: str(v['symbol']) ?? '',
    severity: str(v['severity']) ?? '',
    status: str(v['status']) ?? '',
    assignedTo: num(v['assigned_to']),
    slaDeadline: str(v['sla_deadline']) ?? '',
    slaBreached: v['sla_breached'] === true,
    escalatedSarId: num(v['escalated_sar_id']),
    openedAt: str(v['opened_at']) ?? '',
    closedAt: str(v['closed_at']),
  };
}

export async function fetchCases(api: BoundAdminApi, status?: string): Promise<SurveillanceCase[]> {
  const raw = await api.get<unknown>('/admin/surveillance/cases', {
    status: status === '' ? undefined : status,
    limit: 100,
  });
  if (!isRecord(raw) || !Array.isArray(raw['cases'])) throw malformed('surveillance cases');
  return (raw['cases'] as unknown[])
    .map(parseCase)
    .filter((c): c is SurveillanceCase => c !== null);
}

export async function fetchCaseWorkspace(api: BoundAdminApi, id: number): Promise<CaseWorkspace> {
  const raw = await api.get<unknown>(`/admin/surveillance/cases/${id}`);
  if (!isRecord(raw)) throw malformed('case workspace');
  const c = parseCase(raw['case']);
  if (c === null) throw malformed('case workspace');
  const evidence: CaseEvidence[] = [];
  for (const ev of arr(raw['evidence'])) {
    if (!isRecord(ev)) continue;
    evidence.push({
      id: num(ev['id']) ?? 0,
      kind: str(ev['kind']) ?? '',
      body: str(ev['body']) ?? '',
      attachmentRef: str(ev['attachment_ref']),
      sha256: str(ev['sha256']),
      addedBy: num(ev['added_by']) ?? 0,
      createdAt: str(ev['created_at']) ?? '',
    });
  }
  return {
    case: c,
    evidence,
    linkedSignals: numList(raw['linked_signals']),
    orderAuditIds: numList(raw['order_audit_ids']),
  };
}

export async function fetchSurveillanceSummary(
  api: BoundAdminApi,
  month?: string,
): Promise<CaseMonthlySummary> {
  const raw = await api.get<unknown>('/admin/surveillance/summary', {
    month: month === '' ? undefined : month,
  });
  if (!isRecord(raw)) throw malformed('surveillance summary');
  const accuracy: Record<string, number> = {};
  if (isRecord(raw['accuracy_by_signal'])) {
    for (const [k, v] of Object.entries(raw['accuracy_by_signal'])) {
      const n = num(v);
      if (n !== undefined) accuracy[k] = n;
    }
  }
  return {
    month: str(raw['month']) ?? '',
    opened: num(raw['opened']) ?? 0,
    closed: num(raw['closed']) ?? 0,
    escalated: num(raw['escalated']) ?? 0,
    falsePositives: num(raw['false_positives']) ?? 0,
    avgDispositionHours: num(raw['avg_disposition_hours']) ?? 0,
    accuracyBySignal: accuracy,
    openBreached: num(raw['open_breached']) ?? 0,
  };
}

export async function assignCase(api: BoundAdminApi, id: number, assignee: number): Promise<void> {
  await api.post(`/admin/surveillance/cases/${id}/assign`, { assignee });
}

export async function disposeCase(
  api: BoundAdminApi,
  id: number,
  input: { disposition: string; reason: string; action?: string },
): Promise<SurveillanceCase> {
  const raw = await api.post<unknown>(`/admin/surveillance/cases/${id}/disposition`, {
    disposition: input.disposition,
    reason: input.reason,
    action: input.action ?? '',
  });
  const c = isRecord(raw) ? parseCase(raw['case']) : null;
  if (c === null) throw malformed('case disposition');
  return c;
}

export async function attachEvidence(
  api: BoundAdminApi,
  id: number,
  input: { kind: string; body: string; attachmentRef?: string; sha256?: string },
): Promise<void> {
  await api.post(`/admin/surveillance/cases/${id}/evidence`, {
    kind: input.kind,
    body: input.body,
    attachment_ref: input.attachmentRef ?? '',
    sha256: input.sha256 ?? '',
  });
}

// ---------------------------------------------------------------------------
// SAR lifecycle (Task 21.3.3) + CTR register (Task 21.3.6).
// ---------------------------------------------------------------------------

export interface SarReport {
  id: number;
  triggerType: string;
  accountId?: number;
  subjectRef?: string;
  description: string;
  status: string;
  sourceRef: string;
  detectedAt: string;
  filingDeadline: string;
  filingRef?: string;
  amendsId?: number;
}

export interface CtrReport {
  id: number;
  accountId: number;
  businessDate: string;
  txnCount: number;
  totalUsd?: string;
  status: string;
  sarId?: number;
}

function parseSar(v: unknown): SarReport | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    triggerType: str(v['trigger_type']) ?? '',
    accountId: num(v['account_id']),
    subjectRef: str(v['subject_ref']),
    description: str(v['description']) ?? '',
    status: str(v['status']) ?? '',
    sourceRef: str(v['source_ref']) ?? '',
    detectedAt: str(v['detected_at']) ?? '',
    filingDeadline: str(v['filing_deadline']) ?? '',
    filingRef: str(v['filing_ref']),
    amendsId: num(v['amends_id']),
  };
}

export async function fetchSars(api: BoundAdminApi, status?: string): Promise<SarReport[]> {
  const raw = await api.get<unknown>('/admin/sar', {
    status: status === '' ? undefined : status,
    limit: 100,
  });
  if (!isRecord(raw) || !Array.isArray(raw['reports'])) throw malformed('sar list');
  return (raw['reports'] as unknown[]).map(parseSar).filter((r): r is SarReport => r !== null);
}

export async function createSarDraft(
  api: BoundAdminApi,
  input: { accountId?: number; subjectRef: string; description: string; sourceRef: string },
): Promise<{ sar: SarReport; created: boolean }> {
  const raw = await api.post<unknown>('/admin/sar', {
    account_id: input.accountId,
    subject_ref: input.subjectRef,
    description: input.description,
    source_ref: input.sourceRef,
  });
  if (!isRecord(raw)) throw malformed('sar draft');
  const sar = parseSar(raw['sar']);
  if (sar === null) throw malformed('sar draft');
  return { sar, created: raw['created'] !== false };
}

export async function sarAction(
  api: BoundAdminApi,
  id: number,
  action: 'review' | 'approve' | 'file' | 'reject',
  body: Record<string, string>,
): Promise<SarReport> {
  const raw = await api.post<unknown>(`/admin/sar/${id}/${action}`, body);
  const sar = isRecord(raw) ? parseSar(raw['sar']) : null;
  if (sar === null) throw malformed('sar action');
  return sar;
}

export async function fetchCtrs(api: BoundAdminApi): Promise<CtrReport[]> {
  const raw = await api.get<unknown>('/admin/ctr', { limit: 50 });
  if (!isRecord(raw) || !Array.isArray(raw['reports'])) throw malformed('ctr list');
  const out: CtrReport[] = [];
  for (const v of raw['reports'] as unknown[]) {
    if (!isRecord(v) || num(v['id']) === undefined) continue;
    out.push({
      id: num(v['id']) ?? 0,
      accountId: num(v['account_id']) ?? 0,
      businessDate: str(v['business_date']) ?? '',
      txnCount: num(v['txn_count']) ?? 0,
      totalUsd:
        v['total_usd'] === undefined || v['total_usd'] === null
          ? undefined
          : typeof v['total_usd'] === 'string'
            ? v['total_usd']
            : JSON.stringify(v['total_usd']),
      status: str(v['status']) ?? '',
      sarId: num(v['sar_id']),
    });
  }
  return out;
}

// ---------------------------------------------------------------------------
// AML program (Task 21.3.6).
// ---------------------------------------------------------------------------

export const ARTIFACT_TYPES = [
  'MSB_REGISTRATION',
  'RISK_ASSESSMENT',
  'POLICY',
  'TRAINING',
  'OFFICER_DESIGNATION',
  'ANNUAL_REVIEW',
] as const;

export interface AmlArtifact {
  id: number;
  artifactType: string;
  reference?: string;
  title: string;
  version?: string;
  status: string;
  reviewDueAt?: string;
}

export interface AmlProgram {
  compliant: boolean;
  breaches: string[];
  code?: string;
  artifacts: AmlArtifact[];
}

export interface AmlMonitoringEvent {
  id: number;
  ruleId: string;
  accountId: number;
  businessDate: string;
  score: number;
  sarId?: number;
}

function parseArtifact(v: unknown): AmlArtifact | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    artifactType: str(v['artifact_type']) ?? '',
    reference: str(v['reference']),
    title: str(v['title']) ?? '',
    version: str(v['version']),
    status: str(v['status']) ?? '',
    reviewDueAt: str(v['review_due_at']),
  };
}

export async function fetchAmlProgram(api: BoundAdminApi): Promise<AmlProgram> {
  const raw = await api.get<unknown>('/admin/aml/program');
  if (!isRecord(raw) || !isRecord(raw['program'])) throw malformed('aml program');
  const p = raw['program'];
  return {
    compliant: p['compliant'] === true,
    breaches: strList(p['breaches']),
    code: str(p['code']),
    artifacts: arr(p['artifacts'])
      .map(parseArtifact)
      .filter((a): a is AmlArtifact => a !== null),
  };
}

export async function fetchAmlArtifacts(api: BoundAdminApi): Promise<AmlArtifact[]> {
  const raw = await api.get<unknown>('/admin/aml/artifacts', { limit: 100 });
  if (!isRecord(raw) || !Array.isArray(raw['artifacts'])) throw malformed('aml artifacts');
  return (raw['artifacts'] as unknown[])
    .map(parseArtifact)
    .filter((a): a is AmlArtifact => a !== null);
}

export async function registerAmlArtifact(
  api: BoundAdminApi,
  input: {
    artifactType: string;
    title: string;
    reference?: string;
    version?: string;
    effectiveAt?: string;
    reviewDueAt?: string;
  },
): Promise<void> {
  await api.post('/admin/aml/artifacts', {
    artifact_type: input.artifactType,
    title: input.title,
    reference: input.reference ?? '',
    version: input.version ?? '',
    effective_at: input.effectiveAt,
    review_due_at: input.reviewDueAt,
  });
}

export async function fetchAmlMonitoring(
  api: BoundAdminApi,
  accountId?: number,
): Promise<AmlMonitoringEvent[]> {
  const raw = await api.get<unknown>('/admin/aml/monitoring', {
    account_id: accountId === undefined || accountId <= 0 ? undefined : accountId,
    limit: 50,
  });
  if (!isRecord(raw) || !Array.isArray(raw['events'])) throw malformed('aml monitoring');
  const out: AmlMonitoringEvent[] = [];
  for (const v of raw['events'] as unknown[]) {
    if (!isRecord(v) || num(v['id']) === undefined) continue;
    out.push({
      id: num(v['id']) ?? 0,
      ruleId: str(v['rule_id']) ?? '',
      accountId: num(v['account_id']) ?? 0,
      businessDate: str(v['business_date']) ?? '',
      score: num(v['score']) ?? 0,
      sarId: num(v['sar_id']),
    });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Comms recordings (Task 21.3.20) — WORM register + dual-control read.
// ---------------------------------------------------------------------------

export interface CommsRecording {
  recordingId: number;
  accountId?: number;
  channel: string;
  direction: string;
  source: string;
  startedAt: string;
  endedAt: string;
  contentRef: string;
  sha256: string;
  chainHash: string;
  retentionUntil: string;
  sealed: boolean;
}

function parseRecording(v: unknown): CommsRecording | null {
  if (!isRecord(v)) return null;
  const id = num(v['recording_id']);
  if (id === undefined) return null;
  return {
    recordingId: id,
    accountId: num(v['account_id']),
    channel: str(v['channel']) ?? '',
    direction: str(v['direction']) ?? '',
    source: str(v['source']) ?? '',
    startedAt: str(v['started_at']) ?? '',
    endedAt: str(v['ended_at']) ?? '',
    contentRef: str(v['content_ref']) ?? '',
    sha256: str(v['sha256']) ?? '',
    chainHash: str(v['chain_hash']) ?? '',
    retentionUntil: str(v['retention_until']) ?? '',
    sealed: v['sealed'] === true,
  };
}

export async function fetchCommsRecordings(
  api: BoundAdminApi,
  accountId?: number,
): Promise<CommsRecording[]> {
  const raw = await api.get<unknown>('/admin/comms-recordings', {
    account_id: accountId === undefined || accountId <= 0 ? undefined : accountId,
    limit: 100,
  });
  if (!isRecord(raw) || !Array.isArray(raw['recordings'])) throw malformed('comms recordings');
  return (raw['recordings'] as unknown[])
    .map(parseRecording)
    .filter((r): r is CommsRecording => r !== null);
}

export interface RetrievedRecording {
  recording: CommsRecording;
  bodyPreview: string;
}

export async function retrieveCommsRecording(
  api: BoundAdminApi,
  id: number,
  input: { approverId: number; justification: string; caseRef?: string },
): Promise<RetrievedRecording> {
  const raw = await api.post<unknown>(`/admin/comms-recordings/${id}/retrieve`, {
    approver_id: input.approverId,
    justification: input.justification,
    case_ref: input.caseRef ?? '',
  });
  if (!isRecord(raw)) throw malformed('comms retrieve');
  const recording = parseRecording(raw['recording']);
  const body = raw['body'];
  return {
    recording: recording ?? {
      recordingId: id,
      channel: '',
      direction: '',
      source: '',
      startedAt: '',
      endedAt: '',
      contentRef: '',
      sha256: '',
      chainHash: '',
      retentionUntil: '',
      sealed: false,
    },
    bodyPreview:
      typeof body === 'string'
        ? body
        : body === undefined || body === null
          ? ''
          : JSON.stringify(body),
  };
}

export async function verifyCommsDay(
  api: BoundAdminApi,
  day: string,
): Promise<{ day: string; chainOk: boolean }> {
  const raw = await api.post<unknown>('/admin/comms-recordings/verify-day', { day });
  if (!isRecord(raw)) throw malformed('comms verify-day');
  return { day: str(raw['day']) ?? day, chainOk: raw['chain_ok'] === true };
}

// ---------------------------------------------------------------------------
// Sanctions ops — provider refresh + pending-queue replay (Task 21.3.1).
// Status read is shared from admin-compliance (fetchSanctionsStatus).
// ---------------------------------------------------------------------------

export async function refreshSanctions(api: BoundAdminApi): Promise<Record<string, unknown>> {
  const raw = await api.post<unknown>('/admin/sanctions/refresh', {});
  if (!isRecord(raw)) throw malformed('sanctions refresh');
  return raw;
}

export async function replaySanctionsQueue(api: BoundAdminApi): Promise<Record<string, unknown>> {
  const raw = await api.post<unknown>('/admin/sanctions/queue/replay', {});
  if (!isRecord(raw)) throw malformed('sanctions replay');
  return raw;
}

// ---------------------------------------------------------------------------
// Detection tuning (Phase-21 Task 21.3.27) — versioned signal-parameter
// proposals: DRAFT → atomic ACTIVE swap + FP-rate backtest.
// ---------------------------------------------------------------------------

export interface TuningVersion {
  signalType: string;
  version: number;
  status: string;
  fpTargetPct?: number;
}

export async function fetchTuning(api: BoundAdminApi): Promise<TuningVersion[]> {
  const raw = await api.get<unknown>('/admin/surveillance/tuning', { limit: '100' });
  const rows = isRecord(raw) && Array.isArray(raw['tuning']) ? raw['tuning'] : [];
  return rows.map((v) => {
    const r = isRecord(v) ? v : {};
    return {
      signalType: str(r['signal_type']) ?? str(r['signal']) ?? '',
      version: num(r['version']) ?? 0,
      status: str(r['status']) ?? '',
      fpTargetPct: num(r['fp_target_pct']),
    };
  });
}

export async function proposeTuning(
  api: BoundAdminApi,
  input: { signalType: string; params: string; fpTargetPct: number },
): Promise<unknown> {
  return api.post<unknown>('/admin/surveillance/tuning', {
    signal_type: input.signalType,
    params: JSON.parse(input.params) as unknown,
    fp_target_pct: input.fpTargetPct,
  });
}

export const activateTuning = (
  api: BoundAdminApi,
  signal: string,
  version: number,
): Promise<unknown> =>
  api.post<unknown>(`/admin/surveillance/tuning/${encodeURIComponent(signal)}/activate`, {
    version,
  });

export const backtestTuning = (api: BoundAdminApi, signal: string): Promise<unknown> =>
  api.get<unknown>(`/admin/surveillance/tuning/${encodeURIComponent(signal)}/backtest`);
