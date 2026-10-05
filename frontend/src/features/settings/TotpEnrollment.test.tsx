import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import TotpEnrollment from './TotpEnrollment';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
  signInForTests();
});

describe('TotpEnrollment', () => {
  it('starts the ceremony via /2fa/enroll and re-stages via /2fa/setup', async () => {
    const calls = installFetchMock({
      'POST /api/v1/auth/2fa/enroll': {
        body: { secret: 'SECRET-ONE', otpauth_uri: 'otpauth://totp/x?secret=SECRET-ONE' },
      },
      'POST /api/v1/auth/2fa/setup': {
        body: { secret: 'SECRET-TWO', otpauth_uri: 'otpauth://totp/x?secret=SECRET-TWO' },
      },
    });
    renderApp(<TotpEnrollment />);
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Set up 2FA' }));
    expect(await screen.findByText('SECRET-ONE')).toBeInTheDocument();
    // Re-staging a candidate hits /setup — the staged secret changes.
    await user.click(screen.getByRole('button', { name: 'Get a new secret' }));
    expect(await screen.findByText('SECRET-TWO')).toBeInTheDocument();
    const paths = calls
      .filter((c) => c.url.includes('/2fa/'))
      .map((c) => new URL(c.url, 'http://t').pathname);
    expect(paths).toEqual(['/api/v1/auth/2fa/enroll', '/api/v1/auth/2fa/setup']);
  });

  it('verifies the first code and shows backup codes once', async () => {
    const calls = installFetchMock({
      'POST /api/v1/auth/2fa/enroll': {
        body: { secret: 'S1', otpauth_uri: 'otpauth://totp/x?secret=S1' },
      },
      'POST /api/v1/auth/2fa/verify': { body: { backup_codes: ['BK-1', 'BK-2'] } },
    });
    renderApp(<TotpEnrollment />);
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Set up 2FA' }));
    await screen.findByText('S1');
    await user.type(screen.getByLabelText(/Authenticator code/), '123456');
    await user.click(screen.getByRole('button', { name: 'Verify & activate' }));
    expect(await screen.findByText('BK-1')).toBeInTheDocument();
    const verify = calls.find((c) => c.url.includes('/2fa/verify'));
    expect(JSON.parse(verify?.init?.body as string)).toEqual({ code: '123456' });
    await waitFor(() => {
      expect(screen.queryByText('Verify & activate')).not.toBeInTheDocument();
    });
  });
});
