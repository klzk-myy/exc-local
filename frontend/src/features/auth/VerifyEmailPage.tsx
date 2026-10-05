/**
 * Verify-email landing page — Task 10.5.3.24 (Phase-12 Task 12.3.5).
 *
 * The emailed link lands at /verify-email?token=…; the token is submitted
 * once on mount to POST /auth/verify-email. A manual paste field covers
 * clients that strip the query string. The route registry exposes no
 * resend-verification endpoint — an expired token surfaces the backend
 * error verbatim and points back to sign-in/registration (fail-visible;
 * recorded as a contract gap in the delivery record).
 */
import { useEffect, useRef, useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Link, useSearchParams } from 'react-router';

import { apiClient } from '@/app/runtime';
import { ErrorBox, Field, btnPrimary, cardCls, inputCls } from '@/lib/ui';

import * as api from './api';

export default function VerifyEmailPage() {
  const [params] = useSearchParams();
  const urlToken = params.get('token') ?? '';
  const [manual, setManual] = useState('');
  // StrictMode double-mount must not consume the token twice — guard with
  // a ref keyed on the token value.
  const submitted = useRef<string | null>(null);

  const mut = useMutation({
    mutationFn: (token: string) => api.verifyEmail(apiClient, token),
  });

  useEffect(() => {
    if (urlToken === '' || submitted.current === urlToken) return;
    submitted.current = urlToken;
    mut.mutate(urlToken);
  }, [urlToken, mut]);

  return (
    <div className="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center p-6">
      <div className={cardCls}>
        <h1 className="mb-1 text-xl font-semibold">Verify your email</h1>

        {mut.isPending && (
          <p className="text-sm text-neutral-400" role="status">
            Verifying your email…
          </p>
        )}

        {mut.isSuccess && (
          <div role="status">
            <p className="mb-4 text-sm text-emerald-400">
              Email verified — your account is active.
            </p>
            <Link to="/login" className={`${btnPrimary} inline-block`}>
              Continue to sign in
            </Link>
          </div>
        )}

        {mut.isError && (
          <div>
            <ErrorBox
              error={mut.error}
              onDismiss={() => {
                mut.reset();
                submitted.current = null;
              }}
            />
            <p className="mb-3 text-sm text-neutral-400">
              The link may have expired. Paste a fresh token below, or sign in to request a new
              verification email.
            </p>
          </div>
        )}

        {!mut.isPending && !mut.isSuccess && (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              const token = urlToken !== '' ? urlToken : manual.trim();
              if (token === '') return;
              submitted.current = token;
              mut.mutate(token);
            }}
          >
            {urlToken === '' && (
              <Field label="Verification token" required>
                {(id, describedBy, invalid) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    aria-invalid={invalid}
                    className={inputCls}
                    autoComplete="off"
                    value={manual}
                    onChange={(e) => {
                      setManual(e.target.value);
                    }}
                  />
                )}
              </Field>
            )}
            <button
              type="submit"
              className={btnPrimary}
              disabled={urlToken === '' && manual.trim() === ''}
            >
              Verify email
            </button>
          </form>
        )}

        <div className="mt-4 flex justify-between text-sm">
          <Link to="/login" className="text-sky-400 hover:underline">
            Sign in
          </Link>
          <Link to="/register" className="text-sky-400 hover:underline">
            Create account
          </Link>
        </div>
      </div>
    </div>
  );
}
