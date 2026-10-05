/**
 * Customer 360 & account-lifecycle console (Phase-10.5 Task 10.5.3.4) —
 * the per-customer operations home: the read-only support dossier,
 * active compliance holds, lifecycle actions (freeze/unfreeze/close/
 * jurisdiction/sub-account-limit/client-category) with precondition
 * evidence, the account-scoped support desk, and the KYC card
 * (pending submissions + tax self-certification review).
 * Route: /admin/customers — the per-account drill-down the Task 10.3.6
 * UserLookupPanel links out to.
 */
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import { EnvPill, EnvSwitcher, EnvWatermark, useBoundAdminApi } from '@/lib/env';
import { btnPrimary, inputCls } from '@/lib/ui';

import { RequireAdmin } from '../admin/RequireAdmin';
import { useAdminRole } from '../admin/adminRole';
import { DossierPanel } from './DossierPanel';
import { KycDeskPanel } from './KycDeskPanel';
import { LifecyclePanel } from './LifecyclePanel';
import { SupportDeskPanel } from './SupportDeskPanel';

export default function Customer360Page() {
  return (
    <RequireAdmin>
      <Customer360 />
    </RequireAdmin>
  );
}

function Customer360() {
  const adminApi = useBoundAdminApi(apiClient);
  const role = useAdminRole();
  const [input, setInput] = useState('');
  const [accountId, setAccountId] = useState<number | null>(null);
  const [inputError, setInputError] = useState<string | null>(null);

  const open = () => {
    const id = Number(input);
    if (!Number.isInteger(id) || id <= 0) {
      setInputError('Enter a positive integer account id.');
      return;
    }
    setInputError(null);
    setAccountId(id);
  };

  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Customer 360</h1>
        <div className="flex items-center gap-3">
          <EnvSwitcher />
          {role !== null && (
            <span
              className="rounded bg-sky-500/20 px-2 py-0.5 text-xs font-medium text-sky-300"
              data-testid="admin-role-badge"
              title="Client-side hint only — the server is the authorizer"
            >
              {role}
            </span>
          )}
          <EnvPill />
        </div>
      </div>

      <form
        className="mb-4 flex max-w-md gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          open();
        }}
      >
        <label className="sr-only" htmlFor="c360-id">
          Account id
        </label>
        <input
          id="c360-id"
          className={inputCls}
          inputMode="numeric"
          placeholder="account id"
          value={input}
          onChange={(e) => setInput(e.target.value)}
        />
        <button type="submit" className={btnPrimary}>
          Open customer
        </button>
      </form>
      {inputError !== null && <p className="mb-4 text-sm text-red-400">{inputError}</p>}

      {accountId === null ? (
        <p className="text-sm text-neutral-500">
          Enter an account id to load the dossier, lifecycle actions, support desk and KYC card.
          Every read on this page is audit-logged server-side.
        </p>
      ) : (
        <div className="grid gap-4">
          <div className="grid gap-4 lg:grid-cols-2">
            <DossierPanel adminApi={adminApi} accountId={accountId} />
            <LifecyclePanel adminApi={adminApi} accountId={accountId} />
          </div>
          <div className="grid gap-4 lg:grid-cols-2">
            <SupportDeskPanel adminApi={adminApi} accountId={accountId} />
            <KycDeskPanel adminApi={adminApi} accountId={accountId} />
          </div>
        </div>
      )}
    </div>
  );
}
