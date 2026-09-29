/**
 * Wire protocol — client↔server frame grammar.
 *
 * Mirrors `services/internal/ws/frame.go` + `services/internal/marketdata`
 * (spec §10.5/§10.6/§10.7/§10.9). This is the declared adapter boundary:
 * inbound frames arrive as untyped JSON (`unknown`) and are narrowed here
 * by structural guards — no `any` escapes this module.
 *
 * Client→server actions (snake_case control, spec §10.5):
 *   authenticate / refresh_token / subscribe / unsubscribe /
 *   resume / resync / ping   (+ order.* actions owned by Wave-2 trading)
 *
 * Server→client frames (discriminated on `type`):
 *   response | error | event | subscribed | unsubscribed | pong |
 *   resync | snapshot | resumed | rate_info | server.shutdown |
 *   feed.failover
 */
import { WS_PROTOCOL_VERSION } from './constants';

// ---------------------------------------------------------------------------
// Client → server frames
// ---------------------------------------------------------------------------

export interface ClientFrame {
  action: string;
  request_id?: string;
  token?: string;
  signature?: string;
  timestamp?: number;
  protocol_version?: number;
  params?: readonly string[] | Record<string, unknown>;
  channel?: string;
  channels?: readonly string[];
  last_seq?: number;
}

function send(frame: ClientFrame): string {
  return JSON.stringify(frame);
}

/** spec §10.5 item 1 — in-band auth upgrade. `signature` is required for
 * `ak_` API keys and omitted for JWTs (server distinguishes by prefix). */
export function authenticateFrame(
  token: string,
  opts: { signature?: string; timestampEpochSec?: number; requestId?: string } = {},
): string {
  return send({
    action: 'authenticate',
    token,
    signature: opts.signature,
    timestamp: opts.timestampEpochSec ?? Math.floor(Date.now() / 1000),
    protocol_version: WS_PROTOCOL_VERSION,
    request_id: opts.requestId,
  });
}

/** spec §10.5 item 3 — seamless in-flight token rotation. */
export function refreshTokenFrame(token: string): string {
  return send({ action: 'refresh_token', token });
}

/** spec §10.5 item 4 — canonical subscription grammar. `params` carries
 * the channel array: {"action":"subscribe","params":["bbo@EUR/USD", ...]} */
export function subscribeFrame(channels: readonly string[], requestId?: string): string {
  return send({ action: 'subscribe', params: [...channels], request_id: requestId });
}

export function unsubscribeFrame(channels: readonly string[], requestId?: string): string {
  return send({ action: 'unsubscribe', params: [...channels], request_id: requestId });
}

/** spec §10.1/§10.7 — reconnect resume from the 60s ring buffer. */
export function resumeFrame(channel: string, lastSeq: number): string {
  return send({ action: 'resume', channel, last_seq: lastSeq });
}

/** spec §10.9 item 2 — mid-stream gap repair (prev_last_seq discontinuity
 * or CRC32 mismatch detected client-side). */
export function resyncFrame(channel: string, lastSeq: number): string {
  return send({ action: 'resync', channel, last_seq: lastSeq });
}

export function pingFrame(): string {
  return send({ action: 'ping' });
}

// ---------------------------------------------------------------------------
// Server → client frames (declared field order is canonical on the wire;
// we treat them as pure data here and narrow via `parseServerFrame`)
// ---------------------------------------------------------------------------

export interface ResponseFrame {
  type: 'response';
  request_id?: string;
  action: string;
  status: 'ACK' | 'NACK';
  data: unknown;
  ts_ms: number;
}

export interface ErrorFrame {
  type: 'error';
  request_id?: string;
  action?: string;
  error: string; // §23 code token
  message: string;
  ts_ms: number;
  retry_after_ms?: number;
  code?: number; // e.g. AUTH_EXPIRED carries 4019
}

export interface EventFrame {
  type: 'event';
  channel: string;
  seq: number;
  data: unknown;
  ts_ms: number;
}

export interface SubscribedFrame {
  type: 'subscribed' | 'unsubscribed';
  channels: string[];
  total: number;
  ts_ms: number;
}

export interface PongFrame {
  type: 'pong';
  ts_ms: number;
}

/** Explicit resync directive (Task 6.3.9 item 5, §10.7): the resume cursor
 * fell outside the replay horizon. A `snapshot` frame may follow when the
 * channel type has a SnapshotSource; otherwise refetch via REST. */
export interface ResyncFrame {
  type: 'resync';
  channel: string;
  /** Known reasons: 'gap_too_large' | 'invalid_sequence' |
   * 'no_snapshot_available' (internal/marketdata frames.go). */
  reason: string;
  last_seq: number;
  ts_ms: number;
}

export interface SnapshotFrame {
  type: 'snapshot';
  channel: string;
  reason?: string;
  seq: number;
  data: unknown;
  ts_ms: number;
}

/** Replay confirmation — "replayed N buffered messages, now live". */
export interface ResumedFrame {
  type: 'resumed';
  channel: string;
  from_seq: number;
  to_seq: number;
  count: number;
  ts_ms: number;
}

export interface RateInfoFrame {
  type: 'rate_info';
  remaining: number;
  reset_ms: number;
  ts_ms: number;
}

/** Task 6.3.19 drain advisory. `endpoint`/`endpoints` are the reconnect
 * targets in preference order; `retry_after_ms` overrides the computed
 * backoff for the next attempt. */
export interface ShutdownFrame {
  type: 'server.shutdown';
  reason: string;
  retry_after_ms?: number;
  endpoint?: string;
  endpoints?: string[];
  deadline_ms?: number;
  ts_ms: number;
}

export interface FeedFailoverFrame {
  type: 'feed.failover';
  state: 'healthy' | 'degraded' | 'down';
  endpoints?: string[];
  ts_ms: number;
}

export type ServerFrame =
  | ResponseFrame
  | ErrorFrame
  | EventFrame
  | SubscribedFrame
  | PongFrame
  | ResyncFrame
  | SnapshotFrame
  | ResumedFrame
  | RateInfoFrame
  | ShutdownFrame
  | FeedFailoverFrame;

export type ServerFrameType = ServerFrame['type'];

// ---------------------------------------------------------------------------
// Narrowing (adapter boundary — everything below consumes `unknown`)
// ---------------------------------------------------------------------------

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}

function str(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function strArr(v: unknown): string[] | undefined {
  return Array.isArray(v) && v.every((x) => typeof x === 'string') ? v : undefined;
}

/** Parse one inbound text frame into the discriminated union, or null for
 * unrecognized/malformed input (callers log and drop — fail-closed). */
export function parseServerFrame(text: string): ServerFrame | null {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return null;
  }
  if (!isRecord(raw)) return null;
  const type = str(raw['type']);
  const ts = num(raw['ts_ms']) ?? 0;

  switch (type) {
    case 'response': {
      const action = str(raw['action']);
      const status = str(raw['status']);
      if (!action || (status !== 'ACK' && status !== 'NACK')) return null;
      return {
        type,
        request_id: str(raw['request_id']),
        action,
        status,
        data: raw['data'],
        ts_ms: ts,
      };
    }
    case 'error': {
      const error = str(raw['error']);
      const message = str(raw['message']);
      if (!error) return null;
      return {
        type,
        request_id: str(raw['request_id']),
        action: str(raw['action']),
        error,
        message: message ?? '',
        ts_ms: ts,
        retry_after_ms: num(raw['retry_after_ms']),
        code: num(raw['code']),
      };
    }
    case 'event': {
      const channel = str(raw['channel']);
      const seq = num(raw['seq']);
      if (!channel || seq === undefined) return null;
      return { type, channel, seq, data: raw['data'], ts_ms: ts };
    }
    case 'subscribed':
    case 'unsubscribed': {
      const channels = strArr(raw['channels']) ?? [];
      return { type, channels, total: num(raw['total']) ?? channels.length, ts_ms: ts };
    }
    case 'pong':
      return { type, ts_ms: ts };
    case 'resync': {
      const channel = str(raw['channel']);
      if (!channel) return null;
      return {
        type,
        channel,
        reason: str(raw['reason']) ?? 'no_snapshot_available',
        last_seq: num(raw['last_seq']) ?? 0,
        ts_ms: ts,
      };
    }
    case 'snapshot': {
      const channel = str(raw['channel']);
      const seq = num(raw['seq']);
      if (!channel || seq === undefined) return null;
      return {
        type,
        channel,
        reason: str(raw['reason']),
        seq,
        data: raw['data'],
        ts_ms: ts,
      };
    }
    case 'resumed': {
      const channel = str(raw['channel']);
      if (!channel) return null;
      return {
        type,
        channel,
        from_seq: num(raw['from_seq']) ?? 0,
        to_seq: num(raw['to_seq']) ?? 0,
        count: num(raw['count']) ?? 0,
        ts_ms: ts,
      };
    }
    case 'rate_info':
      return {
        type,
        remaining: num(raw['remaining']) ?? 0,
        reset_ms: num(raw['reset_ms']) ?? 0,
        ts_ms: ts,
      };
    case 'server.shutdown':
      return {
        type,
        reason: str(raw['reason']) ?? 'server_shutdown',
        retry_after_ms: num(raw['retry_after_ms']),
        endpoint: str(raw['endpoint']),
        endpoints: strArr(raw['endpoints']),
        deadline_ms: num(raw['deadline_ms']),
        ts_ms: ts,
      };
    case 'feed.failover': {
      const state = str(raw['state']);
      if (state !== 'healthy' && state !== 'degraded' && state !== 'down') return null;
      return { type, state, endpoints: strArr(raw['endpoints']), ts_ms: ts };
    }
    default:
      return null;
  }
}
