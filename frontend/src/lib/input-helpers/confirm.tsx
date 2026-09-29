/**
 * Unified confirmation & risk-warning framework (Task 10.3.29 item 8).
 *
 * `<ConfirmModal>` carries LOW/MEDIUM/HIGH severity:
 *   LOW    (green)  — standard confirmation (session revoke, ticket close)
 *   MEDIUM (amber)  — financial-impact (limit order, transfer, grid start)
 *   HIGH   (red)    — irreversible/high-risk (close-all, withdrawal to a
 *                     new beneficiary, account closure) — requires typing
 *                     the registered confirmation phrase, never a click.
 *
 * Risk disclosures are injected from `RISK_DISCLOSURES` — the compliance-
 * curated registry — never hardcoded per component (task item 8).
 */
import { useEffect, useRef, useState, type ReactNode } from 'react';

export type ConfirmSeverity = 'LOW' | 'MEDIUM' | 'HIGH';

/** Compliance-curated disclosure registry (§12.9/§16.6 — static, never
 * advisory). Keyed by stable token so screens reference, not duplicate. */
export const RISK_DISCLOSURES = {
  capitalLoss:
    'Trading foreign exchange carries a high level of risk and you can lose all of your invested capital.',
  pastPerformance:
    'Past performance of a strategy or provider is not indicative of future results.',
  leverage:
    'Leveraged trading amplifies both gains and losses and may result in losses exceeding your margin.',
  liquidation:
    'Positions may be liquidated automatically if margin requirements are not maintained.',
  gridBot:
    'Grid bots place automated orders within a price range; a sustained move outside the range can realize losses.',
  copyTrading:
    'Copying a strategy replicates its trades in your account, including its losses; you remain responsible for all copied positions.',
  withdrawalIrreversible:
    'Transfers to a beneficiary are final once released and cannot be reversed.',
} as const;

export type RiskDisclosureKey = keyof typeof RISK_DISCLOSURES;

/** Phrase registry — the typed phrase a HIGH-severity action demands. */
export const CONFIRM_PHRASES: Readonly<Record<string, string>> = {
  closeAllPositions: 'CLOSE ALL',
  stopGridBot: 'STOP BOT',
  startGridBot: 'START BOT',
  emergencyFreeze: 'FREEZE ACCOUNT',
  accountClosure: 'CLOSE ACCOUNT',
  withdrawal: 'CONFIRM WITHDRAWAL',
  unfollow: 'UNFOLLOW',
};

export interface ConfirmModalProps {
  open: boolean;
  severity: ConfirmSeverity;
  title: string;
  children?: ReactNode;
  /** Disclosure keys injected into the modal body. */
  disclosures?: readonly RiskDisclosureKey[];
  /** HIGH severity: typed phrase required to enable Confirm. */
  requirePhrase?: string;
  /** Dual-control actions (Phase-07 Task 7.3.2): shows the pending-
   * approval state — the confirm click submits the request for a second
   * approver rather than executing immediately. */
  dualControl?: boolean;
  confirmLabel?: string;
  cancelLabel?: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

const SEVERITY_STYLE: Record<ConfirmSeverity, { ring: string; badge: string; btn: string }> = {
  LOW: {
    ring: 'border-emerald-700',
    badge: 'bg-emerald-500/20 text-emerald-400',
    btn: 'bg-emerald-600 hover:bg-emerald-500',
  },
  MEDIUM: {
    ring: 'border-amber-700',
    badge: 'bg-amber-500/20 text-amber-400',
    btn: 'bg-amber-600 hover:bg-amber-500',
  },
  HIGH: {
    ring: 'border-red-700',
    badge: 'bg-red-500/20 text-red-400',
    btn: 'bg-red-600 hover:bg-red-500',
  },
};

export function ConfirmModal({
  open,
  severity,
  title,
  children,
  disclosures = [],
  requirePhrase,
  dualControl = false,
  confirmLabel = 'Confirm',
  cancelLabel = 'Cancel',
  busy = false,
  onConfirm,
  onCancel,
}: ConfirmModalProps) {
  const [typed, setTyped] = useState('');
  const cancelRef = useRef<HTMLButtonElement>(null);
  const style = SEVERITY_STYLE[severity];
  const needsPhrase = severity === 'HIGH' && requirePhrase !== undefined && requirePhrase !== '';
  const phraseOk = !needsPhrase || typed === requirePhrase;

  useEffect(() => {
    if (open) {
      setTyped('');
      cancelRef.current?.focus();
    }
  }, [open]);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      role="presentation"
      onClick={(e) => {
        if (e.target === e.currentTarget) onCancel();
      }}
      onKeyDown={(e) => {
        if (e.key === 'Escape') onCancel();
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className={`w-full max-w-md rounded-lg border ${style.ring} bg-neutral-900 p-5 shadow-xl`}
      >
        <div className="mb-3 flex items-center gap-2">
          <span className={`rounded px-2 py-0.5 text-xs font-semibold ${style.badge}`}>
            {severity}
          </span>
          <h2 className="text-lg font-semibold text-neutral-100">{title}</h2>
        </div>

        {disclosures.length > 0 && (
          <ul className="mb-3 space-y-1 rounded border border-neutral-800 bg-neutral-950/60 p-3">
            {disclosures.map((k) => (
              <li key={k} className="text-xs leading-relaxed text-neutral-400">
                {RISK_DISCLOSURES[k]}
              </li>
            ))}
          </ul>
        )}

        <div className="text-sm text-neutral-300">{children}</div>

        {needsPhrase && (
          <div className="mt-3">
            <label htmlFor="confirm-phrase" className="mb-1 block text-xs text-neutral-400">
              Type <span className="font-mono font-semibold text-red-300">{requirePhrase}</span> to
              confirm
            </label>
            <input
              id="confirm-phrase"
              type="text"
              value={typed}
              onChange={(e) => {
                setTyped(e.target.value);
              }}
              autoComplete="off"
              className="w-full rounded border border-neutral-700 bg-neutral-950 px-3 py-1.5 text-sm text-neutral-100 outline-none focus:border-red-600"
            />
          </div>
        )}

        {dualControl && (
          <p className="mt-3 rounded border border-neutral-800 bg-neutral-950/60 p-2 text-xs text-neutral-400">
            This action requires a second approver. Confirming submits a pending request — it does
            not execute until approved.
          </p>
        )}

        <div className="mt-4 flex justify-end gap-2">
          <button
            ref={cancelRef}
            type="button"
            onClick={onCancel}
            className="rounded border border-neutral-700 px-3 py-1.5 text-sm text-neutral-300 hover:bg-neutral-800"
          >
            {cancelLabel}
          </button>
          <button
            type="button"
            disabled={!phraseOk || busy}
            onClick={onConfirm}
            className={`rounded px-3 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:opacity-40 ${style.btn}`}
          >
            {busy ? 'Working…' : dualControl ? 'Submit for approval' : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}

/** Minimal pending-confirm state for callers that open/close the modal. */
export interface PendingConfirm {
  readonly open: boolean;
  readonly ask: () => void;
  readonly close: () => void;
}

export function useConfirmState(): PendingConfirm {
  const [open, setOpen] = useState(false);
  return {
    open,
    ask: () => {
      setOpen(true);
    },
    close: () => {
      setOpen(false);
    },
  };
}
