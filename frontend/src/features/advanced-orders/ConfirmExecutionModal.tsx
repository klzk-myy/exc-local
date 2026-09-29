/**
 * Projected-execution confirmation modal (Task 10.3.10) — every
 * destructive/one-click action shows the estimated fill (book-walk VWAP
 * over canonical L2), slippage vs mid in bps, and the resulting position
 * BEFORE the order fires. Buttons are real buttons → fully keyboard
 * accessible; Esc/backdrop cancels.
 */
import type { ReactNode } from 'react';

import type { FillProjection } from '@/lib/trading/projections';
import { btnDanger, btnGhost, Modal } from '@/lib/ui';

export interface ConfirmExecutionProps {
  open: boolean;
  title: string;
  /** e.g. "Flatten LONG 100,000 EUR/USD" */
  action: string;
  projection: FillProjection | undefined;
  /** Resulting position after the action ("flat", "SHORT 100,000", …). */
  resultingPosition?: string;
  busy?: boolean;
  confirmLabel?: string;
  onConfirm: () => void;
  onClose: () => void;
  /** Extra warnings (e.g. "book thin — partial fill likely"). */
  children?: ReactNode;
}

function Row({ label, value, warn }: { label: string; value: string; warn?: boolean }) {
  return (
    <div className="flex justify-between py-1 text-sm">
      <span className="text-neutral-400">{label}</span>
      <span
        className={warn === true ? 'font-medium text-amber-300' : 'font-medium text-neutral-100'}
      >
        {value}
      </span>
    </div>
  );
}

export function ConfirmExecutionModal({
  open,
  title,
  action,
  projection,
  resultingPosition,
  busy = false,
  confirmLabel = 'Confirm',
  onConfirm,
  onClose,
  children,
}: ConfirmExecutionProps) {
  return (
    <Modal open={open} title={title} onClose={onClose}>
      <p className="mb-2 text-sm text-neutral-300">{action}</p>
      <div className="mb-3 rounded border border-neutral-800 bg-neutral-950 px-3 py-2">
        {projection ? (
          <>
            <Row label="Est. fill price (VWAP)" value={projection.avgPrice.toDisplay(6)} />
            <Row
              label="Est. slippage vs mid"
              value={
                projection.slippageBps
                  ? `${projection.slippageBps.toFixed(2)} bps`
                  : 'unavailable — no mid'
              }
            />
            <Row
              label="Fillable at visible depth"
              value={
                projection.fullyFillable
                  ? projection.filledQty.toDisplay()
                  : `${projection.filledQty.toDisplay()} (partial)`
              }
              warn={!projection.fullyFillable}
            />
          </>
        ) : (
          <p className="py-1 text-sm text-neutral-400" role="status">
            Projection unavailable — order executes at market without an estimate.
          </p>
        )}
        {resultingPosition !== undefined && (
          <Row label="Resulting position" value={resultingPosition} />
        )}
      </div>
      {children}
      <div className="mt-4 flex justify-end gap-2">
        <button type="button" className={btnGhost} onClick={onClose} disabled={busy}>
          Cancel
        </button>
        <button
          type="button"
          className={btnDanger}
          onClick={onConfirm}
          disabled={busy}
          aria-label={confirmLabel}
        >
          {busy ? 'Working…' : confirmLabel}
        </button>
      </div>
    </Modal>
  );
}
