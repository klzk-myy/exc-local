/**
 * Audit log viewer (Task 10.3.6) — keyset-cursor pagination over
 * GET /api/v1/admin/audit-log via TanStack `useInfiniteQuery`
 * (pageParam = the envelope's `next_cursor`). Filters match the
 * handler's set: action, admin_user_id, target_type, from, to.
 */
import { useState } from 'react';
import { useInfiniteQuery } from '@tanstack/react-query';

import type { BoundAdminApi } from '@/lib/env';
import { fetchAuditLog } from '@/lib/admin/api';
import { btnGhost, cardCls, inputCls, tableCls, tdCls, thCls, ErrorBox } from '@/lib/ui';
import { AccessDeniedCard } from './RequireAdmin';
import { isAccessDenied } from './adminRole';

interface Filters {
  action: string;
  adminUserId: string;
  targetType: string;
  from: string;
  to: string;
}

const EMPTY_FILTERS: Filters = { action: '', adminUserId: '', targetType: '', from: '', to: '' };

function toQuery(f: Filters) {
  const adminId = Number(f.adminUserId);
  return {
    action: f.action !== '' ? f.action : undefined,
    adminUserId: Number.isInteger(adminId) && adminId > 0 ? adminId : undefined,
    targetType: f.targetType !== '' ? f.targetType : undefined,
    from: f.from !== '' ? new Date(f.from).toISOString() : undefined,
    to: f.to !== '' ? new Date(f.to).toISOString() : undefined,
    limit: 50,
  };
}

export function AuditLogPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [filters, setFilters] = useState<Filters>(EMPTY_FILTERS);
  const [applied, setApplied] = useState<Filters>(EMPTY_FILTERS);

  const query = useInfiniteQuery({
    queryKey: ['admin', 'audit-log', adminApi.env, applied],
    queryFn: ({ pageParam }) =>
      fetchAuditLog(adminApi, { ...toQuery(applied), cursor: pageParam || undefined }),
    initialPageParam: '',
    getNextPageParam: (last) => (last.nextCursor === '' ? undefined : last.nextCursor),
    retry: false,
  });

  const rows = query.data?.pages.flatMap((p) => p.data) ?? [];
  const total = query.data?.pages[0]?.total;

  return (
    <section className={cardCls} aria-label="Audit log">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Audit log</h2>
      <form
        className="mb-3 grid grid-cols-2 gap-2 md:grid-cols-5"
        onSubmit={(e) => {
          e.preventDefault();
          setApplied(filters);
        }}
      >
        <input
          aria-label="Action filter"
          placeholder="action (e.g. user.freeze)"
          className={inputCls}
          value={filters.action}
          onChange={(e) => setFilters({ ...filters, action: e.target.value })}
        />
        <input
          aria-label="Admin user id filter"
          placeholder="admin_user_id"
          className={inputCls}
          value={filters.adminUserId}
          onChange={(e) => setFilters({ ...filters, adminUserId: e.target.value })}
        />
        <input
          aria-label="Target type filter"
          placeholder="target_type"
          className={inputCls}
          value={filters.targetType}
          onChange={(e) => setFilters({ ...filters, targetType: e.target.value })}
        />
        <input
          aria-label="From (RFC3339)"
          placeholder="from 2026-01-01"
          className={inputCls}
          value={filters.from}
          onChange={(e) => setFilters({ ...filters, from: e.target.value })}
        />
        <button type="submit" className={btnGhost}>
          Apply
        </button>
      </form>

      {query.error !== null && isAccessDenied(query.error) && (
        <AccessDeniedCard detail="The audit log requires a privileged admin role." />
      )}
      {query.error !== null && !isAccessDenied(query.error) && <ErrorBox error={query.error} />}
      {query.error === null && (
        <>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Time (UTC)</th>
                <th className={thCls}>Admin</th>
                <th className={thCls}>Action</th>
                <th className={thCls}>Target</th>
                <th className={thCls}>IP</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.id}>
                  <td className={tdCls}>{r.createdAt}</td>
                  <td className={tdCls}>{r.adminUserId}</td>
                  <td className={tdCls}>{r.action}</td>
                  <td className={tdCls}>
                    {r.targetType ?? ''}
                    {r.targetId !== undefined ? ` #${r.targetId}` : ''}
                  </td>
                  <td className={tdCls}>{r.ipAddress ?? '—'}</td>
                </tr>
              ))}
              {rows.length === 0 && !query.isLoading && (
                <tr>
                  <td className={tdCls} colSpan={5}>
                    No audit rows for this filter.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
          <div className="mt-2 flex items-center gap-3 text-xs text-neutral-500">
            <span data-testid="audit-count">
              {rows.length} loaded{total !== undefined ? ` · ${total} total` : ''}
            </span>
            {query.hasNextPage && (
              <button
                type="button"
                className={btnGhost}
                disabled={query.isFetchingNextPage}
                onClick={() => void query.fetchNextPage()}
              >
                {query.isFetchingNextPage ? 'Loading…' : 'Load more'}
              </button>
            )}
          </div>
        </>
      )}
    </section>
  );
}
