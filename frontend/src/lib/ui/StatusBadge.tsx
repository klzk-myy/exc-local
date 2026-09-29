/**
 * StatusBadge — colored pill for lifecycle enums (funding statuses,
 * KYC states, ticket statuses, review tiers). Unknown values render
 * neutral — never crash on a new enum member (forward-compatible).
 */
const TONE: Record<string, string> = {
  // generic positive / negative / pending
  PENDING: 'bg-amber-500/20 text-amber-400',
  PENDING_REVIEW: 'bg-amber-500/20 text-amber-400',
  IN_PROGRESS: 'bg-sky-500/20 text-sky-400',
  CONFIRMED: 'bg-emerald-500/20 text-emerald-400',
  COMPLETED: 'bg-emerald-500/20 text-emerald-400',
  APPROVED: 'bg-emerald-500/20 text-emerald-400',
  VERIFIED: 'bg-emerald-500/20 text-emerald-400',
  RESOLVED: 'bg-emerald-500/20 text-emerald-400',
  ACTIVE: 'bg-emerald-500/20 text-emerald-400',
  OPEN: 'bg-sky-500/20 text-sky-400',
  REJECTED: 'bg-red-500/20 text-red-400',
  FAILED: 'bg-red-500/20 text-red-400',
  EXPIRED: 'bg-red-500/20 text-red-400',
  AUTO_CANCELLED: 'bg-neutral-700/40 text-neutral-400',
  CANCELLED: 'bg-neutral-700/40 text-neutral-400',
  CLOSED: 'bg-neutral-700/40 text-neutral-400',
  FROZEN: 'bg-sky-700/30 text-sky-300',
  SUSPENDED: 'bg-amber-600/20 text-amber-300',
  // review tiers
  AUTO: 'bg-emerald-500/20 text-emerald-400',
  STANDARD: 'bg-sky-500/20 text-sky-400',
  // KYC tiers
  T0: 'bg-neutral-700/40 text-neutral-300',
  T1: 'bg-sky-500/20 text-sky-400',
  T2: 'bg-emerald-500/20 text-emerald-400',
};

export function StatusBadge({ value }: { value: string }) {
  const tone = TONE[value] ?? 'bg-neutral-700/40 text-neutral-300';
  return (
    <span className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${tone}`}>
      {value.replaceAll('_', ' ')}
    </span>
  );
}
