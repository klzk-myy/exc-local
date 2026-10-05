/**
 * Context-bound admin API client (Task 10.3.20 item 4: "staging pages
 * never call prod endpoints — context-bound API client").
 *
 * `boundAdminApi(api, env)` returns a thin wrapper that stamps the
 * `X-Admin-Env` header on every request, so a fleet/ops page rendered
 * under `env=staging` physically cannot emit a production-scoped call —
 * the header is decided by the bound context, never by request-local
 * input. The server additionally enforces the env axis through the
 * §8.2a.3 binding scope (a dev-only binding gets FORBIDDEN in prod).
 */
import type { ApiClient, QueryParams, RequestOptions } from '@/lib/api/client';

import { ADMIN_ENV_HEADER, normalizeAdminEnv, type AdminEnv } from './env';

export interface BoundAdminApi {
  readonly env: AdminEnv;
  get<T>(path: string, query?: QueryParams): Promise<T>;
  post<T>(path: string, body?: unknown, opts?: RequestOptions): Promise<T>;
  put<T>(path: string, body?: unknown, opts?: RequestOptions): Promise<T>;
  delete<T>(path: string, opts?: RequestOptions): Promise<T>;
}

/** Bind an ApiClient to one environment context. All requests carry
 * `X-Admin-Env: <env>`; callers cannot override it via opts.headers. */
export function boundAdminApi(api: ApiClient, env: AdminEnv): BoundAdminApi {
  const bound = normalizeAdminEnv(env);
  const stamp = (opts?: RequestOptions): RequestOptions => ({
    ...opts,
    headers: { ...opts?.headers, [ADMIN_ENV_HEADER]: bound },
  });
  return {
    env: bound,
    get: <T>(path: string, query?: QueryParams) => api.get<T>(path, { ...stamp(), query }),
    post: <T>(path: string, body?: unknown, opts?: RequestOptions) =>
      api.post<T>(path, body, stamp(opts)),
    put: <T>(path: string, body?: unknown, opts?: RequestOptions) =>
      api.put<T>(path, body, stamp(opts)),
    delete: <T>(path: string, opts?: RequestOptions) => api.delete<T>(path, stamp(opts)),
  };
}
