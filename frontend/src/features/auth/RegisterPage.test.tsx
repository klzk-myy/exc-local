import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp } from '@/test/accountMocks';

import RegisterPage from './RegisterPage';
import { passwordStrength } from './password';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
});

describe('passwordStrength', () => {
  it('scores length + character classes', () => {
    expect(passwordStrength('').score).toBe(0);
    expect(passwordStrength('short').score).toBeLessThan(2);
    expect(passwordStrength('LongPassw0rd!xyz').score).toBe(4);
  });
});

describe('RegisterPage', () => {
  it('registers and shows the email-verification interstitial', async () => {
    installFetchMock({
      'POST /api/v1/auth/register': { status: 201, body: { email_verification_required: true } },
    });
    renderApp(<RegisterPage />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/^Email/), 'new@user.io');
    await user.type(screen.getByLabelText(/^Password/), 'LongPassw0rd!xyz');
    await user.type(screen.getByLabelText(/Confirm password/), 'LongPassw0rd!xyz');
    await user.click(screen.getByRole('checkbox'));
    await user.click(screen.getByRole('button', { name: 'Create account' }));
    expect(await screen.findByRole('status')).toHaveTextContent('We sent a verification link');
  });

  it('blocks submit while terms are unchecked', async () => {
    const calls = installFetchMock({});
    renderApp(<RegisterPage />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/^Email/), 'new@user.io');
    await user.type(screen.getByLabelText(/^Password/), 'LongPassw0rd!xyz');
    await user.type(screen.getByLabelText(/Confirm password/), 'LongPassw0rd!xyz');
    expect(screen.getByRole('button', { name: 'Create account' })).toBeDisabled();
    expect(calls).toHaveLength(0);
  });

  it('shows a mismatch error on divergent confirmation', async () => {
    renderApp(<RegisterPage />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/^Password/), 'LongPassw0rd!xyz');
    await user.type(screen.getByLabelText(/Confirm password/), 'different');
    expect(await screen.findByRole('alert')).toHaveTextContent('Passwords do not match');
  });
});
