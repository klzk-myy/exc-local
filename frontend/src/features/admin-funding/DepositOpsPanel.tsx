/**
 * Deposit-ops panel (Phase-10.5 Task 10.5.3.7 §1) — detected-deposit
 * ingest, second-source confirmation, the four-eyes PENDING_REVIEW
 * decision, inbound-wire registration (HTTP 202 = quarantined, not an
 * error) and rail returns. Every mutation returns a DepositResult —
 * the panel renders the returned review tier/flags verbatim against
 * the canonical tier ladder (<$10K auto / $10K–50K / >$50K 4-eyes).
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
  StatusBadge,
} from '@/lib/ui';

import {
  applyRailReturn,
  confirmDeposit,
  ingestDeposit,
  registerInboundWire,
  reviewDeposit,
  type DepositResult,
} from './api';

export function TierLegend() {
  return (
    <p className={hintTextCls}>
      Review tiers: &lt;$10K auto · $10K–$50K standard · &gt;$50K PENDING_REVIEW (4-eyes +4h)
    </p>
  );
}

function ResultCard({ result }: { result: DepositResult }) {
  return (
    <div aria-label="Deposit result" className="rounded border border-neutral-700 p-2 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        <span>#{result.depositId}</span>
        <StatusBadge value={result.status || 'UNKNOWN'} />
        {result.reviewTier !== undefined ? <StatusBadge value={result.reviewTier} /> : null}
        <span className={hintTextCls}>
          {result.amount} {result.currency}
          {result.usdAmount !== undefined ? ` (~$${result.usdAmount})` : ''} · confirmations{' '}
          {result.confirmations}
        </span>
      </div>
      {result.flags.length > 0 ? (
        <p className="mt-1 text-xs text-amber-300">flags: {result.flags.join(', ')}</p>
      ) : null}
    </div>
  );
}

export function DepositOpsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [ingest, setIngest] = useState({
    accountId: '',
    currency: 'USD',
    amount: '',
    reference: '',
    source: 'STATEMENT',
    originatorName: '',
    originatorAccount: '',
  });
  const [confirm, setConfirm] = useState({ id: '', source: '', senderName: '' });
  const [review, setReview] = useState({ id: '', action: 'APPROVE', approverId: '', note: '' });
  const [wire, setWire] = useState({
    bankTxId: '',
    rail: 'SWIFT',
    currency: 'USD',
    amount: '',
    originatorName: '',
    originatorAccount: '',
    reference: '',
  });
  const [ret, setRet] = useState({ endToEndId: '', returnCode: '', reason: '' });

  const [result, setResult] = useState<DepositResult | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const ingestM = useMutation({
    mutationFn: () =>
      ingestDeposit(adminApi, {
        accountId: Number(ingest.accountId),
        currency: ingest.currency,
        amount: ingest.amount,
        reference: ingest.reference,
        source: ingest.source,
        originatorName: ingest.originatorName === '' ? undefined : ingest.originatorName,
        originatorAccount: ingest.originatorAccount === '' ? undefined : ingest.originatorAccount,
      }),
    onSuccess: (r) => {
      setResult(r);
      setNotice(null);
    },
    onError: onErr,
  });

  const confirmM = useMutation({
    mutationFn: () =>
      confirmDeposit(adminApi, Number(confirm.id), {
        source: confirm.source,
        senderName: confirm.senderName === '' ? undefined : confirm.senderName,
      }),
    onSuccess: (r) => {
      setResult(r);
      setNotice(null);
    },
    onError: onErr,
  });

  const reviewM = useMutation({
    mutationFn: () =>
      reviewDeposit(adminApi, Number(review.id), {
        action: review.action as 'APPROVE' | 'REJECT',
        approverId: Number(review.approverId),
        note: review.note === '' ? undefined : review.note,
      }),
    onSuccess: (r) => {
      setResult(r);
      setNotice(null);
    },
    onError: onErr,
  });

  const wireM = useMutation({
    mutationFn: () =>
      registerInboundWire(adminApi, {
        bankTxId: wire.bankTxId,
        rail: wire.rail,
        currency: wire.currency,
        amount: wire.amount,
        originatorName: wire.originatorName,
        originatorAccount: wire.originatorAccount,
        reference: wire.reference === '' ? undefined : wire.reference,
      }),
    onSuccess: (r) =>
      setNotice(
        r.disposition === 'QUARANTINED'
          ? `Wire quarantined (suspense #${r.suspenseId ?? '?'}) — held, not rejected: ${r.reason ?? ''}`
          : `Wire accepted → deposit #${r.depositId ?? '?'}`,
      ),
    onError: onErr,
  });

  const retM = useMutation({
    mutationFn: () => applyRailReturn(adminApi, ret),
    onSuccess: (r) =>
      setNotice(
        r.quarantined
          ? 'Return applied → quarantined for review.'
          : `Return applied → funding status ${r.fundingStatus ?? 'recorded'}.`,
      ),
    onError: onErr,
  });

  return (
    <section className={cardCls} aria-label="Deposit operations">
      <h2 className="mb-1 text-sm font-semibold">Deposit operations</h2>
      <TierLegend />

      <div className="mt-3 grid grid-cols-1 gap-4 lg:grid-cols-2">
        <form
          aria-label="Ingest detected deposit"
          className="space-y-1"
          onSubmit={(e) => {
            e.preventDefault();
            if (Number(ingest.accountId) > 0 && ingest.amount !== '' && ingest.reference !== '') {
              ingestM.mutate();
            }
          }}
        >
          <p className={hintTextCls}>Ingest detected deposit</p>
          <div className="grid grid-cols-2 gap-1">
            <input
              aria-label="Account id"
              className={inputCls}
              placeholder="account_id"
              value={ingest.accountId}
              onChange={(e) => {
                setIngest({ ...ingest, accountId: e.target.value });
              }}
            />
            <input
              aria-label="Currency"
              className={inputCls}
              value={ingest.currency}
              onChange={(e) => {
                setIngest({ ...ingest, currency: e.target.value });
              }}
            />
            <input
              aria-label="Amount"
              className={inputCls}
              placeholder="amount"
              value={ingest.amount}
              onChange={(e) => {
                setIngest({ ...ingest, amount: e.target.value });
              }}
            />
            <input
              aria-label="Reference"
              className={inputCls}
              placeholder="bank reference"
              value={ingest.reference}
              onChange={(e) => {
                setIngest({ ...ingest, reference: e.target.value });
              }}
            />
            <input
              aria-label="Detection source"
              className={inputCls}
              placeholder="source (STATEMENT/WEBHOOK)"
              value={ingest.source}
              onChange={(e) => {
                setIngest({ ...ingest, source: e.target.value });
              }}
            />
            <input
              aria-label="Originator name"
              className={inputCls}
              placeholder="originator name"
              value={ingest.originatorName}
              onChange={(e) => {
                setIngest({ ...ingest, originatorName: e.target.value });
              }}
            />
          </div>
          <button type="submit" className={btnPrimary} disabled={ingestM.isPending}>
            Ingest
          </button>
        </form>

        <div className="space-y-3">
          <form
            aria-label="Confirm deposit"
            className="space-y-1"
            onSubmit={(e) => {
              e.preventDefault();
              if (Number(confirm.id) > 0 && confirm.source !== '') confirmM.mutate();
            }}
          >
            <p className={hintTextCls}>Second-source confirm</p>
            <div className="flex gap-1">
              <input
                aria-label="Deposit id"
                className={inputCls}
                placeholder="deposit id"
                value={confirm.id}
                onChange={(e) => {
                  setConfirm({ ...confirm, id: e.target.value });
                }}
              />
              <input
                aria-label="Confirm source"
                className={inputCls}
                placeholder="source"
                value={confirm.source}
                onChange={(e) => {
                  setConfirm({ ...confirm, source: e.target.value });
                }}
              />
              <input
                aria-label="Sender name"
                className={inputCls}
                placeholder="sender name"
                value={confirm.senderName}
                onChange={(e) => {
                  setConfirm({ ...confirm, senderName: e.target.value });
                }}
              />
            </div>
            <button type="submit" className={btnGhost} disabled={confirmM.isPending}>
              Confirm
            </button>
          </form>

          <form
            aria-label="Review deposit"
            className="space-y-1 border-t border-neutral-800 pt-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (Number(review.id) > 0 && Number(review.approverId) > 0) reviewM.mutate();
            }}
          >
            <p className={hintTextCls}>PENDING_REVIEW decision (four-eyes — approver ≠ you)</p>
            <div className="flex gap-1">
              <input
                aria-label="Review deposit id"
                className={inputCls}
                placeholder="deposit id"
                value={review.id}
                onChange={(e) => {
                  setReview({ ...review, id: e.target.value });
                }}
              />
              <select
                aria-label="Review action"
                className={inputCls}
                value={review.action}
                onChange={(e) => {
                  setReview({ ...review, action: e.target.value });
                }}
              >
                <option value="APPROVE">APPROVE</option>
                <option value="REJECT">REJECT</option>
              </select>
              <input
                aria-label="Approver id"
                className={inputCls}
                placeholder="approver_id"
                value={review.approverId}
                onChange={(e) => {
                  setReview({ ...review, approverId: e.target.value });
                }}
              />
            </div>
            <input
              aria-label="Review note"
              className={inputCls}
              placeholder="note"
              value={review.note}
              onChange={(e) => {
                setReview({ ...review, note: e.target.value });
              }}
            />
            <button type="submit" className={btnDanger} disabled={reviewM.isPending}>
              Submit review
            </button>
          </form>
        </div>
      </div>

      {result !== null ? (
        <div className="mt-3">
          <ResultCard result={result} />
        </div>
      ) : null}

      <div className="mt-4 grid grid-cols-1 gap-4 border-t border-neutral-800 pt-3 lg:grid-cols-2">
        <form
          aria-label="Register inbound wire"
          className="space-y-1"
          onSubmit={(e) => {
            e.preventDefault();
            if (wire.bankTxId !== '' && wire.amount !== '') wireM.mutate();
          }}
        >
          <p className={hintTextCls}>Inbound wire (DepositGuard screen; 202 = quarantined hold)</p>
          <div className="grid grid-cols-2 gap-1">
            <input
              aria-label="Bank tx id"
              className={inputCls}
              placeholder="bank_tx_id"
              value={wire.bankTxId}
              onChange={(e) => {
                setWire({ ...wire, bankTxId: e.target.value });
              }}
            />
            <input
              aria-label="Rail"
              className={inputCls}
              value={wire.rail}
              onChange={(e) => {
                setWire({ ...wire, rail: e.target.value });
              }}
            />
            <input
              aria-label="Wire currency"
              className={inputCls}
              value={wire.currency}
              onChange={(e) => {
                setWire({ ...wire, currency: e.target.value });
              }}
            />
            <input
              aria-label="Wire amount"
              className={inputCls}
              placeholder="amount"
              value={wire.amount}
              onChange={(e) => {
                setWire({ ...wire, amount: e.target.value });
              }}
            />
            <input
              aria-label="Wire originator name"
              className={inputCls}
              placeholder="originator name"
              value={wire.originatorName}
              onChange={(e) => {
                setWire({ ...wire, originatorName: e.target.value });
              }}
            />
            <input
              aria-label="Wire originator account"
              className={inputCls}
              placeholder="originator account"
              value={wire.originatorAccount}
              onChange={(e) => {
                setWire({ ...wire, originatorAccount: e.target.value });
              }}
            />
          </div>
          <button type="submit" className={btnPrimary} disabled={wireM.isPending}>
            Register wire
          </button>
        </form>

        <form
          aria-label="Apply rail return"
          className="space-y-1"
          onSubmit={(e) => {
            e.preventDefault();
            if (ret.endToEndId !== '' && ret.returnCode !== '') retM.mutate();
          }}
        >
          <p className={hintTextCls}>
            Rail return (pacs.004/R-code; unknown codes quarantine + P1)
          </p>
          <div className="grid grid-cols-2 gap-1">
            <input
              aria-label="End to end id"
              className={inputCls}
              placeholder="end_to_end_id"
              value={ret.endToEndId}
              onChange={(e) => {
                setRet({ ...ret, endToEndId: e.target.value });
              }}
            />
            <input
              aria-label="Return code"
              className={inputCls}
              placeholder="return_code"
              value={ret.returnCode}
              onChange={(e) => {
                setRet({ ...ret, returnCode: e.target.value });
              }}
            />
          </div>
          <input
            aria-label="Return reason"
            className={inputCls}
            placeholder="reason (optional)"
            value={ret.reason}
            onChange={(e) => {
              setRet({ ...ret, reason: e.target.value });
            }}
          />
          <button type="submit" className={btnDanger} disabled={retM.isPending}>
            Apply return
          </button>
        </form>
      </div>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
