/**
 * Trade/order forensics panel (Task 10.5.3.27 gate-coverage wiring) —
 * admin order audit trail (Phase-05 Task 5.3.22, Compliance-Officer+)
 * and the trade bust / price-adjust action (Phase-15 Task 15.3.5,
 * dual-control approver required).
 */
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  JsonRows,
  btnGhost,
  btnPrimary,
  cardCls,
  hintTextCls,
  inputCls,
  labelCls,
  selectCls,
} from '@/lib/ui';

const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null;

export function TradeOpsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [notice, setNotice] = useState<string | null>(null);
  const [actionErr, setActionErr] = useState<unknown>(null);
  const [auditRows, setAuditRows] = useState<Record<string, unknown>[] | null>(null);
  const onErr = (e: unknown) => setActionErr(e);

  const [orderId, setOrderId] = useState('');
  const audit = useMutation({
    mutationFn: async (id: number) => {
      const raw = await adminApi.get<unknown>(`/admin/orders/${id}/audit`);
      return isRecord(raw) && Array.isArray(raw['audit'])
        ? (raw['audit'] as Record<string, unknown>[]).filter(isRecord)
        : [];
    },
    onSuccess: (rows) => {
      setAuditRows(rows);
      setNotice(null);
      setActionErr(null);
    },
    onError: (e) => {
      setAuditRows(null);
      onErr(e);
    },
  });

  const [bust, setBust] = useState({
    tradeId: '',
    action: 'BUST',
    adjustedPrice: '',
    referencePrice: '',
    reason: '',
    approverId: '',
  });
  const doBust = useMutation({
    mutationFn: () =>
      adminApi.post<unknown>(`/admin/trades/${bust.tradeId}/bust`, {
        action: bust.action,
        adjusted_price: bust.adjustedPrice === '' ? undefined : bust.adjustedPrice,
        reference_price: bust.referencePrice === '' ? undefined : bust.referencePrice,
        reason: bust.reason,
        approver_id: Number(bust.approverId),
      }),
    onSuccess: (res) => {
      setNotice(`Trade #${bust.tradeId} ${bust.action} — ${JSON.stringify(res)}`);
      setActionErr(null);
    },
    onError: onErr,
  });

  return (
    <section className={cardCls} aria-label="Trade and order forensics">
      <h2 className="mb-2 text-sm font-semibold">Trade &amp; order forensics</h2>
      {notice !== null && (
        <p role="status" className="mb-2 text-xs text-sky-300">
          {notice}
        </p>
      )}
      {actionErr !== null && (
        <p role="alert" className="mb-2 text-xs text-rose-300">
          {actionErr instanceof Error ? actionErr.message : 'Action failed'}
        </p>
      )}

      <div className="grid gap-4 md:grid-cols-2">
        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Order audit trail
          </h3>
          <form
            className="mb-2 flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              audit.mutate(Number(orderId));
            }}
          >
            <label className={labelCls}>
              Order id
              <input
                className={inputCls}
                value={orderId}
                onChange={(e) => setOrderId(e.target.value)}
                required
                inputMode="numeric"
              />
            </label>
            <button type="submit" className={btnGhost} disabled={audit.isPending}>
              Load audit
            </button>
          </form>
          {auditRows !== null && <JsonRows rows={auditRows} empty="No audit entries." />}
          <p className={`mt-1 ${hintTextCls}`}>
            Compliance Officer or higher; the resolver keys on the admin user id.
          </p>
        </div>

        <form
          className="grid content-start gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            doBust.mutate();
          }}
        >
          <h3 className="text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Trade bust / price adjust (dual control)
          </h3>
          <label className={labelCls}>
            Trade id
            <input
              className={inputCls}
              value={bust.tradeId}
              onChange={(e) => setBust({ ...bust, tradeId: e.target.value })}
              required
              inputMode="numeric"
            />
          </label>
          <label className={labelCls}>
            Action
            <select
              className={selectCls}
              value={bust.action}
              onChange={(e) => setBust({ ...bust, action: e.target.value })}
            >
              <option value="BUST">Bust</option>
              <option value="PRICE_ADJUST">Price adjust</option>
            </select>
          </label>
          {bust.action === 'PRICE_ADJUST' && (
            <>
              <label className={labelCls}>
                Adjusted price
                <input
                  className={inputCls}
                  value={bust.adjustedPrice}
                  onChange={(e) => setBust({ ...bust, adjustedPrice: e.target.value })}
                  required
                  inputMode="decimal"
                />
              </label>
              <label className={labelCls}>
                Reference price
                <input
                  className={inputCls}
                  value={bust.referencePrice}
                  onChange={(e) => setBust({ ...bust, referencePrice: e.target.value })}
                  inputMode="decimal"
                />
              </label>
            </>
          )}
          <label className={labelCls}>
            Reason
            <input
              className={inputCls}
              value={bust.reason}
              onChange={(e) => setBust({ ...bust, reason: e.target.value })}
              required
            />
          </label>
          <label className={labelCls}>
            Approver admin id
            <input
              className={inputCls}
              value={bust.approverId}
              onChange={(e) => setBust({ ...bust, approverId: e.target.value })}
              required
              inputMode="numeric"
            />
          </label>
          <button type="submit" className={btnPrimary} disabled={doBust.isPending}>
            Submit {bust.action === 'BUST' ? 'bust' : 'price adjust'}
          </button>
        </form>
      </div>
    </section>
  );
}
