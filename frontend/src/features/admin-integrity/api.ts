/**
 * Integrity console wire seam (Phase-10.5 Task 10.5.3.3) — the
 * read/verify surface for the immutable audit chain, reconciliation
 * runs, order-record export, WAL archive, DLQ and API-deprecation
 * telemetry. All routes already mounted; this module only binds them.
 *
 *   GET /api/v1/admin/audit/verify?date=YYYY-MM-DD   chain replay report
 *   GET /api/v1/admin/audit/chain                   raw hash-chain slice
 *   GET /api/v1/admin/reconciliation/{latest,runs[?run_id=]}
 *   GET /api/v1/admin/order-records/{id}/export     RTS 6 lifecycle
 *   GET /api/v1/admin/archive/status?shard=N        WAL archive index
 *   GET /api/v1/admin/dlq[?stream=&consumer=&limit=]
 *   GET/POST /api/v1/admin/api-deprecations + GET …/usage
 *
 * The audit-log list itself reuses lib/admin fetchAuditLog —
 * GET /admin/audit-log and the /admin/audit alias share the handler.
 */
import { malformed } from '@/lib/admin/api';
import type { BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;

/** Render an unknown scalar (decimal JSON, nested detail) as text. */
export const renderScalar = (v: unknown): string => {
  if (v === null || v === undefined) return '—';
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean' || typeof v === 'bigint') return String(v);
  return typeof v === 'object' ? JSON.stringify(v) : '—';
};

// ---------------------------------------------------------------------------
// Audit chain verify (Task 7.3.3) + raw chain slice (Task 21.3.27).
// ---------------------------------------------------------------------------

export interface VerifyMerkle {
  storedRoot: boolean;
  verified: boolean;
  recomputed?: string;
  violation?: string;
}

export interface VerifyReport {
  date: string;
  rowsChecked: number;
  ok: boolean;
  violations: unknown[];
  merkle: VerifyMerkle;
  checkedAt?: string;
}

export async function verifyAuditChain(api: BoundAdminApi, date: string): Promise<VerifyReport> {
  const raw = await api.get<unknown>('/admin/audit/verify', { date });
  if (!isRecord(raw)) throw malformed('audit verify');
  const m = isRecord(raw['merkle']) ? raw['merkle'] : {};
  return {
    date: str(raw['date']) ?? date,
    rowsChecked: num(raw['rows_checked']) ?? 0,
    ok: raw['ok'] === true,
    violations: Array.isArray(raw['violations']) ? raw['violations'] : [],
    merkle: {
      storedRoot: m['stored_root'] === true,
      verified: m['verified'] === true,
      recomputed: str(m['recomputed_root']),
      violation: str(m['violation']),
    },
    checkedAt: str(raw['checked_at']),
  };
}

/** Raw audit_hash_chain rows — opaque JSON, rendered verbatim. */
export async function fetchAuditChain(
  api: BoundAdminApi,
  q: { from?: string; to?: string; limit?: number },
): Promise<unknown[]> {
  const raw = await api.get<unknown>('/admin/audit/chain', {
    from: q.from,
    to: q.to,
    limit: q.limit ?? 50,
  });
  if (!isRecord(raw) || !Array.isArray(raw['chain'])) throw malformed('audit chain');
  return raw['chain'] as unknown[];
}

// ---------------------------------------------------------------------------
// Reconciliation (Phase-13 Task 13.3.2; migration-286 semantics).
// ---------------------------------------------------------------------------

export interface ReconHalt {
  scope: string;
  target: string;
  reason: string;
  suspensionId?: number;
  error?: string;
}

export interface ReconRun {
  id: number;
  startedAt: string;
  finishedAt?: string;
  status: string;
  categoriesChecked: number;
  findingsCount: number;
  mismatchCount: number;
  inconclusiveCount: number;
  halts: ReconHalt[];
  error?: string;
}

export interface ReconFinding {
  id: number;
  runId: number;
  category: string;
  subject: string;
  leg: string;
  expected?: unknown;
  actual?: unknown;
  delta?: unknown;
  unit: string;
  severity: string;
  haltScope?: string;
  haltTarget?: string;
  detail?: unknown;
}

function parseHalt(v: unknown): ReconHalt | null {
  if (!isRecord(v)) return null;
  return {
    scope: str(v['scope']) ?? '',
    target: str(v['target']) ?? '',
    reason: str(v['reason']) ?? '',
    suspensionId: num(v['suspension_id']),
    error: str(v['error']),
  };
}

function parseRun(v: unknown): ReconRun | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  if (id === undefined) return null;
  return {
    id,
    startedAt: str(v['started_at']) ?? '',
    finishedAt: str(v['finished_at']),
    status: str(v['status']) ?? '',
    categoriesChecked: num(v['categories_checked']) ?? 0,
    findingsCount: num(v['findings_count']) ?? 0,
    mismatchCount: num(v['mismatch_count']) ?? 0,
    inconclusiveCount: num(v['inconclusive_count']) ?? 0,
    halts: Array.isArray(v['halts_emitted'])
      ? (v['halts_emitted'] as unknown[]).map(parseHalt).filter((h): h is ReconHalt => h !== null)
      : [],
    error: str(v['error']),
  };
}

function parseFinding(v: unknown): ReconFinding | null {
  if (!isRecord(v)) return null;
  return {
    id: num(v['id']) ?? 0,
    runId: num(v['run_id']) ?? 0,
    category: str(v['category']) ?? '',
    subject: str(v['subject']) ?? '',
    leg: str(v['leg']) ?? '',
    expected: v['expected'],
    actual: v['actual'],
    delta: v['delta'],
    unit: str(v['unit']) ?? '',
    severity: str(v['severity']) ?? '',
    haltScope: str(v['halt_scope']),
    haltTarget: str(v['halt_target']),
    detail: v['detail'],
  };
}

const parseFindings = (v: unknown): ReconFinding[] =>
  Array.isArray(v)
    ? (v as unknown[]).map(parseFinding).filter((f): f is ReconFinding => f !== null)
    : [];

/** GET /admin/reconciliation/latest — run=null means "never ran" (the
 * handler deliberately returns an empty report, not a 404). */
export async function fetchReconLatest(
  api: BoundAdminApi,
): Promise<{ run: ReconRun | null; findings: ReconFinding[] }> {
  const raw = await api.get<unknown>('/admin/reconciliation/latest');
  if (!isRecord(raw)) throw malformed('recon latest');
  return { run: parseRun(raw['run']), findings: parseFindings(raw['findings']) };
}

export async function fetchReconRuns(api: BoundAdminApi, limit = 50): Promise<ReconRun[]> {
  const raw = await api.get<unknown>('/admin/reconciliation/runs', { limit: String(limit) });
  if (!isRecord(raw) || !Array.isArray(raw['runs'])) throw malformed('recon runs');
  return (raw['runs'] as unknown[]).map(parseRun).filter((r): r is ReconRun => r !== null);
}

export async function fetchReconRunFindings(
  api: BoundAdminApi,
  runId: number,
): Promise<ReconFinding[]> {
  const raw = await api.get<unknown>('/admin/reconciliation/runs', { run_id: String(runId) });
  if (!isRecord(raw) || !Array.isArray(raw['findings'])) throw malformed('recon findings');
  return parseFindings(raw['findings']);
}

// ---------------------------------------------------------------------------
// Order-record export (RTS 6 Art. 17) + WAL archive status (Task 4.3.2).
// ---------------------------------------------------------------------------

export interface OrderLifecycleExport {
  orderId: number;
  lifecycle: unknown[];
  retention?: string;
}

export async function exportOrderRecord(
  api: BoundAdminApi,
  orderId: number,
): Promise<OrderLifecycleExport> {
  const raw = await api.get<unknown>(`/admin/order-records/${orderId}/export`);
  if (!isRecord(raw) || !Array.isArray(raw['lifecycle'])) throw malformed('order-record export');
  return {
    orderId: num(raw['order_id']) ?? orderId,
    lifecycle: raw['lifecycle'] as unknown[],
    retention: str(raw['retention']),
  };
}

export interface ArchiveSegment {
  name: string;
  sizeBytes: number;
  present: boolean;
}

export interface ArchiveStatus {
  shard: number;
  segments: ArchiveSegment[];
  segmentCount: number;
  totalBytes: number;
  missingSegments: number;
}

export async function fetchArchiveStatus(
  api: BoundAdminApi,
  shard: number,
): Promise<ArchiveStatus> {
  const raw = await api.get<unknown>('/admin/archive/status', { shard: String(shard) });
  if (!isRecord(raw)) throw malformed('archive status');
  const segs: ArchiveSegment[] = [];
  if (Array.isArray(raw['segments'])) {
    for (const s of raw['segments'] as unknown[]) {
      if (!isRecord(s)) continue;
      segs.push({
        name: str(s['name']) ?? str(s['segment']) ?? '',
        sizeBytes: num(s['size_bytes']) ?? num(s['size']) ?? 0,
        present: s['present'] === true,
      });
    }
  }
  return {
    shard: num(raw['shard']) ?? shard,
    segments: segs,
    segmentCount: num(raw['segment_count']) ?? segs.length,
    totalBytes: num(raw['total_bytes']) ?? 0,
    missingSegments: num(raw['missing_segments']) ?? 0,
  };
}

// ---------------------------------------------------------------------------
// DLQ (observability.DLQHandler — 503 SERVICE_DEGRADED when the store
// is absent; the panel renders that state honestly).
// ---------------------------------------------------------------------------

export interface DlqEntry {
  seq: number;
  stream: string;
  consumer: string;
  subject: string;
  reason: string;
  failedAt: string;
  deliveries: number;
  payload?: string;
}

function parseDlqEntry(v: unknown): DlqEntry | null {
  if (!isRecord(v)) return null;
  const seq = num(v['seq']);
  if (seq === undefined) return null;
  // Payload arrives as base64 []byte — decode best-effort for display.
  let payload: string | undefined;
  const raw64 = str(v['payload']);
  if (raw64 !== undefined && raw64 !== '') {
    try {
      payload = atob(raw64);
    } catch {
      payload = raw64;
    }
  }
  return {
    seq,
    stream: str(v['stream']) ?? '',
    consumer: str(v['consumer']) ?? '',
    subject: str(v['subject']) ?? '',
    reason: str(v['reason']) ?? '',
    failedAt: str(v['failed_at']) ?? '',
    deliveries: num(v['deliveries']) ?? 0,
    payload,
  };
}

export async function fetchDlq(
  api: BoundAdminApi,
  q: { stream?: string; consumer?: string; limit?: number },
): Promise<{ entries: DlqEntry[]; count: number }> {
  const raw = await api.get<unknown>('/admin/dlq', {
    stream: q.stream,
    consumer: q.consumer,
    limit: q.limit ?? 100,
  });
  if (!isRecord(raw)) throw malformed('dlq list');
  const entries = Array.isArray(raw['entries'])
    ? (raw['entries'] as unknown[]).map(parseDlqEntry).filter((e): e is DlqEntry => e !== null)
    : [];
  return { entries, count: num(raw['count']) ?? entries.length };
}

// ---------------------------------------------------------------------------
// API deprecations (Task 5.3.20) + usage telemetry.
// ---------------------------------------------------------------------------

export interface DeprecationRule {
  id: number;
  method?: string;
  path: string;
  matchPrefix: boolean;
  announcedAt: string;
  sunsetAt: string;
  replacement?: string;
  migrationUrl?: string;
  notice?: string;
}

export interface DeprecationUsage extends DeprecationRule {
  /** day → [hits, gone_hits] */
  daily: Record<string, [number, number]>;
}

function parseRule(v: unknown): DeprecationRule | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const path = str(v['path']);
  if (id === undefined || path === undefined) return null;
  return {
    id,
    method: str(v['method']),
    path,
    matchPrefix: v['match_prefix'] === true,
    announcedAt: str(v['announced_at']) ?? '',
    sunsetAt: str(v['sunset_at']) ?? '',
    replacement: str(v['replacement']),
    migrationUrl: str(v['migration_url']),
    notice: str(v['notice']),
  };
}

export async function fetchDeprecations(api: BoundAdminApi): Promise<DeprecationRule[]> {
  const raw = await api.get<unknown>('/admin/api-deprecations');
  if (!isRecord(raw) || !Array.isArray(raw['deprecations'])) throw malformed('deprecations');
  return (raw['deprecations'] as unknown[])
    .map(parseRule)
    .filter((r): r is DeprecationRule => r !== null);
}

export async function fetchDeprecationUsage(api: BoundAdminApi): Promise<DeprecationUsage[]> {
  const raw = await api.get<unknown>('/admin/api-deprecations/usage');
  if (!isRecord(raw) || !Array.isArray(raw['usage'])) throw malformed('deprecation usage');
  const out: DeprecationUsage[] = [];
  for (const u of raw['usage'] as unknown[]) {
    const rule = parseRule(u);
    if (rule === null) continue;
    const daily: Record<string, [number, number]> = {};
    if (isRecord(u) && isRecord(u['daily'])) {
      for (const [day, pair] of Object.entries(u['daily'])) {
        if (Array.isArray(pair) && pair.length === 2) {
          daily[day] = [num(pair[0]) ?? 0, num(pair[1]) ?? 0];
        }
      }
    }
    out.push({ ...rule, daily });
  }
  return out;
}

export interface DeprecationInput {
  path: string;
  method?: string;
  matchPrefix: boolean;
  sunsetAt: string; // RFC3339 — policy requires ≥ announced_at + 6 months
  replacement?: string;
  notice?: string;
}

export async function announceDeprecation(
  api: BoundAdminApi,
  input: DeprecationInput,
): Promise<void> {
  await api.post('/admin/api-deprecations', {
    path: input.path,
    method: input.method,
    match_prefix: input.matchPrefix,
    sunset_at: input.sunsetAt,
    replacement: input.replacement,
    notice: input.notice,
  });
}

// ---------------------------------------------------------------------------
// Joined audit trail search (Phase-21 Task 21.3.27) — admin_audit_log ⨝
// audit_hash_chain; mask=1 renders the auditor-shareable export.
// ---------------------------------------------------------------------------

export async function fetchAuditTrail(
  api: BoundAdminApi,
  q: {
    action?: string;
    targetType?: string;
    adminUserId?: string;
    mask?: boolean;
    limit?: number;
  },
): Promise<Record<string, unknown>[]> {
  const raw = await api.get<unknown>('/admin/audit/trail', {
    ...(q.action !== undefined && q.action !== '' ? { action: q.action } : {}),
    ...(q.targetType !== undefined && q.targetType !== '' ? { target_type: q.targetType } : {}),
    ...(q.adminUserId !== undefined && q.adminUserId !== ''
      ? { admin_user_id: q.adminUserId }
      : {}),
    ...(q.mask === true ? { mask: '1' } : {}),
    limit: String(q.limit ?? 100),
  });
  return isRecord(raw) && Array.isArray(raw['rows'])
    ? (raw['rows'] as Record<string, unknown>[]).filter(isRecord)
    : [];
}
