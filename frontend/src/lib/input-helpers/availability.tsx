/**
 * Honest-unavailable surfaces (spec §2.7 fail-closed UX).
 *
 * The gateway mounts StatusStub routes with a 501 NOT_IMPLEMENTED
 * envelope (internal/gateway/router.go). UI surfaces NEVER fabricate
 * data — when a backend isn't live yet, we render this panel naming the
 * owning phase so the gap is explicit and self-documenting.
 */
import { ApiError } from '@/lib/api';

/** True when an error is the gateway's registered-but-unbuilt response. */
export function isNotImplemented(err: unknown): boolean {
  return err instanceof ApiError && (err.status === 501 || err.code === 'NOT_IMPLEMENTED');
}

/** True when a TanStack Query error means "backend route stubbed". */
export function isBackendStub(err: unknown): boolean {
  return isNotImplemented(err);
}

export interface UnavailablePanelProps {
  /** Surface name, e.g. 'Copy trading'. */
  feature: string;
  /** Owning phase/task from the generated route contract (x-owner) or
   * the phase doc — displayed verbatim so the gap is traceable. */
  owner?: string;
  /** Optional extra line (e.g. 'arrives with Phase 16'). */
  note?: string;
}

/**
 * Standard "endpoint unavailable" placeholder. Renders only honest
 * information: the feature name, the owning phase, and that no data is
 * shown. No mock data, no fake charts.
 */
export function UnavailablePanel({ feature, owner, note }: UnavailablePanelProps) {
  return (
    <div
      className="rounded-lg border border-dashed border-neutral-700 bg-neutral-900/50 p-6 text-center"
      role="status"
      aria-label={`${feature} is not yet available`}
    >
      <p className="text-sm font-medium text-neutral-300">{feature} — unavailable</p>
      <p className="mt-1 text-xs text-neutral-500">
        The API route reported unavailable
        {owner ? ` (owner: ${owner})` : ''}; no data is shown.
      </p>
      {note ? <p className="mt-1 text-xs text-neutral-500">{note}</p> : null}
    </div>
  );
}
