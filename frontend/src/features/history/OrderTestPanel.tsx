/**
 * Order test / preview (Task 10.3.27 item 5) — a dry-run ticket that
 * calls POST /api/v1/orders/test and renders fee/margin/spread/filters/
 * warnings via the shared PreviewPanel. HIGH-risk results convert to a
 * live order only behind a typed-phrase ConfirmModal; the preview itself
 * NEVER submits (binding is always false server-side).
 */
import { useMemo, useState } from 'react';
import { useMutation } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ErrorBox, btnGhost, cardCls, selectCls } from '@/lib/ui';
import { submitOrder } from '@/lib/trading/api';
import { newIdempotencyKey } from '@/lib/api';
import {
  ConfirmModal,
  InputField,
  PreviewPanel,
  SymbolAutocomplete,
  severityForRisk,
  useInputHelper,
  useOrderPreview,
  useValidatedField,
} from '@/lib/input-helpers';
import {
  ORDER_TYPES,
  RULE_PRICE,
  RULE_SYMBOL,
  TIME_IN_FORCE,
  type FieldRule,
} from '@/lib/input-helpers/validation';

const QTY_RULE: FieldRule = { name: 'quantity', kind: 'decimal', required: true, positive: true };

export function OrderTestPanel() {
  const helper = useInputHelper('POST /api/v1/orders/test');
  const symbol = useValidatedField({ ...RULE_SYMBOL });
  const qty = useValidatedField(QTY_RULE);
  const price = useValidatedField({ ...RULE_PRICE, required: false });
  const stop = useValidatedField({ ...RULE_PRICE, required: false, name: 'stop_price' });
  const [side, setSide] = useState<'BUY' | 'SELL'>('BUY');
  const [type, setType] = useState<string>('LIMIT');
  const [tif, setTif] = useState<string>('GTC');
  const [confirmOpen, setConfirmOpen] = useState(false);

  const request = useMemo(() => {
    if (!symbol.valid || !qty.valid) return null;
    const needsPrice = type === 'LIMIT' || type === 'ICEBERG' || type === 'STOP_LIMIT';
    const needsStop = type === 'STOP' || type === 'STOP_LIMIT';
    if (needsPrice && !price.valid) return null;
    if (needsStop && !stop.valid) return null;
    return {
      symbol: symbol.value.toUpperCase(),
      side,
      type,
      time_in_force: tif,
      quantity: qty.value,
      ...(needsPrice && price.value !== '' ? { price: price.value } : {}),
      ...(needsStop && stop.value !== '' ? { stop_price: stop.value } : {}),
    };
  }, [
    symbol.valid,
    symbol.value,
    qty.valid,
    qty.value,
    price.valid,
    price.value,
    stop.valid,
    stop.value,
    side,
    type,
    tif,
  ]);

  const preview = useOrderPreview(apiClient, request);
  const severity = severityForRisk(preview.result?.risk_level ?? 'LOW');

  const place = useMutation({
    mutationFn: async () => {
      if (!request) throw new Error('incomplete order');
      return submitOrder(
        {
          symbol: request.symbol,
          side: request.side,
          type: request.type,
          time_in_force: request.time_in_force,
          quantity: request.quantity,
          price: request.price,
          stop_price: request.stop_price,
          client_order_id: newIdempotencyKey(),
        },
        apiClient,
      );
    },
    onSuccess: () => {
      setConfirmOpen(false);
    },
  });

  return (
    <section aria-label="Order test" className={cardCls}>
      <h2 className="text-base font-semibold text-neutral-100">Order test</h2>
      <p className="mt-1 text-xs text-neutral-500">
        Side-effect-free validation against the live matching rules — nothing is submitted.{' '}
        {helper.registered ? '' : '(route contract not found — check generated validators)'}
      </p>

      <div className="mt-3 grid grid-cols-2 gap-2 md:grid-cols-3">
        <SymbolAutocomplete
          value={symbol.value}
          onChange={symbol.setValue}
          onSelect={(s) => symbol.setValue(s)}
          label="Symbol"
        />
        <div>
          <label className="mb-1 block text-xs font-medium text-neutral-400" htmlFor="ot-side">
            Side
          </label>
          <select
            id="ot-side"
            className={selectCls}
            value={side}
            onChange={(e) => setSide(e.target.value as 'BUY' | 'SELL')}
          >
            {['BUY', 'SELL'].map((s) => (
              <option key={s}>{s}</option>
            ))}
          </select>
        </div>
        <div>
          <label className="mb-1 block text-xs font-medium text-neutral-400" htmlFor="ot-type">
            Type
          </label>
          <select
            id="ot-type"
            className={selectCls}
            value={type}
            onChange={(e) => setType(e.target.value)}
          >
            {ORDER_TYPES.map((t) => (
              <option key={t}>{t}</option>
            ))}
          </select>
        </div>
        <div>
          <label className="mb-1 block text-xs font-medium text-neutral-400" htmlFor="ot-tif">
            TIF
          </label>
          <select
            id="ot-tif"
            className={selectCls}
            value={tif}
            onChange={(e) => setTif(e.target.value)}
          >
            {TIME_IN_FORCE.map((t) => (
              <option key={t}>{t}</option>
            ))}
          </select>
        </div>
        <InputField field={qty} label="Quantity" inputMode="decimal" />
        <InputField field={price} label="Price" inputMode="decimal" />
        <InputField field={stop} label="Stop price" inputMode="decimal" />
      </div>

      <div className="mt-3">
        <PreviewPanel state={preview} />
      </div>

      <div className="mt-3 flex justify-end gap-2">
        <button
          type="button"
          className={btnGhost}
          disabled={preview.result === null}
          onClick={() => setConfirmOpen(true)}
        >
          Submit live order…
        </button>
      </div>

      <ConfirmModal
        open={confirmOpen}
        severity={severity}
        title="Submit live order"
        disclosures={severity === 'HIGH' ? ['capitalLoss'] : []}
        requirePhrase={severity === 'HIGH' ? 'SUBMIT ORDER' : undefined}
        busy={place.isPending}
        confirmLabel="Submit order"
        onCancel={() => {
          setConfirmOpen(false);
        }}
        onConfirm={() => {
          place.mutate();
        }}
      >
        <p>
          {side} {qty.value} {symbol.value.toUpperCase()} {type}
          {price.value !== '' ? ` @ ${price.value}` : ''} — risk level{' '}
          {preview.result?.risk_level ?? 'unknown'}.
        </p>
        {place.isError ? (
          <div className="mt-2">
            <ErrorBox error={place.error} />
          </div>
        ) : null}
        {place.isSuccess ? <p className="mt-2 text-xs text-emerald-400">Order submitted.</p> : null}
      </ConfirmModal>
    </section>
  );
}
