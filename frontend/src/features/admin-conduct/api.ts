/**
 * Conduct, governance & DORA API bindings (Phase-10.5 Task
 * 10.5.3.15) — regulatory-change management with the 10-business-day
 * triage SLA, execution-policy versions, product profiles &
 * target-market reviews, FX Global Code assessments, governance
 * packs, recertification campaigns, data-residency policy/access
 * audit, and the DORA ICT provider register. All calls are
 * environment-bound admin calls.
 */
import type { ApiClient } from '@/lib/api/client';
import { ADMIN_ENV_HEADER, type AdminEnv, type BoundAdminApi } from '@/lib/env';

const envOpts = (env: AdminEnv) => ({ headers: { [ADMIN_ENV_HEADER]: env } });

const record = (v: unknown): Record<string, unknown> =>
  typeof v === 'object' && v !== null ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === 'string' ? v : '');
const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
const optStr = (v: unknown): string | undefined =>
  typeof v === 'string' && v !== '' ? v : undefined;
const optNum = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
const boolOf = (v: unknown): boolean => v === true;
const listOf = <T>(v: unknown, parse: (x: unknown) => T): T[] =>
  Array.isArray(v) ? v.map(parse) : [];
/** PascalCase OR snake_case — DORA Provider/Review rows carry no json tags. */
const both = (r: Record<string, unknown>, pascal: string, snake: string): unknown =>
  r[pascal] ?? r[snake];

// ---------- regulatory changes ----------

export interface RegChange {
  changeId: number;
  authority: string;
  instrument: string;
  title: string;
  status: string;
  publishedAt: string;
  effectiveAt?: string;
  triageDueAt: string;
  triagedAt?: string;
  owner?: number;
}

const change = (v: unknown): RegChange => {
  const r = record(v);
  return {
    changeId: num(r['change_id']),
    authority: str(r['authority']),
    instrument: str(r['instrument']),
    title: str(r['title']),
    status: str(r['status']),
    publishedAt: str(r['published_at']),
    effectiveAt: optStr(r['effective_at']),
    triageDueAt: str(r['triage_due_at']),
    triagedAt: optStr(r['triaged_at']),
    owner: optNum(r['owner']),
  };
};

export interface ChangeImpact {
  id: number;
  kind: string;
  ref: string;
  status: string;
  effortEstimate?: string;
  dueAt?: string;
  completedAt?: string;
}

export interface ChangeCorrespondence {
  id: number;
  kind: string;
  summary: string;
  receivedAt: string;
  dueAt?: string;
}

export function fetchRegChanges(api: BoundAdminApi, status?: string): Promise<RegChange[]> {
  return api
    .get(
      `/admin/regulatory-changes${status !== undefined && status !== '' ? `?status=${status}` : ''}`,
    )
    .then((r) => listOf(record(r)['changes'], change));
}

export function createRegChange(
  api: BoundAdminApi,
  body: {
    authority: string;
    instrument: string;
    title: string;
    published_at: string;
    effective_at?: string;
    source_url?: string;
    notes?: string;
  },
): Promise<{ id: number; created: boolean; overlapping: number[] }> {
  return api.post('/admin/regulatory-changes', body).then((r) => {
    const d = record(r);
    return {
      id: num(record(d['change'])['change_id']),
      created: boolOf(d['created']),
      overlapping: listOf(d['overlapping_change_ids'], (x) => num(x)),
    };
  });
}

export interface RegChangeDetail {
  change: RegChange;
  impacts: ChangeImpact[];
  correspondence: ChangeCorrespondence[];
}

export function fetchRegChangeImpact(api: BoundAdminApi, id: number): Promise<RegChangeDetail> {
  return api.get(`/admin/regulatory-changes/${id}/impact`).then((r) => {
    const d = record(r);
    return {
      change: change(d['change']),
      impacts: listOf(d['impacts'], (x) => {
        const i = record(x);
        return {
          id: num(i['id']),
          kind: str(i['kind']),
          ref: str(i['ref']),
          status: str(i['status']),
          effortEstimate: optStr(i['effort_estimate']),
          dueAt: optStr(i['due_at']),
          completedAt: optStr(i['completed_at']),
        };
      }),
      correspondence: listOf(d['correspondence'], (x) => {
        const c = record(x);
        return {
          id: num(c['id']),
          kind: str(c['kind']),
          summary: str(c['summary']),
          receivedAt: str(c['received_at']),
          dueAt: optStr(c['due_at']),
        };
      }),
    };
  });
}

export function putRegChangeImpact(
  api: ApiClient,
  env: AdminEnv,
  id: number,
  body: { kind: string; ref: string; owner?: number; effort_estimate?: string; due_at?: string },
): Promise<void> {
  return api
    .put(`/admin/regulatory-changes/${id}/impact`, body, envOpts(env))
    .then(() => undefined);
}

export function transitionRegChange(
  api: BoundAdminApi,
  id: number,
  body: { action: string; owner?: number; notes?: string; matrix_update_ref?: string },
): Promise<void> {
  return api.post(`/admin/regulatory-changes/${id}/transition`, body).then(() => undefined);
}

export function attachCorrespondence(
  api: BoundAdminApi,
  id: number,
  body: { kind: string; summary: string; received_at: string; due_at?: string },
): Promise<void> {
  return api.post(`/admin/regulatory-changes/${id}/correspondence`, body).then(() => undefined);
}

export function completeImpactItem(api: BoundAdminApi, impactId: number): Promise<void> {
  return api.post(`/admin/regulatory-changes/impacts/${impactId}/done`, {}).then(() => undefined);
}

// ---------- execution policies / product profiles / target markets ----------

export interface ExecPolicy {
  id: number;
  version: string;
  status: string;
  bodyRef: string;
  effectiveFrom?: string;
  reviewDueAt?: string;
  materialChange: boolean;
  reviewNotes?: string;
}

export function fetchExecPolicies(api: BoundAdminApi): Promise<ExecPolicy[]> {
  return api.get('/admin/execution-policies').then((r) =>
    listOf(record(r)['policies'], (x) => {
      const p = record(x);
      return {
        id: num(p['id']),
        version: str(p['version']),
        status: str(p['status']),
        bodyRef: str(p['body_ref']),
        effectiveFrom: optStr(p['effective_from']),
        reviewDueAt: optStr(p['review_due_at']),
        materialChange: boolOf(p['material_change']),
        reviewNotes: optStr(p['review_notes']),
      };
    }),
  );
}

export function draftExecPolicy(
  api: BoundAdminApi,
  body: { version: string; body_ref: string },
): Promise<void> {
  return api.post('/admin/execution-policies', body).then(() => undefined);
}

export function activateExecPolicy(
  api: BoundAdminApi,
  id: number,
  body: {
    effective_from?: string;
    review_due_at?: string;
    material_change?: boolean;
    reg_change_id?: number;
  },
): Promise<void> {
  return api.post(`/admin/execution-policies/${id}/activate`, body).then(() => undefined);
}

export function reviewExecPolicy(
  api: BoundAdminApi,
  id: number,
  body: { notes?: string; review_due_at?: string },
): Promise<void> {
  return api.post(`/admin/execution-policies/${id}/review`, body).then(() => undefined);
}

export interface ProductProfile {
  profileId: number;
  code: string;
  pricingPlan: string;
  status: string;
  minDeposit: string;
}

export function fetchProductProfiles(api: BoundAdminApi): Promise<ProductProfile[]> {
  return api.get('/admin/product-profiles?include_retired=true').then((r) =>
    listOf(record(r)['profiles'], (x) => {
      const p = record(x);
      return {
        profileId: num(p['profile_id']),
        code: str(p['code']),
        pricingPlan: str(p['pricing_plan']),
        status: str(p['status']),
        minDeposit: str(p['min_deposit']),
      };
    }),
  );
}

/** Dual-control submit — returns the pending request id (HTTP 202). */
export function submitProductProfile(
  api: ApiClient,
  env: AdminEnv,
  method: 'POST' | 'PUT',
  body: {
    profile_id?: number;
    reason: string;
    input: {
      code: string;
      pricing_plan: string;
      instrument_scope?: string[];
      subunit_divisor?: number;
      min_deposit?: string;
      status?: string;
    };
  },
): Promise<{ requestId: number }> {
  const call =
    method === 'POST'
      ? api.post('/admin/product-profiles', body, envOpts(env))
      : api.put('/admin/product-profiles', body, envOpts(env));
  return call.then((r) => ({ requestId: num(record(record(r)['request'])['id']) }));
}

export interface TargetMarket {
  id: number;
  profileId: number;
  clientCategory: string;
  status: string;
  reviewDueAt: string;
  lastReviewedAt?: string;
}

export function fetchTargetMarkets(api: BoundAdminApi, overdue?: boolean): Promise<TargetMarket[]> {
  return api
    .get(`/admin/product-target-markets${overdue === true ? '?overdue=true' : ''}`)
    .then((r) =>
      listOf(record(r)['target_markets'], (x) => {
        const t = record(x);
        return {
          id: num(t['id']),
          profileId: num(t['profile_id']),
          clientCategory: str(t['client_category']),
          status: str(t['status']),
          reviewDueAt: str(t['review_due_at']),
          lastReviewedAt: optStr(t['last_reviewed_at']),
        };
      }),
    );
}

export function upsertTargetMarket(
  api: ApiClient,
  env: AdminEnv,
  profileId: number,
  body: {
    client_category: string;
    knowledge_experience?: string;
    risk_tolerance?: string;
    positive_classes?: string[];
    negative_classes?: string[];
    negative_target?: string;
    distribution_strategy?: string;
    review_due_at?: string;
  },
): Promise<void> {
  return api
    .put(`/admin/product-profiles/${profileId}/target-market`, body, envOpts(env))
    .then(() => undefined);
}

export function reviewTargetMarket(
  api: BoundAdminApi,
  id: number,
  action: 'APPROVE' | 'NARROW' | 'SUSPEND',
): Promise<void> {
  return api.post(`/admin/product-target-markets/${id}/review`, { action }).then(() => undefined);
}

// ---------- FX Global Code ----------

export interface FXGCRun {
  id: number;
  codeVersion: string;
  period: string;
  status: string;
  principlesTotal: number;
  principlesAdherent: number;
  principlesPartial: number;
  principlesNon: number;
  principlesPending: number;
  signedAt?: string;
}

const fxgcRun = (v: unknown): FXGCRun => {
  const r = record(v);
  return {
    id: num(r['id']),
    codeVersion: str(r['code_version']),
    period: str(r['period']),
    status: str(r['status']),
    principlesTotal: num(r['principles_total']),
    principlesAdherent: num(r['principles_adherent']),
    principlesPartial: num(r['principles_partial']),
    principlesNon: num(r['principles_non']),
    principlesPending: num(r['principles_pending']),
    signedAt: optStr(r['signed_at']),
  };
};

export function fetchFXGCRuns(api: BoundAdminApi): Promise<FXGCRun[]> {
  return api
    .get('/admin/fx-global-code/assessments')
    .then((r) => listOf(record(r)['runs'], fxgcRun));
}

/** Run export: run + 55-principle matrix + statement metadata — raw for faithful display. */
export function fetchFXGCRun(api: BoundAdminApi, id: number): Promise<Record<string, unknown>> {
  return api
    .get(`/admin/fx-global-code/assessments/${id}`)
    .then((r) => record(record(r)['assessment']));
}

export function openFXGCRun(
  api: BoundAdminApi,
  body: { period: string; code_version?: string },
): Promise<number> {
  return api
    .post('/admin/fx-global-code/assessments', body)
    .then((r) => num(record(record(r)['run'])['id']));
}

export function fxgcVerdict(
  api: BoundAdminApi,
  id: number,
  body: {
    principle_id: number;
    adherence_status: string;
    evidence_summary?: string;
    remediation_ref?: string;
  },
): Promise<void> {
  return api.post(`/admin/fx-global-code/assessments/${id}/verdicts`, body).then(() => undefined);
}

export function fxgcLifecycle(
  api: BoundAdminApi,
  id: number,
  action: 'complete' | 'sign' | 'publish',
): Promise<void> {
  return api.post(`/admin/fx-global-code/assessments/${id}/${action}`, {}).then(() => undefined);
}

// ---------- governance packs / recert ----------

export interface GovernancePack {
  packId: number;
  kind: string;
  period: string;
  status: string;
  contentHash: string;
  generatedAt: string;
  releasedAt?: string;
}

export function fetchPacks(api: BoundAdminApi): Promise<GovernancePack[]> {
  return api.get('/admin/governance-packs').then((r) =>
    listOf(record(r)['governance_packs'], (x) => {
      const p = record(x);
      return {
        packId: num(p['pack_id']),
        kind: str(p['kind']),
        period: str(p['period']),
        status: str(p['status']),
        contentHash: str(p['content_hash']),
        generatedAt: str(p['generated_at']),
        releasedAt: optStr(p['released_at']),
      };
    }),
  );
}

export function fetchPack(
  api: BoundAdminApi,
  id: number,
): Promise<{ hashOk: boolean; raw: Record<string, unknown> }> {
  return api.get(`/admin/governance-packs/${id}`).then((r) => {
    const d = record(r);
    return { hashOk: boolOf(d['hash_ok']), raw: record(d['pack']) };
  });
}

export function generatePack(
  api: BoundAdminApi,
  body: { kind: string; date?: string; year?: number; quarter?: number; label?: string },
): Promise<void> {
  return api.post('/admin/governance-packs/generate', body).then(() => undefined);
}

export function releasePack(
  api: BoundAdminApi,
  id: number,
  approverId: number,
  reason: string,
): Promise<void> {
  return api
    .post(`/admin/governance-packs/${id}/release`, { approver_id: approverId, reason })
    .then(() => undefined);
}

export function startRecert(
  api: BoundAdminApi,
  body: { label: string; ends_at: string },
): Promise<number> {
  return api.post('/admin/recert', body).then((r) => num(record(record(r)['campaign'])['id']));
}

/** Campaign report — shape is the auditor export; rendered raw. */
export function fetchRecertReport(
  api: BoundAdminApi,
  id: number,
): Promise<Record<string, unknown>> {
  return api.get(`/admin/recert/${id}`).then((r) => record(r));
}

export function recertDecide(
  api: BoundAdminApi,
  id: number,
  body: { binding_id: number; approve: boolean; note?: string },
): Promise<void> {
  return api.post(`/admin/recert/${id}/decisions`, body).then(() => undefined);
}

// ---------- residency / ICT providers ----------

export interface ResidencyPolicy {
  jurisdictionCode: string;
  homeRegion: string;
  kmsKeyId: string;
  s3Bucket: string;
  adequate: boolean;
  transferInstrument: string;
}

export function fetchResidencyPolicies(api: BoundAdminApi): Promise<ResidencyPolicy[]> {
  return api.get('/admin/data-residency/policies').then((r) =>
    listOf(record(r)['policies'], (x) => {
      const p = record(x);
      return {
        jurisdictionCode: str(p['jurisdiction_code']),
        homeRegion: str(p['home_region']),
        kmsKeyId: str(p['kms_key_id']),
        s3Bucket: str(p['s3_bucket']),
        adequate: boolOf(p['adequate']),
        transferInstrument: str(p['transfer_instrument']),
      };
    }),
  );
}

export function fetchResidencyAccessLog(api: BoundAdminApi): Promise<Record<string, unknown>[]> {
  return api
    .get('/admin/data-residency/access-log')
    .then((r) => listOf(record(r)['access_log'], (x) => record(x)));
}

export interface ICTProvider {
  id: number;
  name: string;
  ictService: string;
  concentration: string;
  status: string;
  renewalAt?: string;
  nextReviewAt?: string;
  owner?: string;
}

const provider = (v: unknown): ICTProvider => {
  const r = record(v);
  return {
    id: num(both(r, 'ID', 'id')),
    name: str(both(r, 'Name', 'name')),
    ictService: str(both(r, 'ICTService', 'ict_service')),
    concentration: str(both(r, 'Concentration', 'concentration')),
    status: str(both(r, 'Status', 'status')),
    renewalAt: optStr(both(r, 'RenewalAt', 'renewal_at')),
    nextReviewAt: optStr(both(r, 'NextReviewAt', 'next_review_at')),
    owner: optStr(both(r, 'Owner', 'owner')),
  };
};

export function fetchICTProviders(api: BoundAdminApi): Promise<ICTProvider[]> {
  return api.get('/admin/ict-providers').then((r) => listOf(record(r)['ict_providers'], provider));
}

export function upsertICTProvider(
  api: ApiClient,
  env: AdminEnv,
  id: number | null,
  body: Record<string, unknown>,
): Promise<void> {
  const call =
    id === null
      ? api.post('/admin/ict-providers', body, envOpts(env))
      : api.put(`/admin/ict-providers/${id}`, body, envOpts(env));
  return call.then(() => undefined);
}

export function retireICTProvider(api: ApiClient, env: AdminEnv, id: number): Promise<void> {
  return api.delete(`/admin/ict-providers/${id}`, envOpts(env)).then(() => undefined);
}

export interface ICTAlert {
  code: string;
  providerId: number;
  name: string;
  summary: string;
  dueAt: string;
}

export function fetchICTDue(api: BoundAdminApi): Promise<ICTAlert[]> {
  return api.get('/admin/ict-providers/due').then((r) =>
    listOf(record(r)['alerts'], (x) => {
      const a = record(x);
      return {
        code: str(both(a, 'Code', 'code')),
        providerId: num(both(a, 'ProviderID', 'provider_id')),
        name: str(both(a, 'Name', 'name')),
        summary: str(both(a, 'Summary', 'summary')),
        dueAt: str(both(a, 'DueAt', 'due_at')),
      };
    }),
  );
}

export function fetchICTReviews(
  api: BoundAdminApi,
  id: number,
): Promise<Record<string, unknown>[]> {
  return api
    .get(`/admin/ict-providers/${id}/reviews`)
    .then((r) => listOf(record(r)['reviews'], (x) => record(x)));
}

export function recordICTReview(
  api: BoundAdminApi,
  id: number,
  body: {
    kind: string;
    outcome: string;
    evidence_ref?: string;
    notes?: string;
    reviewed_at?: string;
  },
): Promise<void> {
  return api.post(`/admin/ict-providers/${id}/reviews`, body).then(() => undefined);
}
