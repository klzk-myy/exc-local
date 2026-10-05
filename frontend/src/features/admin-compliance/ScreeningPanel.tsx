/**
 * Screening panel (Phase-10.5 Task 10.5.3.5 §1) — on-demand
 * sanctions+PEP screen of one account plus manual adverse-media
 * intake. Renders the sanctions provider-gate state and pending queue
 * depth so a deferred (fail-closed) screen reads as QUARANTINED, never
 * as clean.
 */
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  StatusBadge,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { fetchSanctionsStatus, reportAdverseMedia, screenAccount, type ScreenOutcome } from './api';

function outcomeVerdict(o: ScreenOutcome): string {
  if (o.quarantined) return 'QUARANTINED';
  if (o.sanctionHits > 0) return 'SANCTIONS HIT';
  if (o.pepHits > 0 || o.adverseMediaCount > 0) return 'REVIEW';
  return 'CLEAN';
}

export function ScreeningPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [accountId, setAccountId] = useState('');
  const [outcome, setOutcome] = useState<ScreenOutcome | null>(null);
  const [screenErr, setScreenErr] = useState<unknown>(null);

  const status = useQuery({
    queryKey: ['admin-sanctions-status'],
    queryFn: () => fetchSanctionsStatus(adminApi),
  });

  const screen = useMutation({
    mutationFn: () => screenAccount(adminApi, Number(accountId)),
    onSuccess: (o) => {
      setOutcome(o);
      setScreenErr(null);
    },
    onError: (e) => {
      setOutcome(null);
      setScreenErr(e);
    },
  });

  const [am, setAm] = useState({ accountId: '', headline: '', source: '', url: '', severity: '' });
  const [amMsg, setAmMsg] = useState<string | null>(null);
  const media = useMutation({
    mutationFn: () =>
      reportAdverseMedia(adminApi, {
        accountId: Number(am.accountId),
        headline: am.headline,
        source: am.source === '' ? undefined : am.source,
        url: am.url === '' ? undefined : am.url,
        severity: am.severity === '' ? undefined : am.severity,
      }),
    onSuccess: () => setAmMsg('Adverse media recorded.'),
    onError: (e) => setAmMsg(e instanceof Error ? e.message : 'Intake failed'),
  });

  if (status.error !== null && isAccessDenied(status.error)) {
    return <AccessDeniedCard />;
  }

  const gate = status.data?.['provider_gate'];
  const gateLabel =
    typeof gate === 'object' && gate !== null && 'state' in gate
      ? String((gate as Record<string, unknown>)['state'])
      : typeof gate === 'string'
        ? gate
        : undefined;
  const pendingScreens = status.data?.['pending_screens'];
  const listEntries = status.data?.['entries'];

  return (
    <section className={cardCls} aria-label="Screening">
      <h2 className="mb-2 text-sm font-semibold">Screening</h2>

      {status.data !== undefined ? (
        <div className="mb-3 flex flex-wrap items-center gap-2 text-sm">
          <span className={hintTextCls}>Provider state</span>
          {gateLabel !== undefined ? <StatusBadge value={gateLabel} /> : null}
          {typeof pendingScreens === 'number' ? (
            <span className={hintTextCls}>pending screens {pendingScreens}</span>
          ) : null}
          {typeof listEntries === 'number' ? (
            <span className={hintTextCls}>list entries {listEntries}</span>
          ) : null}
        </div>
      ) : null}
      {status.error !== null ? <ErrorBox error={status.error} /> : null}

      <form
        aria-label="Account screen"
        className="mb-4 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(accountId) > 0) screen.mutate();
        }}
      >
        <div>
          <label className={labelCls} htmlFor="scr-acct">
            Account id
          </label>
          <input
            id="scr-acct"
            className={inputCls}
            value={accountId}
            onChange={(e) => {
              setAccountId(e.target.value);
            }}
          />
        </div>
        <button type="submit" className={btnPrimary} disabled={screen.isPending}>
          Screen account
        </button>
      </form>
      {screenErr !== null ? <ErrorBox error={screenErr} /> : null}
      {outcome !== null ? (
        <div
          aria-label="Screen outcome"
          className="mb-4 rounded border border-neutral-700 p-3 text-sm"
        >
          <div className="mb-2 flex items-center gap-2">
            <StatusBadge value={outcomeVerdict(outcome)} />
            <span className={hintTextCls}>screened {outcome.screenedAt}</span>
          </div>
          <dl className="grid grid-cols-2 gap-x-6 gap-y-1 md:grid-cols-4">
            <div>
              <dt className={hintTextCls}>Sanctions hits</dt>
              <dd>{outcome.sanctionHits}</dd>
            </div>
            <div>
              <dt className={hintTextCls}>PEP hits</dt>
              <dd>{outcome.pepHits}</dd>
            </div>
            <div>
              <dt className={hintTextCls}>Adverse media</dt>
              <dd>{outcome.adverseMediaCount}</dd>
            </div>
            <div>
              <dt className={hintTextCls}>Hold</dt>
              <dd>{outcome.holdId ?? '—'}</dd>
            </div>
          </dl>
        </div>
      ) : null}

      <form
        aria-label="Adverse media intake"
        className="space-y-2 border-t border-neutral-800 pt-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (Number(am.accountId) > 0 && am.headline !== '') media.mutate();
        }}
      >
        <p className={hintTextCls}>Manual adverse-media intake</p>
        <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor="am-acct">
              Account id
            </label>
            <input
              id="am-acct"
              className={inputCls}
              value={am.accountId}
              onChange={(e) => {
                setAm({ ...am, accountId: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="am-headline">
              Headline
            </label>
            <input
              id="am-headline"
              className={inputCls}
              value={am.headline}
              onChange={(e) => {
                setAm({ ...am, headline: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="am-source">
              Source
            </label>
            <input
              id="am-source"
              className={inputCls}
              value={am.source}
              onChange={(e) => {
                setAm({ ...am, source: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="am-url">
              URL
            </label>
            <input
              id="am-url"
              className={inputCls}
              value={am.url}
              onChange={(e) => {
                setAm({ ...am, url: e.target.value });
              }}
            />
          </div>
          <div>
            <label className={labelCls} htmlFor="am-sev">
              Severity
            </label>
            <input
              id="am-sev"
              className={inputCls}
              value={am.severity}
              onChange={(e) => {
                setAm({ ...am, severity: e.target.value });
              }}
            />
          </div>
        </div>
        <button type="submit" className={btnPrimary} disabled={media.isPending}>
          Record adverse media
        </button>
        {amMsg !== null ? <p className="text-sm">{amMsg}</p> : null}
      </form>
    </section>
  );
}
