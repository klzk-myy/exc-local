/**
 * Records/archive + DLQ panel (Phase-10.5 Task 10.5.3.3 §3–4):
 * RTS 6 order-lifecycle export, per-shard WAL archive index status,
 * and the JetStream dead-letter queue. The DLQ endpoint answers 503
 * SERVICE_DEGRADED when the ops-dlq stream is unprovisioned — rendered
 * via ErrorBox as an honest degraded state, never an empty list.
 */
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  labelCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import {
  exportOrderRecord,
  fetchArchiveStatus,
  fetchDlq,
  renderScalar,
  type ArchiveStatus,
  type OrderLifecycleExport,
} from './api';

function OrderExport({ adminApi }: { adminApi: BoundAdminApi }) {
  const [orderId, setOrderId] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [result, setResult] = useState<OrderLifecycleExport | null>(null);

  const run = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      setResult(await exportOrderRecord(adminApi, Number(orderId)));
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
        Order-record export (RTS 6 Art. 17)
      </h3>
      <div className="flex items-end gap-2">
        <div>
          <label className={labelCls} htmlFor="or-id">
            Order id
          </label>
          <input
            id="or-id"
            className={inputCls}
            inputMode="numeric"
            value={orderId}
            onChange={(e) => setOrderId(e.target.value)}
          />
        </div>
        <button
          type="button"
          className={btnPrimary}
          disabled={busy || Number(orderId) <= 0}
          onClick={() => void run()}
        >
          Export lifecycle
        </button>
      </div>
      {error !== null && <ErrorBox error={error} />}
      {result !== null && (
        <div className="mt-2" data-testid="order-lifecycle">
          <p className="text-xs text-neutral-400">
            {result.lifecycle.length} lifecycle events — {result.retention}
          </p>
          <ul className="mt-1 max-h-40 space-y-1 overflow-y-auto font-mono text-xs text-neutral-400">
            {result.lifecycle.map((row, i) => (
              <li key={i} className="break-all">
                {renderScalar(row)}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

function ArchiveStatusView({ adminApi }: { adminApi: BoundAdminApi }) {
  const [shard, setShard] = useState('0');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [result, setResult] = useState<ArchiveStatus | null>(null);

  const run = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      setResult(await fetchArchiveStatus(adminApi, Number(shard)));
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
        WAL archive status
      </h3>
      <div className="flex items-end gap-2">
        <div>
          <label className={labelCls} htmlFor="arc-shard">
            Shard
          </label>
          <input
            id="arc-shard"
            className={inputCls}
            inputMode="numeric"
            value={shard}
            onChange={(e) => setShard(e.target.value)}
          />
        </div>
        <button
          type="button"
          className={btnPrimary}
          disabled={busy || shard === ''}
          onClick={() => void run()}
        >
          Check archive
        </button>
      </div>
      {error !== null && <ErrorBox error={error} />}
      {result !== null && (
        <div className="mt-2 text-sm" data-testid="archive-status">
          <p className="text-neutral-300">
            {result.segmentCount} segments · {(result.totalBytes / 1024 / 1024).toFixed(1)} MiB
            {result.missingSegments > 0 && (
              <span className="text-red-400"> · {result.missingSegments} MISSING</span>
            )}
          </p>
          <ul className="mt-1 max-h-32 space-y-0.5 overflow-y-auto font-mono text-xs text-neutral-400">
            {result.segments.map((s) => (
              <li key={s.name} className={s.present ? '' : 'text-red-400'}>
                {s.name} — {(s.sizeBytes / 1024).toFixed(0)} KiB{s.present ? '' : ' — ABSENT'}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

export function RecordsArchivePanel({ adminApi }: { adminApi: BoundAdminApi }) {
  return (
    <section className={cardCls} aria-label="Records and archive">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Records &amp; archive</h2>
      <div className="grid gap-4 lg:grid-cols-2">
        <OrderExport adminApi={adminApi} />
        <ArchiveStatusView adminApi={adminApi} />
      </div>
    </section>
  );
}

export function DlqPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [stream, setStream] = useState('');
  const [consumer, setConsumer] = useState('');
  const [applied, setApplied] = useState({ stream: '', consumer: '' });

  const query = useQuery({
    queryKey: ['admin-int', 'dlq', adminApi.env, applied],
    queryFn: () =>
      fetchDlq(adminApi, {
        stream: applied.stream || undefined,
        consumer: applied.consumer || undefined,
      }),
    retry: false,
    refetchInterval: 30_000,
  });

  if (isAccessDenied(query.error)) {
    return <AccessDeniedCard detail="The DLQ surface requires an admin role." />;
  }

  return (
    <section className={cardCls} aria-label="Dead-letter queue">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Dead-letter queue</h2>
      <p className={hintTextCls}>
        JetStream terminal deliveries (ops-dlq stream). 503 here means the stream is unprovisioned —
        run `natsctl dlq init`, it is not "zero entries".
      </p>
      <div className="mb-2 flex flex-wrap items-end gap-2">
        <div>
          <label className={labelCls} htmlFor="dlq-stream">
            Stream
          </label>
          <input
            id="dlq-stream"
            className={inputCls}
            value={stream}
            onChange={(e) => setStream(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="dlq-consumer">
            Consumer
          </label>
          <input
            id="dlq-consumer"
            className={inputCls}
            value={consumer}
            onChange={(e) => setConsumer(e.target.value)}
          />
        </div>
        <button
          type="button"
          className={btnPrimary}
          onClick={() => setApplied({ stream, consumer })}
        >
          Filter
        </button>
      </div>
      {query.isError && <ErrorBox error={query.error} />}
      {query.isSuccess &&
        (query.data.entries.length === 0 ? (
          <p className="text-sm text-neutral-500">DLQ empty — no terminal deliveries.</p>
        ) : (
          <div className="max-h-64 overflow-y-auto">
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Seq</th>
                  <th className={thCls}>Stream</th>
                  <th className={thCls}>Consumer</th>
                  <th className={thCls}>Subject</th>
                  <th className={thCls}>Reason</th>
                  <th className={thCls}>Deliveries</th>
                  <th className={thCls}>Failed at</th>
                </tr>
              </thead>
              <tbody>
                {query.data.entries.map((e) => (
                  <tr key={e.seq}>
                    <td className={tdCls}>{e.seq}</td>
                    <td className={tdCls}>{e.stream}</td>
                    <td className={tdCls}>{e.consumer}</td>
                    <td className={tdCls} title={e.payload ?? ''}>
                      {e.subject}
                    </td>
                    <td className={tdCls}>{e.reason}</td>
                    <td className={tdCls}>{e.deliveries}</td>
                    <td className={tdCls}>{e.failedAt}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}
    </section>
  );
}
