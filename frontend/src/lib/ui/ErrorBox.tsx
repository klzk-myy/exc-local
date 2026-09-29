/**
 * ErrorBox — renders ApiError/NetworkError (or a plain Error/string) as a
 * dismissable inline alert, surfacing the §8.7 correlation fields
 * (request_id) and RFC 6585 retry_after when the server advises one —
 * login rate-limit messaging (RATE_LIMIT_TIER_EXCEEDED) relies on it.
 */
import { ApiError, NetworkError } from '@/lib/api';

function describe(err: unknown): {
  title: string;
  detail?: string;
  requestId?: string;
  retryAfterMs?: number;
} {
  if (err instanceof ApiError) {
    return {
      title: err.message || err.code,
      detail: err.code,
      requestId: err.requestId,
      retryAfterMs: err.retryAfterMs,
    };
  }
  if (err instanceof NetworkError) {
    return { title: 'Network error — check your connection and retry.' };
  }
  if (err instanceof Error) return { title: err.message };
  if (typeof err === 'string') return { title: err };
  return { title: 'Unexpected error' };
}

export function ErrorBox({ error, onDismiss }: { error: unknown; onDismiss?: () => void }) {
  if (error == null) return null;
  const { title, detail, requestId, retryAfterMs } = describe(error);
  return (
    <div
      className="mb-4 rounded border border-red-800/60 bg-red-950/40 px-3 py-2 text-sm text-red-300"
      role="alert"
    >
      <div className="flex items-start justify-between gap-3">
        <div>
          <p>{title}</p>
          {retryAfterMs !== undefined && (
            <p className="mt-1 text-xs text-red-400">
              Rate limited — retry after {Math.ceil(retryAfterMs / 1000)}s.
            </p>
          )}
          <p className="mt-1 text-xs text-neutral-500">
            {detail !== undefined && <span>code: {detail} </span>}
            {requestId !== undefined && <span>request: {requestId}</span>}
          </p>
        </div>
        {onDismiss !== undefined && (
          <button
            type="button"
            aria-label="Dismiss error"
            className="text-red-400 hover:text-red-300"
            onClick={onDismiss}
          >
            ×
          </button>
        )}
      </div>
    </div>
  );
}
