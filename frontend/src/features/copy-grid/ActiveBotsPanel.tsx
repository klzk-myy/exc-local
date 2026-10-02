/**
 * Active grid-bot management (Task 10.3.26 item 4) —
 * GET /api/v1/bots/grid list with live P&L + filled levels when the
 * backend supplies them, pause/resume controls (RUNNING→PAUSED freezes
 * new legs while children keep working; PAUSED→RUNNING re-arms), and
 * stop/delete behind a typed-phrase confirmation with an explicit
 * close-all-positions choice.
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ErrorBox, btnDanger, btnGhost, tableCls, tdCls, thCls } from '@/lib/ui';
import {
  CONFIRM_PHRASES,
  ConfirmModal,
  UnavailablePanel,
  isNotImplemented,
} from '@/lib/input-helpers';

import {
  deleteGridBot,
  gridBotControlRegistered,
  listGridBots,
  pauseGridBot,
  resumeGridBot,
  type GridBot,
} from './api';

export function ActiveBotsPanel({ onCount }: { onCount?: (n: number) => void }) {
  const queryClient = useQueryClient();
  const q = useQuery({
    queryKey: ['copy-grid', 'bots'],
    queryFn: async () => {
      const bots = await listGridBots(apiClient);
      onCount?.(bots.length);
      return bots;
    },
    retry: (n, e) => !(e instanceof ApiError && e.status === 501) && n < 2,
  });
  const [stopping, setStopping] = useState<GridBot | null>(null);
  const [closePositions, setClosePositions] = useState(true);
  const [serverErr, setServerErr] = useState<unknown>(null);
  const [controlErr, setControlErr] = useState<unknown>(null);

  const control = useMutation({
    mutationFn: async (bot: GridBot) =>
      bot.status === 'PAUSED' ? resumeGridBot(apiClient, bot.id) : pauseGridBot(apiClient, bot.id),
    onError: setControlErr,
    onSuccess: async () => {
      setControlErr(null);
      await queryClient.invalidateQueries({ queryKey: ['copy-grid', 'bots'] });
    },
  });

  const stop = useMutation({
    mutationFn: async (bot: GridBot) => deleteGridBot(apiClient, bot.id, closePositions),
    onError: setServerErr,
    onSuccess: async () => {
      setStopping(null);
      setServerErr(null);
      await queryClient.invalidateQueries({ queryKey: ['copy-grid', 'bots'] });
    },
  });

  if (q.isPending) return <p className="text-sm text-neutral-500">Loading bots…</p>;
  if (q.isError) {
    if (isNotImplemented(q.error)) {
      return (
        <UnavailablePanel
          feature="Grid bots"
          owner="Phase-16 Task 16.3.19"
          note="Bot listing, live P&L and level fill state arrive with the grid engine."
        />
      );
    }
    return <ErrorBox error={q.error} />;
  }

  const bots = q.data;
  const controlsLive = gridBotControlRegistered();

  return (
    <section aria-label="Active grid bots" className="space-y-2">
      {bots.length === 0 ? (
        <p className="text-sm text-neutral-500">No active grid bots.</p>
      ) : (
        <div className="overflow-x-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Pair</th>
                <th className={thCls}>Range</th>
                <th className={thCls}>Levels</th>
                <th className={thCls}>Investment</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>P&amp;L</th>
                <th className={thCls}>Filled</th>
                <th className={thCls} />
              </tr>
            </thead>
            <tbody>
              {bots.map((b) => (
                <tr key={b.id}>
                  <td className={tdCls + ' font-mono'}>{b.symbol}</td>
                  <td className={tdCls + ' font-mono'}>
                    {b.lowerPrice ?? '—'} – {b.upperPrice ?? '—'}
                  </td>
                  <td className={tdCls}>{b.gridCount ?? '—'}</td>
                  <td className={tdCls}>{b.totalInvestment ?? '—'}</td>
                  <td className={tdCls}>{b.status}</td>
                  <td className={tdCls}>{b.pnl ?? '—'}</td>
                  <td className={tdCls}>{b.filledLevels ?? '—'}</td>
                  <td className={tdCls}>
                    <div className="flex gap-1">
                      <button
                        type="button"
                        className={btnGhost}
                        disabled={
                          !controlsLive ||
                          control.isPending ||
                          (b.status !== 'RUNNING' && b.status !== 'PAUSED')
                        }
                        title={
                          !controlsLive
                            ? 'Pause/resume arrives with Phase-16'
                            : b.status === 'PAUSED'
                              ? 'Resume bot'
                              : 'Pause bot'
                        }
                        onClick={() => {
                          setControlErr(null);
                          control.mutate(b);
                        }}
                      >
                        {b.status === 'PAUSED' ? 'Resume' : 'Pause'}
                      </button>
                      <button
                        type="button"
                        className={btnDanger}
                        onClick={() => {
                          setStopping(b);
                          setServerErr(null);
                        }}
                      >
                        Stop
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {!controlsLive ? (
        <p className="text-xs text-neutral-500">
          Pause/resume controls are not yet registered on this deployment.
        </p>
      ) : null}
      {controlErr !== null ? (
        <div className="mt-1">
          {isNotImplemented(controlErr) ? (
            <p className="text-xs text-amber-400">
              Bot pause/resume returned 501 (route expected live — regression signal). Nothing
              changed.
            </p>
          ) : (
            <ErrorBox error={controlErr} />
          )}
        </div>
      ) : null}

      <ConfirmModal
        open={stopping !== null}
        severity="HIGH"
        title={`Stop grid bot ${stopping?.symbol ?? ''}`}
        disclosures={['gridBot', 'capitalLoss']}
        requirePhrase={CONFIRM_PHRASES['stopGridBot']}
        busy={stop.isPending}
        confirmLabel="Stop bot"
        onCancel={() => {
          setStopping(null);
        }}
        onConfirm={() => {
          if (stopping) {
            setServerErr(null);
            stop.mutate(stopping);
          }
        }}
      >
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={closePositions}
            onChange={(e) => {
              setClosePositions(e.target.checked);
            }}
          />
          Close all positions opened by this bot
        </label>
        {!closePositions ? (
          <p className="mt-2 text-xs text-amber-400">
            Positions stay open after the bot stops — you remain responsible for them.
          </p>
        ) : null}
        {serverErr !== null ? (
          <div className="mt-2">
            {isNotImplemented(serverErr) ? (
              <p className="text-xs text-amber-400">
                Bot stop returned 501 (route is expected live — treat as regression). Nothing was
                stopped.
              </p>
            ) : (
              <ErrorBox error={serverErr} />
            )}
          </div>
        ) : null}
      </ConfirmModal>
    </section>
  );
}
