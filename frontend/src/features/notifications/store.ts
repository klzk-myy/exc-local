/**
 * Session notification store — the `private:notifications` WS channel is
 * push-only (no REST history exists in the route registry), so the inbox
 * holds this session's deliveries in a bounded ring. Cross-reload
 * history is server-side only; the UI never fabricates entries.
 *
 * Wire payload per frame `data` (notifications.WSSender):
 *   { delivery_id, event, subject, payload, sent_at }
 */
import { useSyncExternalStore } from 'react';

export interface InboxItem {
  deliveryId: number | null;
  event: string;
  subject: string;
  payload: unknown;
  sentAt: string;
}

const MAX_ITEMS = 200;

interface InboxSnapshot {
  items: readonly InboxItem[];
  unread: number;
}

let items: InboxItem[] = [];
let unread = 0;
let snap: InboxSnapshot = { items, unread };
const listeners = new Set<() => void>();

function emit(): void {
  snap = { items, unread };
  for (const l of listeners) l();
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}

/** push delivers one `private:notifications` frame payload into the
 * ring. Malformed payloads are dropped (never rendered raw). */
export function pushNotification(data: unknown): void {
  if (!isRecord(data)) return;
  const subject = typeof data['subject'] === 'string' ? data['subject'] : null;
  const event = typeof data['event'] === 'string' ? data['event'] : null;
  if (!subject || !event) return;
  const deliveryId =
    typeof data['delivery_id'] === 'number' && Number.isFinite(data['delivery_id'])
      ? data['delivery_id']
      : null;
  const sentAt = typeof data['sent_at'] === 'string' ? data['sent_at'] : new Date().toISOString();
  items = [
    { deliveryId, event, subject, payload: data['payload'], sentAt },
    ...items.slice(0, MAX_ITEMS - 1),
  ];
  unread += 1;
  emit();
}

export function markAllRead(): void {
  if (unread === 0) return;
  unread = 0;
  emit();
}

/** Test seam — clears the ring. */
export function resetNotifications(): void {
  items = [];
  unread = 0;
  emit();
}

function subscribe(l: () => void): () => void {
  listeners.add(l);
  return () => {
    listeners.delete(l);
  };
}

/** Live inbox snapshot — referentially stable between pushes. */
export function useInbox(): InboxSnapshot {
  return useSyncExternalStore(subscribe, () => snap);
}
