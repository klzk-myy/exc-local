import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import SettingsPage from './SettingsPage';
import { antiPhishingValid } from './api';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  signInForTests();
});

function renderSettings(tab = 'profile') {
  return renderApp(
    <Routes>
      <Route path="/settings" element={<SettingsPage />} />
      <Route path="/login" element={<div>login</div>} />
    </Routes>,
    `/settings?tab=${tab}`,
  );
}

describe('SettingsPage', () => {
  it('renders the profile tab and saves edits', async () => {
    const calls = installFetchMock({
      'GET /api/v1/account/profile': {
        body: { email: 't@x.io', display_name: 'Trader', mifid_category: 'RETAIL', kyc_tier: 'T1' },
      },
      'PUT /api/v1/account/profile': { body: {} },
    });
    renderSettings();
    const name = await screen.findByLabelText(/Display name/);
    expect(name).toHaveValue('Trader');
    expect(screen.getAllByText(/RETAIL/).length).toBeGreaterThan(0);

    const user = userEvent.setup();
    await user.clear(name);
    await user.type(name, 'New Name');
    await user.click(screen.getByRole('button', { name: 'Save profile' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'PUT' && c.url.includes('/account/profile'))).toBe(
        true,
      );
    });
    expect(await screen.findByRole('status')).toHaveTextContent('Profile saved');
  });

  it('switches to the security tab', async () => {
    installFetchMock({
      'GET /api/v1/account/profile': { body: {} },
      'GET /api/v1/account/login-history': { body: { data: [] } },
    });
    renderSettings();
    const user = userEvent.setup();
    await user.click(screen.getByRole('tab', { name: 'Security' }));
    expect((await screen.findAllByText('Change password')).length).toBeGreaterThan(0);
  });
});

describe('SecurityPanel', () => {
  it('changes password with old-password re-verification', async () => {
    const calls = installFetchMock({
      'POST /api/v1/account/change-password': { body: {} },
      'GET /api/v1/account/login-history': { body: { data: [] } },
    });
    renderSettings('security');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/Current password/), 'oldpw');
    await user.type(screen.getByLabelText(/^New password/), 'NewPassw0rd!');
    await user.type(screen.getByLabelText(/Confirm new password/), 'NewPassw0rd!');
    await user.click(screen.getByRole('button', { name: 'Change password' }));
    await waitFor(() => {
      const c = calls.find((x) => x.url.includes('change-password'));
      expect(JSON.parse(c?.init?.body as string)).toEqual({
        current_password: 'oldpw',
        new_password: 'NewPassw0rd!',
      });
    });
  });

  it('rejects an out-of-range anti-phishing code client-side', async () => {
    const calls = installFetchMock({
      'GET /api/v1/account/login-history': { body: { data: [] } },
    });
    renderSettings('security');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/Anti-phishing code/), 'abc');
    expect(screen.getByRole('alert')).toHaveTextContent('4–32 characters');
    expect(screen.getByRole('button', { name: 'Save code' })).toBeDisabled();
    expect(calls.some((c) => c.url.includes('anti-phishing'))).toBe(false);
  });

  it('submits a valid anti-phishing code', async () => {
    const calls = installFetchMock({
      'PUT /api/v1/account/settings/anti-phishing-code': { body: {} },
      'GET /api/v1/account/login-history': { body: { data: [] } },
    });
    renderSettings('security');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/Anti-phishing code/), 'purple-fox');
    await user.click(screen.getByRole('button', { name: 'Save code' }));
    await waitFor(() => {
      const c = calls.find((x) => x.url.includes('anti-phishing'));
      expect(JSON.parse(c?.init?.body as string)).toEqual({ code: 'purple-fox' });
    });
  });

  it('lists login history rows', async () => {
    installFetchMock({
      'GET /api/v1/account/login-history': {
        body: {
          data: [
            {
              id: '1',
              timestamp: '2026-01-02T09:00:00Z',
              device: 'Firefox on macOS',
              ip: '203.0.113.5',
              geo_city: 'Zurich',
              geo_country: 'CH',
              success: true,
            },
          ],
        },
      },
    });
    renderSettings('security');
    expect(await screen.findByText('Firefox on macOS')).toBeInTheDocument();
    expect(screen.getByText('Zurich, CH')).toBeInTheDocument();
  });
});

describe('ApiKeysPanel', () => {
  it('creates a key and shows the one-time secret', async () => {
    installFetchMock({
      'GET /api/v1/developer/api-keys': { body: { api_keys: [] } },
      'GET /api/v1/account/sub-accounts': { body: { data: [] } },
      'POST /api/v1/developer/api-keys': {
        status: 201,
        body: {
          key: {
            key_id: 'k_abc',
            label: 'bot',
            scopes: ['read'],
            rate_limit_tier: 'basic',
            status: 'active',
            created_at: '2026-01-01T00:00:00Z',
          },
          secret: 's3cr3t-once',
          notice: 'store the secret now; it is never shown again',
        },
      },
    });
    renderSettings('api-keys');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/^Key label/), 'bot');
    await user.click(screen.getByRole('button', { name: 'Create API key' }));
    expect(await screen.findByText('s3cr3t-once')).toBeInTheDocument();
  });

  it('revokes a key after confirmation', async () => {
    const calls = installFetchMock({
      'GET /api/v1/developer/api-keys': {
        body: {
          api_keys: [
            {
              key_id: 'k_abc',
              label: 'bot',
              scopes: ['read'],
              rate_limit_tier: 'basic',
              status: 'active',
              created_at: '2026-01-01T00:00:00Z',
            },
          ],
        },
      },
      'GET /api/v1/account/sub-accounts': { body: { data: [] } },
      'DELETE /api/v1/developer/api-keys/k_abc': { body: {} },
    });
    renderSettings('api-keys');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Revoke' }));
    await user.click(await screen.findByRole('button', { name: 'Revoke key' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE' && c.url.includes('k_abc'))).toBe(true);
    });
  });
});

describe('SafetyPanel', () => {
  it('emergency freeze requires the modal confirmation', async () => {
    const calls = installFetchMock({
      'POST /api/v1/account/emergency-freeze': { body: {} },
    });
    renderSettings('safety');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Freeze my account now' }));
    expect(calls.some((c) => c.url.includes('emergency-freeze'))).toBe(false);
    await user.click(await screen.findByRole('button', { name: 'Yes — freeze my account' }));
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('emergency-freeze'))).toBe(true);
    });
  });

  it('account closure requires all preconditions + typed CLOSE', async () => {
    const calls = installFetchMock({
      'POST /api/v1/account/close': { body: {} },
    });
    renderSettings('safety');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: /Begin account closure/ }));
    const confirmBtn = await screen.findByRole('button', { name: 'Close my account' });
    expect(confirmBtn).toBeDisabled();
    for (const box of screen.getAllByRole('checkbox')) {
      await user.click(box);
    }
    await user.type(screen.getByLabelText(/Type "CLOSE"/), 'CLOSE');
    expect(confirmBtn).toBeEnabled();
    await user.click(confirmBtn);
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('/account/close'))).toBe(true);
    });
  });
});

describe('NotificationsPanel', () => {
  it('toggles a matrix cell and PUTs the preferences', async () => {
    const calls = installFetchMock({
      'GET /api/v1/account/notifications/preferences': {
        body: { preferences: { order_fill: { email: true } } },
      },
      'PUT /api/v1/account/notifications/preferences': { body: {} },
      'PUT /api/v1/account/consent': { body: {} },
    });
    renderSettings('notifications');
    const cell = await screen.findByRole('checkbox', { name: 'Order fills via push' });
    const user = userEvent.setup();
    await user.click(cell);
    await user.click(screen.getByRole('button', { name: 'Save preferences' }));
    await waitFor(() => {
      const c = calls.find((x) => x.method === 'PUT' && x.url.includes('notifications'));
      expect(JSON.parse(c?.init?.body as string)).toEqual({
        preferences: { order_fill: { email: true, push: true } },
      });
    });
  });
});

describe('api helpers', () => {
  it('antiPhishingValid enforces the 4–32 range', () => {
    expect(antiPhishingValid('abc')).toBe(false);
    expect(antiPhishingValid('abcd')).toBe(true);
    expect(antiPhishingValid('x'.repeat(32))).toBe(true);
    expect(antiPhishingValid('x'.repeat(33))).toBe(false);
  });
});
