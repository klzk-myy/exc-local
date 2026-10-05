/**
 * TOTP enrollment card — Task 10.3.22 item (2FA in security center) and
 * Task 10.3.21 item 2 (setup/verify/disable, one-time backup codes).
 * Endpoints (Phase-12 Task 12.3.2):
 *   POST /auth/2fa/enroll  → {secret, otpauth_uri} — initial ceremony
 *   POST /auth/2fa/setup   → {secret, otpauth_uri} — re-stage candidate
 *   POST /auth/2fa/verify  → {backup_codes} — displayed exactly once
 *   POST /auth/2fa/disable → {password, code}
 */
import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import * as authApi from '@/features/auth/api';
import { useValidatedField, type FieldRule } from '@/lib/input-helpers';
import {
  CopyButton,
  ErrorBox,
  Field,
  QrBlock,
  btnDanger,
  btnGhost,
  btnPrimary,
  cardCls,
  inputCls,
} from '@/lib/ui';

const REQUIRED = (name: string, label: string): FieldRule => ({
  name,
  label,
  required: true,
  kind: 'string',
});

type Phase =
  | { kind: 'idle' }
  | { kind: 'enrolling'; secret: string; otpauthUri: string }
  | { kind: 'done'; backupCodes: string[] };

export default function TotpEnrollment() {
  const [phase, setPhase] = useState<Phase>({ kind: 'idle' });
  const code = useValidatedField(REQUIRED('totp_code', 'Authenticator code'));
  const password = useValidatedField(REQUIRED('password', 'Password'));
  const disableCode = useValidatedField(REQUIRED('totp_code', 'Authenticator code'));
  const [disabling, setDisabling] = useState(false);

  // /2fa/enroll starts the ceremony; /2fa/setup re-stages a new candidate
  // secret mid-enrollment (same staging semantics — both are wired per
  // Task 10.5.3.24 and remain non-destructive until /verify proves the
  // first code).
  const enroll = useMutation({ mutationFn: () => authApi.totpEnroll(apiClient) });
  const setup = useMutation({ mutationFn: () => authApi.totpSetup(apiClient) });
  const verify = useMutation({
    mutationFn: (c: string) => authApi.totpVerify(apiClient, c),
    onSuccess: (r) => {
      setPhase({ kind: 'done', backupCodes: r.backupCodes });
      code.reset();
    },
  });
  const disable = useMutation({
    mutationFn: () => authApi.totpDisable(apiClient, password.value, disableCode.value),
    onSuccess: () => {
      setDisabling(false);
      password.reset();
      disableCode.reset();
      setPhase({ kind: 'idle' });
    },
  });

  return (
    <div className={cardCls}>
      <h3 className="mb-2 text-sm font-semibold">Two-factor authentication (TOTP)</h3>

      {phase.kind === 'idle' && !disabling && (
        <>
          <p className="mb-3 text-sm text-neutral-400">
            Add an authenticator app (TOTP) as a second sign-in factor. Required before setting an
            anti-phishing code.
          </p>
          <ErrorBox error={enroll.error} />
          <button
            type="button"
            className={btnPrimary}
            disabled={enroll.isPending}
            onClick={() => {
              void enroll.mutateAsync().then((r) => {
                setPhase({ kind: 'enrolling', secret: r.secret, otpauthUri: r.otpauthUri });
              });
            }}
          >
            {enroll.isPending ? 'Starting…' : 'Set up 2FA'}
          </button>
          <button
            type="button"
            className={`${btnGhost} ml-2`}
            onClick={() => {
              setDisabling(true);
            }}
          >
            Disable 2FA…
          </button>
        </>
      )}

      {phase.kind === 'enrolling' && (
        <div>
          <p className="mb-2 text-sm text-neutral-300">
            Scan with your authenticator, or enter the secret manually:
          </p>
          {phase.otpauthUri !== '' && (
            <div className="mb-3">
              <QrBlock value={phase.otpauthUri} label="Authenticator QR code" />
            </div>
          )}
          <p className="mb-3 flex items-center gap-2 break-all font-mono text-xs text-neutral-300">
            {phase.secret}
            <CopyButton text={phase.secret} label="Copy secret" />
          </p>
          <p className="mb-3 text-xs">
            <ErrorBox error={setup.error} />
            <button
              type="button"
              className="text-sky-400 hover:underline"
              disabled={setup.isPending}
              onClick={() => {
                void setup.mutateAsync().then((r) => {
                  setPhase({ kind: 'enrolling', secret: r.secret, otpauthUri: r.otpauthUri });
                  code.reset();
                });
              }}
            >
              {setup.isPending ? 'Staging…' : 'Get a new secret'}
            </button>
          </p>
          <ErrorBox error={verify.error} />
          <form
            onSubmit={(e) => {
              e.preventDefault();
              verify.mutate(code.value);
            }}
          >
            <Field label="Authenticator code" required error={code.error}>
              {(id, describedBy) => (
                <input
                  id={id}
                  aria-describedby={describedBy}
                  className={inputCls}
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  {...code.inputProps}
                />
              )}
            </Field>
            <button type="submit" className={btnPrimary} disabled={verify.isPending || !code.valid}>
              {verify.isPending ? 'Verifying…' : 'Verify & activate'}
            </button>
          </form>
        </div>
      )}

      {phase.kind === 'done' && (
        <div role="status">
          <p className="mb-2 text-sm font-medium text-emerald-400">
            2FA is active. Store these backup codes now — they are shown once.
          </p>
          {phase.backupCodes.length > 0 && (
            <ul className="mb-3 grid grid-cols-2 gap-1 font-mono text-sm text-neutral-200">
              {phase.backupCodes.map((c) => (
                <li key={c}>{c}</li>
              ))}
            </ul>
          )}
          {phase.backupCodes.length > 0 && (
            <CopyButton text={phase.backupCodes.join('\n')} label="Copy backup codes" />
          )}
          <button
            type="button"
            className={`${btnGhost} ml-2`}
            onClick={() => {
              setPhase({ kind: 'idle' });
            }}
          >
            Done
          </button>
        </div>
      )}

      {disabling && (
        <div>
          <p className="mb-2 text-sm text-neutral-300">
            Disabling 2FA requires your password and a current authenticator code.
          </p>
          <ErrorBox error={disable.error} />
          <form
            onSubmit={(e) => {
              e.preventDefault();
              disable.mutate();
            }}
          >
            <Field label="Password" required error={password.error}>
              {(id, describedBy) => (
                <input
                  id={id}
                  aria-describedby={describedBy}
                  className={inputCls}
                  type="password"
                  autoComplete="current-password"
                  {...password.inputProps}
                />
              )}
            </Field>
            <Field label="Authenticator code" required error={disableCode.error}>
              {(id, describedBy) => (
                <input
                  id={id}
                  aria-describedby={describedBy}
                  className={inputCls}
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  {...disableCode.inputProps}
                />
              )}
            </Field>
            <button
              type="submit"
              className={btnDanger}
              disabled={disable.isPending || !password.valid || !disableCode.valid}
            >
              {disable.isPending ? 'Disabling…' : 'Disable 2FA'}
            </button>
            <button
              type="button"
              className={`${btnGhost} ml-2`}
              onClick={() => {
                setDisabling(false);
              }}
            >
              Cancel
            </button>
          </form>
        </div>
      )}
    </div>
  );
}
