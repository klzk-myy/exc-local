/**
 * Regime reports panel (Phase-10.5 Task 10.5.3.13 §2b) — the canned
 * regime views: EMIR-pinned canonical event export (the legacy
 * /admin/emir-report mount), the MiFID combined RTS27+RTS28 listing,
 * the Basel III report by period, and the generic compliance export
 * (MIFID2|EMIR|FINCEN_CTR|FINCEN_SAR|BASEL3|MONTHLY_SUMMARY).
 */
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import { fetchBaselReport, fetchComplianceReport, fetchRegEvents } from './api';

const EXPORT_TYPES = [
  'MIFID2',
  'EMIR',
  'FINCEN_CTR',
  'FINCEN_SAR',
  'BASEL3',
  'MONTHLY_SUMMARY',
] as const;

export function RegimePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [regime, setRegime] = useState<'EMIR' | 'ALL'>('EMIR');
  const [baselPeriod, setBaselPeriod] = useState('');
  const [baselRun, setBaselRun] = useState(false);
  const [exportForm, setExportForm] = useState({
    type: 'MIFID2' as (typeof EXPORT_TYPES)[number],
    from: '',
    to: '',
  });
  const [exportOut, setExportOut] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const emirEvents = useQuery({
    queryKey: ['admin-emir-events', regime],
    queryFn: () =>
      regime === 'EMIR'
        ? fetchRegEvents(adminApi, { pinned: 'EMIR' })
        : fetchRegEvents(adminApi, {}),
  });
  const basel = useQuery({
    queryKey: ['admin-basel-report', baselPeriod, baselRun],
    queryFn: () => fetchBaselReport(adminApi, baselPeriod === '' ? undefined : baselPeriod),
    enabled: baselRun,
  });
  const exportMut = useMutation({
    mutationFn: () =>
      fetchComplianceReport(adminApi, {
        type: exportForm.type,
        from: exportForm.from === '' ? undefined : exportForm.from,
        to: exportForm.to === '' ? undefined : exportForm.to,
      }),
    onSuccess: (out) => {
      setExportOut(out);
      setNotice(`${exportForm.type} export rendered.`);
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Export failed'),
  });

  if (emirEvents.error !== null && isAccessDenied(emirEvents.error)) {
    return <AccessDeniedCard />;
  }

  return (
    <section className={cardCls} aria-label="Regime reports">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Regime reports</h2>
        <select
          aria-label="Event regime"
          className={selectCls}
          value={regime}
          onChange={(e) => {
            setRegime(e.target.value as typeof regime);
          }}
        >
          <option value="EMIR">EMIR (pinned mount)</option>
          <option value="ALL">All regimes</option>
        </select>
      </div>

      {emirEvents.error !== null ? <ErrorBox error={emirEvents.error} /> : null}
      {emirEvents.data?.length === 0 ? (
        <p className="text-sm text-neutral-500">No reportable events.</p>
      ) : null}
      {emirEvents.data !== undefined && emirEvents.data.length > 0 ? (
        <div className="max-h-48 overflow-y-auto">
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Event</th>
                <th className={thCls}>UTI</th>
                <th className={thCls}>Regime</th>
                <th className={thCls}>Action</th>
                <th className={thCls}>Type</th>
                <th className={thCls}>Seq</th>
                <th className={thCls}>Instrument</th>
              </tr>
            </thead>
            <tbody>
              {emirEvents.data.map((e) => (
                <tr key={e.eventId}>
                  <td className={tdCls}>{e.eventId}</td>
                  <td className={`${tdCls} font-mono text-xs`}>{e.uti}</td>
                  <td className={tdCls}>
                    <StatusBadge value={e.regime || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>{e.actionType}</td>
                  <td className={tdCls}>{e.eventType}</td>
                  <td className={tdCls}>{e.reportSeq}</td>
                  <td className={tdCls}>{e.instrumentCode}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      <div className="mt-4 border-t border-neutral-800 pt-3">
        <h3 className="mb-1 text-sm font-semibold">Basel III report</h3>
        <div className="flex flex-wrap items-end gap-2">
          <input
            aria-label="Basel period"
            className={inputCls}
            placeholder="YYYY-MM or YYYY-MM-DD"
            value={baselPeriod}
            onChange={(e) => {
              setBaselPeriod(e.target.value);
            }}
          />
          <button
            type="button"
            className={btnGhost}
            onClick={() => {
              setBaselRun(true);
            }}
          >
            Load report
          </button>
        </div>
        {basel.error !== null ? <ErrorBox error={basel.error} /> : null}
        {basel.data === null && baselRun && !basel.isLoading ? (
          <p className={hintTextCls}>No Basel report persisted for the period.</p>
        ) : null}
        {basel.data !== null && basel.data !== undefined ? (
          <pre className="mt-2 max-h-48 overflow-auto rounded border border-neutral-800 p-2 font-mono text-xs">
            {JSON.stringify(basel.data.raw, null, 2)}
          </pre>
        ) : null}
      </div>

      <div className="mt-4 border-t border-neutral-800 pt-3">
        <h3 className="mb-1 text-sm font-semibold">Compliance export</h3>
        <form
          aria-label="Compliance export"
          className="flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            exportMut.mutate();
          }}
        >
          <select
            aria-label="Export type"
            className={selectCls}
            value={exportForm.type}
            onChange={(e) => {
              setExportForm({ ...exportForm, type: e.target.value as typeof exportForm.type });
            }}
          >
            {EXPORT_TYPES.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
          <input
            aria-label="Export from"
            className={inputCls}
            type="date"
            value={exportForm.from}
            onChange={(e) => {
              setExportForm({ ...exportForm, from: e.target.value });
            }}
          />
          <input
            aria-label="Export to"
            className={inputCls}
            type="date"
            value={exportForm.to}
            onChange={(e) => {
              setExportForm({ ...exportForm, to: e.target.value });
            }}
          />
          <button type="submit" className={btnGhost} disabled={exportMut.isPending}>
            Export
          </button>
        </form>
        {exportOut !== null ? (
          <pre className="mt-2 max-h-48 overflow-auto rounded border border-neutral-800 p-2 font-mono text-xs">
            {JSON.stringify(exportOut, null, 2)}
          </pre>
        ) : null}
      </div>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}
