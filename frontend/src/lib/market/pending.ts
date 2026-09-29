/**
 * Optimistic-order store (Task 10.3.19 item 4 / Task 10.3.3 residual).
 *
 * Orders render as PENDING immediately on submit. Resolution paths:
 *   - REST ack (HTTP 202)                    → confirmed, removed
 *   - private:orders orderAck/orderFill      → confirmed, removed
 *   - private:orders orderReject             → rolled back + notice
 *   - REST ApiError                          → rolled back + notice
 *   - NetworkError / no event within TTL     → rolled back + notice
 *   - WS re-authentication (authEpoch bump)  → ALL pending flushed +
 *     notice — the Task 10.3.19 invariant: optimistic order state must
 *     never survive a full re-authentication. The ws machine's
 *     `flush-optimistic` effect bumps `session.authEpoch`; items carry
 *     the epoch they were submitted under, so a sweep drops stale ones.
 *
 * Notices are the visible rollback surface (error toasts with §23 code +
 * request_id per spec §8.7 correlation).
 */
import { create } from 'zustand';

export interface PendingOrder {
  clientOrderId: string;
  symbol: string;
  side: 'BUY' | 'SELL';
  type: string;
  quantity: string;
  price?: string;
  /** session.authEpoch at submit time — flush boundary. */
  epoch: number;
  submittedAt: number;
}

export type PendingNoticeKind =
  | 'rejected' // server/order reject (§23 code available)
  | 'timeout' // no confirmation inside the TTL
  | 'flushed' // re-auth boundary flush
  | 'confirmed'; // brief ack surface

export interface PendingNotice {
  id: number;
  kind: PendingNoticeKind;
  clientOrderId: string;
  message: string;
  /** §23 code + RFC 7807 request_id for actionable toasts. */
  code?: string;
  requestId?: string;
  ts: number;
}

interface PendingState {
  pending: PendingOrder[];
  notices: PendingNotice[];
  add: (o: Omit<PendingOrder, 'submittedAt'>) => void;
  /** Confirmed by REST ack or a private:orders lifecycle event. */
  confirm: (clientOrderId: string, message?: string) => void;
  /** Rolled back with a user-visible reason. */
  reject: (
    clientOrderId: string,
    msg: { message: string; code?: string; requestId?: string },
  ) => void;
  timeout: (clientOrderId: string) => void;
  /** Drop every pending entry submitted under an epoch != current. */
  flushEpoch: (currentEpoch: number) => void;
  dismissNotice: (id: number) => void;
  /** Test/maintenance helper — clears pending + notices. */
  reset: () => void;
}

let noticeSeq = 0;
const MAX_NOTICES = 20;

export const usePendingOrders = create<PendingState>((set, get) => {
  function notice(n: Omit<PendingNotice, 'id' | 'ts'>): void {
    noticeSeq += 1;
    set((s) => ({
      notices: [...s.notices, { ...n, id: noticeSeq, ts: Date.now() }].slice(-MAX_NOTICES),
    }));
  }
  function remove(clientOrderId: string): PendingOrder | undefined {
    const item = get().pending.find((p) => p.clientOrderId === clientOrderId);
    set((s) => ({ pending: s.pending.filter((p) => p.clientOrderId !== clientOrderId) }));
    return item;
  }
  return {
    pending: [],
    notices: [],
    add: (o) => {
      set((s) => ({
        pending: [
          ...s.pending.filter((p) => p.clientOrderId !== o.clientOrderId),
          { ...o, submittedAt: Date.now() },
        ],
      }));
    },
    confirm: (clientOrderId, message) => {
      const item = remove(clientOrderId);
      if (item && message) {
        notice({ kind: 'confirmed', clientOrderId, message });
      }
    },
    reject: (clientOrderId, msg) => {
      remove(clientOrderId);
      notice({ kind: 'rejected', clientOrderId, ...msg });
    },
    timeout: (clientOrderId) => {
      const item = remove(clientOrderId);
      if (item) {
        notice({
          kind: 'timeout',
          clientOrderId,
          message: `Order ${clientOrderId} unconfirmed — no acknowledgement received; rolled back`,
        });
      }
    },
    flushEpoch: (currentEpoch) => {
      const stale = get().pending.filter((p) => p.epoch !== currentEpoch);
      if (stale.length === 0) return;
      set((s) => ({ pending: s.pending.filter((p) => p.epoch === currentEpoch) }));
      notice({
        kind: 'flushed',
        clientOrderId: '',
        message: `${stale.length} pending order(s) rolled back — session re-authenticated`,
      });
    },
    dismissNotice: (id) => {
      set((s) => ({ notices: s.notices.filter((n) => n.id !== id) }));
    },
    reset: () => {
      set({ pending: [], notices: [] });
    },
  };
});
