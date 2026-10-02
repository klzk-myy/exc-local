/**
 * My follows — GET /api/v1/copy/follows list + DELETE unfollow
 * (Phase-14 Task 14.3.14, live). Unfollow is a HIGH-severity typed-
 * phrase confirm: pending copied orders are cancelled; copied positions
 * already open remain the investor's — the disclosure is shown before
 * confirm (spec §12.9).
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ErrorBox, btnDanger, tableCls, tdCls, thCls } from '@/lib/ui';
import {
  CONFIRM_PHRASES,
  ConfirmModal,
  UnavailablePanel,
  isNotImplemented,
} from '@/lib/input-helpers';

import { listMyFollows, unfollow, type MyFollow } from './api';

export function MyFollowsPanel() {
  const queryClient = useQueryClient();
  const q = useQuery({
    queryKey: ['copy-grid', 'follows'],
    queryFn: () => listMyFollows(apiClient),
    retry: (n, e) => !(e instanceof ApiError && e.status === 501) && n < 2,
  });
  const [target, setTarget] = useState<MyFollow | null>(null);
  const [serverErr, setServerErr] = useState<unknown>(null);

  const stop = useMutation({
    mutationFn: (f: MyFollow) => unfollow(apiClient, f.follow_id),
    onError: setServerErr,
    onSuccess: async () => {
      setServerErr(null);
      setTarget(null);
      await queryClient.invalidateQueries({ queryKey: ['copy-grid', 'follows'] });
    },
  });

  if (q.error !== null && isNotImplemented(q.error)) {
    return (
      <UnavailablePanel
        feature="Follow management"
        owner="Phase-14 Task 14.3.14"
        note="The follows endpoint is not serving on this deployment. No follow data is shown because none could be read."
      />
    );
  }
  const follows = q.data ?? [];

  return (
    <section aria-labelledby="my-follows-h" className="space-y-2">
      <h2 id="my-follows-h" className="text-sm font-semibold text-neutral-200">
        My follows
      </h2>
      {follows.length === 0 ? (
        <p className="text-sm text-neutral-500">
          {q.isLoading
            ? 'Loading follows…'
            : 'No follows yet — pick a strategy above to start copying.'}
        </p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Strategy</th>
              <th className={thCls}>Allocation</th>
              <th className={thCls}>Safety</th>
              <th className={thCls}>Stop-loss</th>
              <th className={thCls}>Status</th>
              <th className={thCls}></th>
            </tr>
          </thead>
          <tbody>
            {follows.map((f) => (
              <tr key={f.follow_id}>
                <td className={tdCls}>
                  <span className="font-medium text-neutral-200">{f.strategy_name}</span>
                  {f.strategy_status !== 'LISTED' ? (
                    <span className="ml-2 rounded bg-amber-500/20 px-1.5 py-0.5 text-xs text-amber-400">
                      {f.strategy_status}
                    </span>
                  ) : null}
                </td>
                <td className={`${tdCls} font-mono`}>
                  {f.allocation_notional} {f.currency}
                </td>
                <td className={tdCls}>{f.safety_mode}</td>
                <td className={`${tdCls} font-mono`}>{f.stop_loss_cap ?? '—'}</td>
                <td className={tdCls}>{f.status}</td>
                <td className={tdCls}>
                  {f.status === 'ACTIVE' ? (
                    <button
                      className={btnDanger}
                      onClick={() => {
                        setServerErr(null);
                        setTarget(f);
                      }}
                    >
                      Unfollow
                    </button>
                  ) : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <ConfirmModal
        open={target !== null}
        severity="HIGH"
        title={`Unfollow ${target?.strategy_name ?? ''}`}
        disclosures={['copyTrading', 'capitalLoss']}
        requirePhrase={CONFIRM_PHRASES['unfollow']}
        busy={stop.isPending}
        confirmLabel="Unfollow"
        onCancel={() => {
          setTarget(null);
        }}
        onConfirm={() => {
          if (target) {
            setServerErr(null);
            stop.mutate(target);
          }
        }}
      >
        <p className="text-sm">
          Unfollowing cancels all pending copied orders. Copied positions already open remain yours
          and are NOT closed automatically — manage or close them explicitly.
        </p>
        {serverErr !== null ? (
          <div className="mt-2">
            {serverErr instanceof ApiError ? (
              <ErrorBox error={serverErr} />
            ) : (
              <p className="text-xs text-amber-400">Unfollow failed — nothing changed.</p>
            )}
          </div>
        ) : null}
      </ConfirmModal>
    </section>
  );
}
