/**
 * Venue-governance API bindings (Phase-10.5 Task 10.5.3.14) — the
 * Phase-21 Task 21.3.15 regulated-venue surface: member admission &
 * lifecycle, rulebook versions + participant notices, market-control
 * interventions, investigation/disciplinary cases, conflicts of
 * interest, annual self-assessments, CCO reports, and the launch
 * prerequisite gate. All calls are environment-bound admin calls.
 */
import type { BoundAdminApi } from '@/lib/env';

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

// ---------- rows ----------

export interface VenueMember {
  memberId: number;
  legalName: string;
  lei: string;
  regulatoryStatus: string;
  accessModel: string;
  approvedProducts: string[];
  approvedPorts: string[];
  dueDiligenceStatus: string;
  admissionDecision: string;
  suspended: boolean;
  suspensionReason?: string;
  terminatedAt?: string;
  terminationReason?: string;
  annualReviewDue?: string;
  jurisdiction?: string;
  createdAt: string;
}

export interface MemberEvent {
  eventId: number;
  eventType: string;
  detail: unknown;
  createdAt: string;
}

export interface MemberReview {
  reviewId: number;
  reviewType: string;
  outcome: string;
  nextReviewDue: string;
  reviewedAt: string;
}

export interface Rulebook {
  rulebookId: number;
  kind: string;
  scopeKey: string;
  version: string;
  bodyRef: string;
  status: string;
  requiresRegulator: boolean;
  regulatorStatus: string;
  regulatorFilingRef?: string;
  emergency: boolean;
  effectiveFrom?: string;
  activatedAt?: string;
}

export interface RuleNotice {
  noticeId: number;
  subject: string;
  memberId?: number;
  issuedAt: string;
}

export interface RuleAck {
  ackId: number;
  memberId: number;
  acknowledgedBy: string;
  acknowledgedAt: string;
}

export interface Intervention {
  interventionId: number;
  kind: string;
  status: string;
  reason: string;
  instrumentId?: number;
  memberId?: number;
  accountId?: number;
  imposedAt: string;
  liftedAt?: string;
}

export interface VenueCase {
  caseId: number;
  caseRef: string;
  kind: string;
  subject: string;
  status: string;
  outcome?: string;
  memberId?: number;
  openedAt: string;
  closedAt?: string;
}

export interface CaseEvidence {
  evidenceId: number;
  evidenceRef: string;
  sha256?: string;
  note?: string;
  attachedAt: string;
}

export interface Conflict {
  conflictId: number;
  memberId?: number;
  officerUserId?: number;
  subject: string;
  nature: string;
  status: string;
  mitigation?: string;
  declaredAt: string;
}

export interface SelfAssessment {
  assessmentId: number;
  periodYear: number;
  version: number;
  status: string;
  assessedAt?: string;
  createdAt: string;
}

export interface CCOReport {
  id: number;
  periodStart: string;
  periodEnd: string;
  version: number;
  status: string;
  regulatorFilingRef?: string;
  boardSignedAt?: string;
  filedAt?: string;
  createdAt: string;
}

export interface Prerequisite {
  prereqId: number;
  kind: string;
  scope: string;
  required: boolean;
  status: string;
  description: string;
  evidenceRef?: string;
  expiresAt?: string;
}

export interface LaunchGate {
  ready: boolean;
  missing: Prerequisite[];
  evaluatedAt: string;
}

// ---------- parsers ----------

const member = (v: unknown): VenueMember => {
  const r = record(v);
  return {
    memberId: num(r.member_id),
    legalName: str(r.legal_name),
    lei: str(r.lei),
    regulatoryStatus: str(r.regulatory_status),
    accessModel: str(r.access_model),
    approvedProducts: listOf(r.approved_products, (x) => str(x)),
    approvedPorts: listOf(r.approved_ports, (x) => str(x)),
    dueDiligenceStatus: str(r.due_diligence_status),
    admissionDecision: str(r.admission_decision),
    suspended: boolOf(r.suspended),
    suspensionReason: optStr(r.suspension_reason),
    terminatedAt: optStr(r.terminated_at),
    terminationReason: optStr(r.termination_reason),
    annualReviewDue: optStr(r.annual_review_due),
    jurisdiction: optStr(r.jurisdiction),
    createdAt: str(r.created_at),
  };
};

const rulebook = (v: unknown): Rulebook => {
  const r = record(v);
  return {
    rulebookId: num(r.rulebook_id),
    kind: str(r.kind),
    scopeKey: str(r.scope_key),
    version: str(r.version),
    bodyRef: str(r.body_ref),
    status: str(r.status),
    requiresRegulator: boolOf(r.requires_regulator_approval),
    regulatorStatus: str(r.regulator_status),
    regulatorFilingRef: optStr(r.regulator_filing_ref),
    emergency: boolOf(r.emergency),
    effectiveFrom: optStr(r.effective_from),
    activatedAt: optStr(r.activated_at),
  };
};

const intervention = (v: unknown): Intervention => {
  const r = record(v);
  return {
    interventionId: num(r.intervention_id),
    kind: str(r.kind),
    status: str(r.status),
    reason: str(r.reason),
    instrumentId: optNum(r.instrument_id),
    memberId: optNum(r.member_id),
    accountId: optNum(r.account_id),
    imposedAt: str(r.imposed_at),
    liftedAt: optStr(r.lifted_at),
  };
};

const venueCase = (v: unknown): VenueCase => {
  const r = record(v);
  return {
    caseId: num(r.case_id),
    caseRef: str(r.case_ref),
    kind: str(r.kind),
    subject: str(r.subject),
    status: str(r.status),
    outcome: optStr(r.outcome),
    memberId: optNum(r.member_id),
    openedAt: str(r.opened_at),
    closedAt: optStr(r.closed_at),
  };
};

const conflict = (v: unknown): Conflict => {
  const r = record(v);
  return {
    conflictId: num(r.conflict_id),
    memberId: optNum(r.member_id),
    officerUserId: optNum(r.officer_user_id),
    subject: str(r.subject),
    nature: str(r.nature),
    status: str(r.status),
    mitigation: optStr(r.mitigation),
    declaredAt: str(r.declared_at),
  };
};

const prereq = (v: unknown): Prerequisite => {
  const r = record(v);
  return {
    prereqId: num(r.prereq_id),
    kind: str(r.kind),
    scope: str(r.scope),
    required: boolOf(r.required),
    status: str(r.status),
    description: str(r.description),
    evidenceRef: optStr(r.evidence_ref),
    expiresAt: optStr(r.expires_at),
  };
};

// ---------- members ----------

export function fetchMembers(api: BoundAdminApi): Promise<VenueMember[]> {
  return api.get('/admin/venue/members').then((r) => listOf(record(r).members, member));
}

export interface MemberDetail {
  member: VenueMember;
  events: MemberEvent[];
  reviews: MemberReview[];
}

export function fetchMember(api: BoundAdminApi, id: number): Promise<MemberDetail> {
  return api.get(`/admin/venue/members/${id}`).then((r) => {
    const d = record(r);
    return {
      member: member(d.member),
      events: listOf(d.events, (x) => {
        const e = record(x);
        return {
          eventId: num(e.event_id),
          eventType: str(e.event_type),
          detail: e.detail,
          createdAt: str(e.created_at),
        };
      }),
      reviews: listOf(d.reviews, (x) => {
        const rv = record(x);
        return {
          reviewId: num(rv.review_id),
          reviewType: str(rv.review_type),
          outcome: str(rv.outcome),
          nextReviewDue: str(rv.next_review_due),
          reviewedAt: str(rv.reviewed_at),
        };
      }),
    };
  });
}

export function registerMember(
  api: BoundAdminApi,
  body: {
    legal_name: string;
    lei: string;
    access_model: string;
    regulatory_status?: string;
    jurisdiction?: string;
  },
): Promise<number> {
  return api
    .post('/admin/venue/members', body)
    .then((r) => num(record(record(r).member).member_id));
}

export function memberAction(
  api: BoundAdminApi,
  id: number,
  action:
    | 'due-diligence'
    | 'agreements'
    | 'products'
    | 'decision'
    | 'suspend'
    | 'reinstate'
    | 'terminate'
    | 'appeals'
    | 'appeal-decision'
    | 'reviews',
  body: Record<string, unknown>,
): Promise<void> {
  return api.post(`/admin/venue/members/${id}/${action}`, body).then(() => undefined);
}

// ---------- rulebooks ----------

export function fetchRulebooks(api: BoundAdminApi): Promise<Rulebook[]> {
  return api.get('/admin/venue/rulebooks').then((r) => listOf(record(r).rulebooks, rulebook));
}

export interface RulebookDetail {
  rulebook: Rulebook;
  notices: RuleNotice[];
  acks: RuleAck[];
}

export function fetchRulebook(api: BoundAdminApi, id: number): Promise<RulebookDetail> {
  return api.get(`/admin/venue/rulebooks/${id}`).then((r) => {
    const d = record(r);
    return {
      rulebook: rulebook(d.rulebook),
      notices: listOf(d.notices, (x) => {
        const n = record(x);
        return {
          noticeId: num(n.notice_id),
          subject: str(n.subject),
          memberId: optNum(n.member_id),
          issuedAt: str(n.issued_at),
        };
      }),
      acks: listOf(d.acknowledgements, (x) => {
        const a = record(x);
        return {
          ackId: num(a.ack_id),
          memberId: num(a.member_id),
          acknowledgedBy: str(a.acknowledged_by),
          acknowledgedAt: str(a.acknowledged_at),
        };
      }),
    };
  });
}

export function draftRulebook(
  api: BoundAdminApi,
  body: {
    kind: string;
    scope_key?: string;
    version: string;
    body_ref: string;
    requires_regulator_approval?: boolean;
  },
): Promise<number> {
  return api
    .post('/admin/venue/rulebooks', body)
    .then((r) => num(record(record(r).rulebook).rulebook_id));
}

export function rulebookAction(
  api: BoundAdminApi,
  id: number,
  action: 'file' | 'regulator-decision' | 'approve' | 'activate' | 'notices' | 'acks',
  body: Record<string, unknown>,
): Promise<void> {
  return api.post(`/admin/venue/rulebooks/${id}/${action}`, body).then(() => undefined);
}

// ---------- interventions / cases / conflicts ----------

export function fetchInterventions(api: BoundAdminApi): Promise<Intervention[]> {
  return api
    .get('/admin/venue/interventions')
    .then((r) => listOf(record(r).interventions, intervention));
}

export function recordIntervention(
  api: BoundAdminApi,
  body: {
    kind: string;
    reason: string;
    instrument_id?: number;
    member_id?: number;
    account_id?: number;
    detail?: Record<string, unknown>;
  },
): Promise<void> {
  return api.post('/admin/venue/interventions', body).then(() => undefined);
}

export function liftIntervention(api: BoundAdminApi, id: number, note?: string): Promise<void> {
  return api
    .post(
      `/admin/venue/interventions/${id}/lift`,
      note !== undefined && note !== '' ? { note } : {},
    )
    .then(() => undefined);
}

export function fetchVenueCases(api: BoundAdminApi): Promise<VenueCase[]> {
  return api.get('/admin/venue/cases').then((r) => listOf(record(r).cases, venueCase));
}

export interface CaseDetail {
  case: VenueCase;
  evidence: CaseEvidence[];
}

export function fetchVenueCase(api: BoundAdminApi, id: number): Promise<CaseDetail> {
  return api.get(`/admin/venue/cases/${id}`).then((r) => {
    const d = record(r);
    return {
      case: venueCase(d.case),
      evidence: listOf(d.evidence, (x) => {
        const e = record(x);
        return {
          evidenceId: num(e.evidence_id),
          evidenceRef: str(e.evidence_ref),
          sha256: optStr(e.sha256),
          note: optStr(e.note),
          attachedAt: str(e.attached_at),
        };
      }),
    };
  });
}

export function openVenueCase(
  api: BoundAdminApi,
  body: { kind: string; subject: string; member_id?: number; account_id?: number },
): Promise<number> {
  return api.post('/admin/venue/cases', body).then((r) => num(record(record(r).case).case_id));
}

export function attachCaseEvidence(
  api: BoundAdminApi,
  id: number,
  body: { evidence_ref: string; sha256?: string; note?: string },
): Promise<void> {
  return api.post(`/admin/venue/cases/${id}/evidence`, body).then(() => undefined);
}

export function transitionCase(
  api: BoundAdminApi,
  id: number,
  status: string,
  outcome?: string,
): Promise<void> {
  return api
    .post(`/admin/venue/cases/${id}/transition`, {
      status,
      ...(outcome !== undefined && outcome !== '' ? { outcome } : {}),
    })
    .then(() => undefined);
}

export function fetchConflicts(api: BoundAdminApi): Promise<Conflict[]> {
  return api.get('/admin/venue/conflicts').then((r) => listOf(record(r).conflicts, conflict));
}

export function declareConflict(
  api: BoundAdminApi,
  body: { member_id?: number; officer_user_id?: number; subject: string; nature: string },
): Promise<void> {
  return api.post('/admin/venue/conflicts', body).then(() => undefined);
}

export function resolveConflict(
  api: BoundAdminApi,
  id: number,
  status: 'MITIGATED' | 'RECUSED' | 'CLOSED',
  mitigation: string,
): Promise<void> {
  return api
    .post(`/admin/venue/conflicts/${id}/resolve`, { status, mitigation })
    .then(() => undefined);
}

// ---------- assessments / CCO / launch ----------

export function fetchAssessments(api: BoundAdminApi): Promise<SelfAssessment[]> {
  return api.get('/admin/venue/self-assessments').then((r) =>
    listOf(record(r).assessments, (x) => {
      const a = record(x);
      return {
        assessmentId: num(a.assessment_id),
        periodYear: num(a.period_year),
        version: num(a.version),
        status: str(a.status),
        assessedAt: optStr(a.assessed_at),
        createdAt: str(a.created_at),
      };
    }),
  );
}

export function fileAssessment(
  api: BoundAdminApi,
  body: {
    period_year: number;
    exceptions?: Record<string, unknown>;
    financial_attestation?: Record<string, unknown>;
    remediation?: Record<string, unknown>;
  },
): Promise<void> {
  return api.post('/admin/venue/self-assessments', body).then(() => undefined);
}

export function completeAssessment(api: BoundAdminApi, id: number): Promise<void> {
  return api.post(`/admin/venue/self-assessments/${id}/complete`, {}).then(() => undefined);
}

export function fetchCCOReports(api: BoundAdminApi): Promise<CCOReport[]> {
  return api.get('/admin/venue/cco-reports').then((r) =>
    listOf(record(r).reports, (x) => {
      const c = record(x);
      return {
        id: num(c.id),
        periodStart: str(c.period_start),
        periodEnd: str(c.period_end),
        version: num(c.version),
        status: str(c.status),
        regulatorFilingRef: optStr(c.regulator_filing_ref),
        boardSignedAt: optStr(c.board_signed_at),
        filedAt: optStr(c.filed_at),
        createdAt: str(c.created_at),
      };
    }),
  );
}

export function generateCCOReport(
  api: BoundAdminApi,
  body: {
    period_start: string;
    period_end: string;
    unresolved_remediation?: Record<string, unknown>;
  },
): Promise<void> {
  return api.post('/admin/venue/cco-reports', body).then(() => undefined);
}

export function ccoAction(
  api: BoundAdminApi,
  id: number,
  action: 'sign' | 'file',
  body: Record<string, unknown>,
): Promise<void> {
  return api.post(`/admin/venue/cco-reports/${id}/${action}`, body).then(() => undefined);
}

export function fetchPrerequisites(api: BoundAdminApi): Promise<Prerequisite[]> {
  return api
    .get('/admin/venue/launch-prerequisites')
    .then((r) => listOf(record(r).prerequisites, prereq));
}

export function evidencePrerequisite(
  api: BoundAdminApi,
  body: { kind: string; scope?: string; evidence_ref: string; expires_at?: string },
): Promise<void> {
  return api.post('/admin/venue/launch-prerequisites', body).then(() => undefined);
}

export function expirePrerequisite(api: BoundAdminApi, id: number): Promise<void> {
  return api.post(`/admin/venue/launch-prerequisites/${id}/expire`, {}).then(() => undefined);
}

export function fetchLaunchGate(api: BoundAdminApi): Promise<LaunchGate> {
  return api.get('/admin/venue/launch-gate').then((r) => {
    const g = record(record(r).launch_gate);
    return {
      ready: boolOf(g.ready),
      missing: listOf(g.missing, prereq),
      evaluatedAt: str(g.evaluated_at),
    };
  });
}
