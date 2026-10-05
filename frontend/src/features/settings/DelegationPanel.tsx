/**
 * Delegation & approvals panel (Task 12.3.11) — institutional delegated
 * logins and the client M-of-N multi-validator surface:
 *
 *   Delegated users — GET/POST/PUT/DELETE /account/delegated-users[/{id}]
 *                     + POST …/revoke-all (emergency master revocation)
 *   Policies        — GET /account/approval-policies,
 *                     PUT upsert {operation, required_approvals,
 *                     threshold_amount?, threshold_currency?,
 *                     expires_in_seconds?}, DELETE …/{id}
 *   Requests        — GET /account/approval-requests (?status=),
 *                     POST …/{id}/decide {approve, note?}
 *
 * Management surfaces are master-owner only; delegate sessions may only
 * read the request list and cast CLIENT_APPROVER votes. Both rules are
 * enforced server-side — the UI just reflects them.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import {
  btnDanger,
  btnGhost,
  btnPrimary,
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

// ---------- wire shapes -------------------------------------------------

interface DelegatedUser {
  id: number;
  userId: number;
  displayName: string;
  status: string;
  role?: string;
  scope?: { account_ids?: number[]; instruments?: string[] };
  bindingExpiresAt?: string;
  revokeReason?: string;
}

interface Policy {
  id: number;
  operation: string;
  requiredApprovals: number;
  thresholdAmount?: string;
  thresholdCurrency?: string;
  expiresInSeconds: number;
  status: string;
}

interface ApprovalRequest {
  id: number;
  policyId: number;
  operation: string;
  status: string;
  approvalsCount: number;
  requiredApprovals: number;
  requestedByUser: number;
  expiresAt: string;
  fingerprint: string;
}

const rec = (v: unknown): Record<string, unknown> =>
  typeof v === 'object' && v !== null ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === 'string' ? v : '');
const num = (v: unknown): number => (typeof v === 'number' ? v : Number(v ?? 0));
const optStr = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined);
const listOf = <T,>(v: unknown, fn: (x: unknown) => T): T[] => (Array.isArray(v) ? v.map(fn) : []);

const ROLE_OPTIONS = [
  'CLIENT_READ_ONLY',
  'CLIENT_TRADER',
  'CLIENT_FINANCE_MANAGER',
  'CLIENT_APPROVER',
];
const OP_OPTIONS = [
  'WITHDRAWAL',
  'BENEFICIARY_CHANGE',
  'API_KEY_PRIVILEGE_CHANGE',
  'INTERNAL_TRANSFER',
];

// ---------- api --------------------------------------------------------

const fetchDelegated = (): Promise<DelegatedUser[]> =>
  apiClient.get('/account/delegated-users').then((r) =>
    listOf(rec(r)['delegated_users'], (x) => {
      const u = rec(x);
      const scope = rec(u['scope']);
      return {
        id: num(u['id']),
        userId: num(u['user_id']),
        displayName: str(u['display_name']),
        status: str(u['status']),
        role: optStr(u['role']),
        scope: {
          account_ids: listOf(scope['account_ids'], (a) => num(a)),
          instruments: listOf(scope['instruments'], (s) => str(s)),
        },
        bindingExpiresAt: optStr(u['binding_expires_at']),
        revokeReason: optStr(u['revoke_reason']),
      };
    }),
  );

const fetchPolicies = (): Promise<Policy[]> =>
  apiClient.get('/account/approval-policies').then((r) =>
    listOf(rec(r)['policies'], (x) => {
      const p = rec(x);
      return {
        id: num(p['id']),
        operation: str(p['operation']),
        requiredApprovals: num(p['required_approvals']),
        thresholdAmount: optStr(p['threshold_amount']),
        thresholdCurrency: optStr(p['threshold_currency']),
        expiresInSeconds: num(p['expires_in_seconds']),
        status: str(p['status']),
      };
    }),
  );

const fetchRequests = (status: string): Promise<ApprovalRequest[]> =>
  apiClient.get(`/account/approval-requests${status !== '' ? `?status=${status}` : ''}`).then((r) =>
    listOf(rec(r)['requests'], (x) => {
      const q = rec(x);
      return {
        id: num(q['id']),
        policyId: num(q['policy_id']),
        operation: str(q['operation']),
        status: str(q['status']),
        approvalsCount: num(q['approvals_count']),
        requiredApprovals: num(q['required_approvals']),
        requestedByUser: num(q['requested_by_user']),
        expiresAt: str(q['expires_at']),
        fingerprint: str(q['fingerprint']),
      };
    }),
  );

// ---------- panel ------------------------------------------------------

export default function DelegationPanel() {
  const qc = useQueryClient();
  const [grant, setGrant] = useState({
    userId: '',
    displayName: '',
    role: 'CLIENT_READ_ONLY',
    accountIds: '',
    instruments: '',
    expiresAt: '',
  });
  const [revokeReason, setRevokeReason] = useState('');
  const [policy, setPolicy] = useState({
    operation: 'WITHDRAWAL',
    required: '2',
    thresholdAmount: '',
    thresholdCcy: '',
    expiresIn: '',
  });
  const [reqStatus, setReqStatus] = useState('PENDING');
  const [notice, setNotice] = useState<string | null>(null);

  const delegated = useQuery({ queryKey: ['account', 'delegated-users'], queryFn: fetchDelegated });
  const policies = useQuery({ queryKey: ['account', 'approval-policies'], queryFn: fetchPolicies });
  const requests = useQuery({
    queryKey: ['account', 'approval-requests', reqStatus],
    queryFn: () => fetchRequests(reqStatus),
  });

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ['account', 'delegated-users'] });
    void qc.invalidateQueries({ queryKey: ['account', 'approval-policies'] });
    void qc.invalidateQueries({ queryKey: ['account', 'approval-requests'] });
  };
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : String(e));

  const grantMut = useMutation({
    mutationFn: () =>
      apiClient.post('/account/delegated-users', {
        user_id: Number(grant.userId),
        display_name: grant.displayName,
        role: grant.role,
        scope: {
          account_ids: grant.accountIds
            .split(',')
            .map((s) => Number(s.trim()))
            .filter((n) => n > 0),
          instruments: grant.instruments
            .split(',')
            .map((s) => s.trim())
            .filter((s) => s !== ''),
        },
        ...(grant.expiresAt !== '' ? { expires_at: grant.expiresAt } : {}),
      }),
    onSuccess: () => {
      setNotice('Delegated binding created.');
      invalidate();
    },
    onError: onErr,
  });
  const revokeMut = useMutation({
    mutationFn: ({ id, all }: { id?: number; all?: boolean }) =>
      all
        ? apiClient.post('/account/delegated-users/revoke-all', { reason: revokeReason })
        : apiClient.delete(`/account/delegated-users/${id}`, { body: { reason: revokeReason } }),
    onSuccess: () => {
      setNotice('Delegation revoked.');
      invalidate();
    },
    onError: onErr,
  });
  // PUT rewrites role+scope — resend the binding's current scope so a
  // role-only change does not wipe the existing binding (SCOPE_CHANGED audit).
  const updateMut = useMutation({
    mutationFn: ({ u, role }: { u: DelegatedUser; role: string }) =>
      apiClient.put(`/account/delegated-users/${u.id}`, {
        role,
        scope: {
          account_ids: u.scope?.account_ids ?? [],
          instruments: u.scope?.instruments ?? [],
        },
        ...(u.bindingExpiresAt ? { expires_at: u.bindingExpiresAt } : {}),
      }),
    onSuccess: () => {
      setNotice('Binding role updated.');
      invalidate();
    },
    onError: onErr,
  });
  const policyMut = useMutation({
    mutationFn: () =>
      apiClient.put('/account/approval-policies', {
        operation: policy.operation,
        required_approvals: Number(policy.required),
        ...(policy.thresholdAmount !== ''
          ? { threshold_amount: policy.thresholdAmount, threshold_currency: policy.thresholdCcy }
          : {}),
        ...(policy.expiresIn !== '' ? { expires_in_seconds: Number(policy.expiresIn) } : {}),
      }),
    onSuccess: () => {
      setNotice('M-of-N policy upserted.');
      invalidate();
    },
    onError: onErr,
  });
  const disablePolicyMut = useMutation({
    mutationFn: (id: number) => apiClient.delete(`/account/approval-policies/${id}`),
    onSuccess: () => {
      setNotice('Policy disabled — open requests still decide, new ones reject.');
      invalidate();
    },
    onError: onErr,
  });
  const decideMut = useMutation({
    mutationFn: ({ id, approve }: { id: number; approve: boolean }) =>
      apiClient.post(`/account/approval-requests/${id}/decide`, { approve }),
    onSuccess: () => {
      setNotice('Vote recorded.');
      invalidate();
    },
    onError: onErr,
  });

  return (
    <section className={cardCls} aria-label="Delegation">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">
        Delegated users &amp; M-of-N approvals
      </h2>
      <ErrorBox error={delegated.error ?? policies.error ?? requests.error} />
      {notice !== null && (
        <p className="mb-2 rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs text-neutral-300">
          {notice}
        </p>
      )}

      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>ID</th>
            <th className={thCls}>User</th>
            <th className={thCls}>Name</th>
            <th className={thCls}>Role</th>
            <th className={thCls}>Scope</th>
            <th className={thCls}>Status</th>
            <th className={thCls}>Expires</th>
            <th className={thCls}></th>
          </tr>
        </thead>
        <tbody>
          {(delegated.data ?? []).map((u) => (
            <tr key={u.id}>
              <td className={tdCls}>{u.id}</td>
              <td className={tdCls}>{u.userId}</td>
              <td className={tdCls}>{u.displayName}</td>
              <td className={tdCls}>
                {u.status === 'ACTIVE' ? (
                  <select
                    aria-label={`Role for ${u.displayName}`}
                    className={selectCls}
                    value={u.role ?? 'CLIENT_READ_ONLY'}
                    disabled={updateMut.isPending}
                    onChange={(e) => updateMut.mutate({ u, role: e.target.value })}
                  >
                    {ROLE_OPTIONS.map((r) => (
                      <option key={r} value={r}>
                        {r}
                      </option>
                    ))}
                  </select>
                ) : (
                  (u.role ?? '—')
                )}
              </td>
              <td className={tdCls}>
                {u.scope?.instruments?.join(', ') || (u.scope?.account_ids?.length ?? 0) > 0
                  ? `${u.scope?.account_ids?.length ?? 0} acct`
                  : 'all'}
              </td>
              <td className={tdCls}>
                <StatusBadge value={u.status} />
              </td>
              <td className={tdCls}>{u.bindingExpiresAt?.slice(0, 10) ?? '—'}</td>
              <td className={tdCls}>
                {u.status === 'ACTIVE' && (
                  <button
                    type="button"
                    className={btnDanger}
                    disabled={revokeMut.isPending}
                    onClick={() => revokeMut.mutate({ id: u.id })}
                  >
                    Revoke
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {delegated.data?.length === 0 && (
        <p className="py-2 text-center text-xs text-neutral-500">No delegated users.</p>
      )}

      <form
        className="mt-3 grid gap-2 sm:grid-cols-3"
        onSubmit={(e) => {
          e.preventDefault();
          grantMut.mutate();
        }}
      >
        {(
          [
            ['userId', 'user_id'],
            ['displayName', 'display_name'],
            ['expiresAt', 'expires_at (RFC3339, opt)'],
          ] as const
        ).map(([k, label]) => (
          <label key={k} className="block">
            <span className="sr-only">{label}</span>
            <input
              aria-label={label}
              className={inputCls}
              placeholder={label}
              value={grant[k]}
              onChange={(e) => setGrant({ ...grant, [k]: e.target.value })}
            />
          </label>
        ))}
        <label className="block">
          <span className="sr-only">Role</span>
          <select
            aria-label="Delegate role"
            className={selectCls}
            value={grant.role}
            onChange={(e) => setGrant({ ...grant, role: e.target.value })}
          >
            {ROLE_OPTIONS.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </label>
        <label className="block">
          <span className="sr-only">account_ids</span>
          <input
            aria-label="account_ids"
            className={inputCls}
            placeholder="account_ids csv (blank = all)"
            value={grant.accountIds}
            onChange={(e) => setGrant({ ...grant, accountIds: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">instruments</span>
          <input
            aria-label="instruments"
            className={inputCls}
            placeholder="instruments csv (blank = all)"
            value={grant.instruments}
            onChange={(e) => setGrant({ ...grant, instruments: e.target.value })}
          />
        </label>
        <button
          type="submit"
          className={btnPrimary}
          disabled={grantMut.isPending || Number(grant.userId) <= 0 || grant.displayName === ''}
        >
          Grant delegate
        </button>
      </form>
      <form
        className="mt-2 flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          revokeMut.mutate({ all: true });
        }}
      >
        <label className="block flex-1">
          <span className="sr-only">Revoke-all reason</span>
          <input
            aria-label="Revoke-all reason"
            className={inputCls}
            placeholder="revoke-all reason (emergency master revocation)"
            value={revokeReason}
            onChange={(e) => setRevokeReason(e.target.value)}
          />
        </label>
        <button
          type="submit"
          className={btnDanger}
          disabled={revokeMut.isPending || revokeReason === ''}
        >
          Revoke ALL
        </button>
      </form>

      <h3 className="mt-6 mb-2 text-sm font-medium text-neutral-400">Approval policies (M-of-N)</h3>
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>ID</th>
            <th className={thCls}>Operation</th>
            <th className={thCls}>M</th>
            <th className={thCls}>Threshold</th>
            <th className={thCls}>Expires in</th>
            <th className={thCls}>Status</th>
            <th className={thCls}></th>
          </tr>
        </thead>
        <tbody>
          {(policies.data ?? []).map((p) => (
            <tr key={p.id}>
              <td className={tdCls}>{p.id}</td>
              <td className={tdCls}>{p.operation}</td>
              <td className={tdCls}>{p.requiredApprovals}</td>
              <td className={tdCls}>
                {p.thresholdAmount !== undefined
                  ? `≥${p.thresholdAmount} ${p.thresholdCurrency ?? ''}`
                  : 'any'}
              </td>
              <td className={tdCls}>{p.expiresInSeconds}s</td>
              <td className={tdCls}>
                <StatusBadge value={p.status} />
              </td>
              <td className={tdCls}>
                {p.status === 'ACTIVE' && (
                  <button
                    type="button"
                    className={btnDanger}
                    disabled={disablePolicyMut.isPending}
                    onClick={() => disablePolicyMut.mutate(p.id)}
                  >
                    Disable
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <form
        className="mt-3 grid gap-2 sm:grid-cols-5"
        onSubmit={(e) => {
          e.preventDefault();
          policyMut.mutate();
        }}
      >
        <label className="block">
          <span className="sr-only">Operation</span>
          <select
            aria-label="Policy operation"
            className={selectCls}
            value={policy.operation}
            onChange={(e) => setPolicy({ ...policy, operation: e.target.value })}
          >
            {OP_OPTIONS.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </select>
        </label>
        <label className="block">
          <span className="sr-only">required_approvals</span>
          <input
            aria-label="required_approvals"
            className={inputCls}
            placeholder="required_approvals (M)"
            value={policy.required}
            onChange={(e) => setPolicy({ ...policy, required: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">threshold_amount</span>
          <input
            aria-label="threshold_amount"
            className={inputCls}
            placeholder="threshold_amount (opt)"
            value={policy.thresholdAmount}
            onChange={(e) => setPolicy({ ...policy, thresholdAmount: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">threshold_currency</span>
          <input
            aria-label="threshold_currency"
            className={inputCls}
            placeholder="threshold_currency"
            value={policy.thresholdCcy}
            onChange={(e) => setPolicy({ ...policy, thresholdCcy: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">expires_in_seconds</span>
          <input
            aria-label="expires_in_seconds"
            className={inputCls}
            placeholder="expires_in_seconds"
            value={policy.expiresIn}
            onChange={(e) => setPolicy({ ...policy, expiresIn: e.target.value })}
          />
        </label>
        <button type="submit" className={btnGhost} disabled={policyMut.isPending}>
          Upsert policy
        </button>
      </form>

      <h3 className="mt-6 mb-2 flex items-center gap-2 text-sm font-medium text-neutral-400">
        Approval requests
        <select
          aria-label="Request status"
          className={selectCls}
          value={reqStatus}
          onChange={(e) => setReqStatus(e.target.value)}
        >
          {['PENDING', 'APPROVED', 'REJECTED', 'EXPIRED', ''].map((s) => (
            <option key={s} value={s}>
              {s === '' ? 'all' : s}
            </option>
          ))}
        </select>
      </h3>
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>ID</th>
            <th className={thCls}>Policy</th>
            <th className={thCls}>Operation</th>
            <th className={thCls}>Votes</th>
            <th className={thCls}>Status</th>
            <th className={thCls}>Expires</th>
            <th className={thCls}></th>
          </tr>
        </thead>
        <tbody>
          {(requests.data ?? []).map((q) => (
            <tr key={q.id}>
              <td className={tdCls}>{q.id}</td>
              <td className={tdCls}>{q.policyId}</td>
              <td className={tdCls}>{q.operation}</td>
              <td className={tdCls}>
                {q.approvalsCount}/{q.requiredApprovals}
              </td>
              <td className={tdCls}>
                <StatusBadge value={q.status} />
              </td>
              <td className={tdCls}>{q.expiresAt.slice(0, 16)}</td>
              <td className={tdCls}>
                {q.status === 'PENDING' && (
                  <div className="flex gap-1">
                    <button
                      type="button"
                      className={btnPrimary}
                      disabled={decideMut.isPending}
                      onClick={() => decideMut.mutate({ id: q.id, approve: true })}
                    >
                      Approve
                    </button>
                    <button
                      type="button"
                      className={btnDanger}
                      disabled={decideMut.isPending}
                      onClick={() => decideMut.mutate({ id: q.id, approve: false })}
                    >
                      Reject
                    </button>
                  </div>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {requests.data?.length === 0 && (
        <p className="py-2 text-center text-xs text-neutral-500">No requests in this state.</p>
      )}
      <p className={hintTextCls}>
        Management surfaces are master-only; CLIENT_APPROVER delegates can only list requests and
        vote. Anti-self-approval, expiry, and single-vote are enforced server-side.
      </p>
    </section>
  );
}
