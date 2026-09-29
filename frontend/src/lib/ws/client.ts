/**
 * Reconnecting WebSocket client — executes the normative Task 10.3.19
 * state machine (`machine.ts`) against injected clock/socket/token ports.
 *
 * Responsibilities:
 *   - dial/handshake/authenticate (+ protocol_version per §10.5)
 *   - subscription registry + `{"action":"subscribe","params":[...]}` grammar
 *   - per-channel `last_seq` cursors → `resume`/`resync` replay protocol
 *     (spec §10.7/§10.9); RESYNCING until every channel converges
 *   - exponential backoff RECONNECT_SCHEDULE ±20% jitter (§21.3)
 *   - stale-data surface: per-channel lastTickAt + STALE state (>3.0s
 *     silence in FX trading hours only — 24/5)
 *   - gap detection → `resync` frame + onGap escalation hook
 *   - close 4019 → token refresh → CONNECTING; refresh failure flushes
 *     optimistic state and notifies the app (→ /login)
 *   - `server.shutdown` advisory → RECONNECTING w/o resetting attempts,
 *     honoring the advisory endpoint + retry_after_ms
 */
import { isFxMarketOpen } from '../market/tradingHours';
import {
  CONNECT_TIMEOUT_MS,
  HEARTBEAT_INTERVAL_MS,
  HEARTBEAT_TIMEOUT_MS,
  STALE_MONITOR_INTERVAL_MS,
  STALE_TICK_THRESHOLD_MS,
} from './constants';
import {
  initialSnapshot,
  orderEntryEnabled,
  transition,
  type WsEvent,
  type WsState,
  type WsStep,
} from './machine';
import {
  authenticateFrame,
  parseServerFrame,
  pingFrame,
  refreshTokenFrame,
  resumeFrame,
  resyncFrame,
  subscribeFrame,
  unsubscribeFrame,
  type EventFrame,
  type ServerFrame,
  type SnapshotFrame,
} from './protocol';

// ---------------------------------------------------------------------------
// Ports (adapter boundaries — tests inject fakes)
// ---------------------------------------------------------------------------

export type TimerHandle = number;

export interface Clock {
  now(): number;
  setTimeout(fn: () => void, ms: number): TimerHandle;
  clearTimeout(h: TimerHandle): void;
}

export const systemClock: Clock = {
  now: () => Date.now(),
  setTimeout: (fn, ms) => globalThis.setTimeout(fn, ms) as unknown as TimerHandle,
  clearTimeout: (h) => {
    globalThis.clearTimeout(h);
  },
};

/** Minimal structural socket — a real `WebSocket` satisfies this, tests
 * inject a mock. Property-handler surface only (no addEventListener). */
export interface WsSocketLike {
  readonly readyState: number;
  onopen: (() => void) | null;
  onclose: ((ev: { code: number; reason: string }) => void) | null;
  onerror: (() => void) | null;
  onmessage: ((ev: { data: string }) => void) | null;
  send(data: string): void;
  close(code?: number, reason?: string): void;
}

export interface SubscriptionHealth {
  /** ms epoch of the last inbound frame on this channel (0 = never). */
  lastTickAt: number;
  /** Last applied channel sequence — the `last_seq` resume cursor. */
  lastSeq: number;
  /** True while a resync/snapshot repair is in flight for the channel. */
  resyncing: boolean;
  /** Silence > STALE_TICK_THRESHOLD_MS (consumer-facing staleness flag —
   * the trading-hours gate only applies to the global STALE state). */
  stale: boolean;
}

export interface WsClientStatus {
  state: WsState;
  /** Reconnect attempt index (0 = not currently cycling). */
  attempt: number;
  /** Order-entry lock per Task 10.3.19 (enabled only in AUTHENTICATED/STALE). */
  orderEntryEnabled: boolean;
  /** Channels with a live subscription registration. */
  subscriptions: readonly string[];
  /** Per-channel staleness/seq surface for consumers. */
  health: Readonly<Record<string, SubscriptionHealth>>;
  /** Last terminal error detail (close code, error frame, …). */
  lastError: string | null;
}

export interface WsClientOptions {
  /** WS endpoint (e.g. wss://host/ws/v1) or a resolver for advisories. */
  url: string | (() => string);
  /** Access-token provider; absent/empty ⇒ anonymous session. */
  tokenProvider?: () => string | null | Promise<string | null>;
  /** Refresh hook for close-4019 (spec §10.5). Must resolve to a fresh
   * token; rejection ⇒ optimistic flush + onAuthFailure. */
  tokenRefresher?: () => Promise<string>;
  socketFactory?: (url: string) => WsSocketLike;
  clock?: Clock;
  /** Jitter RNG — tests inject a deterministic source. */
  rng?: () => number;
  staleThresholdMs?: number;
  staleMonitorIntervalMs?: number;
  heartbeatIntervalMs?: number;
  heartbeatTimeoutMs?: number;
  connectTimeoutMs?: number;
  /** FX-trading-hours gate for STALE (default: 24/5 Sun 21:00→Fri 22:00 UTC). */
  isTradingSession?: (nowMs: number) => boolean;
  /** Invariant hook: drop optimistic order state across re-auth. */
  onOptimisticFlush?: () => void;
  /** Refresh-failed hook: the app redirects to /login here. */
  onAuthFailure?: () => void;
  /** Mid-stream gap escalation (spec §10.9): channel, expected, got. */
  onGap?: (channel: string, expectedSeq: number, gotSeq: number) => void;
  /** Resync directive with no inline snapshot → consumer must refetch via
   * REST (GET /api/v1/book/{symbol}?depth=20 for L2, §10.3). */
  onResyncRequired?: (channel: string, reason: string) => void;
}

type MessageHandler = (frame: EventFrame | SnapshotFrame) => void;

interface ChannelState {
  seq: number;
  lastTickAt: number;
  resyncing: boolean;
  /** Awaiting a convergence confirmation (resumed/snapshot/subscribed/
   * resync-ack) during RESYNCING — only flagged channels decrement the
   * machine's pendingConvergence count. */
  awaitingConfirm: boolean;
  handlers: Set<MessageHandler>;
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

export class WsClient {
  private readonly opts: Required<
    Pick<
      WsClientOptions,
      | 'staleThresholdMs'
      | 'staleMonitorIntervalMs'
      | 'heartbeatIntervalMs'
      | 'heartbeatTimeoutMs'
      | 'connectTimeoutMs'
    >
  > &
    WsClientOptions;

  private readonly clock: Clock;
  private readonly rng: () => number;
  private readonly isTradingSession: (nowMs: number) => boolean;

  private snap = initialSnapshot();
  /** Memoized status — `useSyncExternalStore` requires a stable snapshot
   * identity between notifications or React loops forever. */
  private statusCache: WsClientStatus;
  private socket: WsSocketLike | null = null;
  private readonly channels = new Map<string, ChannelState>();
  private readonly listeners = new Set<(s: WsClientStatus) => void>();
  private readonly frameListeners = new Set<(f: ServerFrame) => void>();

  private backoffTimer: TimerHandle | null = null;
  private connectTimer: TimerHandle | null = null;
  private heartbeatTimer: TimerHandle | null = null;
  private staleTimer: TimerHandle | null = null;
  private lastInboundAt = 0;
  private authRequestInFlight = false;
  private hasAuthenticatedBefore = false;
  private advisoryEndpoint: string | null = null;
  private stopped = true;

  constructor(options: WsClientOptions) {
    this.opts = {
      staleThresholdMs: STALE_TICK_THRESHOLD_MS,
      staleMonitorIntervalMs: STALE_MONITOR_INTERVAL_MS,
      heartbeatIntervalMs: HEARTBEAT_INTERVAL_MS,
      heartbeatTimeoutMs: HEARTBEAT_TIMEOUT_MS,
      connectTimeoutMs: CONNECT_TIMEOUT_MS,
      ...options,
    };
    this.clock = options.clock ?? systemClock;
    this.rng = options.rng ?? Math.random;
    this.isTradingSession = options.isTradingSession ?? defaultTradingSessionGate;
    this.statusCache = this.computeStatus();
  }

  // -- public API -----------------------------------------------------------

  /** Stable snapshot — identity only changes when `notify()` rebuilds it
   * after a state/subscription/channel-health mutation. */
  getStatus(): WsClientStatus {
    return this.statusCache;
  }

  private computeStatus(): WsClientStatus {
    const health: Record<string, SubscriptionHealth> = {};
    const now = this.clock.now();
    for (const [ch, s] of this.channels) {
      health[ch] = {
        lastTickAt: s.lastTickAt,
        lastSeq: s.seq,
        resyncing: s.resyncing,
        stale: s.lastTickAt > 0 && now - s.lastTickAt > this.opts.staleThresholdMs,
      };
    }
    return {
      state: this.snap.state,
      attempt: this.snap.ctx.attempt,
      orderEntryEnabled: orderEntryEnabled(this.snap.state),
      subscriptions: [...this.channels.keys()],
      health,
      lastError: null,
    };
  }

  /** useSyncExternalStore-compatible change subscription. */
  onStatusChange(fn: (s: WsClientStatus) => void): () => void {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  }

  /** Raw server-frame tap for diagnostics/tests. */
  onFrame(fn: (f: ServerFrame) => void): () => void {
    this.frameListeners.add(fn);
    return () => {
      this.frameListeners.delete(fn);
    };
  }

  start(): void {
    if (!this.stopped) return;
    this.stopped = false;
    this.dispatch({ type: 'connect' });
  }

  stop(): void {
    this.stopped = true;
    this.clearTimer('backoff');
    this.clearTimer('connect');
    this.dispatch({ type: 'disconnect' });
  }

  /**
   * Subscribe a channel and register its data handler. Idempotent: repeat
   * calls add a handler without re-sending the frame. Returns an
   * unsubscribe that drops the handler (and the channel when its last
   * handler detaches).
   */
  subscribe(channel: string, onMessage: MessageHandler): () => void {
    let cs = this.channels.get(channel);
    if (!cs) {
      cs = { seq: 0, lastTickAt: 0, resyncing: false, awaitingConfirm: false, handlers: new Set() };
      this.channels.set(channel, cs);
      // Live session → send immediately; otherwise the re-establish pass
      // after the next handshake picks it up.
      if (this.isLiveState()) this.socket?.send(subscribeFrame([channel]));
      this.notify();
    }
    cs.handlers.add(onMessage);
    return () => {
      const c = this.channels.get(channel);
      if (!c) return;
      c.handlers.delete(onMessage);
      if (c.handlers.size === 0) this.unsubscribe(channel);
    };
  }

  unsubscribe(channel: string): void {
    if (!this.channels.delete(channel)) return;
    if (this.isLiveState()) this.socket?.send(unsubscribeFrame([channel]));
    this.notify();
  }

  /** Epoch ms of the last tick on a channel — the stale-data surface for
   * consumers that want a per-subscription timestamp (Task 10.3.19). */
  lastTickAt(channel: string): number {
    return this.channels.get(channel)?.lastTickAt ?? 0;
  }

  /** Current resume cursor for a channel (visible for diagnostics). */
  lastSeq(channel: string): number {
    return this.channels.get(channel)?.seq ?? 0;
  }

  // -- machine plumbing ------------------------------------------------------

  private isLiveState(): boolean {
    return this.snap.state === 'AUTHENTICATED' || this.snap.state === 'STALE';
  }

  private dispatch(e: WsEvent): void {
    const s = transition(this.snap, e, this.rng);
    this.snap = { state: s.state, ctx: s.ctx };
    this.runEffects(s);
    this.notify();
  }

  private runEffects(s: WsStep): void {
    for (const eff of s.effects) {
      switch (eff.kind) {
        case 'dial':
          this.dial();
          break;
        case 'close-socket':
          this.socket?.close();
          break;
        case 'send-authenticate':
          void this.sendAuthenticate();
          break;
        case 're-establish-channels':
          this.reEstablishChannels();
          break;
        case 'schedule-backoff':
          this.armTimer('backoff', eff.delayMs, () => {
            this.dispatch({ type: 'backoff-elapsed' });
          });
          break;
        case 'refresh-auth':
          void this.refreshAuth();
          break;
        case 'flush-optimistic':
          this.opts.onOptimisticFlush?.();
          break;
        case 'notify-auth-failure':
          this.opts.onAuthFailure?.();
          break;
        case 'start-heartbeat':
          this.armTimer('heartbeat', this.opts.heartbeatIntervalMs, () => this.heartbeatTick());
          break;
        case 'stop-heartbeat':
          this.clearTimer('heartbeat');
          break;
        case 'start-stale-monitor':
          this.armTimer('stale', this.opts.staleMonitorIntervalMs, () => this.staleTick());
          break;
        case 'stop-stale-monitor':
          this.clearTimer('stale');
          break;
      }
    }
  }

  private notify(): void {
    const next = this.computeStatus();
    if (statusesEqual(this.statusCache, next)) return;
    this.statusCache = next;
    for (const fn of this.listeners) fn(next);
  }

  // -- socket lifecycle --------------------------------------------------------

  private resolveUrl(): string {
    const base = typeof this.opts.url === 'function' ? this.opts.url() : this.opts.url;
    const url = this.advisoryEndpoint ?? base;
    this.advisoryEndpoint = null; // consumed per-dial
    return url;
  }

  private dial(): void {
    if (this.stopped) return;
    const factory =
      this.opts.socketFactory ?? ((url: string) => new WebSocket(url) as unknown as WsSocketLike);
    const socket = factory(this.resolveUrl());
    this.socket = socket;
    this.lastInboundAt = this.clock.now();

    socket.onopen = () => {
      if (socket !== this.socket) return;
      this.clearTimer('connect');
      this.dispatch({ type: 'socket-open' });
    };
    socket.onmessage = (ev) => {
      if (socket !== this.socket) return;
      this.lastInboundAt = this.clock.now();
      this.handleMessage(ev.data);
    };
    socket.onclose = (ev) => {
      if (socket !== this.socket) return;
      this.socket = null;
      this.clearTimer('connect');
      this.dispatch({ type: 'socket-closed', code: ev.code });
    };
    socket.onerror = () => {
      if (socket !== this.socket) return;
      // onclose follows per RFC 6455; if it doesn't (dial refused), the
      // connect timeout below bounds the wait.
    };

    this.armTimer('connect', this.opts.connectTimeoutMs, () => {
      if (this.snap.state === 'CONNECTING') {
        this.socket?.close();
        this.socket = null;
        this.dispatch({ type: 'connect-failed' });
      }
    });
  }

  private async sendAuthenticate(): Promise<void> {
    if (this.snap.state !== 'CONNECTING' || !this.socket) return;
    const socket = this.socket;
    const token = (await this.opts.tokenProvider?.()) ?? null;
    // Token resolution is async — the socket may have closed meanwhile.
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-condition -- this.snap.state mutates across the await boundary
    if (socket !== this.socket || this.snap.state !== 'CONNECTING') return;
    // Invariant (Task 10.3.19): optimistic order state must never survive
    // a full re-authentication.
    if (this.hasAuthenticatedBefore) this.opts.onOptimisticFlush?.();

    // RESYNCING locks order entry until EVERY subscribed channel converges
    // (Task 10.3.19) — resume channels via replay/snapshot, fresh channels
    // via their `subscribed` ack.
    const convergenceCount = this.channels.size;
    if (token === null) {
      // Anonymous session — handshake complete on open.
      this.dispatch({ type: 'auth-ack', resumeCount: convergenceCount });
      return;
    }
    this.authRequestInFlight = true;
    this.socket.send(authenticateFrame(token));
    // The response/error handler dispatches auth-ack on ACK.
    // A NACK/error tears the socket down server-side; the close event
    // carries the transition.
  }

  private reEstablishChannels(): void {
    const socket = this.socket;
    if (!socket) return;
    const fresh: string[] = [];
    for (const [channel, cs] of this.channels) {
      cs.awaitingConfirm = true;
      if (cs.seq > 0) {
        cs.resyncing = true;
        socket.send(resumeFrame(channel, cs.seq));
      } else {
        fresh.push(channel);
      }
    }
    // Fresh subs converge via their `subscribed` ack frame.
    if (fresh.length > 0) socket.send(subscribeFrame(fresh));
  }

  private async refreshAuth(): Promise<void> {
    const refresher = this.opts.tokenRefresher;
    if (!refresher) {
      this.dispatch({ type: 'auth-refresh-failed' });
      return;
    }
    try {
      const token = await refresher();
      if (typeof token !== 'string' || token.length === 0) throw new Error('empty token');
      this.dispatch({ type: 'auth-refresh-ok' });
    } catch {
      this.dispatch({ type: 'auth-refresh-failed' });
    }
  }

  /** In-flight token rotation on a live socket (spec §10.5 item 3). */
  sendRefreshToken(token: string): void {
    if (this.isLiveState()) this.socket?.send(refreshTokenFrame(token));
  }

  // -- inbound frames -----------------------------------------------------------

  private handleMessage(text: string): void {
    const frame = parseServerFrame(text);
    if (!frame) return;
    for (const fn of this.frameListeners) fn(frame);

    switch (frame.type) {
      case 'response': {
        if (frame.action === 'authenticate') {
          if (frame.status === 'ACK') {
            this.authRequestInFlight = false;
            this.hasAuthenticatedBefore = true;
            this.dispatch({ type: 'auth-ack', resumeCount: this.channels.size });
          }
          // NACK → server closes the socket; socket-closed drives on.
        }
        break;
      }
      case 'error': {
        // Auth-frame errors in CONNECTING → auth rejected; the server
        // closes (UNAUTHORIZED). Force the connect-failed path only if the
        // socket stays open, so backoff doesn't stall.
        if (frame.action === 'authenticate' && this.snap.state === 'CONNECTING') {
          this.socket?.close();
          this.socket = null;
          this.dispatch({ type: 'connect-failed' });
        }
        break;
      }
      case 'event':
        this.handleEvent(frame);
        break;
      case 'snapshot': {
        const cs = this.channelOrDefault(frame.channel);
        cs.seq = frame.seq;
        cs.lastTickAt = this.clock.now();
        cs.resyncing = false;
        this.dispatchTick();
        for (const h of cs.handlers) h(frame);
        this.converge(frame.channel);
        break;
      }
      case 'resumed': {
        const cs = this.channelOrDefault(frame.channel);
        cs.seq = frame.to_seq;
        cs.lastTickAt = this.clock.now();
        cs.resyncing = false;
        this.dispatchTick();
        this.converge(frame.channel);
        break;
      }
      case 'resync': {
        const cs = this.channelOrDefault(frame.channel);
        cs.resyncing = true;
        this.opts.onResyncRequired?.(frame.channel, frame.reason);
        // If no snapshot frame follows, `converge` fires on `subscribed`
        // or the consumer's REST refetch + explicit re-subscribe.
        this.converge(frame.channel);
        break;
      }
      case 'subscribed': {
        for (const ch of frame.channels) this.converge(ch);
        break;
      }
      case 'server.shutdown': {
        this.advisoryEndpoint = frame.endpoint ?? frame.endpoints?.[0] ?? null;
        this.dispatch({
          type: 'shutdown-advisory',
          retryAfterMs: frame.retry_after_ms,
        });
        break;
      }
      case 'pong':
      case 'rate_info':
      case 'feed.failover':
      case 'unsubscribed':
        break;
    }
    this.notify();
  }

  private handleEvent(frame: EventFrame): void {
    const cs = this.channelOrDefault(frame.channel);
    const expected = cs.seq + 1;

    // §10.9 gap detection: replayed frames keep original seq, so during a
    // resync window a non-continuous seq is expected replay output — only
    // flag discontinuities on the live stream.
    if (!cs.resyncing && cs.seq > 0 && frame.seq !== expected) {
      cs.resyncing = true;
      this.opts.onGap?.(frame.channel, expected, frame.seq);
      this.socket?.send(resyncFrame(frame.channel, cs.seq));
      return; // drop — fail-closed, no crossed books (§10.9 item 2)
    }

    cs.seq = Math.max(cs.seq, frame.seq);
    cs.lastTickAt = this.clock.now();
    this.dispatchTick();
    for (const h of cs.handlers) h(frame);
  }

  private dispatchTick(): void {
    this.dispatch({ type: 'tick' });
  }

  /** A channel confirmed (resumed | snapshot | resync-ack | subscribed)
   * during RESYNCING — decrement convergence and let the machine advance.
   * Only channels flagged by reEstablishChannels count, so stray frames
   * (e.g. an unrelated subscribed ack) can't converge the session early. */
  private converge(channel: string): void {
    const cs = this.channels.get(channel);
    if (!cs?.awaitingConfirm) {
      if (cs) cs.resyncing = false;
      return;
    }
    cs.awaitingConfirm = false;
    cs.resyncing = false;
    if (this.snap.state === 'RESYNCING' && this.snap.ctx.pendingConvergence > 0) {
      this.dispatch({ type: 'channel-converged' });
    }
  }

  private channelOrDefault(channel: string): ChannelState {
    let cs = this.channels.get(channel);
    if (!cs) {
      // Server pushed a channel we never registered (e.g. gateway-emitted
      // advisory channel) — track health but never send a subscribe frame.
      cs = { seq: 0, lastTickAt: 0, resyncing: false, awaitingConfirm: false, handlers: new Set() };
      this.channels.set(channel, cs);
    }
    return cs;
  }

  // -- timers ------------------------------------------------------------------

  private heartbeatTick(): void {
    if (this.snap.state === 'DISCONNECTED' || this.snap.state === 'RECONNECTING') return;
    const silentFor = this.clock.now() - this.lastInboundAt;
    if (silentFor >= this.opts.heartbeatTimeoutMs) {
      this.dispatch({ type: 'heartbeat-missed' });
      return;
    }
    if (this.socket && this.isLiveState()) this.socket.send(pingFrame());
    this.armTimer('heartbeat', this.opts.heartbeatIntervalMs, () => this.heartbeatTick());
  }

  private staleTick(): void {
    if (this.snap.state !== 'AUTHENTICATED' && this.snap.state !== 'STALE') return;
    const now = this.clock.now();
    let latest = 0;
    for (const cs of this.channels.values()) {
      if (cs.lastTickAt > latest) latest = cs.lastTickAt;
    }
    const silentFor = latest === 0 ? 0 : now - latest;
    if (silentFor > this.opts.staleThresholdMs && this.isTradingSession(now)) {
      this.dispatch({ type: 'stale-timer' });
    }
    // Refresh the per-channel `stale` flags — equality-checked, so
    // listeners only fire when something actually changed.
    this.notify();
    this.armTimer('stale', this.opts.staleMonitorIntervalMs, () => this.staleTick());
  }

  private armTimer(
    slot: 'backoff' | 'connect' | 'heartbeat' | 'stale',
    ms: number,
    fn: () => void,
  ): void {
    this.clearTimer(slot);
    const h = this.clock.setTimeout(fn, ms);
    if (slot === 'backoff') this.backoffTimer = h;
    else if (slot === 'connect') this.connectTimer = h;
    else if (slot === 'heartbeat') this.heartbeatTimer = h;
    else this.staleTimer = h;
  }

  private clearTimer(slot: 'backoff' | 'connect' | 'heartbeat' | 'stale'): void {
    const h =
      slot === 'backoff'
        ? this.backoffTimer
        : slot === 'connect'
          ? this.connectTimer
          : slot === 'heartbeat'
            ? this.heartbeatTimer
            : this.staleTimer;
    if (h) this.clock.clearTimeout(h);
    if (slot === 'backoff') this.backoffTimer = null;
    else if (slot === 'connect') this.connectTimer = null;
    else if (slot === 'heartbeat') this.heartbeatTimer = null;
    else this.staleTimer = null;
  }
}

function defaultTradingSessionGate(nowMs: number): boolean {
  return isFxMarketOpen(nowMs);
}

/** Structural equality between two status snapshots — lets `notify()` skip
 * listener fan-out when nothing observable changed (stale-monitor ticks,
 * replayed frames that don't move the seq, …). */
function statusesEqual(a: WsClientStatus, b: WsClientStatus): boolean {
  if (
    a.state !== b.state ||
    a.attempt !== b.attempt ||
    a.orderEntryEnabled !== b.orderEntryEnabled ||
    a.lastError !== b.lastError ||
    a.subscriptions.length !== b.subscriptions.length
  ) {
    return false;
  }
  for (let i = 0; i < a.subscriptions.length; i++) {
    if (a.subscriptions[i] !== b.subscriptions[i]) return false;
  }
  const keys = Object.keys(a.health);
  if (keys.length !== Object.keys(b.health).length) return false;
  for (const k of keys) {
    const ha = a.health[k];
    const hb = b.health[k];
    if (!ha || !hb) return false;
    if (
      ha.lastTickAt !== hb.lastTickAt ||
      ha.lastSeq !== hb.lastSeq ||
      ha.resyncing !== hb.resyncing ||
      ha.stale !== hb.stale
    ) {
      return false;
    }
  }
  return true;
}
