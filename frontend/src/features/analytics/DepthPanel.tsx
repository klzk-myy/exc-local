/**
 * DepthPanel — REST book snapshot (L2) + the TierProfessional L3
 * order-level view. L3 is fetched on demand: a 402/403 renders
 * through ErrorBox verbatim rather than implying entitlement.
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { ApiError } from '@/lib/api';

import { apiClient } from '@/app/runtime';
import { ErrorBox, btnGhost, cardCls, tableCls, tdCls, thCls } from '@/lib/ui';

import { fetchDepth, fetchL3 } from './api';

export function DepthPanel({ symbol }: { symbol: string }) {
  const depth = useQuery({
    queryKey: ['analytics', 'depth', symbol],
    queryFn: () => fetchDepth(symbol, 10, apiClient),
    enabled: symbol !== '',
    refetchInterval: 15_000,
    retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
  });
  const [l3Req, setL3Req] = useState(false);
  const [l3, setL3] = useState<{ data?: unknown; error?: unknown } | null>(null);
  const [l3Busy, setL3Busy] = useState(false);

  const bids = depth.data?.bids ?? [];
  const asks = depth.data?.asks ?? [];
  const rows = Math.max(bids.length, asks.length);

  return (
    <section aria-label="Book snapshot" className={`${cardCls} space-y-3`}>
      <h2 className="text-sm font-semibold">Book snapshot — {symbol}</h2>
      {depth.isPending ? (
        <p className="text-xs text-neutral-500">Loading…</p>
      ) : depth.isError ? (
        <ErrorBox error={depth.error} />
      ) : (
        <>
          <p className="text-xs text-neutral-500">
            seq {depth.data.seq ?? '—'} · updated{' '}
            {depth.data.updated_at_ms !== undefined
              ? new Date(depth.data.updated_at_ms).toISOString()
              : '—'}
          </p>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Bid px</th>
                <th className={thCls}>Bid qty</th>
                <th className={thCls}>Ask px</th>
                <th className={thCls}>Ask qty</th>
              </tr>
            </thead>
            <tbody>
              {Array.from({ length: rows }, (_, i) => (
                <tr key={i}>
                  <td className={`${tdCls} font-mono text-emerald-400`}>{bids[i]?.price ?? ''}</td>
                  <td className={`${tdCls} font-mono`}>{bids[i]?.quantity ?? ''}</td>
                  <td className={`${tdCls} font-mono text-red-400`}>{asks[i]?.price ?? ''}</td>
                  <td className={`${tdCls} font-mono`}>{asks[i]?.quantity ?? ''}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}

      {/* L3 — professional tier; fetched on demand so 402/403 is a
          deliberate request, not a page-load failure. */}
      <div>
        <button
          type="button"
          className={btnGhost}
          disabled={l3Busy}
          onClick={() => {
            setL3Req(true);
            setL3Busy(true);
            fetchL3(symbol, apiClient)
              .then((d) => {
                setL3({ data: d });
              })
              .catch((e: unknown) => {
                setL3({ error: e });
              })
              .finally(() => {
                setL3Busy(false);
              });
          }}
        >
          {l3Req ? 'Reload L3 snapshot' : 'Load L3 order-level snapshot (professional tier)'}
        </button>
        {l3Req ? (
          l3?.error !== undefined ? (
            <div className="mt-2">
              <ErrorBox error={l3.error} />
            </div>
          ) : l3?.data !== undefined ? (
            (() => {
              const d = l3.data as {
                count?: number;
                l3_seq?: number;
                wal_seq?: number;
                asof_ms?: number;
                fresh?: boolean;
                orders?: Record<string, unknown>[];
              };
              return (
                <div className="mt-2 text-xs">
                  <p>
                    orders {d.count ?? d.orders?.length ?? 0} · l3_seq {d.l3_seq ?? '—'} · wal{' '}
                    {d.wal_seq ?? '—'}
                    {d.fresh === false ? (
                      <span role="alert" className="ml-1 text-amber-400">
                        stale
                      </span>
                    ) : null}
                  </p>
                  <div className="mt-1 max-h-40 overflow-y-auto" tabIndex={0}>
                    <table className={tableCls}>
                      <tbody>
                        {(d.orders ?? []).slice(0, 25).map((o, i) => (
                          <tr key={i}>
                            <td className={`${tdCls} font-mono`}>{JSON.stringify(o)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </div>
              );
            })()
          ) : null
        ) : null}
      </div>
    </section>
  );
}
