import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests, useSessionStore } from '@/lib/auth/session';
import { installFetchMock, renderApp } from '@/test/accountMocks';

import LoginPage from './LoginPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
});

describe('LoginPage', () => {
  it('submits credentials and stores the session', async () => {
    const calls = installFetchMock({
      'POST /api/v1/auth/login': {
        status: 200,
        body: {
          access_token: 'at-1',
          refresh_token: 'rt-1',
          expires_in: 900,
          user: { email: 'a@b.c' },
        },
      },
    });
    renderApp(<LoginPage />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Email/), 'a@b.c');
    await user.type(screen.getByLabelText(/^Password/), 'pw12345');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));

    await waitFor(() => {
      expect(useSessionStore.getState().accessToken).toBe('at-1');
    });
    expect(useSessionStore.getState().refreshToken).toBe('rt-1');
    const loginCall = calls.find((c) => c.url.includes('/auth/login'));
    expect(JSON.parse(loginCall?.init?.body as string)).toMatchObject({
      email: 'a@b.c',
      password: 'pw12345',
    });
  });

  it('shows the TOTP second step when requires_totp, then resubmits with the code', async () => {
    const calls = installFetchMock({
      'POST /api/v1/auth/login': {
        handler: (_url, init) => {
          const body = JSON.parse(init?.body as string) as Record<string, unknown>;
          if (body['totp_code'] === '123456') {
            return { status: 200, body: { access_token: 'at-2', refresh_token: 'rt-2' } };
          }
          return { status: 200, body: { requires_totp: true, challenge: 'ch-9' } };
        },
      },
    });
    renderApp(<LoginPage />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Email/), 'a@b.c');
    await user.type(screen.getByLabelText(/^Password/), 'pw');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));

    const code = await screen.findByLabelText(/Authenticator code/);
    await user.type(code, '123456');
    await user.click(screen.getByRole('button', { name: 'Verify code' }));

    await waitFor(() => {
      expect(useSessionStore.getState().accessToken).toBe('at-2');
    });
    const second = calls.filter((c) => c.url.includes('/auth/login'))[1];
    expect(JSON.parse(second?.init?.body as string)).toMatchObject({
      totp_code: '123456',
      challenge: 'ch-9',
    });
  });

  it('surfaces rate-limit errors with retry_after', async () => {
    installFetchMock({
      'POST /api/v1/auth/login': {
        status: 429,
        body: {
          type: 'error',
          error: 'RATE_LIMIT_TIER_EXCEEDED',
          message: 'Too many login attempts',
          status: 429,
          retry_after: 30,
        },
      },
    });
    renderApp(<LoginPage />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Email/), 'a@b.c');
    await user.type(screen.getByLabelText(/^Password/), 'pw');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many login attempts');
    expect(screen.getByRole('alert')).toHaveTextContent('retry after 30s');
  });
});
