/**
 * admin-crm wire seam (Phase-10.5 Task 10.5.3.4) — Customer 360 &
 * account-lifecycle endpoints. All binds are mounted admin routes;
 * the shared SupportView/SupportTicket/ComplianceHold parsers live in
 * lib/admin/api.ts and features/admin/consoles-api.ts and are reused,
 * not reparsed here.
 *
 *   GET  /api/v1/admin/support/accounts/{id}        dossier (reuse)
 *   GET  /api/v1/admin/support/tickets?account_id=  account tickets
 *   GET  /api/v1/admin/support/tickets/{id}         detail + notes
 *   POST /api/v1/admin/support/tickets/{id}/notes   internal note
 *   GET  /api/v1/admin/support/complaints/register  MiFID register
 *   GET  /api/v1/admin/accounts/{id}/self-certifications  W-8/W-9 review
 *   POST /api/v1/admin/accounts/{id}/freeze|unfreeze  {reason, approver_id}
 *   POST /api/v1/admin/accounts/{id}/close          {reason} → 202 dual-control
 *   POST /api/v1/admin/accounts/{id}/jurisdiction   {jurisdiction_code, justification}
 *   PUT  /api/v1/admin/accounts/{id}/sub-account-limit {max_sub_accounts}
 *   PUT  /api/v1/admin/accounts/{id}/product-profile   {client_category, evidence}
 *
 * BoundAdminApi exposes get/post only — PUT goes through the raw
 * ApiClient with the X-Admin-Env stamp applied by hand.
 */
import type { ApiClient } from '@/lib/api/client';
import { malformed } from '@/lib/admin/api';
import { ADMIN_ENV_HEADER, type AdminEnv, type BoundAdminApi } from '@/lib/env';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}

const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;

const envOpts = (env: AdminEnv) => ({ headers: { [ADMIN_ENV_HEADER]: env } });

// ---------------------------------------------------------------------------
// Ticket detail + notes (Phase-07 Task 7.3.7 backend).
// ---------------------------------------------------------------------------

export interface TicketNote {
  noteId: number;
  body: string;
  internal: boolean;
  authorAdminId?: number;
  createdAt: string;
}

export interface TicketDetail {
  ticket: unknown; // SupportTicket shape — rendered verbatim, not re-typed
  notes: TicketNote[];
}

function parseNote(v: unknown): TicketNote | null {
  if (!isRecord(v)) return null;
  const noteId = num(v['note_id']);
  const body = str(v['body']);
  if (noteId === undefined || body === undefined) return null;
  return {
    noteId,
    body,
    internal: v['internal'] === true,
    authorAdminId: num(v['author_admin_id']),
    createdAt: str(v['created_at']) ?? '',
  };
}

export async function fetchTicketDetail(api: BoundAdminApi, id: number): Promise<TicketDetail> {
  const raw = await api.get<unknown>(`/admin/support/tickets/${id}`);
  if (!isRecord(raw) || raw['ticket'] === undefined) throw malformed('ticket detail');
  const notes: TicketNote[] = [];
  if (Array.isArray(raw['notes'])) {
    for (const n of raw['notes']) {
      const p = parseNote(n);
      if (p !== null) notes.push(p);
    }
  }
  return { ticket: raw['ticket'], notes };
}

export async function addTicketNote(
  api: BoundAdminApi,
  id: number,
  body: string,
  internal = true,
): Promise<void> {
  await api.post(`/admin/support/tickets/${id}/notes`, { body, internal });
}

// ---------------------------------------------------------------------------
// Tax self-certification review (Task 10.5.3.4 seam — audit-logged read).
// ---------------------------------------------------------------------------

export interface SelfCert {
  id: number;
  formType: string;
  tinCountry?: string;
  tinKind?: string;
  status: string;
  tinValidatedAt?: string;
  supersededBy?: number;
  createdAt: string;
}

function parseSelfCert(v: unknown): SelfCert | null {
  if (!isRecord(v)) return null;
  const id = num(v['id']);
  const formType = str(v['form_type']);
  if (id === undefined || formType === undefined) return null;
  return {
    id,
    formType,
    tinCountry: str(v['tin_country']),
    tinKind: str(v['tin_kind']),
    status: str(v['status']) ?? '',
    tinValidatedAt: str(v['tin_validated_at']),
    supersededBy: num(v['superseded_by']),
    createdAt: str(v['created_at']) ?? '',
  };
}

export async function fetchSelfCerts(api: BoundAdminApi, accountId: number): Promise<SelfCert[]> {
  const raw = await api.get<unknown>(`/admin/accounts/${accountId}/self-certifications`);
  if (!isRecord(raw) || !Array.isArray(raw['certifications']))
    throw malformed('self-certifications');
  return (raw['certifications'] as unknown[])
    .map(parseSelfCert)
    .filter((c): c is SelfCert => c !== null);
}

// ---------------------------------------------------------------------------
// Lifecycle mutations — freeze/unfreeze carry an inline four-eyes
// approver; close submits to the dual-control queue (202 ≠ executed).
// ---------------------------------------------------------------------------

export async function freezeAccount(
  api: BoundAdminApi,
  accountId: number,
  freeze: boolean,
  reason: string,
  approverId: number,
): Promise<void> {
  await api.post(`/admin/accounts/${accountId}/${freeze ? 'freeze' : 'unfreeze'}`, {
    reason,
    approver_id: approverId,
  });
}

export interface CloseResult {
  requestId?: number;
  message: string;
}

export async function closeAccount(
  api: BoundAdminApi,
  accountId: number,
  reason: string,
): Promise<CloseResult> {
  const raw = await api.post<unknown>(`/admin/accounts/${accountId}/close`, { reason });
  if (!isRecord(raw)) throw malformed('close response');
  const req = isRecord(raw['request']) ? num(raw['request']['id']) : undefined;
  return {
    requestId: req,
    message: str(raw['message']) ?? 'dual-control request submitted',
  };
}

export async function pinJurisdiction(
  api: BoundAdminApi,
  accountId: number,
  jurisdictionCode: string,
  justification: string,
): Promise<void> {
  await api.post(`/admin/accounts/${accountId}/jurisdiction`, {
    jurisdiction_code: jurisdictionCode,
    justification,
  });
}

export async function setSubAccountLimit(
  client: ApiClient,
  env: AdminEnv,
  accountId: number,
  maxSubAccounts: number,
): Promise<void> {
  await client.put(
    `/admin/accounts/${accountId}/sub-account-limit`,
    { max_sub_accounts: maxSubAccounts },
    envOpts(env),
  );
}

export async function setClientCategory(
  client: ApiClient,
  env: AdminEnv,
  accountId: number,
  clientCategory: string,
  evidence: string,
): Promise<void> {
  await client.put(
    `/admin/accounts/${accountId}/product-profile`,
    { client_category: clientCategory, evidence },
    envOpts(env),
  );
}

/**
 * POST /admin/accounts/{id}/product-profile — Task 14.3.13 pricing-profile
 * assignment (distinct from the PUT client_category variant above).
 * Server enforces ACTIVE profile, no open exposure, zero balances on
 * divisor-changing switches.
 */
export async function assignProductProfile(
  api: BoundAdminApi,
  accountId: number,
  profileCode: string,
): Promise<void> {
  await api.post(`/admin/accounts/${accountId}/product-profile`, {
    profile_code: profileCode,
  });
}
