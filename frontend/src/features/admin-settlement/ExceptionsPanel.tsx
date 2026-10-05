/**
 * Settlement-exceptions + confirmations panel (Phase-10.5 Task
 * 10.5.3.9 §2–3) — id-driven exception detail plus the dual-control
 * resolution (RETRY|REVERSE|MANUAL → HTTP 202 PENDING, never rendered
 * as executed), and the manual MT900/910 correspondent-confirmation
 * intake.
 */
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  hintTextCls,
  inputCls,
  labelCls,
  selectCls,
  StatusBadge,
} from '@/lib/ui';

import {
  fetchException,
  ingestConfirmation,
  resolveException,
  type SettlementException,
} from './api';

export function ExceptionsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [excId, setExcId] = useState('');
  const [exception, setException] = useState<SettlementException | null>(null);
  const [resolveForm, setResolveForm] = useState({ action: 'RETRY', notes: '' });
  const [conf, setConf] = useState({
    type: 'MT900',
    reference: '',
    relatedRef: '',
    currency: 'USD',
    amount: '',
    valueDate: '',
    nostroIban: '',
  });
  const [notice, setNotice] = useState<string | null>(null);

  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');
  const load = useMutation({
    mutationFn: () => fetchException(adminApi, Number(excId)),
    onSuccess: (ex) => {
      setException(ex);
      setNotice(null);
    },
    onError: (e) => {
      setException(null);
      onErr(e);
    },
  });
  const resolve = useMutation({
    mutationFn: () =>
      resolveException(adminApi, Number(excId), {
        action: resolveForm.action as 'RETRY' | 'REVERSE' | 'MANUAL',
        notes: resolveForm.notes === '' ? undefined : resolveForm.notes,
      }),
    onSuccess: (r) =>
      setNotice(
        `Resolution ${r.action} queued for four-eyes approval` +
          (r.dualControlId !== undefined ? ` (request #${r.dualControlId})` : '') +
          ` — status ${r.status}. Money does not move until a second approver signs.`,
      ),
    onError: onErr,
  });
  const confirm = useMutation({
    mutationFn: () =>
      ingestConfirmation(adminApi, {
        messageType: conf.type as 'MT900' | 'MT910',
        reference: conf.reference,
        relatedReference: conf.relatedRef === '' ? undefined : conf.relatedRef,
        currency: conf.currency,
        amount: conf.amount,
        valueDate: conf.valueDate === '' ? undefined : conf.valueDate,
        nostroIban: conf.nostroIban === '' ? undefined : conf.nostroIban,
      }),
    onSuccess: () => setNotice('Confirmation recorded and applied.'),
    onError: onErr,
  });

  return (
    <section className={cardCls} aria-label="Settlement exceptions">
      <h2 className="mb-1 text-sm font-semibold">Settlement exceptions &amp; confirmations</h2>

      <form
        aria-label="Load exception"
        className="flex items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(excId) > 0) load.mutate();
        }}
      >
        <div>
          <label className={labelCls} htmlFor="exc-id">
            Exception id
          </label>
          <input
            id="exc-id"
            className={inputCls}
            value={excId}
            onChange={(e) => {
              setExcId(e.target.value);
            }}
          />
        </div>
        <button type="submit" className={btnGhost} disabled={load.isPending}>
          Load
        </button>
      </form>

      {exception !== null ? (
        <div className="mt-2 rounded border border-neutral-700 p-2 text-sm">
          <div className="flex flex-wrap items-center gap-2">
            <strong>#{exception.id}</strong>
            <StatusBadge value={exception.type || 'UNKNOWN'} />
            <StatusBadge value={exception.status || 'UNKNOWN'} />
            <span className={hintTextCls}>
              {exception.amount} {exception.currency} · detected by {exception.detectedBy}
            </span>
          </div>
          {exception.detail !== '' ? (
            <p className="mt-1 text-xs text-neutral-300">{exception.detail}</p>
          ) : null}
          {exception.dualControlId !== undefined ? (
            <p className="mt-1 text-xs text-amber-300">
              dual-control request #{exception.dualControlId} in flight
            </p>
          ) : null}
          {exception.status === 'OPEN' || exception.status === 'INVESTIGATING' ? (
            <form
              aria-label="Resolve exception"
              className="mt-2 flex flex-wrap items-end gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                resolve.mutate();
              }}
            >
              <select
                aria-label="Resolution action"
                className={selectCls}
                value={resolveForm.action}
                onChange={(e) => {
                  setResolveForm({ ...resolveForm, action: e.target.value });
                }}
              >
                <option value="RETRY">RETRY</option>
                <option value="REVERSE">REVERSE</option>
                <option value="MANUAL">MANUAL</option>
              </select>
              <input
                aria-label="Resolution notes"
                className={inputCls}
                placeholder="notes"
                value={resolveForm.notes}
                onChange={(e) => {
                  setResolveForm({ ...resolveForm, notes: e.target.value });
                }}
              />
              <button type="submit" className={btnPrimary} disabled={resolve.isPending}>
                Submit for approval
              </button>
            </form>
          ) : null}
        </div>
      ) : null}

      <h3 className="mb-1 mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-400">
        Correspondent confirmation intake (MT900/MT910)
      </h3>
      <form
        aria-label="Ingest confirmation"
        className="grid grid-cols-2 gap-2 md:grid-cols-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (conf.reference !== '' && conf.amount !== '') confirm.mutate();
        }}
      >
        <select
          aria-label="Message type"
          className={selectCls}
          value={conf.type}
          onChange={(e) => {
            setConf({ ...conf, type: e.target.value });
          }}
        >
          <option value="MT900">MT900 (debit)</option>
          <option value="MT910">MT910 (credit)</option>
        </select>
        <input
          aria-label="Confirmation reference"
          className={inputCls}
          placeholder="reference"
          value={conf.reference}
          onChange={(e) => {
            setConf({ ...conf, reference: e.target.value });
          }}
        />
        <input
          aria-label="Related reference"
          className={inputCls}
          placeholder="related_reference"
          value={conf.relatedRef}
          onChange={(e) => {
            setConf({ ...conf, relatedRef: e.target.value });
          }}
        />
        <input
          aria-label="Confirmation currency"
          className={inputCls}
          placeholder="currency"
          value={conf.currency}
          onChange={(e) => {
            setConf({ ...conf, currency: e.target.value });
          }}
        />
        <input
          aria-label="Confirmation amount"
          className={inputCls}
          placeholder="amount"
          value={conf.amount}
          onChange={(e) => {
            setConf({ ...conf, amount: e.target.value });
          }}
        />
        <input
          aria-label="Confirmation value date"
          className={inputCls}
          placeholder="value_date"
          value={conf.valueDate}
          onChange={(e) => {
            setConf({ ...conf, valueDate: e.target.value });
          }}
        />
        <input
          aria-label="Nostro IBAN"
          className={inputCls}
          placeholder="nostro_iban"
          value={conf.nostroIban}
          onChange={(e) => {
            setConf({ ...conf, nostroIban: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={confirm.isPending}>
          Record &amp; apply
        </button>
      </form>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
