/**
 * Support-ticket API — Task 10.3.25 (backend: Phase-07 Task 7.3.7, LIVE).
 *
 *   GET  /api/v1/support/tickets         → {data, next_cursor, limit, total}
 *   POST /api/v1/support/tickets         {category, subject, body, priority,
 *                                         origin_channel[, attachments]}
 *   GET  /api/v1/support/tickets/{id}    → {ticket, notes[]}
 *
 * Canonical enums (services/internal/support/tickets.go):
 *   categories FUNDING|TRADING|KYC|TECHNICAL|COMPLAINT
 *   types      SUPPORT|COMPLAINT|DISPUTE
 *   priorities LOW|NORMAL|HIGH|URGENT
 *   statuses   OPEN|PENDING|IN_PROGRESS|RESOLVED|CLOSED
 *
 * SLA: 8-business-hour acknowledgement (sla_due_at) and 8-week final
 * response for complaints (final_response_due_at).
 *
 * Deviations (documented in the delivery report):
 *   - Attachments ride as an extra `attachments` JSON field — the create
 *     contract ignores unknown keys until a binary-upload route exists.
 *   - Client-side reply/reopen POSTs to /support/tickets/{id}/reopen —
 *     not yet in the route registry; failures surface the envelope error.
 */
import type { ApiClient } from '@/lib/api';

export const TICKET_CATEGORIES = ['FUNDING', 'TRADING', 'KYC', 'TECHNICAL', 'COMPLAINT'] as const;
export type TicketCategory = (typeof TICKET_CATEGORIES)[number];

export const TICKET_PRIORITIES = ['LOW', 'NORMAL', 'HIGH', 'URGENT'] as const;
export const TICKET_STATUSES = ['OPEN', 'PENDING', 'IN_PROGRESS', 'RESOLVED', 'CLOSED'] as const;

/** SLA constants (spec/Phase-07): 8 business hours acknowledgement,
 * 8 weeks final response for complaints (MiFID complaint handling). */
export const ACK_SLA_BUSINESS_HOURS = 8;
export const COMPLAINT_FINAL_WEEKS = 8;
export const MAX_ATTACHMENT_BYTES = 5 * 1024 * 1024;

export interface Ticket {
  ticket_id: number;
  account_id?: number;
  type?: string;
  category: string;
  priority: string;
  subject: string;
  body?: string;
  status: string;
  queue?: string;
  assignee_admin_id?: number;
  origin_channel?: string;
  sla_due_at?: string;
  final_response_due_at?: string;
  acknowledged_at?: string | null;
  resolved_at?: string;
  created_at: string;
  updated_at: string;
  sla_breached?: boolean;
  final_sla_breached?: boolean;
}

export interface TicketNote {
  note_id: number;
  ticket_id: number;
  author_id?: number;
  author_kind: string; // ADMIN | CLIENT
  internal?: boolean;
  body: string;
  created_at: string;
}

export interface TicketDetail {
  ticket: Ticket;
  notes: TicketNote[];
}

export interface TicketEnvelope {
  data: Ticket[];
  next_cursor?: string;
  limit?: number;
  total?: number;
}

export async function listTickets(
  api: ApiClient,
  opts: { status?: string; category?: string; cursor?: string } = {},
): Promise<TicketEnvelope> {
  const q = new URLSearchParams();
  if (opts.status !== undefined && opts.status !== '') q.set('status', opts.status);
  if (opts.category !== undefined && opts.category !== '') q.set('category', opts.category);
  if (opts.cursor !== undefined && opts.cursor !== '') q.set('cursor', opts.cursor);
  const qs = q.toString();
  const res = await api.get<TicketEnvelope | Ticket[]>(
    `/support/tickets${qs === '' ? '' : `?${qs}`}`,
  );
  if (Array.isArray(res)) return { data: res };
  return res;
}

export interface TicketAttachmentInput {
  filename: string;
  content_type: string;
  data_base64: string;
}

export async function createTicket(
  api: ApiClient,
  input: {
    category: TicketCategory;
    subject: string;
    body: string;
    priority: (typeof TICKET_PRIORITIES)[number];
    /** Financial-impact justification — required client-side for URGENT
     * when the account is below T2; forwarded as an extra field. */
    urgencyJustification?: string;
    attachments?: TicketAttachmentInput[];
  },
): Promise<Ticket> {
  const res = await api.post<{ ticket?: Ticket } | Ticket>('/support/tickets', {
    category: input.category,
    subject: input.subject,
    body: input.body,
    priority: input.priority,
    origin_channel: 'web',
    urgency_justification: input.urgencyJustification,
    attachments: input.attachments,
  });
  if ('ticket' in res && res.ticket !== undefined) return res.ticket;
  return res as Ticket;
}

export async function getTicket(api: ApiClient, id: string): Promise<TicketDetail> {
  const res = await api.get<{ ticket?: Ticket; notes?: TicketNote[] }>(
    `/support/tickets/${encodeURIComponent(id)}`,
  );
  return { ticket: res.ticket ?? (res as unknown as Ticket), notes: res.notes ?? [] };
}

/** User-side reopen (RESOLVED/CLOSED → OPEN). Route not yet registered —
 * see the module-level deviation note. */
export async function reopenTicket(api: ApiClient, id: number): Promise<void> {
  await api.post(`/support/tickets/${id}/reopen`, {});
}

/** Urgent priority is available to T2+ accounts, or lower tiers with a
 * financial-impact justification (Task 10.3.25 item 2). */
export function urgentAllowed(kycTier: string | null | undefined, justification: string): boolean {
  if (kycTier === 'T2' || kycTier === 'INSTITUTIONAL') return true;
  return justification.trim().length > 0;
}
