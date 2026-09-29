/**
 * KycStatus tracker — Task 10.3.24 item 1. Renders the current tier,
 * lifecycle state (PENDING/APPROVED/REJECTED/EXPIRED/MANUAL_REVIEW),
 * per-document statuses, re-verification due date, and the §14.2
 * trading-limit impact for the current tier.
 */
import { StatusBadge, cardCls } from '@/lib/ui';

import * as api from './api';

const STEPS = ['SUBMITTED', 'PENDING', 'APPROVED'] as const;

function Timeline({ status }: { status: string }) {
  const rejected = status === 'REJECTED';
  const expired = status === 'EXPIRED';
  return (
    <ol className="mb-4 flex items-center gap-0" aria-label="Verification progress">
      {STEPS.map((s, i) => {
        const reached =
          status === 'APPROVED'
            ? true
            : s === 'SUBMITTED' ||
              (s === 'PENDING' && (status === 'PENDING' || status === 'MANUAL_REVIEW'));
        const failedHere = (rejected || expired) && s === 'APPROVED';
        return (
          <li key={s} className="flex items-center">
            <span
              className={`flex h-6 w-6 items-center justify-center rounded-full text-xs font-medium ${
                failedHere
                  ? 'bg-red-700/40 text-red-300'
                  : reached
                    ? 'bg-sky-600 text-white'
                    : 'bg-neutral-800 text-neutral-500'
              }`}
              aria-current={!failedHere && !reached && i === 1 ? 'step' : undefined}
            >
              {i + 1}
            </span>
            <span className="ml-1 text-xs text-neutral-400">
              {failedHere ? status : s === 'PENDING' ? 'IN REVIEW' : s}
            </span>
            {i < STEPS.length - 1 && (
              <span className="mx-2 h-px w-8 bg-neutral-700" aria-hidden="true" />
            )}
          </li>
        );
      })}
    </ol>
  );
}

export function StatusTracker({ status }: { status: api.KycStatusResponse }) {
  const tier = (api.KYC_TIERS as readonly string[]).includes(status.tier)
    ? (status.tier as api.KycTier)
    : 'T0';
  const limits = api.TIER_LIMITS[tier];
  const reverificationExpired = status.status === 'EXPIRED';
  const institutional = tier === 'INSTITUTIONAL' || status.institutional === true;

  return (
    <div className={cardCls}>
      <div className="mb-3 flex items-center justify-between">
        <h2 className="text-sm font-semibold">Verification status</h2>
        <div className="flex items-center gap-2">
          <StatusBadge value={status.status} />
          <StatusBadge value={tier} />
        </div>
      </div>

      <Timeline status={status.status} />

      {institutional && (
        <p className="mb-3 rounded border border-sky-800/60 bg-sky-950/30 p-2 text-xs text-sky-300">
          Institutional accounts go through manual review — the desk will contact you on the
          registered email channel.
        </p>
      )}

      {status.status === 'REJECTED' && (
        <p
          className="mb-3 rounded border border-red-800/60 bg-red-950/30 p-2 text-xs text-red-300"
          role="alert"
        >
          Your verification was rejected — resubmit with corrected documents below.
        </p>
      )}
      {reverificationExpired && (
        <p
          className="mb-3 rounded border border-amber-700/60 bg-amber-950/40 p-2 text-xs text-amber-300"
          role="alert"
        >
          Re-verification required — your documents expired
          {status.re_verification_due !== undefined &&
            ` (due ${new Date(status.re_verification_due).toLocaleDateString()})`}
          . Access is degraded to T0 limits until you resubmit.
        </p>
      )}
      {status.re_verification_due !== undefined && !reverificationExpired && (
        <p className="mb-3 text-xs text-neutral-400">
          Next re-verification due {new Date(status.re_verification_due).toLocaleDateString()}.
        </p>
      )}

      {status.documents.length > 0 && (
        <table className="mb-3 w-full text-left text-sm">
          <thead>
            <tr className="text-xs text-neutral-500">
              <th className="py-1">Document</th>
              <th className="py-1">Status</th>
              <th className="py-1">Expires</th>
            </tr>
          </thead>
          <tbody>
            {status.documents.map((d, i) => (
              <tr key={`${d.type}-${i}`} className="border-t border-neutral-800/60">
                <td className="py-1.5 text-neutral-200">
                  {api.DOC_TYPES.find((t) => t.id === d.type)?.label ?? d.type}
                  {d.rejection_reason !== undefined && (
                    <span className="ml-2 text-xs text-red-400">{d.rejection_reason}</span>
                  )}
                </td>
                <td className="py-1.5">
                  <StatusBadge value={d.status} />
                </td>
                <td className="py-1.5 text-xs text-neutral-400">
                  {d.expires_at !== undefined ? new Date(d.expires_at).toLocaleDateString() : '—'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <div className="rounded border border-neutral-800 p-2 text-xs text-neutral-300">
        <p className="mb-1 font-medium text-neutral-400">What tier {tier} allows:</p>
        <ul className="list-inside list-disc space-y-0.5">
          <li>{limits.trading}</li>
          <li>{limits.withdrawal}</li>
          <li>{limits.reverify}</li>
        </ul>
      </div>
    </div>
  );
}
