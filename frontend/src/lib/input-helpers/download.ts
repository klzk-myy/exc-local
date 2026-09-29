/**
 * File-download helper for the reports center (Task 10.3.28) — binary
 * fetches (CSV/PDF/MT515) can't go through ApiClient's JSON path, so this
 * wraps fetch with the same auth token provider and surfaces
 * RFC 7807 error bodies when the server answers with JSON instead of a
 * file.
 */
import { useSessionStore } from '@/lib/auth/session';
import { ApiError, parseErrorEnvelope } from '@/lib/api';

export interface DownloadResult {
  blob: Blob;
  /** Filename from Content-Disposition (or the caller's fallback). */
  filename: string;
  contentType: string;
}

const BASE = (): string => (import.meta.env.VITE_API_URL ?? '/api/v1').replace(/\/+$/, '');

export async function downloadFile(
  path: string,
  fallbackFilename: string,
  opts: { query?: Record<string, string | number | undefined>; accept?: string } = {},
): Promise<DownloadResult> {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(opts.query ?? {})) {
    if (v !== undefined) qs.set(k, String(v));
  }
  const url = `${BASE()}${path}${qs.size > 0 ? `?${qs.toString()}` : ''}`;
  const headers: Record<string, string> = { Accept: opts.accept ?? '*/*' };
  const token = useSessionStore.getState().accessToken;
  if (token) headers['Authorization'] = `Bearer ${token}`;

  const res = await fetch(url, { headers, credentials: 'same-origin' });
  const type = res.headers.get('Content-Type') ?? '';
  if (!res.ok) {
    let body: unknown = null;
    try {
      body = await res.json();
    } catch {
      /* non-JSON error page */
    }
    const env =
      parseErrorEnvelope(body) ??
      ({
        type: 'error',
        error: 'INTERNAL_ERROR',
        message: `HTTP ${res.status}`,
        status: res.status,
      } as const);
    throw new ApiError(env);
  }
  const blob = await res.blob();
  const cd = res.headers.get('Content-Disposition') ?? '';
  const m = /filename="?([^";]+)"?/.exec(cd);
  return { blob, filename: m?.[1] ?? fallbackFilename, contentType: type };
}

/** Trigger a browser download for an already-fetched blob. */
export function saveBlob(result: DownloadResult): void {
  const url = URL.createObjectURL(result.blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = result.filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}
