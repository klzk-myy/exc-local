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
import { ErrorBox, Field, btnPrimary, cardCls, inputCls } from '@/lib/ui';

import * as api from './api';
import { safeRedirectTarget } from './redirect';

export default function LoginPage() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const redirect = safeRedirectTarget(params.get('redirect'));

  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [rememberMe, setRememberMe] = useState(true);
  const [totpCode, setTotpCode] = useState('');
  const [challenge, setChallenge] = useState<string | null>(null);

  const mut = useMutation({
    mutationFn: () =>
      api.login(apiClient, {
        email,
        password,
        rememberMe,
        ...(totpCode !== '' ? { totpCode } : {}),
        ...(challenge !== null ? { challenge } : {}),
      }),
    onSuccess: (r) => {
      if (r.kind === 'totp') {
        setChallenge(r.challenge);
        setTotpCode('');
      } else {
        void navigate(redirect);
      }
    },
  });

  const totpStep = challenge !== null;

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
              <Field label="Authenticator code" required>
                {(id, describedBy, invalid) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    aria-invalid={invalid}
                    className={inputCls}
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    pattern="[0-9]{6}"
                    maxLength={6}
                    value={totpCode}
                    onChange={(e) => {
                      setTotpCode(e.target.value.replaceAll(/\D/g, '').slice(0, 6));
                    }}
                    autoFocus
                  />
                )}
              </Field>
            </>
          ) : (
            <>
              <Field label="Email" required>
                {(id, describedBy, invalid) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    aria-invalid={invalid}
                    className={inputCls}
                    type="email"
                    autoComplete="email"
                    value={email}
                    onChange={(e) => {
                      setEmail(e.target.value);
                    }}
                    autoFocus
                  />
                )}
              </Field>
              <Field label="Password" required>
                {(id, describedBy, invalid) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    aria-invalid={invalid}
                    className={inputCls}
                    type="password"
                    autoComplete="current-password"
                    value={password}
                    onChange={(e) => {
                      setPassword(e.target.value);
                    }}
                  />
                )}
              </Field>
              <label className="mb-4 flex items-center gap-2 text-sm text-neutral-300">
                <input
                  type="checkbox"
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
            disabled={
              mut.isPending || (totpStep ? totpCode.length !== 6 : email === '' || password === '')
            }
          >
            {mut.isPending ? 'Signing in…' : totpStep ? 'Verify code' : 'Sign in'}
          </button>
        </form>
        <div className="mt-4 flex justify-between text-sm">
          <Link to="/forgot-password" className="text-sky-400 hover:underline">
            Forgot password?
          </Link>
          <Link to="/register" className="text-sky-400 hover:underline">
            Create account
          </Link>
        </div>
      </div>
    </div>
  );
}
