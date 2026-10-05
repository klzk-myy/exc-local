/**
 * Safety & data tab — Task 10.3.22 items 5–8:
 *   - emergency freeze (blocks logins, cancels open orders, disables
 *     withdrawals) + unfreeze request
 *   - cooling-off / self-exclusion (24h|7d|30d|permanent)
 *   - account closure wizard (precondition checklist + type-to-confirm)
 *   - GDPR export (async job) + erasure (legal-hold carve-out notice)
 */
import { useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import {
  ConfirmAction,
  ErrorBox,
  Field,
  Modal,
  btnDanger,
  btnGhost,
  btnPrimary,
  cardCls,
  inputCls,
  selectCls,
} from '@/lib/ui';

import * as api from './api';

function FreezeCard() {
  const [confirming, setConfirming] = useState(false);
  const [unfreezing, setUnfreezing] = useState(false);
  const [reason, setReason] = useState('');
  const freeze = useMutation({ mutationFn: () => api.emergencyFreeze(apiClient) });
  const unfreeze = useMutation({ mutationFn: () => api.requestUnfreeze(apiClient, reason) });

  return (
    <div className={cardCls}>
      <h3 className="mb-2 text-sm font-semibold text-red-300">Emergency freeze</h3>
      <p className="mb-3 text-sm text-neutral-400">
        Suspect your account is compromised? A freeze immediately blocks logins, cancels all open
        orders, and disables withdrawals until support reviews an unfreeze request.
      </p>
      {freeze.isSuccess ? (
        <p className="mb-2 text-sm text-amber-300" role="status">
          Account freeze requested. Sign-in is blocked; contact support or submit an unfreeze
          request to regain access.
        </p>
      ) : (
        <button
          type="button"
          className={btnDanger}
          onClick={() => {
            setConfirming(true);
          }}
        >
          Freeze my account now
        </button>
      )}
      <ErrorBox error={freeze.error ?? unfreeze.error} />
      {!unfreezing ? (
        <button
          type="button"
          className={`${btnGhost} mt-3`}
          onClick={() => {
            setUnfreezing(true);
          }}
        >
          Request unfreeze…
        </button>
      ) : (
        <form
          className="mt-3"
          onSubmit={(e) => {
            e.preventDefault();
            unfreeze.mutate();
          }}
        >
          <Field label="Reason for unfreeze request" required>
            {(id, describedBy, invalid) => (
              <input
                id={id}
                aria-describedby={describedBy}
                aria-invalid={invalid}
                className={inputCls}
                value={reason}
                onChange={(e) => {
                  setReason(e.target.value);
                }}
              />
            )}
          </Field>
          <button
            type="submit"
            className={btnPrimary}
            disabled={unfreeze.isPending || reason === ''}
          >
            {unfreeze.isPending ? 'Submitting…' : 'Submit unfreeze request'}
          </button>
          {unfreeze.isSuccess && (
            <p className="mt-2 text-sm text-emerald-400" role="status">
              Unfreeze request submitted — support will review it.
            </p>
          )}
        </form>
      )}

      <Modal
        open={confirming}
        title="Freeze account?"
        onClose={() => {
          setConfirming(false);
        }}
      >
        <ConfirmAction
          message={
            <>
              This immediately: <strong>blocks all logins</strong>,{' '}
              <strong>cancels every open order</strong>, and <strong>disables withdrawals</strong>.
              Unfreezing requires a support review. Continue?
            </>
          }
          confirmLabel="Yes — freeze my account"
          busy={freeze.isPending}
          onConfirm={() => {
            freeze.mutate(undefined, {
              onSuccess: () => {
                setConfirming(false);
              },
            });
          }}
          onCancel={() => {
            setConfirming(false);
          }}
        />
      </Modal>
    </div>
  );
}

function CoolingOffCard() {
  const [duration, setDuration] = useState<api.CoolingOffDuration>('24h');
  const [confirming, setConfirming] = useState(false);
  const mut = useMutation({ mutationFn: () => api.coolingOff(apiClient, duration) });

  return (
    <div className={cardCls}>
      <h3 className="mb-2 text-sm font-semibold">Cooling-off / self-exclusion</h3>
      <p className="mb-3 text-sm text-neutral-400">
        Take a break from trading. During cooling-off, order entry is disabled while withdrawals
        remain available (positions are not force-closed).
      </p>
      {mut.isSuccess ? (
        <p className="text-sm text-emerald-400" role="status">
          Cooling-off ({duration}) activated.
        </p>
      ) : (
        <>
          <ErrorBox error={mut.error} />
          <Field label="Duration">
            {(id, describedBy, invalid) => (
              <select
                id={id}
                aria-describedby={describedBy}
                aria-invalid={invalid}
                className={selectCls}
                value={duration}
                onChange={(e) => {
                  setDuration(e.target.value as api.CoolingOffDuration);
                }}
              >
                {api.COOLING_OFF_DURATIONS.map((d) => (
                  <option key={d} value={d}>
                    {d === 'permanent' ? 'Permanent self-exclusion' : d}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <button
            type="button"
            className={btnPrimary}
            onClick={() => {
              setConfirming(true);
            }}
          >
            Start cooling-off
          </button>
        </>
      )}
      <Modal
        open={confirming}
        title="Start cooling-off?"
        onClose={() => {
          setConfirming(false);
        }}
      >
        <ConfirmAction
          message={
            <>
              Order entry will be disabled for <strong>{duration}</strong>
              {duration === 'permanent' ? ' — permanent self-exclusion cannot be lifted early' : ''}
              . Withdrawals stay available. Confirm?
            </>
          }
          confirmLabel="Start cooling-off"
          busy={mut.isPending}
          onConfirm={() => {
            mut.mutate(undefined, {
              onSuccess: () => {
                setConfirming(false);
              },
            });
          }}
          onCancel={() => {
            setConfirming(false);
          }}
        />
      </Modal>
    </div>
  );
}

const CLOSE_PRECONDITIONS = [
  'All balances are zero (residual funds withdrawn)',
  'No open positions',
  'No open orders',
  'No pending withdrawals awaiting confirmation',
];

function CloseAccountCard() {
  const [open, setOpen] = useState(false);
  const [checked, setChecked] = useState<boolean[]>(CLOSE_PRECONDITIONS.map(() => false));
  const [typed, setTyped] = useState('');
  const mut = useMutation({ mutationFn: () => api.closeAccount(apiClient, typed) });
  const ready = checked.every(Boolean) && typed === 'CLOSE';

  return (
    <div className={cardCls}>
      <h3 className="mb-2 text-sm font-semibold text-red-300">Close account</h3>
      <p className="mb-3 text-sm text-neutral-400">
        Closing is permanent — open a withdrawal for residual balances first; records are retained
        per regulatory requirements even after closure.
      </p>
      {mut.isSuccess ? (
        <p className="text-sm text-emerald-400" role="status">
          Account closure request submitted — it completes once preconditions are verified.
        </p>
      ) : (
        <button
          type="button"
          className={btnDanger}
          onClick={() => {
            setOpen(true);
          }}
        >
          Begin account closure…
        </button>
      )}
      <Modal
        open={open}
        title="Close account"
        onClose={() => {
          setOpen(false);
        }}
      >
        <div className="mb-3 space-y-2">
          <p className="text-sm text-neutral-300">Confirm each precondition:</p>
          {CLOSE_PRECONDITIONS.map((c, i) => (
            <label key={c} className="flex items-start gap-2 text-sm text-neutral-200">
              <input
                type="checkbox"
                className="h-6 w-6 mt-0.5"
                checked={checked[i] === true}
                onChange={(e) => {
                  setChecked((prev) => prev.map((v, j) => (j === i ? e.target.checked : v)));
                }}
              />
              {c}
            </label>
          ))}
        </div>
        <Field label='Type "CLOSE" to confirm' required>
          {(id, describedBy, invalid) => (
            <input
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={inputCls}
              value={typed}
              onChange={(e) => {
                setTyped(e.target.value);
              }}
            />
          )}
        </Field>
        <ErrorBox error={mut.error} />
        <div className="flex justify-end gap-2">
          <button
            type="button"
            className={btnGhost}
            onClick={() => {
              setOpen(false);
            }}
          >
            Cancel
          </button>
          <button
            type="button"
            className={btnDanger}
            disabled={!ready || mut.isPending}
            onClick={() => {
              mut.mutate(undefined, {
                onSuccess: () => {
                  setOpen(false);
                },
              });
            }}
          >
            {mut.isPending ? 'Closing…' : 'Close my account'}
          </button>
        </div>
      </Modal>
    </div>
  );
}

function GdprCard() {
  const requests = useQuery({
    queryKey: ['account', 'gdpr-requests'],
    queryFn: () => api.gdprRequests(apiClient),
  });
  const export_ = useMutation({
    mutationFn: () => api.gdprExport(apiClient),
    onSuccess: () => requests.refetch(),
  });
  const [eraseConfirming, setEraseConfirming] = useState(false);
  const erase = useMutation({
    mutationFn: () => api.gdprErase(apiClient, 'ERASE'),
    onSuccess: () => requests.refetch(),
  });

  return (
    <div className={cardCls}>
      <h3 className="mb-2 text-sm font-semibold">Your data (GDPR)</h3>
      <p className="mb-3 text-sm text-neutral-400">
        Export a copy of your personal data, or request erasure. Records under a legal hold
        (transaction history, regulatory retention) are carved out of erasure by law.
      </p>
      <ErrorBox error={export_.error ?? erase.error} />
      {export_.isSuccess && (
        <p className="mb-2 text-sm text-emerald-400" role="status">
          Export started — you’ll receive a download link when the archive is ready.
        </p>
      )}
      {erase.isSuccess && (
        <p className="mb-2 text-sm text-emerald-400" role="status">
          Erasure request submitted — processing follows the retention schedule.
        </p>
      )}
      <button
        type="button"
        className={btnPrimary}
        disabled={export_.isPending}
        onClick={() => {
          export_.mutate();
        }}
      >
        {export_.isPending ? 'Requesting…' : 'Export my data'}
      </button>
      <button
        type="button"
        className={`${btnDanger} ml-2`}
        onClick={() => {
          setEraseConfirming(true);
        }}
      >
        Erase my data…
      </button>
      <Modal
        open={eraseConfirming}
        title="Erase personal data?"
        onClose={() => {
          setEraseConfirming(false);
        }}
      >
        <ConfirmAction
          message={
            <>
              Personal data will be erased where the law allows. Transaction and audit records under
              regulatory retention or an active legal hold are excluded. This cannot be undone —
              withdraw funds and close positions first.
            </>
          }
          confirmLabel="Request erasure"
          busy={erase.isPending}
          onConfirm={() => {
            erase.mutate(undefined, {
              onSuccess: () => {
                setEraseConfirming(false);
              },
            });
          }}
          onCancel={() => {
            setEraseConfirming(false);
          }}
        />
      </Modal>

      <ErrorBox error={requests.error} />
      {(requests.data?.length ?? 0) > 0 && (
        <div className="mt-3">
          <h4 className="mb-1 text-xs font-medium text-neutral-400">Request history</h4>
          <table className="w-full text-left text-xs">
            <thead>
              <tr className="text-neutral-500">
                <th className="py-1 pr-2 font-normal">Kind</th>
                <th className="py-1 pr-2 font-normal">Status</th>
                <th className="py-1 pr-2 font-normal">Requested</th>
                <th className="py-1 font-normal">Completed</th>
              </tr>
            </thead>
            <tbody>
              {(requests.data ?? []).map((r) => (
                <tr key={r.id} className="border-t border-neutral-800">
                  <td className="py-1 pr-2">{r.kind}</td>
                  <td className="py-1 pr-2">{r.status}</td>
                  <td className="py-1 pr-2 text-neutral-500">
                    {r.createdAt.slice(0, 19).replace('T', ' ')}
                  </td>
                  <td className="py-1 text-neutral-500">
                    {r.completedAt?.slice(0, 19).replace('T', ' ') ?? '—'}
                    {r.sha256 !== undefined && (
                      <span className="ml-1 text-neutral-600" title={`sha256 ${r.sha256}`}>
                        ⛁
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

export default function SafetyPanel() {
  return (
    <div className="space-y-4">
      <FreezeCard />
      <CoolingOffCard />
      <CloseAccountCard />
      <GdprCard />
    </div>
  );
}
