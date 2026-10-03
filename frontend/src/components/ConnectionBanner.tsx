/**
 * Connection state surfaces (Task 10.3.19):
 *   - DISCONNECTED/RECONNECTING → prominent red banner + order entry locked
 *   - RESYNCING → locked banner while ring-buffer replay/snapshot converges
 *   - STALE → amber STALE_PRICING badge (order entry stays enabled —
 *     server-side execution collars are the safety net, spec §22.2)
 */
import { useWsStatus } from '@/lib/ws';
import { bannerRequired } from '@/lib/ws';
import { wsClient } from '@/app/runtime';

export function ConnectionBanner() {
  const status = useWsStatus(wsClient);

  if (bannerRequired(status.state)) {
    return (
      <div
        role="alert"
        className="bg-red-700 px-4 py-2 text-center text-sm font-semibold text-white"
      >
        {status.state === 'RECONNECTING'
          ? `DISCONNECTED — RECONNECTING… (attempt ${status.attempt})`
          : 'DISCONNECTED — order entry is locked'}
      </div>
    );
  }
  if (status.state === 'RESYNCING') {
    return (
      <div
        role="alert"
        className="bg-amber-500 px-4 py-2 text-center text-sm font-semibold text-neutral-950"
      >
        RESYNCING — replaying missed market data…
      </div>
    );
  }
  return null;
}

/** Amber STALE_PRICING badge (§21.3 item 3). */
export function StalePricingBadge() {
  const status = useWsStatus(wsClient);
  if (status.state !== 'STALE') return null;
  return (
    <span
      role="status"
      className="rounded bg-amber-500/20 px-2 py-0.5 text-xs font-semibold text-amber-400"
    >
      STALE_PRICING
    </span>
  );
}
