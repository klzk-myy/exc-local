/**
 * RFC 7807 error envelope — spec §8.7, emitted by the gateway's
 * `internal/gateway/envelope.go`:
 *
 *   {"type":"error","error":"<CODE>","message":"...","status":N,
 *    "request_id":"...","timestamp":"...","details":{...}}
 *
 * `retry_after` is expressed in SECONDS on HTTP surfaces (RFC 6585; the
 * WS twin is `retry_after_ms` — spec §10.5 item 5). `ApiError` normalizes
 * the envelope into the `{code,message}` shape UI consumers use.
 */
import type { ApiErrorCode } from './codes';

export interface ApiErrorEnvelope {
  type: 'error';
  /** §23 code token (field name on the wire is `error`, not `code`). */
  error: ApiErrorCode;
  message: string;
  status: number;
  request_id?: string;
  timestamp?: string;
  details?: Record<string, unknown>;
  retry_after?: number;
}

export class ApiError extends Error {
  readonly code: ApiErrorCode;
  readonly status: number;
  readonly requestId: string | undefined;
  readonly details: Record<string, unknown> | undefined;
  /** Server-advised retry delay in ms (normalized from `retry_after` s). */
  readonly retryAfterMs: number | undefined;

  constructor(envelope: ApiErrorEnvelope) {
    super(envelope.message || envelope.error);
    this.name = 'ApiError';
    this.code = envelope.error;
    this.status = envelope.status;
    this.requestId = envelope.request_id;
    this.details = envelope.details;
    this.retryAfterMs =
      envelope.retry_after !== undefined ? envelope.retry_after * 1000 : undefined;
  }
}

/** Thrown when the transport itself fails (DNS, TLS, abort, socket reset)
 * — there is no envelope to parse, so code is fixed `NETWORK_ERROR`. */
export class NetworkError extends Error {
  readonly code = 'NETWORK_ERROR' as const;
  readonly status = 0;
  constructor(cause: unknown) {
    super(cause instanceof Error ? cause.message : 'network request failed');
    this.name = 'NetworkError';
    this.cause = cause;
  }
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}

/** Narrow an untyped body to the RFC 7807 envelope, or null. */
export function parseErrorEnvelope(body: unknown): ApiErrorEnvelope | null {
  if (!isRecord(body)) return null;
  if (body['type'] !== 'error') return null;
  const code = body['error'];
  if (typeof code !== 'string' || code.length === 0) return null;
  return {
    type: 'error',
    error: code,
    message: typeof body['message'] === 'string' ? body['message'] : '',
    status: typeof body['status'] === 'number' ? body['status'] : 0,
    request_id: typeof body['request_id'] === 'string' ? body['request_id'] : undefined,
    timestamp: typeof body['timestamp'] === 'string' ? body['timestamp'] : undefined,
    details: isRecord(body['details']) ? body['details'] : undefined,
    retry_after: typeof body['retry_after'] === 'number' ? body['retry_after'] : undefined,
  };
}

const HTTP_DEFAULT_CODE: Readonly<Record<number, ApiErrorCode>> = {
  400: 'INVALID_REQUEST',
  401: 'UNAUTHORIZED',
  403: 'FORBIDDEN',
  404: 'NOT_FOUND',
  423: 'ACCOUNT_BUSY',
  429: 'RATE_LIMIT_TIER_EXCEEDED',
  500: 'INTERNAL_ERROR',
  503: 'DEGRADED_MODE',
};

/** Synthesize an envelope when the response carried no parseable body
 * (e.g. a proxy error page) — code defaults by HTTP status, fail-closed. */
export function fallbackEnvelope(status: number, statusText: string): ApiErrorEnvelope {
  return {
    type: 'error',
    error: HTTP_DEFAULT_CODE[status] ?? 'INTERNAL_ERROR',
    message: statusText || `HTTP ${status}`,
    status,
  };
}
