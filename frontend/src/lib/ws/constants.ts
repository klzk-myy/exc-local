/**
 * Shared WS constants — CANONICAL, spec §21.3 item 1.
 *
 * `RECONNECT_SCHEDULE_MS` is a cross-phase contract shared between
 * Phase-06 (market data) and Phase-10 (this client): any change must land
 * in both. Constraints pinned by spec §21.3:
 *   - 10s cap ⇒ ≤6 attempts/min < Phase-06 Task 6.3.21 throttle (10/min/IP)
 *   - 10s cap < Phase-06 Task 6.3.9 ring-buffer horizon (60s) so `last_seq`
 *     resume stays inside the replay window and avoids snapshot fallback.
 *
 * The ±20% jitter decorrelates reconnect storms across the fleet.
 */
export const RECONNECT_SCHEDULE_MS = [100, 250, 500, 1000, 2000, 10_000] as const;

export const RECONNECT_MAX_MS = 10_000;
export const RECONNECT_JITTER_RATIO = 0.2;

/** STALE_PRICING threshold — spec §21.3 item 3: silence >3.0s on an active
 * pair during trading hours flips the client to `STALE`. */
export const STALE_TICK_THRESHOLD_MS = 3_000;

/** How often the stale monitor samples per-channel last-tick timestamps. */
export const STALE_MONITOR_INTERVAL_MS = 250;

/** Client keepalive cadence. The server pings every 30s and drops the conn
 * after a 60s pong deadline (Phase-06 Server defaults) — we ping at 25s and
 * declare heartbeat-miss when no inbound frame arrives for 60s. */
export const HEARTBEAT_INTERVAL_MS = 25_000;
export const HEARTBEAT_TIMEOUT_MS = 60_000;

/** Dial + auth-handshake ceiling before CONNECTING → DISCONNECTED. */
export const CONNECT_TIMEOUT_MS = 10_000;

/** Registered RFC 6455 private-use close codes (spec §10.6, §23). */
export const WS_CLOSE_CODES = {
  /** Session token expired without renewal → refresh then reconnect. */
  AUTH_EXPIRED: 4019,
  /** Subscription churn abuse — reconnecting immediately will re-offend;
   * the client must still back off and surface the reason. */
  WS_ABUSE_DETECTED: 4003,
  /** Slow-consumer eviction (outbound buffer undrained >2s). */
  SLOW_CONSUMER_DROP: 4008,
} as const;

/** Mandatory on the `authenticate` frame (spec §10.5 item 1, §8.6). */
export const WS_PROTOCOL_VERSION = 1;

/** Default WS endpoint — same-origin `/ws/v1` (spec §10.5). */
export const DEFAULT_WS_PATH = '/ws/v1';
