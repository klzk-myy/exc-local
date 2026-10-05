/**
 * Report download center (Task 10.3.28 item 1).
 *
 *   LIVE   tax report     — GET /api/v1/account/tax-report?year&method&format (canonical; supersedes /tax/report)
 *                           (csv/pdf via Accept negotiation).
 *   STUBS  statements     — GET /api/v1/account/statements   (Phase-20)
 *          income         — GET /api/v1/account/income       (Phase-20)
 *          snapshots       — GET /api/v1/account/snapshots    (Phase-20)
 *          confirmations   — GET /api/v1/account/confirmations/{trade_id}
 *          TCA             — GET /api/v1/reports/tca/{account_id} (Phase-23)
 *
 * Binary responses bypass ApiClient's JSON path — `downloadFile`
 * streams the blob and maps an RFC 7807 error body to ApiError, which
 * renders as an explicit "not available" note on failure.
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { ApiError } from '@/lib/api';
import { apiClient } from '@/app/runtime';
import { listStatements } from '../funding/api';
import { useSessionStore } from '@/lib/auth/session';
import { btnPrimary, cardCls, inputCls, labelCls, selectCls } from '@/lib/ui';
import { downloadFile, isNotImplemented, saveBlob, useValidatedField } from '@/lib/input-helpers';
import { TAX_METHODS, type FieldRule } from '@/lib/input-helpers/validation';
import { useAccountScope, MASTER_ACCOUNT_KEY, scopeAccountId } from '@/lib/trading/accountScope';

type Status =
  | { kind: 'idle' }
  | { kind: 'busy' }
  | { kind: 'error'; err: unknown }
  | { kind: 'done'; filename: string };

function DownloadCard({
  title,
  desc,
  children,
  action,
  status,
  onGo,
}: {
  title: string;
  desc: string;
  children?: React.ReactNode;
  action: string;
  status: Status;
  onGo: () => void;
}) {
  return (
    <div className={cardCls}>
      <h2 className="text-sm font-semibold text-neutral-100">{title}</h2>
      <p className="mt-1 text-xs text-neutral-500">{desc}</p>
      {children}
      <div className="mt-3 flex items-center gap-2">
        <button
          type="button"
          className={btnPrimary}
          disabled={status.kind === 'busy'}
          onClick={onGo}
        >
          {status.kind === 'busy' ? 'Preparing…' : action}
        </button>
        {status.kind === 'done' ? (
          <span className="text-xs text-emerald-400">Downloaded {status.filename}</span>
        ) : null}
      </div>
      {status.kind === 'error' ? (
        isNotImplemented(status.err) ? (
          <p className="mt-2 text-xs text-amber-400">
            This report returned 501 NOT_IMPLEMENTED (route expected live — regression signal).
          </p>
        ) : status.err instanceof ApiError ? (
          <p className="mt-2 text-xs text-red-400">
            {status.err.code}: {status.err.message}
          </p>
        ) : null
      ) : null}
    </div>
  );
}

const YEAR_RULE: FieldRule = {
  name: 'year',
  kind: 'integer',
  required: true,
  minInt: 2000,
  maxInt: 2100,
};

export function DownloadCenter() {
  const scopeKey = useAccountScope((s) => s.scopeKey);
  const [statuses, setStatuses] = useState<Record<string, Status>>({});
  const year = useValidatedField(YEAR_RULE, String(new Date().getUTCFullYear() - 1));
  const tradeId = useValidatedField({ name: 'trade_id', kind: 'string' });
  const [method, setMethod] = useState<string>('FIFO');
  const [format, setFormat] = useState<'csv' | 'pdf'>('csv');

  const run = (
    key: string,
    fn: () => Promise<{ blob: Blob; filename: string; contentType: string }>,
  ) => {
    setStatuses((st) => ({ ...st, [key]: { kind: 'busy' } }));
    fn()
      .then((r) => {
        saveBlob(r);
        setStatuses((st) => ({ ...st, [key]: { kind: 'done', filename: r.filename } }));
      })
      .catch((err: unknown) => {
        setStatuses((st) => ({ ...st, [key]: { kind: 'error', err } }));
      });
  };
  const st = (k: string): Status => statuses[k] ?? { kind: 'idle' };

  return (
    <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
      <DownloadCard
        title="Tax report"
        desc="Per-lot disposal report (FIFO/LIFO/HIFO/AVG_COST) — live endpoint; downloads CSV or PDF."
        action={`Download ${format.toUpperCase()}`}
        status={st('tax')}
        onGo={() => {
          if (!year.valid) return;
          run('tax', () =>
            downloadFile('/account/tax-report', `tax-report-${year.value}.${format}`, {
              query: { year: year.value, method, format },
              accept: format === 'csv' ? 'text/csv' : 'application/pdf',
            }),
          );
        }}
      >
        <div className="mt-3 grid grid-cols-3 gap-2">
          <div>
            <label className={labelCls} htmlFor="tax-year">
              Year
            </label>
            <input id="tax-year" className={inputCls} inputMode="numeric" {...year.inputProps} />
          </div>
          <div>
            <label className={labelCls} htmlFor="tax-method">
              Method
            </label>
            <select
              id="tax-method"
              className={selectCls}
              value={method}
              onChange={(e) => setMethod(e.target.value)}
            >
              {TAX_METHODS.map((m) => (
                <option key={m}>{m}</option>
              ))}
            </select>
          </div>
          <div>
            <label className={labelCls} htmlFor="tax-format">
              Format
            </label>
            <select
              id="tax-format"
              className={selectCls}
              value={format}
              onChange={(e) => setFormat(e.target.value as 'csv' | 'pdf')}
            >
              <option value="csv">CSV</option>
              <option value="pdf">PDF</option>
            </select>
          </div>
        </div>
        {year.error ? <p className="mt-1 text-xs text-red-400">{year.error}</p> : null}
      </DownloadCard>

      <StatementRegistry
        onDownload={(id) =>
          run(`stmt-${id}`, () =>
            // Backend selects the artifact by ?format= (Accept is not
            // negotiated — handlers_statements.go defaults to pdf).
            downloadFile(`/account/statements/${id}/download?format=csv`, `statement-${id}.csv`, {
              accept: 'text/csv',
            }),
          )
        }
        statusFor={(id) => st(`stmt-${id}`)}
      />

      <DownloadCard
        title="Trade confirmation"
        desc="Per-trade MiFID II confirmation (Phase-20) — enter a trade id; endpoint live."
        action="Download confirmation"
        status={st('conf')}
        onGo={() => {
          if (!tradeId.validateNow() || tradeId.value === '') return;
          run('conf', () =>
            downloadFile(
              `/account/confirmations/${encodeURIComponent(tradeId.value)}`,
              `confirmation-${tradeId.value}.pdf`,
              { accept: 'application/pdf' },
            ),
          );
        }}
      >
        <div className="mt-3">
          <label className={labelCls} htmlFor="conf-trade-id">
            Trade id
          </label>
          <input
            id="conf-trade-id"
            className={inputCls}
            {...tradeId.inputProps}
            placeholder="e.g. 104200"
          />
        </div>
      </DownloadCard>

      <DownloadCard
        title="Income report"
        desc="Funding/fee income ledger export (Phase-20) — endpoint live."
        action="Download income"
        status={st('income')}
        onGo={() => {
          run('income', () =>
            downloadFile('/account/income', 'income.csv', { accept: 'text/csv' }),
          );
        }}
      />

      <DownloadCard
        title="Account snapshot"
        desc="Point-in-time balances & positions snapshot (Phase-20) — endpoint live."
        action="Download snapshot"
        status={st('snap')}
        onGo={() => {
          run('snap', () => downloadFile('/account/snapshots', 'snapshot.json'));
        }}
      />

      <DownloadCard
        title="TCA report"
        desc="Transaction-cost analysis per account (Phase-23) — endpoint live."
        action="Download TCA"
        status={st('tca')}
        onGo={() => {
          // TCA is claims-account-scoped: the master scope resolves to
          // the session account id; a sub-account key would be a foreign
          // id the backend rejects — request the claims account instead.
          const claimsId = useSessionStore.getState().user?.accountId ?? 0;
          const id = scopeKey === MASTER_ACCOUNT_KEY ? claimsId : scopeAccountId(scopeKey);
          run('tca', () =>
            downloadFile(`/reports/tca/${encodeURIComponent(String(id))}`, 'tca-report.csv', {
              accept: 'text/csv',
            }),
          );
        }}
      />
    </div>
  );
}

/**
 * Statement registry (Task 10.5.3.27 gate-coverage wiring,
 * Phase-20 Task 20.3.6) — GET /account/statements returns the JSON
 * registry; each row downloads its CSV artifact via /{id}/download.
 */
function StatementRegistry({
  onDownload,
  statusFor,
}: {
  onDownload: (id: number) => void;
  statusFor: (id: number) => Status;
}) {
  const q = useQuery({
    queryKey: ['account', 'statements'],
    queryFn: () => listStatements(apiClient),
    retry: false,
  });
  return (
    <div className={cardCls}>
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-semibold text-neutral-100">Account statements</h2>
        <button
          type="button"
          className="text-xs text-sky-300 hover:underline"
          onClick={() => void q.refetch()}
        >
          Refresh
        </button>
      </div>
      <p className="mt-1 text-xs text-neutral-500">
        Statement registry (Phase-20 Task 20.3.6) — pick a period; the CSV artifact lives at /
        {'{id}'}/download.
      </p>
      {q.isError ? (
        <p role="alert" className="mt-2 text-xs text-red-400">
          {q.error instanceof Error ? q.error.message : 'Failed to load statements'}
        </p>
      ) : null}
      <ul className="mt-2 space-y-1 text-xs">
        {(q.data ?? []).map((s) => {
          const stt = statusFor(s.id);
          return (
            <li
              key={s.id}
              className="flex items-center justify-between rounded border border-neutral-800 px-2 py-1"
            >
              <span className="text-neutral-300">
                {s.statementType !== undefined ? `${s.statementType} · ` : ''}
                {s.period ?? ''} {s.periodStart ?? '?'} → {s.periodEnd ?? '?'}
                {s.generatedAt !== undefined ? (
                  <span className="ml-2 text-neutral-500">gen {s.generatedAt}</span>
                ) : null}
              </span>
              <span className="flex items-center gap-2">
                {stt.kind === 'done' ? (
                  <span className="text-emerald-300">{stt.filename}</span>
                ) : null}
                {stt.kind === 'error' ? (
                  <span className="text-red-400">
                    {stt.err instanceof ApiError ? stt.err.message : 'Download failed'}
                  </span>
                ) : null}
                <button
                  type="button"
                  className={btnPrimary}
                  disabled={stt.kind === 'busy'}
                  onClick={() => onDownload(s.id)}
                >
                  {stt.kind === 'busy' ? 'Preparing…' : 'CSV'}
                </button>
              </span>
            </li>
          );
        })}
        {q.data?.length === 0 ? (
          <li className="text-neutral-500">No statements generated yet.</li>
        ) : null}
      </ul>
    </div>
  );
}
