/**
 * Forgot / reset password — Task 10.3.21 (Phase-12 Task 12.3.1: email
 * link, 1h expiry). Two screens:
 *   /forgot-password → request the reset email
 *   /reset-password?token=… → set the new password
 */
import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Link, useNavigate, useSearchParams } from 'react-router';

import { apiClient } from '@/app/runtime';
import { useValidatedField, type FieldRule } from '@/lib/input-helpers';
import { ErrorBox, Field, btnPrimary, cardCls, inputCls } from '@/lib/ui';

import * as api from './api';

const REQUIRED = (name: string, label: string): FieldRule => ({
  name,
  label,
  required: true,
  kind: 'string',
});

export function ForgotPasswordPage() {
  const email = useValidatedField(REQUIRED('email', 'Email'));
  const [sent, setSent] = useState(false);
  const mut = useMutation({
    mutationFn: () => api.forgotPassword(apiClient, email.value),
    onSuccess: () => {
      setSent(true);
    },
  });

  return (
    <div className="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center p-6">
      <div className={cardCls}>
        <h1 className="mb-4 text-xl font-semibold">Reset your password</h1>
        {sent ? (
          <p className="text-sm text-neutral-300" role="status">
            If an account exists for <strong>{email.value}</strong>, a reset link is on its way — it
            expires in 1 hour.
          </p>
        ) : (
          <>
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
              <Field label="Email" required error={email.error}>
                {(id, describedBy) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    className={inputCls}
                    type="email"
                    autoComplete="email"
                    {...email.inputProps}
                  />
                )}
              </Field>
              <button
                type="submit"
                className={`${btnPrimary} w-full`}
                disabled={mut.isPending || !email.valid}
              >
                {mut.isPending ? 'Sending…' : 'Send reset link'}
              </button>
            </form>
          </>
        )}
        <p className="mt-4 text-center text-sm">
          <Link to="/login" className="text-sky-400 hover:underline">
            Back to sign in
          </Link>
        </p>
      </div>
    </div>
  );
}

export function ResetPasswordPage() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const token = params.get('token') ?? '';
  const password = useValidatedField(REQUIRED('password', 'New password'));
  const confirm = useValidatedField(REQUIRED('confirm', 'Confirm new password'));
  // Cross-field rule — the shared framework has no equality combinator.
  const mismatch = confirm.value !== '' && confirm.value !== password.value;

  const mut = useMutation({
    mutationFn: () => api.resetPassword(apiClient, { token, password: password.value }),
    onSuccess: () => {
      void navigate('/login?reset=1');
    },
  });

  return (
    <div className="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center p-6">
      <div className={cardCls}>
        <h1 className="mb-4 text-xl font-semibold">Choose a new password</h1>
        {token === '' ? (
          <p className="text-sm text-red-300" role="alert">
            This reset link is missing its token — request a fresh one.
          </p>
        ) : (
          <>
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
              <Field label="New password" required error={password.error}>
                {(id, describedBy) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    className={inputCls}
                    type="password"
                    autoComplete="new-password"
                    {...password.inputProps}
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
                className={`${btnPrimary} w-full`}
                disabled={mut.isPending || !password.valid || mismatch}
              >
                {mut.isPending ? 'Updating…' : 'Update password'}
              </button>
            </form>
          </>
        )}
      </div>
    </div>
  );
}
