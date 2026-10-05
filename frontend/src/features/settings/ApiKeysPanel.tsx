/**
 * API keys tab — Task 10.3.22 item 5 + Phase-05 Task 5.3.11/5.3.16.
 *
 *   Developer keys (main account): GET/POST /developer/api-keys,
 *     DELETE /developer/api-keys/{id}. HMAC secret shown exactly once.
 *   Sub-account keys: POST /account/sub-accounts/{id}/api-keys restricted
 *     to read|trade scopes — transfer/admin are never issuable there.
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ConfirmModal, useValidatedField, type FieldRule } from '@/lib/input-helpers';
import {
  CopyButton,
  ErrorBox,
  Field,
  StatusBadge,
  btnGhost,
  btnPrimary,
  cardCls,
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import * as api from './api';

const RATE_TIERS = ['basic', 'standard', 'professional', 'institutional'] as const;
const RULE_LABEL: FieldRule = {
  name: 'label',
  label: 'Key label',
  required: true,
  kind: 'string',
};

function ScopeChecklist({
  allowed,
  value,
  onChange,
}: {
  allowed: readonly string[];
  value: string[];
  onChange: (v: string[]) => void;
}) {
  return (
    <fieldset className="mb-4">
      <legend className="mb-1 text-sm font-medium text-neutral-300">Scopes</legend>
      <div className="flex flex-wrap gap-3">
        {allowed.map((s) => (
          <label key={s} className="flex items-center gap-1.5 text-sm text-neutral-200">
            <input
              type="checkbox"
              className="h-6 w-6"
              checked={value.includes(s)}
              onChange={(e) => {
                onChange(e.target.checked ? [...value, s] : value.filter((x) => x !== s));
              }}
            />
            {s}
          </label>
        ))}
      </div>
    </fieldset>
  );
}

function OneTimeSecret({ secret, notice }: { secret: string; notice?: string }) {
  return (
    <div className="mb-4 rounded border border-amber-700/50 bg-amber-950/40 p-3" role="status">
      <p className="mb-1 text-sm font-medium text-amber-300">
        {notice ?? 'Store the secret now — it is never shown again.'}
      </p>
      <p className="flex items-center gap-2 break-all font-mono text-xs text-amber-100">
        {secret}
        <CopyButton text={secret} label="Copy secret" />
      </p>
    </div>
  );
}

export default function ApiKeysPanel() {
  const qc = useQueryClient();
  const keys = useQuery({
    queryKey: ['developer', 'api-keys'],
    queryFn: () => api.listApiKeys(apiClient),
  });
  const subs = useQuery({
    queryKey: ['account', 'sub-accounts'],
    queryFn: () => api.listSubAccounts(apiClient),
  });

  const label = useValidatedField(RULE_LABEL);
  const [scopes, setScopes] = useState<string[]>(['read']);
  const [tier, setTier] = useState<string>('basic');
  const [revoking, setRevoking] = useState<api.ApiKeyView | null>(null);
  const [issued, setIssued] = useState<api.IssuedDeveloperKey | null>(null);
  const [issuedSub, setIssuedSub] = useState<api.IssuedSubAccountKey | null>(null);
  const [subTarget, setSubTarget] = useState<number | null>(null);
  const subLabel = useValidatedField(RULE_LABEL);
  const [subScopes, setSubScopes] = useState<string[]>(['read']);

  const create = useMutation({
    mutationFn: () =>
      api.createApiKey(apiClient, { label: label.value, scopes, rateLimitTier: tier }),
    onSuccess: async (r) => {
      setIssued(r);
      label.reset();
      await qc.invalidateQueries({ queryKey: ['developer', 'api-keys'] });
    },
  });
  const revoke = useMutation({
    mutationFn: (keyId: string) => api.revokeApiKey(apiClient, keyId),
    onSuccess: async () => {
      setRevoking(null);
      await qc.invalidateQueries({ queryKey: ['developer', 'api-keys'] });
    },
  });
  const createSubKey = useMutation({
    mutationFn: () =>
      api.createSubAccountApiKey(apiClient, subTarget ?? 0, {
        label: subLabel.value,
        scopes: subScopes,
      }),
    onSuccess: (r) => {
      setIssuedSub(r);
      subLabel.reset();
    },
  });
  const createSub = useMutation({
    mutationFn: () => api.createSubAccount(apiClient),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['account', 'sub-accounts'] });
    },
  });

  return (
    <div className="space-y-4">
      <div className={cardCls}>
        <h3 className="mb-2 text-sm font-semibold">API keys</h3>
        {issued?.secret !== undefined && (
          <OneTimeSecret secret={issued.secret} notice={issued.notice} />
        )}
        <ErrorBox error={keys.error ?? create.error ?? revoke.error} />
        {keys.isPending ? (
          <p className="text-sm text-neutral-400">Loading keys…</p>
        ) : (
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Label</th>
                <th className={thCls}>Key ID</th>
                <th className={thCls}>Scopes</th>
                <th className={thCls}>Tier</th>
                <th className={thCls}>Status</th>
                <th className={thCls} />
              </tr>
            </thead>
            <tbody>
              {(keys.data ?? []).map((k) => (
                <tr key={k.key_id}>
                  <td className={tdCls}>
                    {k.label}
                    {k.needs_rotation === true && (
                      <span className="ml-2 text-xs text-amber-400">rotation due</span>
                    )}
                  </td>
                  <td className={`${tdCls} font-mono text-xs`}>{k.key_id}</td>
                  <td className={tdCls}>{k.scopes.join(', ')}</td>
                  <td className={tdCls}>{k.rate_limit_tier}</td>
                  <td className={tdCls}>
                    <StatusBadge value={k.status.toUpperCase()} />
                  </td>
                  <td className={tdCls}>
                    {k.status.toUpperCase() !== 'REVOKED' && (
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => {
                          setRevoking(k);
                        }}
                      >
                        Revoke
                      </button>
                    )}
                  </td>
                </tr>
              ))}
              {(keys.data ?? []).length === 0 && (
                <tr>
                  <td className={tdCls} colSpan={6}>
                    No keys yet — create one below.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        )}

        <form
          className="mt-4"
          onSubmit={(e) => {
            e.preventDefault();
            create.mutate();
          }}
        >
          <Field label="Key label" required error={label.error}>
            {(id, describedBy) => (
              <input
                id={id}
                aria-describedby={describedBy}
                className={inputCls}
                {...label.inputProps}
              />
            )}
          </Field>
          <ScopeChecklist allowed={api.KEY_SCOPES} value={scopes} onChange={setScopes} />
          <Field label="Rate-limit tier">
            {(id, describedBy, invalid) => (
              <select
                id={id}
                aria-describedby={describedBy}
                aria-invalid={invalid}
                className={selectCls}
                value={tier}
                onChange={(e) => {
                  setTier(e.target.value);
                }}
              >
                {RATE_TIERS.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <button
            type="submit"
            className={btnPrimary}
            disabled={create.isPending || !label.valid || scopes.length === 0}
          >
            {create.isPending ? 'Creating…' : 'Create API key'}
          </button>
        </form>
      </div>

      <div className={cardCls}>
        <h3 className="mb-2 text-sm font-semibold">Sub-accounts</h3>
        <p className="mb-3 text-sm text-neutral-400">
          Sub-account keys are restricted to <code>read</code>/<code>trade</code> scopes — they can
          never move funds.
        </p>
        {issuedSub !== null && <OneTimeSecret secret={issuedSub.secret} />}
        <ErrorBox error={subs.error ?? createSub.error ?? createSubKey.error} />
        {subs.isPending ? (
          <p className="text-sm text-neutral-400">Loading sub-accounts…</p>
        ) : (
          <ul className="space-y-2">
            {(subs.data ?? []).map((s) => (
              <li
                key={s.id}
                className="flex items-center justify-between rounded border border-neutral-800 px-3 py-2"
              >
                <span className="text-sm">
                  Sub-account #{s.id} <StatusBadge value={s.status} />
                </span>
                <button
                  type="button"
                  className={btnGhost}
                  onClick={() => {
                    setSubTarget(s.id);
                    setIssuedSub(null);
                  }}
                >
                  New key
                </button>
              </li>
            ))}
            {(subs.data ?? []).length === 0 && (
              <li className="text-sm text-neutral-400">No sub-accounts.</li>
            )}
          </ul>
        )}
        <button
          type="button"
          className={`${btnPrimary} mt-3`}
          disabled={createSub.isPending}
          onClick={() => {
            createSub.mutate();
          }}
        >
          {createSub.isPending ? 'Creating…' : 'Create sub-account'}
        </button>

        {subTarget !== null && (
          <form
            className="mt-4 rounded border border-neutral-800 p-3"
            onSubmit={(e) => {
              e.preventDefault();
              createSubKey.mutate();
            }}
          >
            <p className="mb-2 text-sm font-medium">New key for sub-account #{subTarget}</p>
            <Field label="Key label" required error={subLabel.error}>
              {(id, describedBy) => (
                <input
                  id={id}
                  aria-describedby={describedBy}
                  className={inputCls}
                  {...subLabel.inputProps}
                />
              )}
            </Field>
            <ScopeChecklist
              allowed={api.SUB_ACCOUNT_KEY_SCOPES}
              value={subScopes}
              onChange={setSubScopes}
            />
            <button
              type="submit"
              className={btnPrimary}
              disabled={createSubKey.isPending || !subLabel.valid || subScopes.length === 0}
            >
              {createSubKey.isPending ? 'Issuing…' : 'Issue key'}
            </button>
            <button
              type="button"
              className={`${btnGhost} ml-2`}
              onClick={() => {
                setSubTarget(null);
              }}
            >
              Cancel
            </button>
          </form>
        )}
      </div>

      <ConfirmModal
        open={revoking !== null}
        severity="MEDIUM"
        title="Revoke API key"
        confirmLabel="Revoke key"
        busy={revoke.isPending}
        onConfirm={() => {
          if (revoking !== null) revoke.mutate(revoking.key_id);
        }}
        onCancel={() => {
          setRevoking(null);
        }}
      >
        <p className="text-sm text-neutral-300">
          Revoke key <strong>{revoking?.label}</strong> ({revoking?.key_id})? Connected applications
          lose access immediately. This cannot be undone.
        </p>
      </ConfirmModal>
    </div>
  );
}
