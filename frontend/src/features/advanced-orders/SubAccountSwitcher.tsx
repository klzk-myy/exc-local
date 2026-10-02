/**
 * Sub-account switcher (Task 10.3.7) — lists master + every sub-account
 * under it with per-account balances, and switches the trading scope.
 *
 * Switching writes the scope to `useAccountScope`; every TanStack query
 * in the cluster keys on that scope so cached data re-scopes atomically.
 *
 * Honesty rules (fail-closed §2.7):
 *   - the endpoint is live (Phase-05): an error still renders
 *     "unavailable" with the error code, never an empty fake list;
 *   - sub-account order routing has no on-behalf-of primitive yet —
 *     non-master scopes append ?account_id= and surface FORBIDDEN if the
 *     gateway rejects it (per-account API keys land in Task 5.3.11).
 */
import { useEffect, useRef, useState } from 'react';

import { MASTER_ACCOUNT_KEY, useAccountScope } from '@/lib/trading/accountScope';
import { useSubAccounts } from '@/lib/trading/queries';
import { ErrorBox } from '@/lib/ui';

export function SubAccountSwitcher() {
  const subs = useSubAccounts();
  const scopeKey = useAccountScope((s) => s.scopeKey);
  const scopeLabel = useAccountScope((s) => s.scopeLabel);
  const setScope = useAccountScope((s) => s.setScope);
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  // Close the menu on outside click / Escape.
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', onDoc);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDoc);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);

  const list = subs.data ?? [];

  return (
    <div ref={rootRef} className="relative inline-block text-left">
      <button
        type="button"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label="Switch trading account"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-2 rounded border border-neutral-700 bg-neutral-900 px-3 py-1.5 text-sm text-neutral-200 hover:bg-neutral-800 focus-visible:ring-2 focus-visible:ring-sky-500"
      >
        <span className="text-neutral-500">Account:</span>
        <span className="font-medium">{scopeLabel}</span>
        <span aria-hidden="true">▾</span>
      </button>

      {open && (
        <div
          role="listbox"
          aria-label="Accounts"
          className="absolute right-0 z-40 mt-1 w-80 rounded-lg border border-neutral-700 bg-neutral-900 p-1 shadow-xl"
        >
          <button
            type="button"
            role="option"
            aria-selected={scopeKey === MASTER_ACCOUNT_KEY}
            onClick={() => {
              setScope(MASTER_ACCOUNT_KEY, 'Master account');
              setOpen(false);
            }}
            className="block w-full rounded px-3 py-2 text-left text-sm hover:bg-neutral-800 focus-visible:ring-2 focus-visible:ring-sky-500"
          >
            <span className="font-medium text-neutral-100">Master account</span>
            <span className="mt-0.5 block text-xs text-neutral-500">primary trading account</span>
          </button>

          {subs.isError && (
            <div className="px-2 py-1">
              <ErrorBox error={subs.error} />
              <p className="text-xs text-neutral-500">
                Sub-account listing unavailable — trading continues on the master scope.
              </p>
            </div>
          )}
          {subs.isSuccess && list.length === 0 && (
            <p className="px-3 py-2 text-xs text-neutral-500">No sub-accounts on this master.</p>
          )}

          {list.map((sa) => {
            const key = String(sa.id);
            const label = `Sub-account #${sa.id}`;
            const selected = scopeKey === key;
            return (
              <button
                key={key}
                type="button"
                role="option"
                aria-selected={selected}
                disabled={!sa.tradingEnabled}
                onClick={() => {
                  setScope(key, label);
                  setOpen(false);
                }}
                className="block w-full rounded px-3 py-2 text-left text-sm hover:bg-neutral-800 focus-visible:ring-2 focus-visible:ring-sky-500 disabled:opacity-50"
              >
                <span className="flex items-center justify-between">
                  <span className="font-medium text-neutral-100">
                    {label}
                    {selected && <span className="ml-2 text-xs text-sky-400">current</span>}
                  </span>
                  <span
                    className={`text-xs ${sa.tradingEnabled ? 'text-emerald-400' : 'text-neutral-500'}`}
                  >
                    {sa.tradingEnabled ? 'ACTIVE' : sa.status}
                  </span>
                </span>
                {sa.balances.length > 0 && (
                  <span className="mt-1 block text-xs text-neutral-500">
                    {sa.balances.map((b) => `${b.total.toDisplay()} ${b.currency}`).join(' · ')}
                  </span>
                )}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
