/**
 * Reports & transparency adapters (Task 10.3.28).
 *
 *   Live  — GET /fees · GET /announcements · GET /system/status ·
 *           GET /tax/report (csv/pdf/json) · statements / income /
 *           snapshots / confirmations / TCA / solvency+PoR are `v1live`
 *           (supersedes the earlier "registered, all 501 today" note).
 * Wire payloads are narrowed from `unknown`; nothing is fabricated.
 */
import { apiClient } from '@/app/runtime';
import type { ApiClient } from '@/lib/api';
import { tryDec } from '@/lib/decimal/decimal';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
const s = (v: unknown): string | null => (typeof v === 'string' ? v : null);
const n = (v: unknown): number | null => (typeof v === 'number' && Number.isFinite(v) ? v : null);

// ---------------------------------------------------------------------------
// GET /api/v1/fees (live — services/internal/api/fees.go)
// ---------------------------------------------------------------------------

export interface FeeSchedule {
  accountId: number | null;
  tierId: number | null;
  tierName: string | null;
  makerBps: string;
  takerBps: string;
  effectiveMakerBps: string;
  effectiveTakerBps: string;
  promoActive: boolean;
  promoUntil: string | null;
  promoMakerBps: string | null;
  promoTakerBps: string | null;
}

export function parseFeeSchedule(v: unknown): FeeSchedule {
  const r = isRecord(v) ? v : {};
  const promo = isRecord(r['promo']) ? r['promo'] : null;
  const dec = (x: unknown) => s(x) ?? tryDec(x)?.toString() ?? '0';
  return {
    accountId: n(r['account_id']),
    tierId: n(r['tier_id']),
    tierName: s(r['tier_name']),
    makerBps: dec(r['maker_bps']),
    takerBps: dec(r['taker_bps']),
    effectiveMakerBps: dec(r['effective_maker_bps']),
    effectiveTakerBps: dec(r['effective_taker_bps']),
    promoActive: r['promo_active'] === true,
    promoUntil: promo ? s(promo['until']) : null,
    promoMakerBps: promo ? s(promo['maker_bps']) : null,
    promoTakerBps: promo ? s(promo['taker_bps']) : null,
  };
}

export async function fetchFees(api: Pick<ApiClient, 'get'> = apiClient): Promise<FeeSchedule> {
  return parseFeeSchedule(await api.get('/fees'));
}

// ---------------------------------------------------------------------------
// GET /api/v1/announcements (live — handlers_announce.go + marketapi.Announcement)
// ---------------------------------------------------------------------------

export interface Announcement {
  id: string;
  title: string;
  body: string;
  category: string;
  status: string;
  publishAt: string | null;
  expiresAt: string | null;
}

export function parseAnnouncement(v: unknown): Announcement | null {
  if (!isRecord(v)) return null;
  const idRaw = v['id'];
  const id = s(idRaw) ?? (n(idRaw) !== null ? String(idRaw) : null);
  const title = s(v['title']);
  if (!id || !title) return null;
  return {
    id,
    title,
    body: s(v['body']) ?? '',
    category: s(v['category']) ?? 'GENERAL',
    status: s(v['status']) ?? 'PUBLISHED',
    publishAt: s(v['publish_at']),
    expiresAt: s(v['expires_at']),
  };
}

export async function fetchAnnouncements(
  opts: { category?: string; limit?: number } = {},
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<Announcement[]> {
  const res: unknown = await api.get('/announcements', {
    query: { category: opts.category, limit: opts.limit ?? 50 },
  });
  const rows = isRecord(res) ? (res['data'] ?? res['announcements']) : res;
  if (!Array.isArray(rows)) return [];
  return rows.map(parseAnnouncement).filter((a): a is Announcement => a !== null);
}

// ---------------------------------------------------------------------------
// GET /api/v1/system/status (live — ops/status_exporter.go Status)
// ---------------------------------------------------------------------------

export interface ComponentHealth {
  name: string;
  state: string;
  critical: boolean;
  latencyMs: number | null;
  detail: string | null;
}
export interface SystemStatus {
  status: string;
  mode: string;
  components: ComponentHealth[];
  metrics: Record<string, number>;
  source: string;
  updatedAt: string | null;
}

export function parseSystemStatus(v: unknown): SystemStatus {
  const r = isRecord(v) ? v : {};
  const comps = Array.isArray(r['components']) ? r['components'] : [];
  return {
    status: s(r['status']) ?? 'unknown',
    mode: s(r['mode']) ?? 'unknown',
    components: comps.filter(isRecord).map((c) => ({
      name: s(c['name']) ?? '?',
      state: s(c['state']) ?? 'unknown',
      critical: c['critical'] === true,
      latencyMs: n(c['latency_ms']),
      detail: s(c['detail']),
    })),
    metrics: isRecord(r['metrics'])
      ? Object.fromEntries(
          Object.entries(r['metrics']).filter(
            (e): e is [string, number] => typeof e[1] === 'number',
          ),
        )
      : {},
    source: s(r['source']) ?? 'unknown',
    updatedAt: s(r['updated_at']),
  };
}

export async function fetchSystemStatus(
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<SystemStatus> {
  return parseSystemStatus(await api.get('/system/status'));
}

// ---------------------------------------------------------------------------
// Solvency / proof-of-reserves (live — Phase-13 Task 13.3.7)
// ---------------------------------------------------------------------------

/** Narrowed PoR daily-root payload — rendered verbatim when live. */
export interface ProofRoot {
  root: string | null;
  date: string | null;
  totalLiabilities: string | null;
  treeHeight: number | null;
}

export async function fetchProofRoot(
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<ProofRoot | null> {
  const res: unknown = await api.get('/public/proof-of-reserves/daily-root');
  if (!isRecord(res)) return null;
  return {
    root: s(res['root']) ?? s(res['merkle_root']),
    date: s(res['date']) ?? s(res['as_of']),
    totalLiabilities:
      s(res['total_liabilities']) ?? tryDec(res['total_liabilities'])?.toString() ?? null,
    treeHeight: n(res['tree_height']) ?? n(res['height']),
  };
}

/** Account-scoped solvency proof (Merkle leaf + path). */
export async function fetchSolvencyProof(
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<unknown> {
  return api.get('/solvency/proof');
}

/** One sibling step on the leaf→root verification path. */
export interface ProofStep {
  position: 'left' | 'right';
  hash: string;
}

/** Narrowed account inclusion proof — the caller's own leaf plus the
 * sibling digests needed to recompute the published root offline. */
export interface AccountProof {
  snapshotId: number | null;
  generatedAt: string | null;
  merkleRoot: string;
  currency: string;
  balance: string;
  salt: string;
  leafIndex: number | null;
  leafHash: string;
  path: ProofStep[];
}

function narrowProofStep(v: unknown): ProofStep | null {
  if (!isRecord(v)) return null;
  const pos = s(v['position']);
  const hash = s(v['hash']);
  if ((pos !== 'left' && pos !== 'right') || hash == null) return null;
  return { position: pos, hash };
}

function narrowAccountProof(v: unknown): AccountProof | null {
  if (!isRecord(v)) return null;
  const merkleRoot = s(v['merkle_root']);
  const currency = s(v['currency']);
  if (merkleRoot == null || currency == null) return null;
  const rawPath = Array.isArray(v['path']) ? v['path'] : [];
  const path: ProofStep[] = [];
  for (const step of rawPath) {
    const p = narrowProofStep(step);
    if (p === null) return null;
    path.push(p);
  }
  return {
    snapshotId: n(v['snapshot_id']),
    generatedAt: s(v['generated_at']),
    merkleRoot,
    currency,
    balance: s(v['balance']) ?? tryDec(v['balance'])?.toString() ?? '0',
    salt: s(v['salt']) ?? '',
    leafIndex: n(v['leaf_index']),
    leafHash: s(v['leaf_hash']) ?? '',
    path,
  };
}

/** Every currency leaf the caller holds in the latest published
 * snapshot — GET /api/v1/account/solvency-proof. */
export async function fetchAccountProofs(
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<AccountProof[]> {
  const res: unknown = await api.get('/account/solvency-proof');
  if (!isRecord(res) || !Array.isArray(res['proofs'])) return [];
  const out: AccountProof[] = [];
  for (const p of res['proofs']) {
    const narrowed = narrowAccountProof(p);
    if (narrowed !== null) out.push(narrowed);
  }
  return out;
}
export async function fetchLatestSolvency(
  api: Pick<ApiClient, 'get'> = apiClient,
): Promise<unknown> {
  return api.get('/solvency/latest');
}
