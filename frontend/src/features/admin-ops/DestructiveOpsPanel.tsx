/**
 * Destructive-ops panel (Phase-10.5 Task 10.5.3.1 §6) — the levers that
 * must never be one click: cross-account mass cancel, manual
 * liquidation, cache warm, and FIX-session entitlement update. Each
 * action is behind a typed confirmation; mass-cancel and liquidation
 * surface their required role in the copy (Risk Manager+; liquidation
 * additionally needs a distinct approver — four-eyes server-side).
 */
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
  btnPrimary,
  cardCls,
  ConfirmAction,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  Modal,
  selectCls,
} from '@/lib/ui';

import { adminCacheWarm, adminManualLiquidation, adminMassCancel, updateFixSession } from './api';

type PendingAction =
  | { kind: 'mass-cancel' }
  | { kind: 'liquidation' }
  | { kind: 'cache-warm' }
  | { kind: 'fix-session' };

export function DestructiveOpsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  // mass cancel form
  const [mcAccount, setMcAccount] = useState('');
  const [mcSymbol, setMcSymbol] = useState('');
  const [mcSide, setMcSide] = useState('');
  const [mcType, setMcType] = useState('');
  // manual liquidation form
  const [liqAccount, setLiqAccount] = useState('');
  const [liqInstrument, setLiqInstrument] = useState('');
  const [liqReason, setLiqReason] = useState('');
  const [liqOverride, setLiqOverride] = useState(false);
  const [liqApprover, setLiqApprover] = useState('');
  // fix session form
  const [fixId, setFixId] = useState('');
  const [fixAccount, setFixAccount] = useState('');
  const [fixApiKey, setFixApiKey] = useState('');
  const [fixInstruments, setFixInstruments] = useState('');
  const [fixCod, setFixCod] = useState<'unchanged' | 'true' | 'false'>('unchanged');

  const [pending, setPending] = useState<PendingAction | null>(null);
  const [actionError, setActionError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const confirmLabels: Record<PendingAction['kind'], string> = {
    'mass-cancel': 'Mass cancel',
    liquidation: 'Force liquidation',
    'cache-warm': 'Run cache warm',
    'fix-session': 'Update FIX session',
  };

  const confirmCopy: Record<PendingAction['kind'], string> = {
    'mass-cancel': `Cancel ${mcAccount.trim() === '' ? 'ALL open orders across ALL accounts' : `open orders on account ${mcAccount.trim()}`}${mcSymbol.trim() !== '' ? ` on ${mcSymbol.trim()}` : ''}? Requires Risk Manager — immediate and irreversible.`,
    liquidation: `Force-liquidate account ${liqAccount.trim()}${liqInstrument.trim() !== '' ? ` instrument ${liqInstrument.trim()}` : ' (all positions)'}? Dual control — approver ${liqApprover.trim()} must be a distinct admin. ${liqOverride ? 'OVERRIDE: skips the auction CALL phase → FORCE_CASH at mark.' : ''}`,
    'cache-warm':
      'Run a synchronous cache warm? Executes under the P0+P1 budget (~35s worst case) and reports the result inline.',
    'fix-session': `Apply entitlement patch to FIX session ${fixId.trim()}? Changes take effect on the next inbound message.`,
  };

  const execute = async () => {
    if (pending === null) return;
    setBusy(true);
    setActionError(null);
    setNotice(null);
    try {
      switch (pending.kind) {
        case 'mass-cancel': {
          const res = await adminMassCancel(adminApi, {
            accountId: mcAccount.trim() === '' ? undefined : Number(mcAccount),
            symbol: mcSymbol.trim() === '' ? undefined : mcSymbol.trim(),
            side: mcSide === '' ? undefined : mcSide,
            orderType: mcType === '' ? undefined : mcType,
          });
          setNotice(`Mass cancel submitted — ${JSON.stringify(res)}`);
          break;
        }
        case 'liquidation': {
          const res = await adminManualLiquidation(adminApi, {
            accountId: Number(liqAccount),
            instrumentId: liqInstrument.trim() === '' ? undefined : Number(liqInstrument),
            reason: liqReason.trim(),
            overrideAuction: liqOverride,
            approverId: Number(liqApprover),
          });
          setNotice(`Manual liquidation recorded — ${JSON.stringify(res)}`);
          break;
        }
        case 'cache-warm': {
          const res = await adminCacheWarm(adminApi);
          setNotice(`Cache warm finished — ${JSON.stringify(res)}`);
          break;
        }
        case 'fix-session': {
          await updateFixSession(apiClient, adminApi.env, fixId.trim(), {
            accountId: fixAccount.trim() === '' ? null : Number(fixAccount),
            apiKeyId: fixApiKey.trim() === '' ? null : Number(fixApiKey),
            allowedInstruments: fixInstruments.trim() === '' ? null : fixInstruments.trim(),
            cancelOnDisconnect: fixCod === 'unchanged' ? undefined : fixCod === 'true',
          });
          setNotice(`FIX session ${fixId.trim()} patched`);
          break;
        }
      }
      setPending(null);
    } catch (e) {
      setActionError(e);
    } finally {
      setBusy(false);
    }
  };

  const liqValid =
    liqAccount.trim() !== '' &&
    Number(liqAccount) > 0 &&
    liqReason.trim() !== '' &&
    liqApprover.trim() !== '' &&
    Number(liqApprover) > 0;
  const fixValid = fixId.trim() !== '';

  return (
    <section className={cardCls} aria-label="Destructive operations">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Destructive operations</h2>
      <p className={hintTextCls}>
        Every action here is typed-confirmed and audit-written. Mass cancel and manual liquidation
        require Risk Manager; liquidation additionally requires a distinct approver.
      </p>

      {actionError !== null && <ErrorBox error={actionError} />}
      {notice !== null && (
        <p className="mb-2 break-all text-sm text-emerald-300" role="status">
          {notice}
        </p>
      )}

      <div className="grid gap-6 lg:grid-cols-2">
        {/* Mass cancel */}
        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Mass cancel (cross-account)
          </h3>
          <div className="grid grid-cols-2 gap-2">
            <div>
              <label className={labelCls} htmlFor="mc-account">
                Account id (empty = ALL)
              </label>
              <input
                id="mc-account"
                className={inputCls}
                inputMode="numeric"
                value={mcAccount}
                onChange={(e) => setMcAccount(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="mc-symbol">
                Symbol (optional)
              </label>
              <input
                id="mc-symbol"
                className={inputCls}
                value={mcSymbol}
                onChange={(e) => setMcSymbol(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="mc-side">
                Side (optional)
              </label>
              <select
                id="mc-side"
                className={selectCls}
                value={mcSide}
                onChange={(e) => setMcSide(e.target.value)}
              >
                <option value="">Any</option>
                <option value="BUY">BUY</option>
                <option value="SELL">SELL</option>
              </select>
            </div>
            <div>
              <label className={labelCls} htmlFor="mc-type">
                Order type (optional)
              </label>
              <input
                id="mc-type"
                className={inputCls}
                value={mcType}
                placeholder="LIMIT / STOP…"
                onChange={(e) => setMcType(e.target.value)}
              />
            </div>
          </div>
          <button
            type="button"
            className={`${btnDanger} mt-2`}
            disabled={busy}
            onClick={() => setPending({ kind: 'mass-cancel' })}
          >
            Mass cancel…
          </button>
        </div>

        {/* Manual liquidation */}
        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Manual liquidation (four-eyes)
          </h3>
          <div className="grid grid-cols-2 gap-2">
            <div>
              <label className={labelCls} htmlFor="liq-account">
                Account id
              </label>
              <input
                id="liq-account"
                className={inputCls}
                inputMode="numeric"
                value={liqAccount}
                onChange={(e) => setLiqAccount(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="liq-instrument">
                Instrument id (optional)
              </label>
              <input
                id="liq-instrument"
                className={inputCls}
                inputMode="numeric"
                value={liqInstrument}
                onChange={(e) => setLiqInstrument(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="liq-reason">
                Reason
              </label>
              <input
                id="liq-reason"
                className={inputCls}
                value={liqReason}
                onChange={(e) => setLiqReason(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="liq-approver">
                Approver id (distinct admin)
              </label>
              <input
                id="liq-approver"
                className={inputCls}
                inputMode="numeric"
                value={liqApprover}
                onChange={(e) => setLiqApprover(e.target.value)}
              />
            </div>
          </div>
          <label className="mt-2 flex items-center gap-2 text-sm text-neutral-300">
            <input
              type="checkbox"
              className="h-4 w-4"
              checked={liqOverride}
              onChange={(e) => setLiqOverride(e.target.checked)}
            />
            Override auction — FORCE_CASH at mark (skip CALL phase)
          </label>
          <button
            type="button"
            className={`${btnDanger} mt-2`}
            disabled={busy || !liqValid}
            onClick={() => setPending({ kind: 'liquidation' })}
          >
            Force liquidation…
          </button>
        </div>

        {/* Cache warm */}
        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Cache warm
          </h3>
          <p className={hintTextCls}>
            Synchronous warm of the price/session caches (P0+P1 budgets).
          </p>
          <button
            type="button"
            className={btnPrimary}
            disabled={busy}
            onClick={() => setPending({ kind: 'cache-warm' })}
          >
            Run cache warm…
          </button>
        </div>

        {/* FIX session entitlement */}
        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            FIX session entitlement
          </h3>
          <div className="grid grid-cols-2 gap-2">
            <div>
              <label className={labelCls} htmlFor="fix-id">
                Session id
              </label>
              <input
                id="fix-id"
                className={inputCls}
                value={fixId}
                onChange={(e) => setFixId(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="fix-account">
                Account id (empty = clear)
              </label>
              <input
                id="fix-account"
                className={inputCls}
                inputMode="numeric"
                value={fixAccount}
                onChange={(e) => setFixAccount(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="fix-key">
                API key id (empty = clear)
              </label>
              <input
                id="fix-key"
                className={inputCls}
                inputMode="numeric"
                value={fixApiKey}
                onChange={(e) => setFixApiKey(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="fix-instruments">
                Allowed instruments (empty = clear)
              </label>
              <input
                id="fix-instruments"
                className={inputCls}
                value={fixInstruments}
                placeholder="EUR/USD,GBP/USD"
                onChange={(e) => setFixInstruments(e.target.value)}
              />
            </div>
            <div>
              <label className={labelCls} htmlFor="fix-cod">
                Cancel-on-disconnect
              </label>
              <select
                id="fix-cod"
                className={selectCls}
                value={fixCod}
                onChange={(e) => setFixCod(e.target.value as typeof fixCod)}
              >
                <option value="unchanged">Unchanged</option>
                <option value="true">true</option>
                <option value="false">false</option>
              </select>
            </div>
          </div>
          <button
            type="button"
            className={`${btnDanger} mt-2`}
            disabled={busy || !fixValid}
            onClick={() => setPending({ kind: 'fix-session' })}
          >
            Patch session…
          </button>
        </div>
      </div>

      <Modal
        open={pending !== null}
        title={pending !== null ? confirmLabels[pending.kind] : ''}
        onClose={() => setPending(null)}
      >
        {pending !== null && (
          <ConfirmAction
            message={confirmCopy[pending.kind]}
            confirmLabel={confirmLabels[pending.kind]}
            busy={busy}
            danger={pending.kind !== 'cache-warm'}
            onCancel={() => setPending(null)}
            onConfirm={() => void execute()}
          />
        )}
      </Modal>
    </section>
  );
}
