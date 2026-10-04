/**
 * Circuit-breaker panel (Phase-10.5 Task 10.5.3.1 §2) — manual trip and
 * dual-controlled reset over the Phase-13 five-tier breaker backend.
 *
 * POST /admin/circuit-breaker/{symbol} trips (single approver); the
 * path symbol drives INSTRUMENT scope by default, the body may retarget
 * ACCOUNT:{id} or MARKET_WIDE. POST .../reset submits a PENDING
 * dual-control request (15-min window) — the UI surfaces the request id
 * rather than pretending the breaker closed.
 */
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  selectCls,
} from '@/lib/ui';

import {
  CB_SCOPES,
  resetCircuitBreaker,
  tripCircuitBreaker,
  type CircuitBreakerScope,
} from './api';

export function CircuitBreakerPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [symbol, setSymbol] = useState('EUR/USD');
  const [scope, setScope] = useState<CircuitBreakerScope>('INSTRUMENT');
  const [targetId, setTargetId] = useState('');
  const [reason, setReason] = useState('');
  const [actionError, setActionError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState<'trip' | 'reset' | null>(null);

  const submit = async (kind: 'trip' | 'reset') => {
    setBusy(kind);
    setActionError(null);
    setNotice(null);
    try {
      const input = {
        symbol: symbol.trim(),
        scope,
        targetId: targetId.trim(),
        reason: reason.trim(),
      };
      if (kind === 'trip') {
        const res = await tripCircuitBreaker(adminApi, input);
        setNotice(`Breaker OPEN on ${res.scope}:${res.targetId}`);
      } else {
        const res = await resetCircuitBreaker(adminApi, input);
        setNotice(
          res.requestId !== undefined
            ? `Reset submitted for dual-control approval — request #${res.requestId} (15-min window)`
            : 'Reset submitted for dual-control approval',
        );
      }
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(null);
    }
  };

  return (
    <section className={cardCls} aria-label="Circuit breaker">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Circuit breaker</h2>
      <p className={hintTextCls}>
        Manual trip is single-approver; reset is four-eyes — it queues a dual-control request
        approved by a distinct Risk Manager in the dual-control queue.
      </p>

      {actionError !== null && <ErrorBox error={actionError} />}
      {notice !== null && (
        <p className="mb-2 text-sm text-emerald-300" role="status">
          {notice}
        </p>
      )}

      <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-5">
        <div>
          <label className={labelCls} htmlFor="cb-symbol">
            Symbol
          </label>
          <input
            id="cb-symbol"
            className={inputCls}
            value={symbol}
            onChange={(e) => setSymbol(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="cb-scope">
            Scope
          </label>
          <select
            id="cb-scope"
            className={selectCls}
            value={scope}
            onChange={(e) => setScope(e.target.value as CircuitBreakerScope)}
          >
            {CB_SCOPES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className={labelCls} htmlFor="cb-target">
            Target (defaults to symbol)
          </label>
          <input
            id="cb-target"
            className={inputCls}
            value={targetId}
            placeholder={scope === 'MARKET_WIDE' ? 'ignored' : symbol}
            onChange={(e) => setTargetId(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="cb-reason">
            Reason
          </label>
          <input
            id="cb-reason"
            className={inputCls}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </div>
        <div className="flex items-end gap-2">
          <button
            type="button"
            className={btnDanger}
            disabled={busy !== null || symbol.trim() === ''}
            onClick={() => void submit('trip')}
          >
            {busy === 'trip' ? 'Tripping…' : 'Trip'}
          </button>
          <button
            type="button"
            className={btnPrimary}
            disabled={busy !== null || symbol.trim() === ''}
            onClick={() => void submit('reset')}
          >
            {busy === 'reset' ? 'Submitting…' : 'Reset (4-eyes)'}
          </button>
        </div>
      </div>
    </section>
  );
}
