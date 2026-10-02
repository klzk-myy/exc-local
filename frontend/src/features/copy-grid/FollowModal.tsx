/**
 * Follow flow (Task 10.3.26 item 2) — POST /api/v1/copy/follows with
 * allocation / max-per-trade copy size / provider-drawdown stop-loss.
 * The compliance risk disclosure must be acknowledged before the
 * follow request is enabled (mandatory per task text).
 *
 * Unfollow is live (DELETE /api/v1/copy/follows/{id}) — see
 * MyFollowsPanel for the investor-side management surface.
 */
import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ErrorBox, inputCls, labelCls } from '@/lib/ui';
import {
  ConfirmModal,
  RISK_DISCLOSURES,
  isNotImplemented,
  useValidatedField,
  type ValidatedField,
} from '@/lib/input-helpers';
import type { FieldRule } from '@/lib/input-helpers/validation';

import { followStrategy, type CopyStrategy } from './api';

const positiveDecimal: FieldRule = {
  name: 'amount',
  kind: 'decimal',
  required: true,
  positive: true,
};
const optionalDecimal: FieldRule = { name: 'cap', kind: 'decimal', positive: true };

export function FollowModal({
  strategy,
  open,
  onClose,
}: {
  strategy: CopyStrategy | null;
  open: boolean;
  onClose: () => void;
}) {
  const allocation = useValidatedField(positiveDecimal);
  const maxCopy = useValidatedField(optionalDecimal);
  const stopLoss = useValidatedField(optionalDecimal);
  const [riskAccepted, setRiskAccepted] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [serverErr, setServerErr] = useState<unknown>(null);

  const submit = useMutation({
    mutationFn: async () => {
      if (!strategy) throw new Error('no strategy selected');
      return followStrategy(apiClient, {
        strategy_id: strategy.id,
        allocation: allocation.value,
        ...(maxCopy.value !== '' ? { max_copy_size: maxCopy.value } : {}),
        ...(stopLoss.value !== '' ? { stop_loss_drawdown_pct: stopLoss.value } : {}),
      });
    },
    onError: setServerErr,
    onSuccess: () => {
      setServerErr(null);
      setConfirmOpen(false);
      onClose();
    },
  });

  if (!strategy) return null;
  const fieldsValid = allocation.valid && maxCopy.valid && stopLoss.valid;
  const stubbed = isNotImplemented(serverErr);

  const field = (f: ValidatedField, label: string, hint?: string) => {
    // Stable control id — explicit label association (implicit-only labels
    // are fragile under axe's DOM label resolution).
    const id = `fm-${label.toLowerCase().replace(/[^a-z0-9]+/g, '-')}`;
    return (
      <div>
        <label className={labelCls} htmlFor={id}>
          {label}
        </label>
        <input id={id} className={inputCls} inputMode="decimal" {...f.inputProps} />
        {f.error ? <p className="mt-1 text-xs text-red-400">{f.error}</p> : null}
        {!f.error && hint ? <p className="mt-1 text-xs text-neutral-500">{hint}</p> : null}
      </div>
    );
  };

  return (
    <>
      {open && !confirmOpen ? (
        <div
          className="fixed inset-0 z-40 flex items-center justify-center bg-black/60 p-4"
          role="presentation"
        >
          <div
            role="dialog"
            aria-modal="true"
            aria-label={`Follow ${strategy.alias}`}
            className="w-full max-w-md rounded-lg border border-neutral-700 bg-neutral-900 p-5"
          >
            <h2 className="mb-3 text-lg font-semibold text-neutral-100">Follow {strategy.alias}</h2>
            <div className="space-y-3">
              {field(allocation, 'Allocation (quote currency)', 'Notional the strategy may deploy')}
              {field(maxCopy, 'Max per-trade copy size', 'Optional cap on each copied trade')}
              {field(
                stopLoss,
                'Stop-loss on provider drawdown (%)',
                'Unfollow triggers if provider drawdown breaches this cap',
              )}
              <label
                className="flex items-start gap-2 text-sm text-neutral-300"
                htmlFor="fm-risk-accept"
              >
                <input
                  id="fm-risk-accept"
                  type="checkbox"
                  checked={riskAccepted}
                  onChange={(e) => {
                    setRiskAccepted(e.target.checked);
                  }}
                  className="mt-1"
                />
                <span>
                  I have read and accept the copy-trading risk disclosure.
                  <span className="mt-1 block text-xs text-neutral-500">
                    {RISK_DISCLOSURES.copyTrading} {RISK_DISCLOSURES.pastPerformance}
                  </span>
                </span>
              </label>
            </div>
            <div className="mt-4 flex justify-end gap-2">
              <button
                type="button"
                className="rounded border border-neutral-700 px-3 py-1.5 text-sm text-neutral-300 hover:bg-neutral-800"
                onClick={onClose}
              >
                Cancel
              </button>
              <button
                type="button"
                disabled={!fieldsValid || !riskAccepted}
                className="rounded bg-sky-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-sky-500 disabled:cursor-not-allowed disabled:opacity-40"
                onClick={() => {
                  setConfirmOpen(true);
                }}
              >
                Review &amp; follow
              </button>
            </div>
          </div>
        </div>
      ) : null}

      <ConfirmModal
        open={confirmOpen}
        severity="MEDIUM"
        title={`Follow ${strategy.alias}`}
        disclosures={['copyTrading', 'capitalLoss', 'pastPerformance']}
        busy={submit.isPending}
        confirmLabel="Start copying"
        onCancel={() => {
          setConfirmOpen(false);
        }}
        onConfirm={() => {
          setServerErr(null);
          submit.mutate();
        }}
      >
        <p>
          Allocate <span className="font-mono">{allocation.value}</span> to copy {strategy.alias}
          {maxCopy.value !== '' ? (
            <>
              {' '}
              with a per-trade cap of <span className="font-mono">{maxCopy.value}</span>
            </>
          ) : null}
          {stopLoss.value !== '' ? <> and a {stopLoss.value}% provider-drawdown stop-loss</> : null}
          .
        </p>
        {serverErr !== null ? (
          <div className="mt-2">
            {stubbed ? (
              <p className="text-xs text-amber-400">
                The follow endpoint is unavailable on this deployment. Your request was NOT
                submitted.
              </p>
            ) : serverErr instanceof ApiError ? (
              <ErrorBox error={serverErr} />
            ) : null}
          </div>
        ) : null}
      </ConfirmModal>
    </>
  );
}
