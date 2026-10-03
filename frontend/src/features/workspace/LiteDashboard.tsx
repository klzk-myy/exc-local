/**
 * Lite dashboard (Task 10.3.9) — the simplified surface for T0/T1
 * tiers: balances, a deliberately-minimal order form (market / limit,
 * GTC), and a read-only positions table. Advanced surfaces stay in Pro
 * mode; nothing safety-critical is hidden — the order ticket is the
 * dashboard itself.
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';

import { apiClient, wsClient } from '@/app/runtime';
import { useWsStatus, type WsClient } from '@/lib/ws';
import {
  ErrorBox,
  btnPrimary,
  cardCls,
  inputCls,
  labelCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';
import { newIdempotencyKey } from '@/lib/api';
import { submitOrder, type Api } from '@/lib/trading/api';
import { formatPrice, parseInput } from '@/lib/trading/fx';
import { useInstruments, usePositions } from '@/lib/trading/queries';
import { useScopeKey } from '@/lib/trading/queries';

import { BalancesPanel } from './BalancesPanel';

export function LiteOrderForm({
  client = wsClient,
  api = apiClient,
}: {
  client?: WsClient;
  api?: Api;
}) {
  const ws = useWsStatus(client);
  const instruments = useInstruments();
  const scope = useScopeKey();
  const queryClient = useQueryClient();
  const [symbol, setSymbol] = useState('EUR/USD');
  const [side, setSide] = useState<'BUY' | 'SELL'>('BUY');
  const [type, setType] = useState<'MARKET' | 'LIMIT'>('MARKET');
  const [qty, setQty] = useState('');
  const [price, setPrice] = useState('');
  const [err, setErr] = useState<unknown>(null);
  const [fieldErr, setFieldErr] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);

  const submit = useMutation({
    mutationFn: () =>
      submitOrder(
        {
          symbol,
          side,
          type,
          quantity: qty,
          price: type === 'LIMIT' ? price : undefined,
          time_in_force: type === 'LIMIT' ? 'GTC' : undefined,
          client_order_id: newIdempotencyKey(),
        },
        api,
      ),
    onSuccess: async () => {
      setDone('Order submitted.');
      setErr(null);
      await queryClient.invalidateQueries({ queryKey: ['orders', scope] });
    },
    onError: (e) => {
      setDone(null);
      setErr(e);
    },
  });

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setFieldErr(null);
    setDone(null);
    const q = parseInput(qty);
    if (!q?.isPositive()) {
      setFieldErr('Enter a quantity greater than zero.');
      return;
    }
    if (type === 'LIMIT') {
      const p = parseInput(price);
      if (!p?.isPositive()) {
        setFieldErr('Enter a limit price greater than zero.');
        return;
      }
    }
    submit.mutate();
  };

  return (
    <form onSubmit={onSubmit} className={cardCls} aria-label="Simple order form">
      <h2 className="mb-3 text-sm font-semibold text-neutral-200">Place an order</h2>
      <div className="mb-3 grid grid-cols-2 gap-1" role="group" aria-label="Side">
        {(['BUY', 'SELL'] as const).map((s) => (
          <button
            key={s}
            type="button"
            aria-pressed={side === s}
            onClick={() => setSide(s)}
            className={`rounded py-1.5 text-sm font-semibold focus-visible:ring-2 focus-visible:ring-sky-500 ${
              side === s
                ? s === 'BUY'
                  ? 'bg-emerald-700 text-white'
                  : 'bg-red-700 text-white'
                : 'bg-neutral-800 text-neutral-400 hover:bg-neutral-700'
            }`}
          >
            {s === 'BUY' ? 'Buy' : 'Sell'}
          </button>
        ))}
      </div>
      <div className="mb-3">
        <label htmlFor="lite-symbol" className={labelCls}>
          Pair
        </label>
        <select
          id="lite-symbol"
          value={symbol}
          onChange={(e) => setSymbol(e.target.value)}
          className={selectCls}
        >
          {(instruments.data ?? []).map((i) => (
            <option key={i.symbol} value={i.symbol}>
              {i.symbol}
            </option>
          ))}
          {instruments.data === undefined && <option value={symbol}>{symbol}</option>}
        </select>
      </div>
      <div className="mb-3 grid grid-cols-2 gap-2">
        <div>
          <label htmlFor="lite-qty" className={labelCls}>
            Quantity
          </label>
          <input
            id="lite-qty"
            inputMode="decimal"
            value={qty}
            onChange={(e) => setQty(e.target.value)}
            aria-invalid={fieldErr !== null}
            className={inputCls}
          />
        </div>
        <div>
          <label htmlFor="lite-type" className={labelCls}>
            Type
          </label>
          <select
            id="lite-type"
            value={type}
            onChange={(e) => setType(e.target.value as 'MARKET' | 'LIMIT')}
            className={selectCls}
          >
            <option value="MARKET">Market</option>
            <option value="LIMIT">Limit</option>
          </select>
        </div>
      </div>
      {type === 'LIMIT' && (
        <div className="mb-3">
          <label htmlFor="lite-price" className={labelCls}>
            Limit price
          </label>
          <input
            id="lite-price"
            inputMode="decimal"
            value={price}
            onChange={(e) => setPrice(e.target.value)}
            className={inputCls}
          />
        </div>
      )}
      {fieldErr !== null && (
        <p className="mb-2 text-xs text-red-400" role="alert">
          {fieldErr}
        </p>
      )}
      <ErrorBox error={err} onDismiss={() => setErr(null)} />
      <div aria-live="polite">
        {done !== null && <p className="mb-2 text-xs text-emerald-400">{done}</p>}
      </div>
      <button
        type="submit"
        className={`${btnPrimary} w-full`}
        disabled={!ws.orderEntryEnabled || submit.isPending}
      >
        {submit.isPending
          ? 'Submitting…'
          : ws.orderEntryEnabled
            ? `${side === 'BUY' ? 'Buy' : 'Sell'} ${symbol}`
            : `Order entry locked (${ws.state})`}
      </button>
    </form>
  );
}

function LitePositions() {
  const positions = usePositions();
  return (
    <div className={cardCls}>
      <h2 className="mb-3 text-sm font-semibold text-neutral-200">Open positions</h2>
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>Pair</th>
            <th className={thCls}>Side</th>
            <th className={thCls}>Size</th>
            <th className={thCls}>Entry</th>
            <th className={thCls}>Mark</th>
            <th className={thCls}>P&L</th>
          </tr>
        </thead>
        <tbody>
          {(positions.data ?? []).map((p) => (
            <tr key={p.id}>
              <td className={tdCls}>{p.symbol}</td>
              <td className={tdCls}>{p.side}</td>
              <td className={tdCls}>{p.quantity.toDisplay()}</td>
              <td className={tdCls}>{formatPrice(p.symbol, p.entryPrice)}</td>
              <td className={tdCls}>{formatPrice(p.symbol, p.markPrice)}</td>
              <td
                className={`${tdCls} ${p.unrealizedPnl.isNegative() ? 'text-red-400' : 'text-emerald-400'}`}
              >
                {p.unrealizedPnl.toDisplay(2)}
              </td>
            </tr>
          ))}
          {(positions.data ?? []).length === 0 && positions.isSuccess && (
            <tr>
              <td className={`${tdCls} text-neutral-500`} colSpan={6}>
                No open positions.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}

export function LiteDashboard() {
  return (
    <div className="mx-auto grid max-w-4xl gap-4 p-4 md:grid-cols-2">
      <LiteOrderForm />
      <div className="flex flex-col gap-4">
        <BalancesPanel />
        <LitePositions />
      </div>
    </div>
  );
}
