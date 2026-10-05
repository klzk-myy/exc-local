/**
 * Best-execution production panel (Phase-10.5 Task 10.5.3.13 §2) —
 * the RTS 27 quarterly pipeline (materialize daily stats → generate
 * quarter reports → publish) and the RTS 28 annual report set
 * (generate per instrument class → publish).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  JsonRows,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { isAccessDenied } from '../admin/adminRole';
import { AccessDeniedCard } from '../admin/RequireAdmin';
import {
  fetchReportDetail,
  fetchRTS27,
  fetchRTS28,
  generateRTS27,
  generateRTS28,
  materializeRTS27,
  publishReport,
} from './api';

export function BestExecPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState('');
  const [day, setDay] = useState('');
  const [quarter, setQuarter] = useState('');
  const [year, setYear] = useState('');
  const [notice, setNotice] = useState<string | null>(null);
  const [detail, setDetail] = useState<{ kind: 'rts27' | 'rts28'; id: number } | null>(null);

  const r27 = useQuery({
    queryKey: ['admin-rts27', statusFilter],
    queryFn: () => fetchRTS27(adminApi, statusFilter === '' ? undefined : statusFilter),
  });
  const r28 = useQuery({
    queryKey: ['admin-rts28'],
    queryFn: () => fetchRTS28(adminApi),
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['admin-rts27'] });
    void qc.invalidateQueries({ queryKey: ['admin-rts28'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const mat = useMutation({
    mutationFn: () => materializeRTS27(adminApi, day === '' ? undefined : day),
    onSuccess: () => {
      setNotice('Daily stats materialized (idempotent upsert).');
      invalidate();
    },
    onError: onErr,
  });
  const gen27 = useMutation({
    mutationFn: () => generateRTS27(adminApi, quarter === '' ? undefined : quarter),
    onSuccess: (n) => {
      setNotice(`RTS27 quarter generated — ${n} instrument-class reports filed as DRAFT.`);
      invalidate();
    },
    onError: onErr,
  });
  const gen28 = useMutation({
    mutationFn: () => generateRTS28(adminApi, Number(year)),
    onSuccess: (n) => {
      setNotice(`RTS28 year generated — ${n} instrument-class reports filed as DRAFT.`);
      invalidate();
    },
    onError: onErr,
  });
  const publish = useMutation({
    mutationFn: (v: { kind: 'rts27' | 'rts28'; id: number }) =>
      publishReport(adminApi, v.kind, v.id),
    onSuccess: (_d, v) => {
      setNotice(`${v.kind.toUpperCase()} report #${v.id} published.`);
      invalidate();
    },
    onError: onErr,
  });

  const denied =
    (r27.error !== null && isAccessDenied(r27.error)) ||
    (r28.error !== null && isAccessDenied(r28.error));
  if (denied) return <AccessDeniedCard />;

  return (
    <section className={cardCls} aria-label="Best execution reports">
      <h2 className="mb-2 text-sm font-semibold">Best execution (RTS 27 / RTS 28)</h2>

      <div className="mb-1 flex items-center justify-between gap-2">
        <h3 className="text-sm font-semibold">RTS 27 — quarterly venue report</h3>
        <select
          aria-label="RTS27 status filter"
          className={selectCls}
          value={statusFilter}
          onChange={(e) => {
            setStatusFilter(e.target.value);
          }}
        >
          <option value="">ALL</option>
          <option value="DRAFT">DRAFT</option>
          <option value="PUBLISHED">PUBLISHED</option>
        </select>
      </div>
      {r27.error !== null ? <ErrorBox error={r27.error} /> : null}
      {r27.data !== undefined && r27.data.length > 0 ? (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>ID</th>
              <th className={thCls}>Quarter</th>
              <th className={thCls}>Class</th>
              <th className={thCls}>v</th>
              <th className={thCls}>Days</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>
                <span className="sr-only">Publish</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {r27.data.map((r) => (
              <tr key={r.id}>
                <td className={tdCls}>{r.id}</td>
                <td className={tdCls}>{r.quarterStart.slice(0, 10)}</td>
                <td className={tdCls}>{r.instrumentClass}</td>
                <td className={tdCls}>{r.version}</td>
                <td className={tdCls}>{r.zeroActivity ? '0 (zero-activity)' : r.daysCovered}</td>
                <td className={tdCls}>
                  <StatusBadge value={r.status || 'UNKNOWN'} />
                </td>
                <td className={tdCls}>
                  <span className="flex gap-1">
                    {r.status !== 'PUBLISHED' ? (
                      <button
                        type="button"
                        className={btnPrimary}
                        disabled={publish.isPending}
                        onClick={() => {
                          publish.mutate({ kind: 'rts27', id: r.id });
                        }}
                      >
                        Publish
                      </button>
                    ) : (
                      (r.publishedAt?.slice(0, 10) ?? 'published')
                    )}
                    <button
                      type="button"
                      className={btnGhost}
                      onClick={() => {
                        setDetail(
                          detail?.kind === 'rts27' && detail.id === r.id
                            ? null
                            : { kind: 'rts27', id: r.id },
                        );
                      }}
                    >
                      Detail
                    </button>
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      {detail?.kind === 'rts27' && <ReportDetail adminApi={adminApi} kind="rts27" id={detail.id} />}
      <div className="mt-2 flex flex-wrap items-end gap-2">
        <input
          aria-label="Materialize day"
          className={inputCls}
          type="date"
          value={day}
          onChange={(e) => {
            setDay(e.target.value);
          }}
        />
        <button
          type="button"
          className={btnGhost}
          disabled={mat.isPending}
          onClick={() => {
            mat.mutate();
          }}
        >
          Materialize day
        </button>
        <input
          aria-label="Quarter date"
          className={inputCls}
          type="date"
          value={quarter}
          onChange={(e) => {
            setQuarter(e.target.value);
          }}
        />
        <button
          type="button"
          className={btnPrimary}
          disabled={gen27.isPending}
          onClick={() => {
            gen27.mutate();
          }}
        >
          Generate quarter
        </button>
      </div>

      <div className="mt-4 border-t border-neutral-800 pt-3">
        <h3 className="mb-1 text-sm font-semibold">RTS 28 — annual top-5 report</h3>
        {r28.error !== null ? <ErrorBox error={r28.error} /> : null}
        {r28.data !== undefined && r28.data.length > 0 ? (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>ID</th>
                <th className={thCls}>Year</th>
                <th className={thCls}>Class</th>
                <th className={thCls}>v</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>
                  <span className="sr-only">Publish</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {r28.data.map((r) => (
                <tr key={r.id}>
                  <td className={tdCls}>{r.id}</td>
                  <td className={tdCls}>{r.year}</td>
                  <td className={tdCls}>{r.instrumentClass}</td>
                  <td className={tdCls}>{r.version}</td>
                  <td className={tdCls}>
                    <StatusBadge value={r.status || 'UNKNOWN'} />
                  </td>
                  <td className={tdCls}>
                    <span className="flex gap-1">
                      {r.status !== 'PUBLISHED' ? (
                        <button
                          type="button"
                          className={btnPrimary}
                          disabled={publish.isPending}
                          onClick={() => {
                            publish.mutate({ kind: 'rts28', id: r.id });
                          }}
                        >
                          Publish
                        </button>
                      ) : (
                        (r.publishedAt?.slice(0, 10) ?? 'published')
                      )}
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          setDetail(
                            detail?.kind === 'rts28' && detail.id === r.id
                              ? null
                              : { kind: 'rts28', id: r.id },
                          );
                        }}
                      >
                        Detail
                      </button>
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
        {detail?.kind === 'rts28' && (
          <ReportDetail adminApi={adminApi} kind="rts28" id={detail.id} />
        )}
        <div className="mt-2 flex flex-wrap items-end gap-2">
          <input
            aria-label="RTS28 year"
            className={inputCls}
            placeholder="year"
            value={year}
            onChange={(e) => {
              setYear(e.target.value);
            }}
          />
          <button
            type="button"
            className={btnPrimary}
            disabled={gen28.isPending || Number(year) < 2000}
            onClick={() => {
              gen28.mutate();
            }}
          >
            Generate year
          </button>
        </div>
        <p className={hintTextCls}>
          Generate files one DRAFT per instrument class; publish flips each to PUBLISHED.
        </p>
      </div>
      {notice !== null ? <p className="mt-2 text-sm">{notice}</p> : null}
    </section>
  );
}

function ReportDetail({
  adminApi,
  kind,
  id,
}: {
  adminApi: BoundAdminApi;
  kind: 'rts27' | 'rts28';
  id: number;
}) {
  const q = useQuery({
    queryKey: ['admin-bestexec-detail', kind, id],
    queryFn: () => fetchReportDetail(adminApi, kind, id),
  });
  if (q.error !== null) return <ErrorBox error={q.error} />;
  if (q.data === undefined) return <p className="py-2 text-xs text-neutral-500">Loading…</p>;
  return (
    <div className="my-2 rounded border border-neutral-800 p-2">
      <p className="mb-1 text-xs text-neutral-500">
        {kind.toUpperCase()} report #{id} — raw payload.
      </p>
      <JsonRows rows={[q.data]} />
    </div>
  );
}
