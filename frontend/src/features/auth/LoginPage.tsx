/**
 * Login — Task 10.3.21 item 1 (spec §21.4).
 *
 * email + password → POST /api/v1/auth/login; a `requires_totp` response
 * flips the form to the second factor step (challenge token resubmitted).
 * "Remember me" pins the session to localStorage (refresh survives a
 * browser restart); unchecked keeps it tab-scoped. Rate limits surface
 * via the RFC 7807 envelope's retry_after (ErrorBox).
 */
import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Link, useNavigate, useSearchParams } from 'react-router';

import { apiClient } from '@/app/runtime';
import { credentialToJSON, toRequestOptions, webauthnSupported } from '@/lib/auth/webauthn';
import { useValidatedField, type FieldRule } from '@/lib/input-helpers';
import { ErrorBox, Field, btnGhost, btnPrimary, cardCls, inputCls } from '@/lib/ui';

import * as api from './api';
import { safeRedirectTarget } from './redirect';

const REQUIRED = (name: string, label: string): FieldRule => ({
  name,
  label,
  required: true,
  kind: 'string',
});
/** TOTP code — the input strips non-digits; the rule enforces the
 * 6-digit contract (§12.3.2). */
const RULE_TOTP: FieldRule = {
  name: 'totp_code',
  label: 'Authenticator code',
  required: true,
  kind: 'string',
  pattern: /^\d{6}$/,
};

export default function LoginPage() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const redirect = safeRedirectTarget(params.get('redirect'));

  const email = useValidatedField(REQUIRED('email', 'Email'));
  const password = useValidatedField(REQUIRED('password', 'Password'));
  const totp = useValidatedField(RULE_TOTP);
  const [rememberMe, setRememberMe] = useState(true);
  const [challenge, setChallenge] = useState<string | null>(null);

  const mut = useMutation({
    mutationFn: () =>
      api.login(apiClient, {
        email: email.value,
        password: password.value,
        rememberMe,
        ...(totp.value !== '' ? { totpCode: totp.value } : {}),
        ...(challenge !== null ? { challenge } : {}),
      }),
    onSuccess: (r) => {
      if (r.kind === 'totp') {
        setChallenge(r.challenge);
        totp.reset();
      } else {
        void navigate(redirect);
      }
    },
  });

  const totpStep = challenge !== null;
  const passkeySupported = webauthnSupported();

  // POST /auth/passkey/assert: empty body → {challenge_id, publicKey};
  // browser assertion → {challenge_id, credential} → session bundle
  // (amr ["fido2"], two_factor_verified). Task 10.5.3.24.
  const passkey = useMutation({
    mutationFn: async () => {
      const ch = await api.passkeyAssertBegin(apiClient);
      const cred = await navigator.credentials.get({
        publicKey: toRequestOptions(ch.publicKey),
      });
      if (cred === null) throw new Error('passkey assertion cancelled');
      await api.passkeyAssertFinish(apiClient, ch.challengeId, credentialToJSON(cred), rememberMe);
    },
    onSuccess: () => {
      void navigate(redirect);
    },
  });

  return (
    <div className="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center p-6">
      <div className={cardCls}>
        <h1 className="mb-1 text-xl font-semibold">Sign in</h1>
        <p className="mb-4 text-xs text-neutral-500" data-testid="anti-phishing-slot">
          Official exchange emails always carry your anti-phishing code — set one in Settings →
          Security.
        </p>
        <ErrorBox
          error={mut.error}
          onDismiss={() => {
            mut.reset();
          }}
        />
        <form
          onSubmit={(e) => {
            e.preventDefault();
            mut.mutate();
          }}
        >
          {totpStep ? (
            <>
              <p className="mb-3 text-sm text-neutral-300">
                Two-factor authentication is enabled on this account. Enter the 6-digit code from
                your authenticator app.
              </p>
              <Field label="Authenticator code" required error={totp.error}>
                {(id, describedBy) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    className={inputCls}
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    pattern="[0-9]{6}"
                    maxLength={6}
                    {...totp.inputProps}
                    onChange={(e) => {
                      totp.setValue(e.target.value.replaceAll(/\D/g, '').slice(0, 6));
                    }}
                    autoFocus
                  />
                )}
              </Field>
            </>
          ) : (
            <>
              <Field label="Email" required error={email.error}>
                {(id, describedBy) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    className={inputCls}
                    type="email"
                    autoComplete="email"
                    {...email.inputProps}
                    autoFocus
                  />
                )}
              </Field>
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
              <label className="mb-4 flex items-center gap-2 text-sm text-neutral-300">
                <input
                  type="checkbox"
                  className="h-6 w-6 accent-sky-500"
                  checked={rememberMe}
                  onChange={(e) => {
                    setRememberMe(e.target.checked);
                  }}
                />
                Keep me signed in on this device
              </label>
            </>
          )}
          <button
            type="submit"
            className={`${btnPrimary} w-full`}
            disabled={mut.isPending || (totpStep ? !totp.valid : !email.valid || !password.valid)}
          >
            {mut.isPending ? 'Signing in…' : totpStep ? 'Verify code' : 'Sign in'}
          </button>
        </form>

        {!totpStep && (
          <div className="mt-4 border-t border-neutral-800 pt-4">
            <ErrorBox error={passkey.error} />
            <button
              type="button"
              className={`${btnGhost} w-full`}
              disabled={!passkeySupported || passkey.isPending}
              onClick={() => {
                passkey.mutate();
              }}
            >
              {passkey.isPending ? 'Waiting for passkey…' : 'Continue with a passkey'}
            </button>
            {!passkeySupported && (
              <p className="mt-2 text-xs text-neutral-500" role="status">
                Passkey sign-in requires a browser with WebAuthn support.
              </p>
            )}
          </div>
        )}

        <div className="mt-4 flex justify-between text-sm">
          <Link to="/forgot-password" className="text-sky-400 hover:underline">
            Forgot password?
          </Link>
          <Link to="/verify-email" className="text-sky-400 hover:underline">
            Verify email
          </Link>
          <Link to="/register" className="text-sky-400 hover:underline">
            Create account
          </Link>
        </div>
      </div>
    </div>
  );
}
