/**
 * Balances panel — per-currency available/locked/total for the current
 * account scope (Task 10.3.14 workspace tile; reused by the Lite
 * dashboard as the primary surface).
 */
import { ErrorBox, tableCls, tdCls, thCls } from '@/lib/ui';
import { useBalances } from '@/lib/trading/queries';

export function BalancesPanel({ bare = false }: { bare?: boolean }) {
  const balances = useBalances();
  const rows = balances.data ?? [];

  return (
    <section
      aria-label="Balances"
      className={bare ? '' : 'rounded-lg border border-neutral-800 bg-neutral-900 p-4'}
    >
      {bare ? null : <h2 className="mb-3 text-sm font-semibold text-neutral-200">Balances</h2>}
      <ErrorBox error={balances.isError ? balances.error : null} />
      <div className="relative overflow-x-auto" tabIndex={0}>
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Currency</th>
              <th className={thCls}>Available</th>
              <th className={thCls}>Locked</th>
              <th className={thCls}>Total</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((b) => (
              <tr key={b.currency}>
                <td className={`${tdCls} font-medium`}>{b.currency}</td>
                <td className={tdCls}>{b.available.toDisplay(2)}</td>
                <td className={tdCls}>{b.locked.toDisplay(2)}</td>
                <td className={tdCls}>{b.total.toDisplay(2)}</td>
              </tr>
            ))}
            {rows.length === 0 && balances.isSuccess && (
              <tr>
                <td className={`${tdCls} text-neutral-500`} colSpan={4}>
                  No balances on this account.
                </td>
              </tr>
            )}
            {balances.isLoading && (
              <tr>
                <td className={`${tdCls} text-neutral-500`} colSpan={4}>
                  Loading…
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </section>
  );
}
