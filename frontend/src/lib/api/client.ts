/**
 * Typed fetch wrapper for the `/api/v1` gateway (Phase-10 Task 10.3.1
 * item 4).
 *
 *   - Authorization: Bearer injection via a pluggable token provider
 *   - RFC 7807 error envelope → `ApiError` ({code,message} + request_id),
 *     HTTP-status fallback for non-envelope bodies
 *   - `Idempotency-Key` auto-attach for mutating POSTs (`idempotent: true`
 *     or explicit `idempotencyKey`)
 *   - X-Request-Id echo honored via ApiError.requestId for error toasts
 *     (spec §8.7 correlation, Task 10.3.19 item 4)
 *   - 401 → `onUnauthorized` hook (silent refresh lives in Wave-2 auth;
 *     the hook receives the ApiError so the app decides)
 *
 * Server-state caching/dedup is owned by TanStack Query — this client is
 * deliberately a thin transport.
 */
import { ApiError, NetworkError, fallbackEnvelope, parseErrorEnvelope } from './errors';
import { IDEMPOTENCY_KEY_HEADER, newIdempotencyKey } from './idempotency';

export type QueryParams = Record<string, string | number | boolean | undefined>;

export interface RequestOptions {
  /** Query string params; `undefined` values are dropped. */
  query?: QueryParams;
  /** JSON body (serialized with JSON.stringify). */
  body?: unknown;
  /** Attach a fresh Idempotency-Key (money-moving POSTs, spec §8.8). */
  idempotent?: boolean;
  /** Explicit key — reuse ONLY to retry the identical payload. */
  idempotencyKey?: string;
  headers?: Record<string, string>;
  signal?: AbortSignal;
}

export interface ApiClientOptions {
  /** e.g. `/api/v1` or `https://api.example.com/api/v1`. */
  baseUrl: string;
  /** Bearer token provider — sync or async; absent ⇒ anonymous. */
  getAuthToken?: () => string | null | Promise<string | null>;
  /** Called once per 401 ApiError before it propagates. */
  onUnauthorized?: (err: ApiError) => void;
  fetchImpl?: typeof fetch;
}

export class ApiClient {
  private readonly baseUrl: string;
  private readonly getAuthToken: ApiClientOptions['getAuthToken'];
  private readonly onUnauthorized: ApiClientOptions['onUnauthorized'];
  private readonly fetchImpl: typeof fetch;

  constructor(opts: ApiClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/+$/, '');
    this.getAuthToken = opts.getAuthToken;
    this.onUnauthorized = opts.onUnauthorized;
    this.fetchImpl = opts.fetchImpl ?? fetch.bind(globalThis);
  }

  async get<T>(path: string, opts?: RequestOptions): Promise<T> {
    return this.request<T>('GET', path, opts);
  }
  async post<T>(path: string, body?: unknown, opts?: RequestOptions): Promise<T> {
    return this.request<T>('POST', path, { ...opts, body });
  }
  async put<T>(path: string, body?: unknown, opts?: RequestOptions): Promise<T> {
    return this.request<T>('PUT', path, { ...opts, body });
  }
  async patch<T>(path: string, body?: unknown, opts?: RequestOptions): Promise<T> {
    return this.request<T>('PATCH', path, { ...opts, body });
  }
  async delete<T>(path: string, opts?: RequestOptions): Promise<T> {
    return this.request<T>('DELETE', path, opts);
  }

  async request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
    const url = this.buildUrl(path, opts.query);
    const headers: Record<string, string> = { ...opts.headers };
    if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
    if (opts.idempotent || opts.idempotencyKey) {
      headers[IDEMPOTENCY_KEY_HEADER] = opts.idempotencyKey ?? newIdempotencyKey();
    }
    const token = this.getAuthToken ? await this.getAuthToken() : null;
    if (token) headers['Authorization'] = `Bearer ${token}`;

    let res: Response;
    try {
      res = await this.fetchImpl(url, {
        method,
        headers,
        body: opts.body !== undefined ? JSON.stringify(opts.body) : null,
        signal: opts.signal ?? null,
        credentials: 'same-origin',
      });
    } catch (cause) {
      throw new NetworkError(cause);
    }

    if (res.status === 204) return undefined as T;

    const text = await res.text();
    const body: unknown = text.length > 0 ? safeJson(text) : null;

    if (!res.ok) {
      const envelope = parseErrorEnvelope(body) ?? fallbackEnvelope(res.status, res.statusText);
      const err = new ApiError(envelope);
      if (res.status === 401) this.onUnauthorized?.(err);
      throw err;
    }
    return body as T;
  }

  private buildUrl(path: string, query?: QueryParams): string {
    const p = path.startsWith('/') ? path : `/${path}`;
    const url = `${this.baseUrl}${p}`;
    if (!query) return url;
    const qs = new URLSearchParams();
    for (const [k, v] of Object.entries(query)) {
      if (v !== undefined) qs.set(k, String(v));
    }
    const s = qs.toString();
    return s ? `${url}?${s}` : url;
  }
}

function safeJson(text: string): unknown {
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return null; // non-JSON error page → caller falls back by HTTP status
  }
}
