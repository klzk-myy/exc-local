import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes, useLocation } from 'react-router';

import { resetSessionForTests, useSessionStore } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import { ForgotPasswordPage, ResetPasswordPage } from './ForgotPasswordPage';
import { RequireAuth, RequireRole } from './guards';
import { safeRedirectTarget } from './redirect';
import LogoutPage from './LogoutPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
});

describe('ForgotPasswordPage', () => {
  it('posts the email and shows the 1-hour-expiry confirmation', async () => {
    const calls = installFetchMock({ 'POST /api/v1/auth/forgot-password': { body: {} } });
    renderApp(<ForgotPasswordPage />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Email/), 'lost@user.io');
    await user.click(screen.getByRole('button', { name: 'Send reset link' }));
    expect(await screen.findByRole('status')).toHaveTextContent('expires in 1 hour');
    const call = calls.find((c) => c.url.includes('forgot-password'));
    expect(JSON.parse(call?.init?.body as string)).toEqual({ email: 'lost@user.io' });
  });
});

describe('ResetPasswordPage', () => {
  it('rejects a link with no token', () => {
    renderApp(<ResetPasswordPage />, '/reset-password');
    expect(screen.getByRole('alert')).toHaveTextContent('missing its token');
  });

  it('posts token + password', async () => {
    const calls = installFetchMock({
      'POST /api/v1/auth/reset-password': { body: {} },
    });
    renderApp(
      <Routes>
        <Route path="/reset-password" element={<ResetPasswordPage />} />
        <Route path="/login" element={<div>login screen</div>} />
      </Routes>,
      '/reset-password?token=tok-abc',
    );
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/^New password/), 'NewPassw0rd!x');
    await user.type(screen.getByLabelText(/Confirm new password/), 'NewPassw0rd!x');
    await user.click(screen.getByRole('button', { name: 'Update password' }));
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('reset-password'))).toBe(true);
    });
    const call = calls.find((c) => c.url.includes('reset-password'));
    expect(JSON.parse(call?.init?.body as string)).toEqual({
      token: 'tok-abc',
      password: 'NewPassw0rd!x',
    });
    // lands back on the login screen
    expect(await screen.findByText('login screen')).toBeInTheDocument();
  });
});

describe('LogoutPage', () => {
  it('revokes server-side then clears the session store', async () => {
    signInForTests();
    const calls = installFetchMock({ 'POST /api/v1/auth/logout': { body: {} } });
    renderApp(
      <Routes>
        <Route path="/logout" element={<LogoutPage />} />
        <Route path="/login" element={<div>login screen</div>} />
      </Routes>,
      '/logout',
    );
    await waitFor(() => {
      expect(useSessionStore.getState().accessToken).toBeNull();
    });
    expect(calls.some((c) => c.url.includes('/auth/logout'))).toBe(true);
    expect(await screen.findByText('login screen')).toBeInTheDocument();
  });

  it('still clears local state when the revoke call fails', async () => {
    signInForTests();
    installFetchMock({ 'POST /api/v1/auth/logout': { status: 500, body: {} } });
    renderApp(<LogoutPage />, '/logout');
    await waitFor(() => {
      expect(useSessionStore.getState().accessToken).toBeNull();
    });
  });
});

describe('guards', () => {
  it('safeRedirectTarget keeps same-origin paths, rejects scheme tricks', () => {
    expect(safeRedirectTarget('/funding?x=1')).toBe('/funding?x=1');
    expect(safeRedirectTarget('//evil.com')).toBe('/');
    expect(safeRedirectTarget('https://evil.com')).toBe('/');
    expect(safeRedirectTarget(null)).toBe('/');
  });

  it('RequireAuth bounces to /login preserving the target', () => {
    function Probe() {
      const l = useLocation();
      return <div data-testid="loc">{`${l.pathname}${l.search}`}</div>;
    }
    renderApp(
      <Routes>
        <Route
          path="/funding"
          element={
            <RequireAuth>
              <div>secret</div>
            </RequireAuth>
          }
        />
        <Route path="/login" element={<Probe />} />
      </Routes>,
      '/funding?x=1',
    );
    expect(screen.getByTestId('loc')).toHaveTextContent('/login?redirect=%2Ffunding%3Fx%3D1');
  });

  it('RequireRole renders children for a matching role, alert otherwise', () => {
    signInForTests({ roles: ['Support Agent'] });
    renderApp(
      <RequireRole roles={['Support Agent']}>
        <div>staff stuff</div>
      </RequireRole>,
    );
    expect(screen.getByText('staff stuff')).toBeInTheDocument();
  });

  it('RequireRole denies without the role', () => {
    signInForTests({ roles: [] });
    renderApp(
      <RequireRole roles={['Super Admin']}>
        <div>staff stuff</div>
      </RequireRole>,
    );
    expect(screen.getByRole('alert')).toHaveTextContent('Insufficient permissions');
  });
});
