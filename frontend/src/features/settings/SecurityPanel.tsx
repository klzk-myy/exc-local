/**
 * Security tab — password change, TOTP, passkeys, anti-phishing code,
 * login history, and a link to the session list (auth feature owns the
 * device/session table — Task 10.3.21).
 */
import { useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { Link } from 'react-router';

import { apiClient } from '@/app/runtime';
import { useValidatedField, validateField, type FieldRule } from '@/lib/input-helpers';
import { ErrorBox, Field, btnPrimary, cardCls, inputCls, tableCls, tdCls, thCls } from '@/lib/ui';

import * as api from './api';
import TotpEnrollment from './TotpEnrollment';
import WebAuthnPanel from './WebAuthnPanel';

const REQUIRED = (name: string, label: string): FieldRule => ({
  name,
  label,
  required: true,
  kind: 'string',
});
/** Anti-phishing code rule — bounds mirror api.ANTI_PHISHING_{MIN,MAX}
 * (12.3.10). Live-validated below since the error shows on type. */
const RULE_ANTI_PHISHING: FieldRule = {
  name: 'code',
  label: 'Anti-phishing code',
  required: true,
  kind: 'string',
  minLength: api.ANTI_PHISHING_MIN,
  maxLength: api.ANTI_PHISHING_MAX,
};

function PasswordChangeForm() {
  const current = useValidatedField(REQUIRED('current_password', 'Current password'));
  const next = useValidatedField(REQUIRED('new_password', 'New password'));
  const confirm = useValidatedField(REQUIRED('confirm', 'Confirm new password'));
  // Cross-field rule — the shared framework has no equality combinator.
  const mismatch = confirm.value !== '' && confirm.value !== next.value;
  const mut = useMutation({
    mutationFn: () => api.changePassword(apiClient, current.value, next.value),
    onSuccess: () => {
      current.reset();
      next.reset();
      confirm.reset();
    },
  });

  return (
    <div className={cardCls}>
      <h3 className="mb-2 text-sm font-semibold">Change password</h3>
      {mut.isSuccess && (
        <p className="mb-2 text-sm text-emerald-400" role="status">
          Password updated — other sessions may be signed out.
        </p>
      )}
      <ErrorBox error={mut.error} />
      <form
        onSubmit={(e) => {
          e.preventDefault();
          mut.mutate();
        }}
      >
        <Field label="Current password" required error={current.error}>
          {(id, describedBy) => (
            <input
              id={id}
              aria-describedby={describedBy}
              className={inputCls}
              type="password"
              autoComplete="current-password"
              {...current.inputProps}
            />
          )}
        </Field>
        <Field label="New password" required error={next.error}>
          {(id, describedBy) => (
            <input
              id={id}
              aria-describedby={describedBy}
              className={inputCls}
              type="password"
              autoComplete="new-password"
              {...next.inputProps}
            />
          )}
        </Field>
        <Field
          label="Confirm new password"
          required
          error={mismatch ? 'Passwords do not match' : confirm.error}
        >
          {(id, describedBy) => (
            <input
              id={id}
              aria-describedby={describedBy}
              className={inputCls}
              type="password"
              autoComplete="new-password"
              {...confirm.inputProps}
              aria-invalid={mismatch || confirm.inputProps['aria-invalid']}
            />
          )}
        </Field>
        <button
          type="submit"
          className={btnPrimary}
          disabled={mut.isPending || !current.valid || !next.valid || mismatch}
        >
          {mut.isPending ? 'Updating…' : 'Change password'}
        </button>
      </form>
    </div>
  );
}

function AntiPhishingForm() {
  const [code, setCode] = useState('');
  // Task 10.3.29 shared rule — validated eagerly (error shows on type,
  // not just blur) so the live hint survives unchanged.
  const codeErr = code !== '' ? (validateField(RULE_ANTI_PHISHING, code)?.message ?? null) : null;
  const valid = validateField(RULE_ANTI_PHISHING, code) === null;
  const mut = useMutation({ mutationFn: () => api.setAntiPhishingCode(apiClient, code) });

  return (
    <div className={cardCls}>
      <h3 className="mb-2 text-sm font-semibold">Anti-phishing code</h3>
      <p className="mb-3 text-sm text-neutral-400">
        This code appears in every genuine platform email — an email without it is phishing.
        {api.ANTI_PHISHING_MIN}–{api.ANTI_PHISHING_MAX} characters; requires 2FA enabled.
      </p>
      {mut.isSuccess && (
        <p className="mb-2 text-sm text-emerald-400" role="status">
          Anti-phishing code updated.
        </p>
      )}
      <ErrorBox error={mut.error} />
      <form
        onSubmit={(e) => {
          e.preventDefault();
          mut.mutate();
        }}
      >
        <Field label="Anti-phishing code" required error={codeErr}>
          {(id, describedBy, invalid) => (
            <input
              id={id}
              aria-describedby={describedBy}
              aria-invalid={invalid}
              className={inputCls}
              maxLength={api.ANTI_PHISHING_MAX}
              value={code}
              onChange={(e) => {
                setCode(e.target.value);
              }}
            />
          )}
        </Field>
        <button type="submit" className={btnPrimary} disabled={mut.isPending || !valid}>
          {mut.isPending ? 'Saving…' : 'Save code'}
        </button>
      </form>
    </div>
  );
}

function LoginHistoryTable() {
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const q = useQuery({
    queryKey: ['account', 'login-history', cursor],
    queryFn: () => api.loginHistory(apiClient, cursor),
  });
  const rows = q.data?.data ?? [];

  return (
    <div className={cardCls}>
      <h3 className="mb-2 text-sm font-semibold">Login history</h3>
      {q.isError && <ErrorBox error={q.error} />}
      {q.isPending ? (
        <p className="text-sm text-neutral-400">Loading…</p>
      ) : rows.length === 0 && !q.isError ? (
        <p className="text-sm text-neutral-400">No login events recorded.</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>When</th>
              <th className={thCls}>Device</th>
              <th className={thCls}>IP</th>
              <th className={thCls}>Location</th>
              <th className={thCls}>Result</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((e, i) => (
              <tr key={e.id ?? i}>
                <td className={tdCls}>{new Date(e.timestamp).toLocaleString()}</td>
                <td className={tdCls}>{e.device ?? '—'}</td>
                <td className={tdCls}>{e.ip ?? '—'}</td>
                <td className={tdCls}>
                  {[e.geo_city, e.geo_country].filter(Boolean).join(', ') || '—'}
                </td>
                <td className={tdCls}>
                  {e.success ? (
                    <span className="text-emerald-400">success</span>
                  ) : (
                    <span className="text-red-400">failed</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {q.data?.next_cursor !== undefined && q.data.next_cursor !== '' && (
        <button
          type="button"
          className="mt-3 text-sm text-sky-400 hover:underline"
          onClick={() => {
            setCursor(q.data?.next_cursor);
          }}
        >
          Load older events
        </button>
      )}
    </div>
  );
}

export default function SecurityPanel() {
  return (
    <div className="space-y-4">
      <PasswordChangeForm />
      <TotpEnrollment />
      <WebAuthnPanel />
      <AntiPhishingForm />
      <div className={cardCls}>
        <h3 className="mb-2 text-sm font-semibold">Devices & sessions</h3>
        <p className="mb-3 text-sm text-neutral-400">
          Manage signed-in devices and revoke sessions you don’t recognize.
        </p>
        <Link to="/account/sessions" className="text-sm text-sky-400 hover:underline">
          Open session manager →
        </Link>
      </div>
      <LoginHistoryTable />
    </div>
  );
}
