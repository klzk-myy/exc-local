/**
 * axe-core WCAG 2.1 AA audit — Phase-10 Task 10.3.14.
 *
 * Covers every Phase-10 DoD row that gates on "axe-core audit not run":
 *   - Task 10.3.20  ops routes (fleet / releases / ops board)
 *   - Task 10.3.21  auth screens (login, register, forgot, sessions, TOTP)
 *   - Task 10.3.22  settings screens (all five tabs)
 *   - Task 10.3.23  funding screens (all four tabs)
 *   - Task 10.3.24  KYC screens (status tracker + upload wizard)
 *   - Task 10.3.25  support screens (list, form modal, ticket thread)
 *   - Task 10.3.26  strategy/bot screens (browser + grid bots)
 *   - Task 10.3.27  order-history screens (all six tabs)
 *   - Task 10.3.28  report screens (all five tabs)
 *   - Task 10.3.29  input-helper framework (fields, autocomplete, confirm,
 *                   help, shortcuts overlay, preview, availability)
 *
 * jsdom limitation: rules requiring rendered layout (e.g. `color-contrast`)
 * cannot be evaluated here — axe reports them `incomplete`. The per-suite
 * notes are logged via `console.info` so CI output shows what was and was
 * not machine-checkable.
 */
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';

import { resetSessionForTests } from '@/lib/auth/session';
import { useAdminEnvStore } from '@/lib/env';
import {
  CONFIRM_PHRASES,
  ConfirmModal,
  Autocomplete,
  AmountPresets,
  GlossaryList,
  HelpTooltip,
  InputField,
  PreviewPanel,
  ShortcutHelpOverlay,
  UnavailablePanel,
  fieldHelp,
  shortcutRegistry,
  useValidatedField,
} from '@/lib/input-helpers';
import { RULE_SYMBOL } from '@/lib/input-helpers/validation';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';
import { expectNoAxeViolations } from '@/test/axe';

import LoginPage from '@/features/auth/LoginPage';
import RegisterPage from '@/features/auth/RegisterPage';
import { ForgotPasswordPage } from '@/features/auth/ForgotPasswordPage';
import { SessionList } from '@/features/auth/SessionList';
import CopyGridPage from '@/features/copy-grid/CopyGridPage';
import FundingPage from '@/features/funding/FundingPage';
import HistoryPage from '@/features/history/HistoryPage';
import KycPage from '@/features/kyc/KycPage';
import FleetPage from '@/features/ops/FleetPage';
import OpsBoardPage from '@/features/ops/OpsBoardPage';
import ReleasesPage from '@/features/ops/ReleasesPage';
import ReportsPage from '@/features/reports/ReportsPage';
import SettingsPage from '@/features/settings/SettingsPage';
import SupportPage from '@/features/support/SupportPage';
import TicketDetailPage from '@/features/support/TicketDetailPage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const STUB_501 = {
  status: 501,
  body: { type: 'error', error: 'NOT_IMPLEMENTED', message: 'stub', status: 501 },
};

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  resetSessionForTests();
  useAdminEnvStore.setState({ env: 'dev', pendingEnv: null });
});

async function audit(label: string) {
  // Give react-query settles + lazy tabs a tick, then axe the whole body so
  // portaled content (modals, overlays) is included.
  await waitFor(() => expect(document.body.textContent ?? '').not.toBe(''));
  const incomplete = await expectNoAxeViolations(document.body);
  if (incomplete !== '') console.info(`[a11y] ${label} incomplete rules: ${incomplete}`);
}

// ---------------------------------------------------------------------------
// Task 10.3.20 — ops routes
// ---------------------------------------------------------------------------

const HOSTS = {
  hosts: [
    {
      id: 7,
      env: 'dev',
      hostname: 'dev-matcher-0',
      role: 'matcher',
      shard_id: 0,
      az: 'az-a',
      health: 'healthy',
      state: 'ACTIVE',
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    },
  ],
  env: 'dev',
  count: 1,
};

const TOPOLOGY = {
  env: 'dev',
  shards: { '0': [{ id: 7, hostname: 'dev-matcher-0' }] },
  roles: { matcher: ['dev-matcher-0'] },
  health: { healthy: 1 },
};

const RELEASES = {
  releases: [
    {
      id: 3,
      component: 'matcher',
      version: '1.4.2',
      artifact_hash: 'abcdef0123456789',
      env: 'dev',
      status: 'DEPLOYED',
      gate_evidence: { soak: { status: 'ok' } },
      created_by: 5,
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
    },
  ],
  env: 'dev',
  count: 1,
};

const OPS_HEALTH = {
  status: 'operational',
  mode: 'Normal',
  components: [
    { name: 'gateway', state: 'operational', critical: true, latency_ms: 1.4 },
    { name: 'matcher-0', state: 'degraded', critical: true, latency_ms: 9.9, detail: 'lagging' },
  ],
  source: 'aggregator',
  updated_at: '2026-01-02T00:00:00Z',
};

describe('a11y: ops routes (Task 10.3.20)', () => {
  beforeEach(() => {
    signInForTests({ roles: ['Super Admin'] });
  });

  it('FleetPage passes wcag21aa', async () => {
    installFetchMock({
      'GET /api/v1/admin/fleet/hosts': { body: HOSTS },
      'GET /api/v1/admin/fleet/topology': { body: TOPOLOGY },
    });
    renderApp(<FleetPage />);
    await screen.findByText('dev-matcher-0');
    await audit('FleetPage');
  });

  it('ReleasesPage passes wcag21aa', async () => {
    installFetchMock({
      'GET /api/v1/admin/releases': { body: RELEASES },
    });
    renderApp(<ReleasesPage />);
    await screen.findByText('matcher');
    await audit('ReleasesPage');
  });

  it('OpsBoardPage passes wcag21aa', async () => {
    installFetchMock({
      'GET /api/v1/system/status': { body: OPS_HEALTH },
      'GET /api/v1/admin/ops/health': {
        body: { mode: 'Normal', components: { gateway: { state: 'up' } }, recent_events: [] },
      },
    });
    renderApp(<OpsBoardPage />);
    await screen.findByText('matcher-0');
    await audit('OpsBoardPage');
  });
});

// ---------------------------------------------------------------------------
// Task 10.3.21 — auth screens
// ---------------------------------------------------------------------------

describe('a11y: auth screens (Task 10.3.21)', () => {
  function renderAuth() {
    return renderApp(
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/register" element={<RegisterPage />} />
        <Route path="/forgot-password" element={<ForgotPasswordPage />} />
        <Route path="/settings" element={<div>settings</div>} />
      </Routes>,
      '/login',
    );
  }

  it('LoginPage passes wcag21aa', async () => {
    installFetchMock({});
    renderAuth();
    await screen.findByLabelText(/Email/);
    await audit('LoginPage');
  });

  it('LoginPage TOTP step passes wcag21aa', async () => {
    installFetchMock({
      'POST /api/v1/auth/login': {
        body: { requires_totp: true, challenge: 'ch-9' },
      },
    });
    renderAuth();
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/Email/), 'a@b.c');
    await user.type(screen.getByLabelText(/^Password/), 'pw');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await screen.findByLabelText(/Authenticator code/);
    await audit('LoginPage TOTP step');
  });

  it('RegisterPage passes wcag21aa', async () => {
    installFetchMock({});
    renderApp(
      <Routes>
        <Route path="/register" element={<RegisterPage />} />
        <Route path="/login" element={<div>login</div>} />
      </Routes>,
      '/register',
    );
    await screen.findByLabelText(/^Email/);
    await audit('RegisterPage');
  });

  it('ForgotPasswordPage passes wcag21aa', async () => {
    installFetchMock({});
    renderApp(
      <Routes>
        <Route path="/forgot-password" element={<ForgotPasswordPage />} />
        <Route path="/login" element={<div>login</div>} />
      </Routes>,
      '/forgot-password',
    );
    await screen.findByLabelText(/Email/);
    await audit('ForgotPasswordPage');
  });

  it('SessionList passes wcag21aa', async () => {
    signInForTests();
    installFetchMock({
      'GET /api/v1/account/sessions': {
        body: {
          data: [
            {
              id: 's-1',
              device: 'Firefox on macOS',
              ip: '203.0.113.5',
              geo_city: 'Zurich',
              geo_country: 'CH',
              created_at: '2026-01-01T08:00:00Z',
              last_active_at: '2026-01-02T09:00:00Z',
              current: true,
            },
            {
              id: 's-2',
              device: 'Safari on iPhone',
              ip: '203.0.113.6',
              created_at: '2026-01-01T08:00:00Z',
              last_active_at: '2026-01-02T09:00:00Z',
              current: false,
            },
          ],
        },
      },
    });
    renderApp(<SessionList />);
    await screen.findByText('Firefox on macOS');
    await audit('SessionList');
  });
});

// ---------------------------------------------------------------------------
// Task 10.3.22 — settings screens
// ---------------------------------------------------------------------------

function renderSettings(tab: string) {
  return renderApp(
    <Routes>
      <Route path="/settings" element={<SettingsPage />} />
      <Route path="/login" element={<div>login</div>} />
    </Routes>,
    `/settings?tab=${tab}`,
  );
}

describe('a11y: settings screens (Task 10.3.22)', () => {
  beforeEach(() => {
    signInForTests();
    installFetchMock({
      'GET /api/v1/account/profile': {
        body: { email: 't@x.io', display_name: 'Trader', mifid_category: 'RETAIL', kyc_tier: 'T1' },
      },
      'GET /api/v1/account/login-history': { body: { data: [] } },
      'GET /api/v1/developer/api-keys': { body: { data: [] } },
      'GET /api/v1/account/sub-accounts': { body: { data: [] } },
      'GET /api/v1/account/notifications/preferences': { body: { preferences: {} } },
    });
  });

  it('profile tab passes wcag21aa', async () => {
    renderSettings('profile');
    await screen.findByLabelText(/Display name/);
    await audit('Settings:profile');
  });

  it('security tab passes wcag21aa', async () => {
    renderSettings('security');
    await screen.findAllByText('Change password');
    await audit('Settings:security');
  });

  it('api-keys tab passes wcag21aa', async () => {
    renderSettings('api-keys');
    await waitFor(() => expect(document.body.textContent ?? '').toMatch(/api key/i));
    await audit('Settings:api-keys');
  });

  it('notifications tab passes wcag21aa', async () => {
    renderSettings('notifications');
    await waitFor(() => expect(document.body.textContent ?? '').toMatch(/notification/i));
    await audit('Settings:notifications');
  });

  it('safety tab passes wcag21aa', async () => {
    renderSettings('safety');
    await waitFor(() => expect(document.body.textContent ?? '').toMatch(/freeze|close|data/i));
    await audit('Settings:safety');
  });
});

// ---------------------------------------------------------------------------
// Task 10.3.23 — funding screens
// ---------------------------------------------------------------------------

const BALANCES = {
  account_id: 1001,
  balances: [{ currency: 'USD', available: '25000', locked: '0', total: '25000' }],
};

function renderFunding(tab: string) {
  return renderApp(
    <Routes>
      <Route path="/funding" element={<FundingPage />} />
      <Route path="/login" element={<div>login</div>} />
    </Routes>,
    `/funding?tab=${tab}`,
  );
}

describe('a11y: funding screens (Task 10.3.23)', () => {
  beforeEach(() => {
    signInForTests();
    installFetchMock({
      'GET /api/v1/account/balances': { body: BALANCES },
      'GET /api/v1/deposits/USD': {
        body: {
          currency: 'USD',
          account_id: 1001,
          reference: 'EXC-1001-USD',
          instructions: [
            {
              id: 1,
              currency: 'USD',
              bank_name: 'Bank of Test',
              bank_code: 'TESTUS33',
              account_number: '12345',
              status: 'ACTIVE',
            },
          ],
        },
      },
      'GET /api/v1/funding*': { body: { data: [], total: 0 } },
      'GET /api/v1/transfers': { body: { data: [], next_cursor: '' } },
      'GET /api/v1/account/sub-accounts': { body: { data: [] } },
    });
  });

  it('deposit tab passes wcag21aa', async () => {
    renderFunding('deposit');
    await screen.findByText('EXC-1001-USD');
    await audit('Funding:deposit');
  });

  it('withdraw tab passes wcag21aa', async () => {
    renderFunding('withdraw');
    await waitFor(() => expect(document.body.textContent ?? '').toMatch(/withdraw/i));
    await audit('Funding:withdraw');
  });

  it('transfer tab passes wcag21aa', async () => {
    renderFunding('transfer');
    await waitFor(() => expect(document.body.textContent ?? '').toMatch(/transfer/i));
    await audit('Funding:transfer');
  });

  it('history tab passes wcag21aa', async () => {
    renderFunding('history');
    await waitFor(() => expect(document.body.textContent ?? '').toMatch(/history|type/i));
    await audit('Funding:history');
  });
});

// ---------------------------------------------------------------------------
// Task 10.3.24 — KYC screens
// ---------------------------------------------------------------------------

describe('a11y: KYC screens (Task 10.3.24)', () => {
  beforeEach(() => {
    signInForTests();
  });

  function renderKyc() {
    return renderApp(
      <Routes>
        <Route path="/kyc" element={<KycPage />} />
        <Route path="/login" element={<div>login</div>} />
      </Routes>,
      '/kyc',
    );
  }

  it('status tracker (approved T1) passes wcag21aa', async () => {
    installFetchMock({
      'GET /api/v1/kyc/status': {
        body: {
          tier: 'T1',
          status: 'APPROVED',
          documents: [
            { type: 'GOVERNMENT_ID', status: 'APPROVED', submitted_at: '2026-01-01T00:00:00Z' },
          ],
        },
      },
    });
    renderKyc();
    await screen.findByText('Verification status');
    await audit('KycPage:status-tracker');
  });

  it('upload wizard (T0 pending) passes wcag21aa', async () => {
    installFetchMock({
      'GET /api/v1/kyc/status': { body: { tier: 'T0', status: 'PENDING', documents: [] } },
    });
    renderKyc();
    await screen.findByLabelText(/First name/);
    await audit('KycPage:upload-wizard');
  });
});

// ---------------------------------------------------------------------------
// Task 10.3.25 — support screens
// ---------------------------------------------------------------------------

const TICKET = {
  ticket_id: 42,
  account_id: 1001,
  type: 'SUPPORT',
  category: 'FUNDING',
  priority: 'NORMAL',
  subject: 'Wire not credited',
  body: 'Sent yesterday.',
  status: 'OPEN',
  sla_due_at: new Date(Date.now() + 4 * 3600_000).toISOString(),
  created_at: '2026-01-01T09:00:00Z',
  updated_at: '2026-01-02T09:00:00Z',
};

describe('a11y: support screens (Task 10.3.25)', () => {
  beforeEach(() => {
    signInForTests();
  });

  function renderSupport(route = '/support') {
    return renderApp(
      <Routes>
        <Route path="/support" element={<SupportPage />} />
        <Route path="/support/tickets/:id" element={<TicketDetailPage />} />
        <Route path="/login" element={<div>login</div>} />
      </Routes>,
      route,
    );
  }

  it('ticket list passes wcag21aa', async () => {
    installFetchMock({
      'GET /api/v1/support/tickets*': { body: { data: [TICKET], next_cursor: '', total: 1 } },
    });
    renderSupport();
    await screen.findByText('Wire not credited');
    await audit('SupportPage:list');
  });

  it('new-ticket form passes wcag21aa', async () => {
    installFetchMock({
      'GET /api/v1/support/tickets*': { body: { data: [] } },
    });
    renderSupport();
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'New ticket' }));
    await screen.findByLabelText(/^Subject/);
    await audit('SupportPage:ticket-form');
  });

  it('ticket thread passes wcag21aa', async () => {
    installFetchMock({
      'GET /api/v1/support/tickets/42': { body: { ticket: TICKET, notes: [] } },
    });
    renderSupport('/support/tickets/42');
    await screen.findByText('Wire not credited');
    await audit('TicketDetailPage');
  });
});

// ---------------------------------------------------------------------------
// Task 10.3.26 — strategy / bot screens
// ---------------------------------------------------------------------------

describe('a11y: strategy & bot screens (Task 10.3.26)', () => {
  beforeEach(() => {
    signInForTests();
    installFetchMock({
      'GET /api/v1/copy/strategies': {
        body: {
          data: [
            {
              strategy_id: 's1',
              alias: 'AlphaFX',
              return_30d_pct: '4.2',
              risk_class: 'MEDIUM',
              followers: 12,
            },
            { strategy_id: 's2', alias: 'BetaCarry', return_30d_pct: '1.1', risk_class: 'LOW' },
          ],
        },
      },
      'GET /api/v1/bots/grid': {
        body: {
          data: [
            {
              bot_id: 'b1',
              symbol: 'EUR/USD',
              status: 'RUNNING',
              lower_price: '1.00',
              upper_price: '1.10',
              grid_count: 11,
              total_investment: '11000',
              pnl: '42.5',
              filled_levels: 4,
            },
          ],
        },
      },
      'GET /api/v1/instruments': { body: { data: [] } },
    });
  });

  it('copy-trading strategy browser passes wcag21aa', async () => {
    renderApp(<CopyGridPage />);
    await screen.findByText('AlphaFX');
    await audit('CopyGridPage:strategies');
  });

  it('grid-bots tab (active bots + wizard) passes wcag21aa', async () => {
    renderApp(<CopyGridPage />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('tab', { name: 'Grid bots' }));
    await screen.findByText('EUR/USD');
    await audit('CopyGridPage:bots');
  });

  it('follow-strategy modal passes wcag21aa', async () => {
    renderApp(<CopyGridPage />);
    const user = userEvent.setup();
    const followBtns = await screen.findAllByRole('button', { name: 'Follow' });
    await user.click(followBtns[0]!);
    await screen.findByRole('dialog');
    await audit('FollowModal');
  });
});

// ---------------------------------------------------------------------------
// Task 10.3.27 — order-history screens
// ---------------------------------------------------------------------------

describe('a11y: order-history screens (Task 10.3.27)', () => {
  beforeEach(() => {
    signInForTests();
    installFetchMock({
      'GET /api/v1/orders': {
        body: {
          data: [
            {
              order_id: 'o1',
              symbol: 'EUR/USD',
              side: 'BUY',
              type: 'LIMIT',
              status: 'OPEN',
              price: '1.0850',
              qty: '10000',
              filled_qty: '0',
              created_at: '2026-01-02T09:00:00Z',
            },
          ],
          next_cursor: '',
          total: 1,
        },
      },
      'GET /api/v1/algo-orders': {
        body: {
          data: [
            {
              algo_order_id: 'a1',
              symbol: 'EUR/USD',
              algo_type: 'TWAP',
              status: 'RUNNING',
              side: 'BUY',
            },
          ],
        },
      },
      'GET /api/v1/order-lists': {
        body: {
          data: [
            {
              list_id: 'l1',
              type: 'OCO',
              symbol: 'EUR/USD',
              status: 'OPEN',
              orders: [{}, {}],
            },
          ],
        },
      },
      'GET /api/v1/order-lists/history': { body: { data: [] } },
    });
  });

  const tabs: { name: string; label: string }[] = [
    { name: 'open', label: 'Open orders' },
    { name: 'history', label: 'History' },
    { name: 'algo', label: 'Algo orders' },
    { name: 'lists', label: 'Order lists' },
    { name: 'safety', label: 'Safety & test' },
    { name: 'tape', label: 'Live tape' },
  ];

  for (const t of tabs) {
    it(`${t.label} tab passes wcag21aa`, async () => {
      renderApp(<HistoryPage />);
      const user = userEvent.setup();
      await user.click(await screen.findByRole('tab', { name: t.label }));
      await waitFor(() => expect(document.body.textContent ?? '').not.toBe(''));
      await audit(`HistoryPage:${t.name}`);
    });
  }
});

// ---------------------------------------------------------------------------
// Task 10.3.28 — report screens
// ---------------------------------------------------------------------------

describe('a11y: report screens (Task 10.3.28)', () => {
  beforeEach(() => {
    signInForTests();
    installFetchMock({
      'GET /api/v1/fees': {
        body: {
          data: [{ tier: 'Tier 1', maker_bps: '1.0', taker_bps: '2.0' }],
        },
      },
      'GET /api/v1/public/proof-of-reserves/daily-root': STUB_501,
      'GET /api/v1/solvency/proof': STUB_501,
      'GET /api/v1/solvency/latest': STUB_501,
      'GET /api/v1/system/status': { body: OPS_HEALTH },
      'GET /api/v1/announcements': { body: { data: [] } },
    });
  });

  const tabs = ['Downloads', 'Fees', 'Solvency', 'System', 'Announcements'] as const;

  for (const label of tabs) {
    it(`${label} tab passes wcag21aa`, async () => {
      renderApp(<ReportsPage />);
      const user = userEvent.setup();
      await user.click(await screen.findByRole('tab', { name: label }));
      await waitFor(() => expect(document.body.textContent ?? '').not.toBe(''));
      await audit(`ReportsPage:${label}`);
    });
  }
});

// ---------------------------------------------------------------------------
// Task 10.3.29 — input-helper framework
// ---------------------------------------------------------------------------

function HelperFixture() {
  const symbol = useValidatedField(RULE_SYMBOL, '');
  return (
    <div>
      <InputField field={symbol} label="Symbol" help="BASE/QUOTE pair" unit="FX" />
      <Autocomplete
        items={[
          { id: 'a', label: 'EUR/USD' },
          { id: 'b', label: 'EUR/GBP' },
        ]}
        value="eu"
        onChange={() => undefined}
        onSelect={() => undefined}
        itemKey={(i) => i.id}
        itemLabel={(i) => i.label}
        label="Search symbols"
      />
      <AmountPresets available="1000" onPick={() => undefined} />
      <HelpTooltip help={fieldHelp('price')!} />
      <PreviewPanel state={{ result: null, pending: false, error: null, notImplemented: false }} />
      <UnavailablePanel feature="Copy trading" owner="Phase-14 Task 14.3.14" />
    </div>
  );
}

describe('a11y harness sanity', () => {
  it('detects a known violation (unlabeled image/button)', async () => {
    const { container } = render(
      <div>
        {/* deliberately bad fixture — img without alt, button without name */}
        <img src="x.png" />
        <button type="button" />
      </div>,
    );
    const results = await (await import('@/test/axe')).axe(container);
    expect(results.violations.length).toBeGreaterThan(0);
  });
});

describe('a11y: input-helper framework (Task 10.3.29)', () => {
  function renderPlain(node: ReactNode) {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(<QueryClientProvider client={qc}>{node}</QueryClientProvider>);
  }

  it('fields + autocomplete + presets + help + preview + unavailable pass wcag21aa', async () => {
    renderPlain(<HelperFixture />);
    const input = screen.getByRole('combobox');
    fireEvent.focus(input); // open the listbox so options exist in the DOM
    await screen.findByRole('listbox');
    await audit('input-helpers fixture');
  });

  it('ConfirmModal (HIGH severity, typed phrase) passes wcag21aa', async () => {
    renderPlain(
      <ConfirmModal
        open
        title="Close all positions"
        severity="HIGH"
        disclosures={['capitalLoss']}
        requirePhrase={CONFIRM_PHRASES['closeAllPositions']}
        onConfirm={() => undefined}
        onCancel={() => undefined}
      />,
    );
    await screen.findByRole('dialog');
    await audit('ConfirmModal HIGH');
  });

  it('ShortcutHelpOverlay passes wcag21aa', async () => {
    shortcutRegistry.register({
      id: 'a11y-test-search',
      key: '/',
      description: 'search',
      scope: 'global',
      action: () => undefined,
    });
    act(() => {
      shortcutRegistry.setHelp(true);
    });
    renderPlain(<ShortcutHelpOverlay />);
    await screen.findByRole('dialog');
    await audit('ShortcutHelpOverlay');
    act(() => {
      shortcutRegistry.setHelp(false);
    });
  });

  it('GlossaryList passes wcag21aa', async () => {
    renderPlain(<GlossaryList />);
    await waitFor(() => expect(document.body.textContent ?? '').not.toBe(''));
    await audit('GlossaryList');
  });
});
