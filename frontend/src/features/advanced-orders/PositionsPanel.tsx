/**
 * Positions panel — open positions with unrealized P&L, ADL rank
 * (Task 10.3.13) and the Task 10.3.10 one-click quick actions
 * (Flatten / Reverse per row, Close All) — each behind a
 * projected-execution confirmation modal.
 *
 * Live overlay: `private:positions` supplies adl_indicator + fresh
 * mark/liq prices; REST `/positions` reconciles every 15s. No field is
 * ever fabricated — missing ADL data renders "ADL n/a".
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient, wsClient } from '@/app/runtime';
import { useWsStatus, type WsClient } from '@/lib/ws';
import { ErrorBox, btnGhost, tableCls, tdCls, thCls } from '@/lib/ui';
import { useAccountScope } from '@/lib/trading/accountScope';
import { closeAllPositions, submitOrder, type Api } from '@/lib/trading/api';
import { formatPrice } from '@/lib/trading/fx';
import { bookMid, projectFill } from '@/lib/trading/projections';
import { useDepthBook, useMarketFeed } from '@/lib/trading/marketStore';
import { usePrivatePositionsFeed, usePositionOverlay } from '@/lib/trading/positionFeed';
import { usePositions, useScopeKey } from '@/lib/trading/queries';
import type { Position } from '@/lib/trading/types';
import { newIdempotencyKey } from '@/lib/api';

import { AdlIndicator } from './AdlIndicator';
import { ConfirmExecutionModal } from './ConfirmExecutionModal';

type PendingAction =
  | { kind: 'flatten'; position: Position }
  | { kind: 'reverse'; position: Position }
  | { kind: 'close-all' };

function oppositeSide(p: Position): 'BUY' | 'SELL' {
  return p.side === 'LONG' ? 'SELL' : 'BUY';
}

function PositionActions({
  position,
  onAction,
  disabled,
}: {
  position: Position;
  onAction: (a: PendingAction) => void;
  disabled: boolean;
}) {
  return (
    <span className="flex gap-1">
      <button
        type="button"
        className={`${btnGhost} px-2 py-0.5 text-xs`}
        disabled={disabled}
        aria-label={`Flatten ${position.side} ${position.symbol}`}
        onClick={() => onAction({ kind: 'flatten', position })}
      >
        Flatten
      </button>
      <button
        type="button"
        className={`${btnGhost} px-2 py-0.5 text-xs`}
        disabled={disabled}
        aria-label={`Reverse ${position.side} ${position.symbol}`}
        onClick={() => onAction({ kind: 'reverse', position })}
      >
        Reverse
      </button>
    </span>
  );
}

function PositionRow({
  position,
  onAction,
  orderEnabled,
}: {
  position: Position;
  onAction: (a: PendingAction) => void;
  orderEnabled: boolean;
}) {
  const overlay = usePositionOverlay(position);
  const mark = overlay?.markPrice ?? position.markPrice;
  const liq = overlay?.liquidationPrice ?? position.liquidationPrice;
  const upnl = overlay?.unrealizedPnl ?? position.unrealizedPnl;
  const adl = overlay?.adlIndicator ?? position.adlIndicator;
  const pnlClass = upnl.isNegative() ? 'text-red-400' : 'text-emerald-400';

  return (
    <tr>
      <td className={tdCls}>
        <span className="font-medium">{position.symbol}</span>
        <span
          className={`ml-2 text-xs ${position.side === 'LONG' ? 'text-emerald-400' : 'text-red-400'}`}
        >
          {position.side}
        </span>
      </td>
      <td className={tdCls}>{position.quantity.toDisplay()}</td>
      <td className={tdCls}>{formatPrice(position.symbol, position.entryPrice)}</td>
      <td className={tdCls}>{formatPrice(position.symbol, mark)}</td>
      <td className={`${tdCls} ${pnlClass}`}>
        {upnl.toDisplay(2)} {upnl.isNegative() ? '(loss)' : '(gain)'}
      </td>
      <td className={tdCls}>{formatPrice(position.symbol, liq)}</td>
      <td className={tdCls}>
        <AdlIndicator rank={adl} symbol={position.symbol} />
      </td>
      <td className={tdCls}>
        <PositionActions position={position} onAction={onAction} disabled={!orderEnabled} />
      </td>
    </tr>
  );
}

export function PositionsPanel({
  client = wsClient,
  api = apiClient,
  bare = false,
}: {
  client?: WsClient;
  api?: Api;
  /** Strip tile chrome when embedded in a framed panel. */
  bare?: boolean;
}) {
  const ws = useWsStatus(client);
  const positions = usePositions();
  const scope = useScopeKey();
  const scopeLabel = useAccountScope((s) => s.scopeLabel);
  const queryClient = useQueryClient();
  const [pending, setPending] = useState<PendingAction | null>(null);
  const [tfa, setTfa] = useState('');
  const [lastError, setLastError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);

  usePrivatePositionsFeed(client);
  // Live depth for the projection of whichever position is being acted on.
  const pendingSymbol =
    pending !== null && pending.kind !== 'close-all' ? pending.position.symbol : undefined;
  useMarketFeed(pendingSymbol, client, { depth: true });
  const book = useDepthBook(pendingSymbol);

  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: ['positions', scope] });
    await queryClient.invalidateQueries({ queryKey: ['orders', scope] });
  };

  const run = useMutation({
    mutationFn: async (a: PendingAction) => {
      if (a.kind === 'close-all') {
        await closeAllPositions({ twoFactorToken: tfa || undefined }, api);
        return 'Close-all submitted';
      }
      const p = a.position;
      const side = oppositeSide(p);
      const qty = p.quantity.toString();
      // Flatten: reduce-only market close. Reverse: close, then open the
      // equal-size opposite position (per Task 10.3.10 spec text).
      await submitOrder(
        {
          symbol: p.symbol,
          side,
          type: 'MARKET',
          quantity: qty,
          reduce_only: true,
          client_order_id: newIdempotencyKey(),
        },
        api,
      );
      if (a.kind === 'reverse') {
        await submitOrder(
          {
            symbol: p.symbol,
            side,
            type: 'MARKET',
            quantity: qty,
            client_order_id: newIdempotencyKey(),
          },
          api,
        );
        return `Reversed — now ${p.side === 'LONG' ? 'SHORT' : 'LONG'} ${qty} ${p.symbol}`;
      }
      return `Flatten submitted — ${p.symbol} → flat`;
    },
    onSuccess: async (msg) => {
      setNotice(msg);
      setPending(null);
      setLastError(null);
      await refresh();
    },
    onError: (e) => setLastError(e),
  });

  const projection =
    pending !== null && pending.kind !== 'close-all'
      ? projectFill(book, oppositeSide(pending.position), pending.position.quantity, bookMid(book))
      : undefined;

  const title =
    pending?.kind === 'flatten'
      ? 'Flatten position'
      : pending?.kind === 'reverse'
        ? 'Reverse position'
        : 'Close all positions';
  const actionText =
    pending !== null && pending.kind !== 'close-all'
      ? `${pending.kind === 'flatten' ? 'Close' : 'Close and reverse'} ${pending.position.side} ${pending.position.quantity.toDisplay()} ${pending.position.symbol} at market`
      : `Close every open position on ${scopeLabel} at market`;
  const resulting =
    pending === null
      ? undefined
      : pending.kind === 'flatten' || pending.kind === 'close-all'
        ? 'flat'
        : `${pending.position.side === 'LONG' ? 'SHORT' : 'LONG'} ${pending.position.quantity.toDisplay()}`;

  return (
    <section
      aria-label="Open positions"
      className={bare ? '' : 'rounded-lg border border-neutral-800 bg-neutral-900 p-4'}
    >
      <div className="mb-3 flex items-center justify-between">
        <h2 className="text-sm font-semibold text-neutral-200">Positions — {scopeLabel}</h2>
        <button
          type="button"
          className="rounded border border-red-800 px-3 py-1 text-xs font-medium text-red-300 hover:bg-red-950/40 disabled:opacity-50"
          disabled={!ws.orderEntryEnabled || (positions.data ?? []).length === 0}
          onClick={() => setPending({ kind: 'close-all' })}
        >
          Close all positions
        </button>
      </div>

      <ErrorBox
        error={positions.isError ? positions.error : lastError}
        onDismiss={() => setLastError(null)}
      />
      <div aria-live="polite">
        {notice !== null && <p className="mb-2 text-xs text-emerald-400">{notice}</p>}
      </div>

      <div className="overflow-x-auto">
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Symbol</th>
              <th className={thCls}>Qty</th>
              <th className={thCls}>Entry</th>
              <th className={thCls}>Mark</th>
              <th className={thCls}>uPnL</th>
              <th className={thCls}>Liq. est.</th>
              <th className={thCls}>ADL</th>
              <th className={thCls}>
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {(positions.data ?? []).map((p) => (
              <PositionRow
                key={p.id}
                position={p}
                onAction={setPending}
                orderEnabled={ws.orderEntryEnabled}
              />
            ))}
            {(positions.data ?? []).length === 0 && positions.isSuccess && (
              <tr>
                <td className={`${tdCls} text-neutral-500`} colSpan={8}>
                  No open positions{scope !== 'master' ? ` on ${scopeLabel}` : ''}.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      <ConfirmExecutionModal
        open={pending !== null}
        title={title}
        action={actionText}
        projection={projection}
        resultingPosition={resulting}
        busy={run.isPending}
        confirmLabel={
          pending?.kind === 'reverse'
            ? 'Reverse'
            : pending?.kind === 'flatten'
              ? 'Flatten'
              : 'Close all'
        }
        onClose={() => setPending(null)}
        onConfirm={() => {
          if (pending) run.mutate(pending);
        }}
      >
        {pending?.kind === 'close-all' && (
          <div className="mb-2">
            <label htmlFor="closeall-2fa" className="mb-1 block text-xs text-neutral-400">
              2FA token (required when the account mandates it — sent as X-2FA-Token)
            </label>
            <input
              id="closeall-2fa"
              inputMode="numeric"
              autoComplete="one-time-code"
              value={tfa}
              onChange={(e) => setTfa(e.target.value)}
              className="w-full rounded border border-neutral-700 bg-neutral-950 px-3 py-1.5 text-sm"
            />
          </div>
        )}
        {pending?.kind === 'reverse' && (
          <p className="mb-2 text-xs text-amber-300">
            Reverse = reduce-only market close + equal-size opposite market order — two executions,
            two fills.
          </p>
        )}
        {lastError !== null && <ErrorBox error={lastError} />}
      </ConfirmExecutionModal>
    </section>
  );
}
