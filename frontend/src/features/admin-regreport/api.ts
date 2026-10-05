/**
 * admin-regreport wire seam (Phase-10.5 Task 10.5.3.13) — regulatory
 * reporting desk: submissions lifecycle + RTS 27/28 production +
 * regime reports.
 *
 *   GET   /api/v1/admin/regreporting/queue[?regime=]   merged repair queue
 *   GET   /api/v1/admin/regreporting/events[?regime=&from=&to=]
 *   GET   /api/v1/admin/regreporting/submissions
 *   POST  /api/v1/admin/regreporting/breaks/{id}/resolve
 *   POST  /api/v1/admin/regreporting/submissions/{id}/resubmit  (201 correction)
 *   POST  /api/v1/admin/regreporting/{acks,party-identifiers,reconcile}
 *   GET   /api/v1/admin/emir-report                    EMIR-pinned event export
 *   GET   /api/v1/admin/mifid-report                   {rts27_reports,rts28_reports}
 *   GET   /api/v1/admin/basel-report[?period=|versions=1]
 *   GET   /api/v1/admin/compliance-report?type=&from=&to=
 *   GET   /api/v1/admin/bestexec/{rts27,rts28}[?status=]
 *   POST  /api/v1/admin/bestexec/rts27/{generate,materialize}
 *   POST  /api/v1/admin/bestexec/rts28/generate
 *   POST  /api/v1/admin/bestexec/{rts27,rts28}/{id}/publish
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

// ---------------------------------------------------------------------------
// Repair queue + breaks + submissions.
// ---------------------------------------------------------------------------

export interface RegBreak {
  breakId: number;
  eventId?: number;
  uti?: string;
  regime?: string;
  breakType: string;
  status: string;
  detectedBy: string;
  slaDueAt: string;
  detectedAt: string;
}

export interface TransportItem {
  kind: string;
  id?: number;
  raw: Record<string, unknown>;
}

export interface RegQueue {
  breaks: RegBreak[];
  transport: TransportItem[];
}

export async function fetchRegQueue(api: BoundAdminApi, regime?: string): Promise<RegQueue> {
  const raw = await api.get<unknown>(
    `/admin/regreporting/queue${regime !== undefined && regime !== '' ? `?regime=${regime}` : ''}`,
  );
  if (!isRecord(raw)) throw malformed('reg queue');
  return {
    breaks: arr(raw['breaks']).flatMap((b) => {
      if (!isRecord(b)) return [];
      return [
        {
          breakId: num(b['break_id']) ?? 0,
          eventId: num(b['event_id']),
          uti: str(b['uti']),
          regime: str(b['regime']),
          breakType: str(b['break_type']) ?? '',
          status: str(b['status']) ?? '',
          detectedBy: str(b['detected_by']) ?? '',
          slaDueAt: str(b['sla_due_at']) ?? '',
          detectedAt: str(b['detected_at']) ?? '',
        },
      ];
    }),
    transport: arr(raw['transport']).flatMap((t) => {
      if (!isRecord(t)) return [];
      return [
        {
          kind: str(t['kind']) ?? str(t['status']) ?? 'transport',
          id: num(t['id']) ?? num(t['submission_id']),
          raw: t,
        },
      ];
    }),
  };
}

export async function resolveBreak(
  api: BoundAdminApi,
  id: number,
  resolution: 'RESOLVED' | 'WONT_FIX',
  notes: string,
): Promise<boolean> {
  const raw = await api.post<unknown>(`/admin/regreporting/breaks/${id}/resolve`, {
    resolution,
    notes,
  });
  if (!isRecord(raw)) throw malformed('break resolve');
  return raw['applied'] === true;
}

export interface RegEvent {
  eventId: number;
  uti: string;
  regime: string;
  actionType: string;
  eventType: string;
  reportSeq: number;
  instrumentCode: string;
  accountId?: number;
}

export async function fetchRegEvents(
  api: BoundAdminApi,
  f: { regime?: string; from?: string; to?: string; pinned?: 'EMIR' },
): Promise<RegEvent[]> {
  const path =
    f.pinned === 'EMIR'
      ? '/admin/emir-report'
      : `/admin/regreporting/events${(() => {
          const q = new URLSearchParams();
          if (f.regime) q.set('regime', f.regime);
          if (f.from) q.set('from', f.from);
          if (f.to) q.set('to', f.to);
          return q.size > 0 ? `?${q.toString()}` : '';
        })()}`;
  const raw = await api.get<unknown>(path);
  if (!isRecord(raw)) throw malformed('reg events');
  return arr(raw['events']).flatMap((e) => {
    if (!isRecord(e)) return [];
    return [
      {
        eventId: num(e['event_id']) ?? 0,
        uti: str(e['uti']) ?? '',
        regime: str(e['regime']) ?? '',
        actionType: str(e['action_type']) ?? '',
        eventType: str(e['event_type']) ?? '',
        reportSeq: num(e['report_seq']) ?? 0,
        instrumentCode: str(e['instrument_code']) ?? '',
        accountId: num(e['account_id']),
      },
    ];
  });
}

export interface RegSubmission {
  id: number;
  eventId: number;
  regime: string;
  destination: string;
  attempt: number;
  status: string;
  externalRef?: string;
  errorCode?: string;
  errorText?: string;
  createdAt: string;
}

export async function fetchRegSubmissions(api: BoundAdminApi): Promise<RegSubmission[]> {
  const raw = await api.get<unknown>('/admin/regreporting/submissions');
  if (!isRecord(raw)) throw malformed('submissions');
  return arr(raw['submissions']).flatMap((s) => {
    if (!isRecord(s)) return [];
    return [
      {
        id: num(s['report_submission_id']) ?? 0,
        eventId: num(s['event_id']) ?? 0,
        regime: str(s['regime']) ?? '',
        destination: str(s['destination']) ?? '',
        attempt: num(s['attempt']) ?? 0,
        status: str(s['status']) ?? '',
        externalRef: str(s['external_ref']),
        errorCode: str(s['error_code']),
        errorText: str(s['error_text']),
        createdAt: str(s['created_at']) ?? '',
      },
    ];
  });
}

/** Corrected resubmission — creates a NEW submission row (201). */
export async function resubmitSubmission(
  api: BoundAdminApi,
  id: number,
  corrections: Record<string, unknown>,
): Promise<number> {
  const raw = await api.post<unknown>(`/admin/regreporting/submissions/${id}/resubmit`, {
    corrections,
  });
  if (!isRecord(raw)) throw malformed('resubmit');
  const sub = raw['submission'];
  if (!isRecord(sub)) return 0;
  return num(sub['report_submission_id']) ?? 0;
}

export async function ingestAck(
  api: BoundAdminApi,
  input: {
    reportSubmissionId: number;
    externalRef: string;
    status: string;
    code: string;
    text: string;
  },
): Promise<void> {
  await api.post('/admin/regreporting/acks', {
    report_submission_id: input.reportSubmissionId,
    external_ref: input.externalRef,
    status: input.status,
    code: input.code,
    text: input.text,
  });
}

export async function upsertPartyIdentifiers(
  api: BoundAdminApi,
  input: {
    accountId: number;
    lei: string;
    nationalIdType: string;
    nationalId: string;
    decisionMakerId: string;
    decisionMakerType: string;
  },
): Promise<void> {
  await api.post('/admin/regreporting/party-identifiers', {
    account_id: input.accountId,
    lei: input.lei,
    national_id_type: input.nationalIdType,
    national_id: input.nationalId,
    decision_maker_id: input.decisionMakerId,
    decision_maker_type: input.decisionMakerType,
  });
}

export async function runReconcile(api: BoundAdminApi): Promise<unknown> {
  const raw = await api.post<unknown>('/admin/regreporting/reconcile', {});
  if (!isRecord(raw)) throw malformed('reconcile');
  return raw['report'];
}

// ---------------------------------------------------------------------------
// RTS 27 / RTS 28 best execution.
// ---------------------------------------------------------------------------

export interface RTS27Report {
  id: number;
  quarterStart: string;
  instrumentClass: string;
  version: number;
  status: string;
  daysCovered: number;
  zeroActivity: boolean;
  publishedAt?: string;
}

export interface RTS28Report {
  id: number;
  year: number;
  instrumentClass: string;
  version: number;
  status: string;
  publishedAt?: string;
}

export async function fetchRTS27(api: BoundAdminApi, status?: string): Promise<RTS27Report[]> {
  const raw = await api.get<unknown>(
    `/admin/bestexec/rts27${status !== undefined && status !== '' ? `?status=${status}` : ''}`,
  );
  if (!isRecord(raw)) throw malformed('rts27 list');
  return arr(raw['reports']).flatMap((r) => {
    if (!isRecord(r)) return [];
    return [
      {
        id: num(r['id']) ?? 0,
        quarterStart: str(r['quarter_start']) ?? '',
        instrumentClass: str(r['instrument_class']) ?? '',
        version: num(r['version']) ?? 0,
        status: str(r['status']) ?? '',
        daysCovered: num(r['days_covered']) ?? 0,
        zeroActivity: r['zero_activity'] === true,
        publishedAt: str(r['published_at']),
      },
    ];
  });
}

export async function fetchRTS28(api: BoundAdminApi): Promise<RTS28Report[]> {
  const raw = await api.get<unknown>('/admin/bestexec/rts28');
  if (!isRecord(raw)) throw malformed('rts28 list');
  return arr(raw['reports']).flatMap((r) => {
    if (!isRecord(r)) return [];
    return [
      {
        id: num(r['id']) ?? 0,
        year: num(r['year']) ?? 0,
        instrumentClass: str(r['instrument_class']) ?? '',
        version: num(r['version']) ?? 0,
        status: str(r['status']) ?? '',
        publishedAt: str(r['published_at']),
      },
    ];
  });
}

export async function materializeRTS27(api: BoundAdminApi, day?: string): Promise<void> {
  await api.post(
    '/admin/bestexec/rts27/materialize',
    day !== undefined && day !== '' ? { day } : {},
  );
}

export async function generateRTS27(api: BoundAdminApi, quarter?: string): Promise<number> {
  const raw = await api.post<unknown>(
    '/admin/bestexec/rts27/generate',
    quarter !== undefined && quarter !== '' ? { quarter } : {},
  );
  if (!isRecord(raw)) throw malformed('rts27 generate');
  return arr(raw['reports']).length;
}

export async function generateRTS28(api: BoundAdminApi, year: number): Promise<number> {
  const raw = await api.post<unknown>('/admin/bestexec/rts28/generate', { year });
  if (!isRecord(raw)) throw malformed('rts28 generate');
  return arr(raw['reports']).length;
}

export async function publishReport(
  api: BoundAdminApi,
  kind: 'rts27' | 'rts28',
  id: number,
): Promise<void> {
  await api.post(`/admin/bestexec/${kind}/${id}/publish`, {});
}

/** GET /admin/bestexec/{rts27,rts28}/{id} — raw report detail (schema varies by kind). */
export async function fetchReportDetail(
  api: BoundAdminApi,
  kind: 'rts27' | 'rts28',
  id: number,
): Promise<Record<string, unknown>> {
  const raw = await api.get<unknown>(`/admin/bestexec/${kind}/${id}`);
  if (!isRecord(raw)) throw malformed(`${kind} detail`);
  return raw;
}

// ---------------------------------------------------------------------------
// Regime reports (basel / compliance export).
// ---------------------------------------------------------------------------

export interface BaselReportSummary {
  period: string;
  status?: string;
  raw: Record<string, unknown>;
}

export async function fetchBaselReport(
  api: BoundAdminApi,
  period?: string,
): Promise<BaselReportSummary | null> {
  const raw = await api.get<unknown>(
    `/admin/basel-report${period !== undefined && period !== '' ? `?period=${period}` : ''}`,
  );
  if (!isRecord(raw)) throw malformed('basel report');
  const rep = raw['report'];
  if (rep === null || rep === undefined) return null;
  if (!isRecord(rep)) throw malformed('basel report');
  return {
    period: str(rep['period']) ?? str(rep['as_of']) ?? '',
    status: str(rep['status']),
    raw: rep,
  };
}

export async function fetchComplianceReport(
  api: BoundAdminApi,
  f: { type: string; from?: string; to?: string },
): Promise<unknown> {
  const q = new URLSearchParams({ type: f.type });
  if (f.from) q.set('from', f.from);
  if (f.to) q.set('to', f.to);
  return api.get<unknown>(`/admin/compliance-report?${q.toString()}`);
}

// ---------------------------------------------------------------------------
// Event detail + MiFID II report bundle (Task 10.5.3.27 gate wiring) —
// event detail joins the immutable artifact + ack history; the MiFID
// bundle merges the RTS27/28 registers server-side.
// ---------------------------------------------------------------------------

export async function fetchRegEventDetail(
  api: BoundAdminApi,
  id: number,
): Promise<Record<string, unknown> | null> {
  const raw = await api.get<unknown>(`/admin/regreporting/events/${id}`);
  return isRecord(raw) ? raw : null;
}

export interface MifidBundle {
  rts27: Record<string, unknown>[];
  rts28: Record<string, unknown>[];
}

export async function fetchMifidReport(api: BoundAdminApi, status?: string): Promise<MifidBundle> {
  const raw = await api.get<unknown>('/admin/mifid-report', {
    ...(status !== undefined && status !== '' ? { status } : {}),
    limit: '50',
  });
  const list = (k: string) =>
    isRecord(raw) && Array.isArray(raw[k]) ? (raw[k] as Record<string, unknown>[]) : [];
  return { rts27: list('rts27_reports'), rts28: list('rts28_reports') };
}
