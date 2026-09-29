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
import { ErrorBox, Field, btnPrimary, cardCls, inputCls } from '@/lib/ui';

import * as api from './api';

export function ForgotPasswordPage() {
  const [email, setEmail] = useState('');
  const [sent, setSent] = useState(false);
  const mut = useMutation({
    mutationFn: () => api.forgotPassword(apiClient, email),
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
            If an account exists for <strong>{email}</strong>, a reset link is on its way — it
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
                  />
                )}
              </Field>
              <button
                type="submit"
                className={`${btnPrimary} w-full`}
                disabled={mut.isPending || email === ''}
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
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const mismatch = confirm !== '' && confirm !== password;

  const mut = useMutation({
    mutationFn: () => api.resetPassword(apiClient, { token, password }),
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
              <Field label="New password" required>
                {(id, describedBy, invalid) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    aria-invalid={invalid}
                    className={inputCls}
                    type="password"
                    autoComplete="new-password"
                    value={password}
                    onChange={(e) => {
                      setPassword(e.target.value);
                    }}
                  />
                )}
              </Field>
              <Field
                label="Confirm new password"
                required
                error={mismatch ? 'Passwords do not match' : null}
              >
                {(id, describedBy, invalid) => (
                  <input
                    id={id}
                    aria-describedby={describedBy}
                    aria-invalid={invalid || mismatch}
                    className={inputCls}
                    type="password"
                    autoComplete="new-password"
                    value={confirm}
                    onChange={(e) => {
                      setConfirm(e.target.value);
                    }}
                  />
                )}
              </Field>
              <button
                type="submit"
                className={`${btnPrimary} w-full`}
                disabled={mut.isPending || password === '' || mismatch}
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
