/**
 * CLS PvP panel (Phase-10.5 Task 10.5.3.9 §1) — paired-instruction
 * lifecycle. Submit creates the RECEIVED instruction; the ref-driven
 * console applies the member verbs (dispatch, amend, rescind, pay-in,
 * authenticated finality, member-status notification) and renders the
 * returned state honestly — SETTLED only ever arrives via the
 * authenticated /finality leg.
 */
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnDanger,
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
  clsAmend,
  clsDispatch,
  clsFinality,
  clsMemberStatus,
  clsPayIn,
  clsRescind,
  submitClsInstruction,
  type ClsInstruction,
} from './api';

const LIFECYCLE = ['RECEIVED', 'VALIDATED', 'MATCHED', 'ELIGIBLE', 'PAY_IN', 'SETTLED'];
const SIDE_STATES = ['UNMATCHED', 'INELIGIBLE', 'RESCINDED', 'REJECTED', 'EXPIRED'];

export function ClsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [submit, setSubmit] = useState({
    ref: '',
    cpAccountId: '',
    memberBic: '',
    product: 'SPOT',
    buyCcy: 'EUR',
    buyAmt: '',
    sellCcy: 'USD',
    sellAmt: '',
    valueDate: '',
  });
  const [opRef, setOpRef] = useState('');
  const [amend, setAmend] = useState({ buyAmt: '', sellAmt: '', valueDate: '' });
  const [memberRef, setMemberRef] = useState('');
  const [memberTo, setMemberTo] = useState('MATCHED');
  const [instruction, setInstruction] = useState<ClsInstruction | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const show = (r: ClsInstruction) => {
    setInstruction(r);
    setNotice(null);
  };
  const onErr = (e: unknown) => {
    setInstruction(null);
    setNotice(e instanceof Error ? e.message : 'CLS action failed');
  };

  const submitM = useMutation({
    mutationFn: () =>
      submitClsInstruction(adminApi, {
        instructionRef: submit.ref === '' ? undefined : submit.ref,
        counterpartyAccountId: Number(submit.cpAccountId),
        memberBic: submit.memberBic,
        product: submit.product,
        buyCurrency: submit.buyCcy,
        buyAmount: submit.buyAmt,
        sellCurrency: submit.sellCcy,
        sellAmount: submit.sellAmt,
        valueDate: submit.valueDate,
      }),
    onSuccess: (r) => {
      show(r);
      setOpRef(r.instructionRef);
    },
    onError: onErr,
  });
  const verb = useMutation({
    mutationFn: (fn: () => Promise<ClsInstruction>) => fn(),
    onSuccess: show,
    onError: onErr,
  });

  const ref = opRef.trim();
  const busy = submitM.isPending || verb.isPending;

  return (
    <section className={cardCls} aria-label="CLS PvP">
      <h2 className="mb-1 text-sm font-semibold">CLS PvP instructions</h2>
      <div className="mb-2 flex flex-wrap items-center gap-1" aria-label="CLS lifecycle">
        {LIFECYCLE.map((s) => (
          <span
            key={s}
            className={
              instruction !== null && instruction.status === s
                ? 'rounded bg-sky-500/30 px-2 py-0.5 text-xs font-semibold text-sky-200'
                : 'rounded bg-neutral-800 px-2 py-0.5 text-xs text-neutral-400'
            }
          >
            {s}
          </span>
        ))}
        {instruction !== null && SIDE_STATES.includes(instruction.status) ? (
          <StatusBadge value={instruction.status} />
        ) : null}
      </div>

      <form
        aria-label="Submit CLS instruction"
        className="grid grid-cols-2 gap-2 md:grid-cols-5"
        onSubmit={(e) => {
          e.preventDefault();
          if (
            Number(submit.cpAccountId) > 0 &&
            submit.memberBic !== '' &&
            submit.buyAmt !== '' &&
            submit.sellAmt !== '' &&
            submit.valueDate !== ''
          ) {
            submitM.mutate();
          }
        }}
      >
        <input
          aria-label="Instruction ref"
          className={inputCls}
          placeholder="instruction_ref (auto)"
          value={submit.ref}
          onChange={(e) => {
            setSubmit({ ...submit, ref: e.target.value });
          }}
        />
        <input
          aria-label="Counterparty account"
          className={inputCls}
          placeholder="counterparty_account_id"
          value={submit.cpAccountId}
          onChange={(e) => {
            setSubmit({ ...submit, cpAccountId: e.target.value });
          }}
        />
        <input
          aria-label="Member BIC"
          className={inputCls}
          placeholder="member_bic"
          value={submit.memberBic}
          onChange={(e) => {
            setSubmit({ ...submit, memberBic: e.target.value });
          }}
        />
        <select
          aria-label="Product"
          className={selectCls}
          value={submit.product}
          onChange={(e) => {
            setSubmit({ ...submit, product: e.target.value });
          }}
        >
          <option value="SPOT">SPOT</option>
          <option value="FORWARD">FORWARD</option>
          <option value="SWAP">SWAP</option>
          <option value="NDF">NDF</option>
        </select>
        <input
          aria-label="Value date"
          className={inputCls}
          placeholder="value_date YYYY-MM-DD"
          value={submit.valueDate}
          onChange={(e) => {
            setSubmit({ ...submit, valueDate: e.target.value });
          }}
        />
        <input
          aria-label="Buy currency"
          className={inputCls}
          placeholder="buy_ccy"
          value={submit.buyCcy}
          onChange={(e) => {
            setSubmit({ ...submit, buyCcy: e.target.value });
          }}
        />
        <input
          aria-label="Buy amount"
          className={inputCls}
          placeholder="buy_amount"
          value={submit.buyAmt}
          onChange={(e) => {
            setSubmit({ ...submit, buyAmt: e.target.value });
          }}
        />
        <input
          aria-label="Sell currency"
          className={inputCls}
          placeholder="sell_ccy"
          value={submit.sellCcy}
          onChange={(e) => {
            setSubmit({ ...submit, sellCcy: e.target.value });
          }}
        />
        <input
          aria-label="Sell amount"
          className={inputCls}
          placeholder="sell_amount"
          value={submit.sellAmt}
          onChange={(e) => {
            setSubmit({ ...submit, sellAmt: e.target.value });
          }}
        />
        <button type="submit" className={btnPrimary} disabled={busy}>
          Submit paired
        </button>
      </form>

      <div className="mt-3 rounded border border-neutral-700 p-2">
        <div className="flex flex-wrap items-end gap-2">
          <div>
            <label className={labelCls} htmlFor="cls-ref">
              Instruction ref
            </label>
            <input
              id="cls-ref"
              className={inputCls}
              value={opRef}
              onChange={(e) => {
                setOpRef(e.target.value);
              }}
            />
          </div>
          <button
            type="button"
            className={btnPrimary}
            disabled={busy || ref === ''}
            onClick={() => {
              verb.mutate(() => clsDispatch(adminApi, ref));
            }}
          >
            Dispatch
          </button>
          <button
            type="button"
            className={btnGhost}
            disabled={busy || ref === ''}
            onClick={() => {
              verb.mutate(() => clsPayIn(adminApi, ref));
            }}
          >
            Pay-in
          </button>
          <button
            type="button"
            className={btnDanger}
            disabled={busy || ref === ''}
            onClick={() => {
              verb.mutate(() => clsRescind(adminApi, ref, 'ops rescind'));
            }}
          >
            Rescind
          </button>
        </div>
        <div className="mt-2 grid grid-cols-1 gap-2 md:grid-cols-2">
          <form
            aria-label="Amend instruction"
            className="flex items-end gap-1"
            onSubmit={(e) => {
              e.preventDefault();
              if (ref === '') return;
              verb.mutate(() =>
                clsAmend(adminApi, ref, {
                  buyAmount: amend.buyAmt === '' ? undefined : amend.buyAmt,
                  sellAmount: amend.sellAmt === '' ? undefined : amend.sellAmt,
                  valueDate: amend.valueDate === '' ? undefined : amend.valueDate,
                }),
              );
            }}
          >
            <input
              aria-label="Amend buy amount"
              className={inputCls}
              placeholder="buy_amount"
              value={amend.buyAmt}
              onChange={(e) => {
                setAmend({ ...amend, buyAmt: e.target.value });
              }}
            />
            <input
              aria-label="Amend sell amount"
              className={inputCls}
              placeholder="sell_amount"
              value={amend.sellAmt}
              onChange={(e) => {
                setAmend({ ...amend, sellAmt: e.target.value });
              }}
            />
            <input
              aria-label="Amend value date"
              className={inputCls}
              placeholder="value_date"
              value={amend.valueDate}
              onChange={(e) => {
                setAmend({ ...amend, valueDate: e.target.value });
              }}
            />
            <button type="submit" className={btnGhost} disabled={busy || ref === ''}>
              Amend
            </button>
          </form>
          <form
            aria-label="Member notification"
            className="flex items-end gap-1"
            onSubmit={(e) => {
              e.preventDefault();
              if (ref === '' || memberRef === '') return;
              verb.mutate(() =>
                memberTo === 'SETTLED'
                  ? clsFinality(adminApi, ref, memberRef)
                  : clsMemberStatus(adminApi, ref, { to: memberTo, memberRef }),
              );
            }}
          >
            <select
              aria-label="Member status"
              className={selectCls}
              value={memberTo}
              onChange={(e) => {
                setMemberTo(e.target.value);
              }}
            >
              <option value="MATCHED">MATCHED</option>
              <option value="UNMATCHED">UNMATCHED</option>
              <option value="ELIGIBLE">ELIGIBLE</option>
              <option value="INELIGIBLE">INELIGIBLE</option>
              <option value="SETTLED">SETTLED (authenticated finality)</option>
              <option value="REJECTED">REJECTED</option>
              <option value="EXPIRED">EXPIRED</option>
            </select>
            <input
              aria-label="Member ref"
              className={inputCls}
              placeholder="member_ref"
              value={memberRef}
              onChange={(e) => {
                setMemberRef(e.target.value);
              }}
            />
            <button type="submit" className={btnPrimary} disabled={busy || ref === ''}>
              {memberTo === 'SETTLED' ? 'Record finality' : 'Apply status'}
            </button>
          </form>
        </div>
        <p className={hintTextCls}>
          SETTLED posts through /finality with the authenticated flag — the plain status verb
          refuses it (central-bank-money finality only).
        </p>
      </div>

      {instruction !== null ? (
        <div
          aria-label="CLS instruction result"
          className="mt-3 rounded border border-neutral-700 p-2 text-sm"
        >
          <div className="flex flex-wrap items-center gap-2">
            <strong>{instruction.instructionRef}</strong>
            <StatusBadge value={instruction.status || 'UNKNOWN'} />
            <span className={hintTextCls}>
              {instruction.buyAmount} {instruction.buyCurrency} / {instruction.sellAmount}{' '}
              {instruction.sellCurrency} · {instruction.product} · {instruction.memberBic}
            </span>
          </div>
          {instruction.rejectReason !== undefined ? (
            <p className="mt-1 text-xs text-red-300">{instruction.rejectReason}</p>
          ) : null}
          {instruction.memberAckRef !== undefined ? (
            <p className="mt-1 text-xs text-neutral-400">ack {instruction.memberAckRef}</p>
          ) : null}
        </div>
      ) : null}
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
