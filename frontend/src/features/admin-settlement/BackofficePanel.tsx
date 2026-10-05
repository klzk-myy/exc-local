/**
 * Backoffice settlement panel (Task 10.5.3.27 gate-coverage wiring) —
 * nostro statement ingest + journal (MT940/MT942/CAMT.053, Task
 * 24.3.12), the SSI register (Task 24.3.9), and suspense routing
 * (Task 24.3.21). List rows expand to normalized entries; every
 * mutation surfaces the service verdict verbatim.
 */
import { skipToken, useMutation, useQuery } from '@tanstack/react-query';
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
} from '@/lib/ui';

import {
  fetchAdminStatements,
  fetchSsis,
  fetchStatementEntries,
  ingestStatement,
  registerSsi,
  resolveSuspense,
  revokeSsi,
  routeSuspense,
  type AdminStatement,
} from './api';

export function BackofficePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [notice, setNotice] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<number | null>(null);
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const statements = useQuery({
    queryKey: ['admin-settlement', 'statements'],
    queryFn: () => fetchAdminStatements(adminApi),
    retry: false,
  });
  const entries = useQuery({
    queryKey: ['admin-settlement', 'stmt-entries', expanded],
    queryFn: expanded === null ? skipToken : () => fetchStatementEntries(adminApi, expanded),
    retry: false,
  });

  const [ingest, setIngest] = useState({ nostro: '', format: 'MT940', content: '' });
  const doIngest = useMutation({
    mutationFn: () =>
      ingestStatement(adminApi, {
        nostroAccountId: Number(ingest.nostro),
        format: ingest.format as 'MT940' | 'MT942' | 'CAMT053',
        content: ingest.content,
      }),
    onSuccess: () => {
      setNotice('Statement ingested');
      setIngest({ ...ingest, content: '' });
      void statements.refetch();
    },
    onError: onErr,
  });

  const [ssiAcct, setSsiAcct] = useState('');
  const ssis = useQuery({
    queryKey: ['admin-settlement', 'ssis', ssiAcct],
    queryFn: () => fetchSsis(adminApi, Number(ssiAcct)),
    enabled: ssiAcct !== '' && Number(ssiAcct) > 0,
    retry: false,
  });
  const [ssiForm, setSsiForm] = useState({ bankAccountId: '', currency: 'USD', ref: '', bic: '' });
  const ssiRegister = useMutation({
    mutationFn: () =>
      registerSsi(adminApi, {
        accountId: Number(ssiAcct),
        bankAccountId: Number(ssiForm.bankAccountId),
        currency: ssiForm.currency,
        ref: ssiForm.ref,
        bic: ssiForm.bic === '' ? undefined : ssiForm.bic,
      }),
    onSuccess: () => {
      setNotice('SSI registered');
      void ssis.refetch();
    },
    onError: onErr,
  });
  const ssiRevoke = useMutation({
    mutationFn: (id: number) => revokeSsi(adminApi, id),
    onSuccess: () => {
      setNotice('SSI revoked');
      void ssis.refetch();
    },
    onError: onErr,
  });

  const [route, setRoute] = useState({
    bankTxId: '',
    rail: 'SWIFT',
    currency: 'USD',
    amount: '',
    originatorName: '',
    originatorAccount: '',
    reference: '',
  });
  const doRoute = useMutation({
    mutationFn: () => routeSuspense(adminApi, route),
    onSuccess: (res) => setNotice(`Suspense routed — ${JSON.stringify(res)}`),
    onError: onErr,
  });
  const [resolveForm, setResolveForm] = useState({
    id: '',
    action: 'RELEASE_TO_CLIENT' as 'RELEASE_TO_CLIENT' | 'RETURN_TO_SOURCE',
    notes: '',
  });
  const doResolve = useMutation({
    mutationFn: () =>
      resolveSuspense(
        adminApi,
        Number(resolveForm.id),
        resolveForm.action,
        resolveForm.notes === '' ? undefined : resolveForm.notes,
      ),
    onSuccess: () => setNotice(`Suspense #${resolveForm.id} ${resolveForm.action}`),
    onError: onErr,
  });

  return (
    <section className={cardCls} aria-label="Backoffice settlement">
      <h2 className="mb-2 text-sm font-semibold">Backoffice — statements, SSI, suspense</h2>
      {notice !== null && (
        <p role="status" className="mb-2 text-xs text-sky-300">
          {notice}
        </p>
      )}

      <div className="grid gap-4 md:grid-cols-2">
        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Nostro statements
          </h3>
          <form
            className="mb-3 grid gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              doIngest.mutate();
            }}
          >
            <label className={labelCls}>
              Nostro account id
              <input
                className={inputCls}
                value={ingest.nostro}
                onChange={(e) => setIngest({ ...ingest, nostro: e.target.value })}
                required
                inputMode="numeric"
              />
            </label>
            <label className={labelCls}>
              Format
              <select
                className={selectCls}
                value={ingest.format}
                onChange={(e) => setIngest({ ...ingest, format: e.target.value })}
              >
                {['MT940', 'MT942', 'CAMT053'].map((f) => (
                  <option key={f} value={f}>
                    {f}
                  </option>
                ))}
              </select>
            </label>
            <label className={labelCls}>
              Statement content
              <textarea
                className={inputCls}
                rows={3}
                value={ingest.content}
                onChange={(e) => setIngest({ ...ingest, content: e.target.value })}
                required
              />
            </label>
            <button type="submit" className={btnPrimary} disabled={doIngest.isPending}>
              Ingest statement
            </button>
          </form>

          {statements.isPending && <p className={hintTextCls}>Loading…</p>}
          {statements.isError && (
            <p role="alert" className="text-xs text-rose-300">
              {statements.error.message}
            </p>
          )}
          <ul className="divide-y divide-neutral-800 text-xs">
            {(statements.data ?? []).map((s: AdminStatement) => (
              <li key={s.id}>
                <button
                  type="button"
                  className="flex w-full items-center justify-between py-1.5 text-left"
                  onClick={() => setExpanded(expanded === s.id ? null : s.id)}
                  aria-expanded={expanded === s.id}
                >
                  <span>
                    #{s.id} nostro {s.nostroAccountId} · {s.format}
                  </span>
                  <span className={hintTextCls}>
                    {s.status} · {s.entryCount} entries
                  </span>
                </button>
                {expanded === s.id && (
                  <div className="mb-2 rounded border border-neutral-800 p-2">
                    {entries.isPending && <p className={hintTextCls}>Loading entries…</p>}
                    {entries.isError && (
                      <p role="alert" className="text-xs text-rose-300">
                        {entries.error.message}
                      </p>
                    )}
                    <ul className="max-h-40 overflow-y-auto text-neutral-300" tabIndex={0}>
                      {(entries.data ?? []).map((en, i) => (
                        <li key={i} className="py-0.5 font-mono">
                          {JSON.stringify(en)}
                        </li>
                      ))}
                      {entries.data?.length === 0 && (
                        <li className={hintTextCls}>No normalized entries.</li>
                      )}
                    </ul>
                  </div>
                )}
              </li>
            ))}
          </ul>
        </div>

        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            SSI register
          </h3>
          <form
            className="mb-2 flex flex-wrap items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              void ssis.refetch();
            }}
          >
            <label className={labelCls}>
              Account id
              <input
                className={inputCls}
                value={ssiAcct}
                onChange={(e) => setSsiAcct(e.target.value)}
                required
                inputMode="numeric"
              />
            </label>
            <button type="submit" className={btnGhost}>
              Load SSIs
            </button>
          </form>
          {ssis.isError && (
            <p role="alert" className="text-xs text-rose-300">
              {ssis.error.message}
            </p>
          )}
          <ul className="mb-3 text-xs">
            {(ssis.data ?? []).map((s) => (
              <li key={s.id} className="flex items-center justify-between py-1">
                <span>
                  #{s.id} {s.currency} {s.ref}
                  {s.bic !== '' ? ` · ${s.bic}` : ''}
                  {s.isDefault ? ' · default' : ''}
                  {s.revoked ? ' · REVOKED' : ''}
                </span>
                {!s.revoked && (
                  <button
                    type="button"
                    className={btnGhost}
                    onClick={() => ssiRevoke.mutate(s.id)}
                    disabled={ssiRevoke.isPending}
                  >
                    Revoke
                  </button>
                )}
              </li>
            ))}
            {ssis.data?.length === 0 && <li className={hintTextCls}>No SSIs for this account.</li>}
          </ul>
          <form
            className="grid gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              ssiRegister.mutate();
            }}
          >
            <label className={labelCls}>
              Bank account id
              <input
                className={inputCls}
                value={ssiForm.bankAccountId}
                onChange={(e) => setSsiForm({ ...ssiForm, bankAccountId: e.target.value })}
                required
                inputMode="numeric"
              />
            </label>
            <label className={labelCls}>
              Currency
              <input
                className={inputCls}
                value={ssiForm.currency}
                onChange={(e) => setSsiForm({ ...ssiForm, currency: e.target.value })}
                required
              />
            </label>
            <label className={labelCls}>
              Nostro/beneficiary ref
              <input
                className={inputCls}
                value={ssiForm.ref}
                onChange={(e) => setSsiForm({ ...ssiForm, ref: e.target.value })}
                required
              />
            </label>
            <label className={labelCls}>
              BIC (optional)
              <input
                className={inputCls}
                value={ssiForm.bic}
                onChange={(e) => setSsiForm({ ...ssiForm, bic: e.target.value })}
              />
            </label>
            <button
              type="submit"
              className={btnPrimary}
              disabled={ssiRegister.isPending || ssiAcct === ''}
            >
              Register SSI
            </button>
          </form>
        </div>
      </div>

      <div className="mt-4 grid gap-4 md:grid-cols-2">
        <form
          className="grid gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            doRoute.mutate();
          }}
        >
          <h3 className="text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Route inbound suspense
          </h3>
          {(
            [
              ['bankTxId', 'Bank tx id'],
              ['rail', 'Rail'],
              ['currency', 'Currency'],
              ['amount', 'Amount'],
              ['originatorName', 'Originator name'],
              ['originatorAccount', 'Originator account'],
              ['reference', 'Reference'],
            ] as const
          ).map(([k, lab]) => (
            <label key={k} className={labelCls}>
              {lab}
              <input
                className={inputCls}
                value={route[k]}
                onChange={(e) => setRoute({ ...route, [k]: e.target.value })}
                required={k !== 'rail'}
              />
            </label>
          ))}
          <button type="submit" className={btnPrimary} disabled={doRoute.isPending}>
            Route suspense
          </button>
        </form>

        <form
          className="grid content-start gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            doResolve.mutate();
          }}
        >
          <h3 className="text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Resolve suspense (four-eyes)
          </h3>
          <label className={labelCls}>
            Suspense id
            <input
              className={inputCls}
              value={resolveForm.id}
              onChange={(e) => setResolveForm({ ...resolveForm, id: e.target.value })}
              required
              inputMode="numeric"
            />
          </label>
          <label className={labelCls}>
            Action
            <select
              className={selectCls}
              value={resolveForm.action}
              onChange={(e) =>
                setResolveForm({
                  ...resolveForm,
                  action: e.target.value as typeof resolveForm.action,
                })
              }
            >
              <option value="RELEASE_TO_CLIENT">Release to client</option>
              <option value="RETURN_TO_SOURCE">Return to source</option>
            </select>
          </label>
          <label className={labelCls}>
            Notes
            <input
              className={inputCls}
              value={resolveForm.notes}
              onChange={(e) => setResolveForm({ ...resolveForm, notes: e.target.value })}
            />
          </label>
          <button type="submit" className={btnPrimary} disabled={doResolve.isPending}>
            Resolve
          </button>
        </form>
      </div>
    </section>
  );
}
