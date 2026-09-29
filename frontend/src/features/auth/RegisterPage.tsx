/**
 * Registration — Task 10.3.21 item 2 (spec §21.4 → §21.7).
 *
 * email + password (strength meter) + confirm + country + terms consent
 * → POST /api/v1/auth/register. Success renders the email-verification
 * interstitial; continuing routes to the KYC submission wizard (the
 * §21.4 "registration routes to KYC" contract).
 */
import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Link, useNavigate } from 'react-router';

import { apiClient } from '@/app/runtime';
import { ErrorBox, Field, btnPrimary, cardCls, inputCls, selectCls } from '@/lib/ui';

import * as api from './api';
import { passwordStrength } from './password';

const COUNTRIES = [
  'US',
  'GB',
  'DE',
  'FR',
  'ES',
  'IT',
  'NL',
  'CH',
  'SG',
  'JP',
  'AU',
  'CA',
  'AE',
  'HK',
] as const;

export default function RegisterPage() {
  const navigate = useNavigate();
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [country, setCountry] = useState('US');
  const [acceptTerms, setAcceptTerms] = useState(false);
  const [verificationSent, setVerificationSent] = useState(false);

  const strength = passwordStrength(password);
  const mismatch = confirm !== '' && confirm !== password;

  const mut = useMutation({
    mutationFn: () => api.register(apiClient, { email, password, country, acceptTerms }),
    onSuccess: (r) => {
      if (r.emailVerificationRequired) {
        setVerificationSent(true);
      } else {
        void navigate('/kyc?welcome=1');
      }
    },
  });

  if (verificationSent) {
    return (
      <div className="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center p-6">
        <div className={cardCls} role="status">
          <h1 className="mb-2 text-xl font-semibold">Verify your email</h1>
          <p className="mb-4 text-sm text-neutral-300">
            We sent a verification link to <strong>{email}</strong>. Confirm it to activate your
            account — the link expires shortly and you can request a new one by registering again.
          </p>
          <button
            type="button"
            className={`${btnPrimary} w-full`}
            onClick={() => {
              void navigate('/kyc?welcome=1');
            }}
          >
            Continue to identity verification
          </button>
          <p className="mt-3 text-center text-sm">
            <Link to="/login" className="text-sky-400 hover:underline">
              Back to sign in
            </Link>
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center p-6">
      <div className={cardCls}>
        <h1 className="mb-4 text-xl font-semibold">Create your account</h1>
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
          <Field
            label="Password"
            required
            hint="At least 12 characters with mixed case, a digit and a symbol recommended."
          >
            {(id, describedBy, invalid) => (
              <>
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
                {password !== '' && (
                  <div className="mt-1" role="status" aria-label="Password strength">
                    <div className="h-1.5 w-full rounded bg-neutral-800">
                      <div
                        className={`h-1.5 rounded transition-all ${
                          strength.score <= 1
                            ? 'bg-red-500'
                            : strength.score === 2
                              ? 'bg-amber-500'
                              : 'bg-emerald-500'
                        }`}
                        style={{ width: `${String((strength.score / 4) * 100)}%` }}
                      />
                    </div>
                    <p className="mt-1 text-xs text-neutral-500">{strength.label}</p>
                  </div>
                )}
              </>
            )}
          </Field>
          <Field
            label="Confirm password"
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
          <Field label="Country of residence" required>
            {(id, describedBy, invalid) => (
              <select
                id={id}
                aria-describedby={describedBy}
                aria-invalid={invalid}
                className={selectCls}
                value={country}
                onChange={(e) => {
                  setCountry(e.target.value);
                }}
              >
                {COUNTRIES.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <label className="mb-4 flex items-start gap-2 text-sm text-neutral-300">
            <input
              type="checkbox"
              className="mt-1"
              checked={acceptTerms}
              onChange={(e) => {
                setAcceptTerms(e.target.checked);
              }}
            />
            <span>
              I accept the{' '}
              <a
                href="/api/v1/execution-policy"
                target="_blank"
                rel="noreferrer"
                className="text-sky-400 hover:underline"
              >
                order execution policy
              </a>{' '}
              and consent to the processing of my data as described in the privacy notice.
            </span>
          </label>
          <button
            type="submit"
            className={`${btnPrimary} w-full`}
            disabled={
              mut.isPending ||
              email === '' ||
              password === '' ||
              mismatch ||
              !acceptTerms ||
              strength.score < 2
            }
          >
            {mut.isPending ? 'Creating account…' : 'Create account'}
          </button>
        </form>
        <p className="mt-4 text-center text-sm text-neutral-400">
          Already registered?{' '}
          <Link to="/login" className="text-sky-400 hover:underline">
            Sign in
          </Link>
        </p>
      </div>
    </div>
  );
}
