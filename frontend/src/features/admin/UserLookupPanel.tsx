/**
 * User lookup panel (Task 10.3.6) — the read-only support-view dossier
 * at GET /api/v1/admin/support/accounts/{id} (account status, KYC tier,
 * balances, recent orders/tickets). Server-side the view is audit-logged
 * and gated to Support Agent / Super Admin — those responses surface as
 * the denial card.
 */
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import { fetchSupportAccount, type SupportView } from '@/lib/admin/api';
import { btnPrimary, cardCls, inputCls, ErrorBox, StatusBadge } from '@/lib/ui';
import { AccessDeniedCard } from './RequireAdmin';
import { isAccessDenied } from './adminRole';

function Dossier({ view }: { view: SupportView }) {
  return (
    <div className="mt-3 space-y-3" data-testid="support-view">
      <div className="flex flex-wrap items-center gap-3 text-sm">
        <span className="font-medium">Account #{view.accountId}</span>
        <StatusBadge value={view.status.toUpperCase()} />
        <span className="text-neutral-400">KYC {view.kycTier}</span>
        <span className="text-neutral-400">{view.accountType}</span>
        <span className="text-neutral-500">opened {view.createdAt}</span>
      </div>
      {view.balances.length > 0 && (
        <div>
          <h3 className="mb-1 text-xs font-medium text-neutral-400">Balances</h3>
          <ul className="text-xs text-neutral-300">
            {view.balances.map((b) => (
              <li key={b.currency}>
                {b.currency}: {b.available} available · {b.locked} locked
              </li>
            ))}
          </ul>
        </div>
      )}
      {view.recentOrders.length > 0 && (
        <div>
          <h3 className="mb-1 text-xs font-medium text-neutral-400">Recent orders</h3>
          <ul className="text-xs text-neutral-300">
            {view.recentOrders.map((o) => (
              <li key={o.id}>
                #{o.id} {o.side} {o.quantity} {o.symbol ?? ''} — {o.status} (filled{' '}
                {o.filledQuantity})
              </li>
            ))}
          </ul>
        </div>
      )}
      {view.recentTickets.length > 0 && (
        <div>
          <h3 className="mb-1 text-xs font-medium text-neutral-400">Recent tickets</h3>
          <ul className="text-xs text-neutral-300">
            {view.recentTickets.map((t) => (
              <li key={t.id}>
                #{t.id} [{t.priority}] {t.subject} — {t.status}
              </li>
            ))}
          </ul>
        </div>
      )}
      {view.kycDocuments.length > 0 && (
        <div>
          <h3 className="mb-1 text-xs font-medium text-neutral-400">KYC documents</h3>
          <ul className="text-xs text-neutral-300">
            {view.kycDocuments.map((d) => (
              <li key={d.id}>
                #{d.id} {d.type} — {d.status}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

export function UserLookupPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [accountId, setAccountId] = useState('');
  const [result, setResult] = useState<SupportView | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  const lookup = async () => {
    const id = Number(accountId);
    if (!Number.isInteger(id) || id <= 0) {
      setError(new Error('Enter a positive integer account id.'));
      setResult(null);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      setResult(await fetchSupportAccount(adminApi, id));
    } catch (e) {
      setResult(null);
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className={cardCls} aria-label="User lookup">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">User lookup</h2>
      <form
        className="flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          void lookup();
        }}
      >
        <input
          aria-label="Account id"
          placeholder="account id"
          className={inputCls}
          value={accountId}
          onChange={(e) => setAccountId(e.target.value)}
        />
        <button type="submit" className={btnPrimary} disabled={busy}>
          {busy ? 'Loading…' : 'Look up'}
        </button>
      </form>
      {error !== null && isAccessDenied(error) && (
        <div className="mt-3">
          <AccessDeniedCard detail="Support view requires the Support Agent role." />
        </div>
      )}
      {error !== null && !isAccessDenied(error) && (
        <div className="mt-3">
          <ErrorBox error={error} />
        </div>
      )}
      {result !== null && <Dossier view={result} />}
    </section>
  );
}
