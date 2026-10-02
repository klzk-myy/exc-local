/**
 * Webhook endpoint management (Phase-05 Task 5.3.17). Routes are live:
 *   POST   /webhooks                        — register + one-time secret
 *   GET    /webhooks                        — list + allowed_events
 *   DELETE /webhooks/{id}                   — disable
 *   POST   /webhooks/{id}/rotate-secret     — overlap ≤72h
 *   GET    /webhooks/{id}/deliveries        — delivery log incl. DLQ rows
 *   GET    /admin/webhooks/dead-letters     — admin DLQ (Task 14.3.12)
 *   POST   /admin/webhooks/dead-letters/{id}/retransmit
 */
import type { ApiClient } from '@/lib/api';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
function str(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}
function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

export interface WebhookEndpoint {
  id: string;
  url: string;
  events: string[];
  status: string;
  secretOverlapUntil: string | null;
  createdAt: string;
}

export interface WebhookDelivery {
  id: string;
  event: string;
  status: string;
  attempts: number;
  lastStatusCode: number | null;
  lastError: string | null;
  nextAttemptAt: string | null;
  deliveredAt: string | null;
  createdAt: string;
}

export function parseEndpoint(v: unknown): WebhookEndpoint | null {
  if (!isRecord(v)) return null;
  const id = str(v['endpoint_id']) ?? str(v['id']) ?? (num(v['id']) !== undefined ? String(v['id']) : null);
  const url = str(v['url']);
  const createdAt = str(v['created_at']);
  if (!id || !url || !createdAt) return null;
  const events: string[] = Array.isArray(v['events'])
    ? v['events'].filter((e): e is string => typeof e === 'string')
    : [];
  return {
    id,
    url,
    events,
    status: str(v['status']) ?? 'ACTIVE',
    secretOverlapUntil: str(v['secret_overlap_until']) ?? null,
    createdAt,
  };
}

export function parseDelivery(v: unknown): WebhookDelivery | null {
  if (!isRecord(v)) return null;
  const id = str(v['delivery_id']) ?? str(v['id']) ?? (num(v['id']) !== undefined ? String(v['id']) : null);
  const event = str(v['event']);
  const status = str(v['status']);
  if (!id || !event || !status) return null;
  return {
    id,
    event,
    status,
    attempts: num(v['attempts']) ?? 0,
    lastStatusCode: num(v['last_status_code']) ?? null,
    lastError: str(v['last_error']) ?? null,
    nextAttemptAt: str(v['next_attempt_at']) ?? null,
    deliveredAt: str(v['delivered_at']) ?? null,
    createdAt: str(v['created_at']) ?? '',
  };
}

export interface WebhookList {
  endpoints: WebhookEndpoint[];
  allowedEvents: string[];
}

export async function listWebhooks(api: ApiClient): Promise<WebhookList> {
  const res = await api.get<unknown>('/webhooks');
  const rec = isRecord(res) ? res : {};
  const raw = Array.isArray(rec['webhooks']) ? rec['webhooks'] : [];
  const allowed: string[] = Array.isArray(rec['allowed_events'])
    ? rec['allowed_events'].filter((e): e is string => typeof e === 'string')
    : [];
  return {
    endpoints: raw.map(parseEndpoint).filter((e): e is WebhookEndpoint => e !== null),
    allowedEvents: allowed,
  };
}

export async function registerWebhook(
  api: ApiClient,
  url: string,
  events: string[],
): Promise<{ endpoint: WebhookEndpoint; secret: string }> {
  const res = await api.post<unknown>('/webhooks', { url, events });
  const rec = isRecord(res) ? res : {};
  const endpoint = parseEndpoint(rec['endpoint']);
  const secret = str(rec['secret']);
  if (!endpoint || !secret) throw new Error('webhooks: malformed registration response');
  return { endpoint, secret };
}

export async function disableWebhook(api: ApiClient, id: string): Promise<void> {
  await api.delete(`/webhooks/${id}`);
}

export async function rotateSecret(
  api: ApiClient,
  id: string,
  overlapSeconds: number,
): Promise<string> {
  const res = await api.post<unknown>(`/webhooks/${id}/rotate-secret`, {
    overlap_seconds: overlapSeconds,
  });
  const secret = isRecord(res) ? str(res['secret']) : undefined;
  if (!secret) throw new Error('webhooks: malformed rotation response');
  return secret;
}

export async function listDeliveries(
  api: ApiClient,
  endpointId: string,
): Promise<WebhookDelivery[]> {
  const res = await api.get<unknown>(
    `/webhooks/${endpointId}/deliveries?limit=200`,
  );
  const rec = isRecord(res) ? res : {};
  const raw = Array.isArray(rec['deliveries']) ? rec['deliveries'] : [];
  return raw.map(parseDelivery).filter((d): d is WebhookDelivery => d !== null);
}

// ---- Admin DLQ (Task 14.3.12) ----

export async function listDeadLetters(api: ApiClient): Promise<WebhookDelivery[]> {
  const res = await api.get<unknown>('/admin/webhooks/dead-letters?limit=200');
  const rec = isRecord(res) ? res : {};
  const raw = Array.isArray(rec['dead_letters']) ? rec['dead_letters'] : [];
  return raw.map(parseDelivery).filter((d): d is WebhookDelivery => d !== null);
}

export async function retransmitDeadLetter(api: ApiClient, deliveryId: string): Promise<void> {
  await api.post<unknown>(`/admin/webhooks/dead-letters/${deliveryId}/retransmit`);
}
