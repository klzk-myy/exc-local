/**
 * WS client state machine — NORMATIVE per Phase-10 Task 10.3.19
 * (spec §2.7, §10.6, §21.3, §24 #310).
 *
 * Pure reducer: `transition(snapshot, event) → { state, ctx, effects }`.
 * The reducer owns no timers, sockets, or I/O — `client.ts` interprets
 * the emitted effects against injected clock/socket/token ports, so the
 * entire transition table is unit-testable deterministically.
 *
 * Canonical states (order-entry lock semantics per Task 10.3.19):
 *
 *   CONNECTING    dialing + auth handshake                      locked
 *   AUTHENTICATED subscribed, live, per-channel last_seq live   enabled
 *   STALE         connected, no md tick >3.0s in trading hours  enabled+amber
 *   DISCONNECTED  socket closed                                 locked+banner
 *   RECONNECTING  waiting on backoff attempt                    locked+banner
 *   RESYNCING     handshake OK, ring-buffer replay/snapshot     locked until
 *                 every subscribed channel converges
 *
 * Invariant: the client may never hold optimistic order state across a
 * full re-authentication — enforced by the `flush-optimistic` effect on
 * every authenticate after the first, and on auth-refresh failure.
 */
import { RECONNECT_JITTER_RATIO, RECONNECT_SCHEDULE_MS, WS_CLOSE_CODES } from './constants';

export type WsState =
  'CONNECTING' | 'AUTHENTICATED' | 'STALE' | 'DISCONNECTED' | 'RECONNECTING' | 'RESYNCING';

export interface WsMachineCtx {
  /** Reconnect attempt index into RECONNECT_SCHEDULE_MS. Resets to 0 on a
   * successful handshake (socket open) — NOT on stream resume, and NOT on
   * a server.shutdown advisory (Task 10.3.19). */
  attempt: number;
  /** Channels awaiting convergence during RESYNCING (resumed/snapshot/
   * subscribed confirmations outstanding). */
  pendingConvergence: number;
}

export const INITIAL_CTX: WsMachineCtx = { attempt: 0, pendingConvergence: 0 };

export type WsEvent =
  /** User/runtime asks the client to connect. */
  | { type: 'connect' }
  /** WebSocket opened = handshake success (resets the attempt counter). */
  | { type: 'socket-open' }
  /** Auth ACK (or anonymous handshake completion). `resumeCount` = number
   * of subscribed channels carrying a live `last_seq` cursor that must
   * converge via resume/replay/snapshot before going AUTHENTICATED. */
  | { type: 'auth-ack'; resumeCount: number }
  /** Dial failed, connect timeout, or the socket closed mid-handshake. */
  | { type: 'connect-failed' }
  /** Socket closed after the handshake (code is the RFC 6455 close code). */
  | { type: 'socket-closed'; code: number }
  /** No inbound frame inside the heartbeat window — dead connection. */
  | { type: 'heartbeat-missed' }
  /** Channel data arrived (event/snapshot/resumed — any tick). */
  | { type: 'tick' }
  /** Stale monitor observed silence > STALE_TICK_THRESHOLD_MS during
   * trading hours (24/5 gate handled by the client before dispatching). */
  | { type: 'stale-timer' }
  /** One channel converged during RESYNCING. */
  | { type: 'channel-converged' }
  /** Backoff timer fired — dial again. */
  | { type: 'backoff-elapsed' }
  /** Close-4019 path: token refresh completed → reconnect immediately. */
  | { type: 'auth-refresh-ok' }
  /** Close-4019 path: refresh failed → flush optimistic state, stay down. */
  | { type: 'auth-refresh-failed' }
  /** `server.shutdown` advisory (Task 6.3.19). retryAfterMs overrides the
   * computed backoff; the attempt counter is NOT reset nor incremented. */
  | { type: 'shutdown-advisory'; retryAfterMs?: number }
  /** User-initiated close — terminal DISCONNECTED, no reconnect. */
  | { type: 'disconnect' };

export type WsEffect =
  | { kind: 'dial' }
  | { kind: 'close-socket' }
  | { kind: 'send-authenticate' }
  /** Re-apply the subscription registry: `resume` for channels holding a
   * `last_seq` cursor, `subscribe` for the rest. Emitted on every entry
   * into the post-handshake phase. */
  | { kind: 're-establish-channels' }
  | { kind: 'schedule-backoff'; delayMs: number }
  /** Close-4019 path: call the configured token refresher. */
  | { kind: 'refresh-auth' }
  /** Invariant hook: drop all optimistic order state (re-auth boundary or
   * auth-refresh failure). */
  | { kind: 'flush-optimistic' }
  /** Refresh failed — surface to the app so it can route to /login. */
  | { kind: 'notify-auth-failure' }
  | { kind: 'start-heartbeat' }
  | { kind: 'stop-heartbeat' }
  | { kind: 'start-stale-monitor' }
  | { kind: 'stop-stale-monitor' };

export interface WsStep {
  /** Terminal state after the event. */
  state: WsState;
  /** Every state visited, including transient intermediates (e.g.
   * DISCONNECTED is always traversed on the way to RECONNECTING so the
   * banner semantics stay observable). */
  visited: WsState[];
  ctx: WsMachineCtx;
  effects: WsEffect[];
}

export function initialSnapshot(): { state: WsState; ctx: WsMachineCtx } {
  return { state: 'DISCONNECTED', ctx: { ...INITIAL_CTX } };
}

/** spec §21.3: delay = schedule[min(attempt, last)] with ±20% jitter. */
export function backoffDelayMs(attempt: number, rng: () => number = Math.random): number {
  const idx = Math.min(Math.max(attempt, 0), RECONNECT_SCHEDULE_MS.length - 1);
  const base = RECONNECT_SCHEDULE_MS[idx] ?? 10_000;
  const jitter = base * RECONNECT_JITTER_RATIO;
  return Math.max(1, Math.round(base + (rng() * 2 - 1) * jitter));
}

/** Order-entry lock (Task 10.3.19 table): only AUTHENTICATED and STALE
 * permit order entry; everything else is locked. */
export function orderEntryEnabled(state: WsState): boolean {
  return state === 'AUTHENTICATED' || state === 'STALE';
}

/** Whether the disconnect banner must render. */
export function bannerRequired(state: WsState): boolean {
  return state === 'DISCONNECTED' || state === 'RECONNECTING';
}

interface Snapshot {
  state: WsState;
  ctx: WsMachineCtx;
}

function step(s: Snapshot, to: WsState, ctx: Partial<WsMachineCtx>, effects: WsEffect[]): WsStep {
  return {
    state: to,
    visited: to === s.state ? [to] : [s.state, to],
    ctx: { ...s.ctx, ...ctx },
    effects,
  };
}

/** Composite step that traverses DISCONNECTED on the way to RECONNECTING —
 * the canonical "socket lost" path. */
function lostConnection(
  s: Snapshot,
  attempt: number,
  delayMs: number,
  extra: WsEffect[] = [],
): WsStep {
  return {
    state: 'RECONNECTING',
    visited:
      s.state === 'DISCONNECTED'
        ? ['DISCONNECTED', 'RECONNECTING']
        : [s.state, 'DISCONNECTED', 'RECONNECTING'],
    ctx: { ...s.ctx, attempt },
    effects: [
      { kind: 'stop-heartbeat' },
      { kind: 'stop-stale-monitor' },
      ...extra,
      { kind: 'schedule-backoff', delayMs },
    ],
  };
}

function stillWaiting(s: Snapshot): WsStep {
  return { state: s.state, visited: [s.state], ctx: s.ctx, effects: [] };
}

/**
 * The transition table. Anything not listed is a no-op (visited=[state],
 * no effects) — the machine is total over the event type.
 */
export function transition(s: Snapshot, e: WsEvent, rng: () => number = Math.random): WsStep {
  const nextAttempt = s.ctx.attempt + 1;

  switch (e.type) {
    case 'connect':
      if (s.state === 'DISCONNECTED') {
        return step(s, 'CONNECTING', { attempt: 0, pendingConvergence: 0 }, [
          { kind: 'dial' },
          { kind: 'start-heartbeat' },
        ]);
      }
      return stillWaiting(s);

    case 'socket-open':
      if (s.state !== 'CONNECTING') return stillWaiting(s);
      // Successful handshake resets the attempt counter (Task 10.3.19).
      // Auth ack decides AUTHENTICATED vs RESYNCING.
      return step(s, 'CONNECTING', { attempt: 0 }, [
        { kind: 'send-authenticate' },
        { kind: 'start-heartbeat' },
      ]);

    case 'auth-ack':
      if (s.state !== 'CONNECTING') return stillWaiting(s);
      if (e.resumeCount > 0) {
        return step(s, 'RESYNCING', { pendingConvergence: e.resumeCount }, [
          { kind: 're-establish-channels' },
        ]);
      }
      return step(s, 'AUTHENTICATED', { pendingConvergence: 0 }, [
        { kind: 're-establish-channels' },
        { kind: 'start-stale-monitor' },
      ]);

    case 'channel-converged': {
      if (s.state !== 'RESYNCING') return stillWaiting(s);
      const remaining = Math.max(0, s.ctx.pendingConvergence - 1);
      if (remaining > 0) return step(s, 'RESYNCING', { pendingConvergence: remaining }, []);
      return step(s, 'AUTHENTICATED', { pendingConvergence: 0 }, [{ kind: 'start-stale-monitor' }]);
    }

    case 'tick':
      if (s.state === 'STALE') {
        // Any tick clears STALE (AUTHENTICATED ↔ STALE, Task 10.3.19).
        return step(s, 'AUTHENTICATED', {}, []);
      }
      return stillWaiting(s);

    case 'stale-timer':
      if (s.state === 'AUTHENTICATED') return step(s, 'STALE', {}, []);
      return stillWaiting(s);

    case 'backoff-elapsed':
      if (s.state === 'RECONNECTING') {
        return step(s, 'CONNECTING', {}, [{ kind: 'dial' }]);
      }
      return stillWaiting(s);

    case 'auth-refresh-ok':
      // Task 10.3.19: AUTH_EXPIRED forces token refresh then CONNECTING —
      // immediate re-dial, no additional backoff.
      if (s.state === 'DISCONNECTED' || s.state === 'RECONNECTING') {
        return step(s, 'CONNECTING', {}, [{ kind: 'dial' }]);
      }
      return stillWaiting(s);

    case 'auth-refresh-failed':
      // Refresh failure: flush ALL optimistic order state and notify the
      // app (→ /login). Terminal until `connect` fires again.
      return step(s, 'DISCONNECTED', { pendingConvergence: 0 }, [
        { kind: 'flush-optimistic' },
        { kind: 'notify-auth-failure' },
      ]);

    case 'shutdown-advisory':
      // Task 10.3.19: RECONNECTING on the advisory's target WITHOUT
      // resetting/incrementing the attempt counter.
      if (s.state === 'DISCONNECTED' || s.state === 'RECONNECTING') return stillWaiting(s);
      return lostConnection(
        s,
        s.ctx.attempt,
        e.retryAfterMs ?? backoffDelayMs(s.ctx.attempt, rng),
        [{ kind: 'close-socket' }],
      );

    case 'heartbeat-missed':
      // Close the dead socket; the socket-closed event performs the
      // transition so we never double-schedule backoff.
      if (s.state === 'DISCONNECTED' || s.state === 'RECONNECTING') return stillWaiting(s);
      return step(s, s.state, {}, [{ kind: 'close-socket' }]);

    case 'connect-failed':
      if (s.state !== 'CONNECTING') return stillWaiting(s);
      return lostConnection(s, nextAttempt, backoffDelayMs(s.ctx.attempt, rng));

    case 'socket-closed':
      if (s.state === 'CONNECTING') {
        return lostConnection(s, nextAttempt, backoffDelayMs(s.ctx.attempt, rng));
      }
      if (s.state === 'AUTHENTICATED' || s.state === 'STALE' || s.state === 'RESYNCING') {
        if (e.code === WS_CLOSE_CODES.AUTH_EXPIRED) {
          // 4019 AUTH_EXPIRED → refresh then CONNECTING; the socket is
          // already gone, so land in DISCONNECTED pending the refresh.
          return step(s, 'DISCONNECTED', { pendingConvergence: 0 }, [
            { kind: 'stop-heartbeat' },
            { kind: 'stop-stale-monitor' },
            { kind: 'refresh-auth' },
          ]);
        }
        return lostConnection(s, nextAttempt, backoffDelayMs(s.ctx.attempt, rng));
      }
      // Already RECONNECTING/DISCONNECTED — the backoff is scheduled (e.g.
      // the close that follows a shutdown advisory); do not double-book.
      return stillWaiting(s);

    case 'disconnect':
      return step(s, 'DISCONNECTED', { attempt: 0, pendingConvergence: 0 }, [
        { kind: 'close-socket' },
        { kind: 'stop-heartbeat' },
        { kind: 'stop-stale-monitor' },
      ]);

    default:
      return stillWaiting(s);
  }
}
