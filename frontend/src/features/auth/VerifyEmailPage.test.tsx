import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp } from '@/test/accountMocks';

import VerifyEmailPage from './VerifyEmailPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
});

function renderAt(route: string) {
  return renderApp(
    <Routes>
      <Route path="/verify-email" element={<VerifyEmailPage />} />
      <Route path="/login" element={<div>login</div>} />
    </Routes>,
    route,
  );
}

describe('VerifyEmailPage', () => {
  it('consumes the link token on mount and confirms verification', async () => {
    const calls = installFetchMock({
      'POST /api/v1/auth/verify-email': {
        body: { user_id: 42, email_verified: true },
      },
    });
    renderAt('/verify-email?token=tok-abc');
    expect(await screen.findByText(/Email verified — your account is active/)).toBeInTheDocument();
    const post = calls.filter((c) => c.url.includes('/auth/verify-email'));
    expect(post).toHaveLength(1);
    expect(JSON.parse(post[0]?.init?.body as string)).toEqual({ token: 'tok-abc' });
    expect(screen.getByRole('link', { name: 'Continue to sign in' })).toBeInTheDocument();
  });

  it('accepts a manually pasted token when the URL carries none', async () => {
    const calls = installFetchMock({
      'POST /api/v1/auth/verify-email': {
        body: { user_id: 7, email_verified: true },
      },
    });
    renderAt('/verify-email');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/Verification token/), 'manual-tok');
    await user.click(screen.getByRole('button', { name: 'Verify email' }));
    await screen.findByText(/Email verified/);
    const post = calls.filter((c) => c.url.includes('/auth/verify-email'));
    expect(JSON.parse(post[0]?.init?.body as string)).toEqual({ token: 'manual-tok' });
  });

  it('surfaces the backend error verbatim on an expired token', async () => {
    installFetchMock({
      'POST /api/v1/auth/verify-email': {
        status: 410,
        body: { type: 'error', error: 'TOKEN_EXPIRED', message: 'verification link expired' },
      },
    });
    renderAt('/verify-email?token=stale');
    const alert = await screen.findByRole('alert');
    await waitFor(() => {
      expect(alert.textContent).toContain('verification link expired');
    });
    // Fail-visible retry: the expired link can be retried with a fresh
    // token after dismissing the error.
    expect(screen.getByText(/sign in to request a new verification email/)).toBeInTheDocument();
  });
});
