import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';
import { parseGridBot, parseStrategy } from './api';
import MarketplacePanel from './MarketplacePanel';
import { StrategyBrowser } from './StrategyBrowser';
import { GridBotWizard } from './GridBotWizard';
import { ActiveBotsPanel } from './ActiveBotsPanel';
import { MyFollowsPanel } from './MyFollowsPanel';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const STUB_501 = {
  status: 501,
  body: {
    type: 'error',
    error: 'NOT_IMPLEMENTED',
    message: 'route registered, handler pending',
    status: 501,
  },
};

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
  signInForTests();
});

describe('wire narrowing', () => {
  it('parseStrategy picks the documented fields and rejects junk', () => {
    expect(
      parseStrategy({
        strategy_id: 's1',
        alias: 'AlphaFX',
        return_30d_pct: '4.2',
        max_drawdown_pct: '9.9',
        sharpe: '1.4',
        followers: 12,
        risk_class: 'MEDIUM',
      }),
    ).toMatchObject({ id: 's1', alias: 'AlphaFX', return30d: '4.2', followers: 12 });
    expect(parseStrategy({})).toBeNull();
    expect(parseStrategy('x')).toBeNull();
  });
  it('parseGridBot narrows bot rows', () => {
    expect(
      parseGridBot({
        bot_id: 'b1',
        symbol: 'EUR/USD',
        status: 'RUNNING',
        grid_count: 20,
        pnl: '12.5',
      }),
    ).toMatchObject({ id: 'b1', symbol: 'EUR/USD', status: 'RUNNING', pnl: '12.5' });
    expect(parseGridBot({ symbol: 'EUR/USD' })).toBeNull();
  });
});

describe('StrategyBrowser', () => {
  it('renders the honest 501 unavailable state — no fabricated rows', async () => {
    installFetchMock({ 'GET /api/v1/copy/strategies': STUB_501 });
    renderApp(<StrategyBrowser onFollow={() => undefined} />);
    await waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent(/unavailable/i);
    });
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('renders provider rows and toggles to cards when live data exists', async () => {
    installFetchMock({
      'GET /api/v1/copy/strategies': {
        status: 200,
        body: {
          data: [
            { strategy_id: 's1', alias: 'AlphaFX', return_30d_pct: '4.2', risk_class: 'MEDIUM' },
            { strategy_id: 's2', alias: 'BetaCarry', return_30d_pct: '1.1', risk_class: 'LOW' },
          ],
        },
      },
    });
    renderApp(<StrategyBrowser onFollow={() => undefined} />);
    await waitFor(() => {
      expect(screen.getByText('AlphaFX')).toBeInTheDocument();
    });
    expect(screen.getByText('BetaCarry')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cards' }));
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
    // risk filter narrows
    fireEvent.change(screen.getByLabelText('Risk class filter'), { target: { value: 'LOW' } });
    expect(screen.queryByText('AlphaFX')).not.toBeInTheDocument();
    expect(screen.getByText('BetaCarry')).toBeInTheDocument();
  });
});

describe('GridBotWizard', () => {
  it('derives grid math and requires risk acceptance + typed phrase', async () => {
    installFetchMock({
      'GET /api/v1/instruments': { status: 200, body: { data: [] } },
      'POST /api/v1/bots/grid': STUB_501,
    });
    renderApp(<GridBotWizard activeBots={0} feeBps="2" />);
    // Fill the form
    fireEvent.change(screen.getByLabelText('Pair'), { target: { value: 'EUR/USD' } });
    fireEvent.change(screen.getByLabelText('Lower price'), { target: { value: '1.00' } });
    fireEvent.change(screen.getByLabelText('Upper price'), { target: { value: '1.10' } });
    fireEvent.change(screen.getByLabelText('Grid count'), { target: { value: '11' } });
    fireEvent.change(screen.getByLabelText('Total investment'), { target: { value: '11000' } });
    fireEvent.change(screen.getByLabelText(/Per-grid quantity/), { target: { value: '' } });
    fireEvent.change(screen.getByLabelText(/Stop loss/), { target: { value: '' } });
    fireEvent.change(screen.getByLabelText(/Take profit/), { target: { value: '' } });
    // Derivation renders (0.01 spacing)
    await waitFor(() => {
      expect(screen.getByText('Grid spacing')).toBeInTheDocument();
    });
    // Create blocked until disclosure accepted
    const createBtn = screen.getByRole('button', { name: 'Create bot' });
    expect(createBtn).toBeDisabled();
    fireEvent.click(screen.getByRole('checkbox'));
    expect(createBtn).toBeEnabled();
    fireEvent.click(createBtn);
    // HIGH severity demands typed phrase
    await waitFor(() => {
      expect(screen.getByRole('dialog', { name: 'Enable grid bot' })).toBeInTheDocument();
    });
    const confirm = screen.getByRole('button', { name: 'Start bot' });
    expect(confirm).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/to confirm/i), { target: { value: 'START BOT' } });
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);
    // POST lands on a route reporting 501 → honest regression note inside the modal
    await waitFor(() => {
      expect(screen.getByText(/returned 501 NOT_IMPLEMENTED/)).toBeInTheDocument();
    });
  });

  it('enforces the max-5 concurrent bot cap', () => {
    installFetchMock({ 'GET /api/v1/instruments': { status: 200, body: { data: [] } } });
    renderApp(<GridBotWizard activeBots={5} feeBps={null} />);
    expect(screen.getByText(/Maximum of 5 concurrent grid bots/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Create bot' })).toBeDisabled();
  });
});

describe('ActiveBotsPanel pause/resume', () => {
  const botList = (status: string) => ({
    'GET /api/v1/bots/grid': {
      status: 200,
      body: {
        data: [{ bot_id: 'b1', symbol: 'EUR/USD', status, grid_count: 11, realized_pnl: '3.5' }],
      },
    },
  });

  it('posts pause for a RUNNING bot', async () => {
    const calls = installFetchMock({
      ...botList('RUNNING'),
      'POST /api/v1/bots/grid/b1/pause': { status: 200, body: {} },
    });
    renderApp(<ActiveBotsPanel />);
    await waitFor(() => {
      expect(screen.getByText('EUR/USD')).toBeInTheDocument();
    });
    const pauseBtn = screen.getByRole('button', { name: 'Pause' });
    expect(pauseBtn).toBeEnabled();
    fireEvent.click(pauseBtn);
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'POST' && c.url.includes('/bots/grid/b1/pause'))).toBe(
        true,
      );
    });
  });

  it('labels the control Resume for a PAUSED bot and posts resume', async () => {
    const calls = installFetchMock({
      ...botList('PAUSED'),
      'POST /api/v1/bots/grid/b1/resume': { status: 200, body: {} },
    });
    renderApp(<ActiveBotsPanel />);
    await waitFor(() => {
      expect(screen.getByText('PAUSED')).toBeInTheDocument();
    });
    const resumeBtn = screen.getByRole('button', { name: 'Resume' });
    expect(resumeBtn).toBeEnabled();
    fireEvent.click(resumeBtn);
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'POST' && c.url.includes('/bots/grid/b1/resume'))).toBe(
        true,
      );
    });
  });

  it('disables the control for terminal bots', async () => {
    installFetchMock(botList('STOPPED'));
    renderApp(<ActiveBotsPanel />);
    await waitFor(() => {
      expect(screen.getByText('STOPPED')).toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: 'Pause' })).toBeDisabled();
  });
});

describe('MyFollowsPanel', () => {
  const followsList = {
    'GET /api/v1/copy/follows': {
      status: 200,
      body: {
        follows: [
          {
            follow_id: 7,
            strategy_id: 11,
            strategy_name: 'AlphaFX',
            strategy_status: 'LISTED',
            allocation_notional: '5000',
            currency: 'USD',
            safety_mode: 'FULL',
            status: 'ACTIVE',
            created_at: '2026-10-01T00:00:00Z',
          },
          {
            follow_id: 4,
            strategy_id: 9,
            strategy_name: 'BetaCarry',
            strategy_status: 'SUSPENDED',
            allocation_notional: '1200',
            currency: 'USD',
            safety_mode: 'HALF_RISK',
            status: 'UNFOLLOWED',
            unfollowed_at: '2026-10-05T00:00:00Z',
            created_at: '2026-09-01T00:00:00Z',
          },
        ],
      },
    },
  };

  it('renders follows with joined strategy fields; unfollow only on ACTIVE', async () => {
    installFetchMock(followsList);
    renderApp(<MyFollowsPanel />);
    await waitFor(() => {
      expect(screen.getByText('AlphaFX')).toBeInTheDocument();
    });
    expect(screen.getByText('BetaCarry')).toBeInTheDocument();
    expect(screen.getByText('SUSPENDED')).toBeInTheDocument();
    const btns = screen.getAllByRole('button', { name: 'Unfollow' });
    expect(btns).toHaveLength(1); // only the ACTIVE follow
  });

  it('unfollow requires the typed phrase and DELETEs the follow', async () => {
    const calls = installFetchMock({
      ...followsList,
      'DELETE /api/v1/copy/follows/7': { status: 200, body: { disclosure: 'ok' } },
    });
    renderApp(<MyFollowsPanel />);
    await waitFor(() => {
      expect(screen.getByText('AlphaFX')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole('button', { name: 'Unfollow' }));
    // HIGH severity — confirm disabled until the phrase is typed.
    const dialog = await screen.findByRole('dialog');
    const modalConfirm = Array.from(dialog.querySelectorAll('button')).find(
      (b) => b.textContent === 'Unfollow',
    )!;
    expect(modalConfirm).toBeDefined();
    expect(modalConfirm.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText(/to confirm/i), {
      target: { value: 'UNFOLLOW' },
    });
    expect(modalConfirm.disabled).toBe(false);
    fireEvent.click(modalConfirm);
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE' && c.url.includes('/copy/follows/7'))).toBe(
        true,
      );
    });
  });
});

describe('MarketplacePanel', () => {
  it('lists strategies and pauses one via POST /strategies/{id}/pause', async () => {
    const calls = installFetchMock({
      'GET /api/v1/strategies': {
        body: {
          strategies: [
            {
              strategy_id: 3,
              kind: 'DCA',
              label: 'Weekly EUR',
              from_currency: 'USD',
              to_currency: 'EUR',
              amount: '250',
              schedule: 'WEEKLY',
              status: 'ACTIVE',
              run_count: 4,
            },
          ],
        },
      },
      'POST /api/v1/strategies/3/pause': {
        body: { strategy_id: 3, kind: 'DCA', status: 'PAUSED' },
      },
      'GET /api/v1/strategy-templates': { body: { templates: [] } },
    });
    renderApp(<MarketplacePanel />);
    const user = userEvent.setup();
    expect(await screen.findByText('Weekly EUR')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Pause' }));
    await waitFor(() => {
      expect(calls.some((c) => c.url === '/api/v1/strategies/3/pause')).toBe(true);
    });
  });

  it('creates a DCA strategy with the full contract body', async () => {
    const calls = installFetchMock({
      'GET /api/v1/strategies': { body: { strategies: [] } },
      'GET /api/v1/strategy-templates': { body: { templates: [] } },
      'POST /api/v1/strategies': {
        status: 201,
        body: { strategy_id: 9, kind: 'DCA', status: 'ACTIVE' },
      },
    });
    renderApp(<MarketplacePanel />);
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/^Label/), 'Rent money');
    await user.selectOptions(screen.getByLabelText(/^From/), 'USD');
    await user.selectOptions(screen.getByLabelText(/^To/), 'EUR');
    await user.type(screen.getByLabelText(/Amount per run/), '250');
    await user.click(screen.getByRole('button', { name: 'Create strategy' }));
    await waitFor(() => {
      const c = calls.find((x) => x.method === 'POST' && x.url.endsWith('/strategies'));
      expect(JSON.parse(c?.init?.body as string)).toMatchObject({
        kind: 'DCA',
        label: 'Rent money',
        from_currency: 'USD',
        to_currency: 'EUR',
        amount: '250',
        schedule: 'WEEKLY',
      });
    });
  });

  it('instantiates an approved template and publishes one for review', async () => {
    const calls = installFetchMock({
      'GET /api/v1/strategies': { body: { strategies: [] } },
      'GET /api/v1/strategy-templates': {
        body: {
          templates: [
            {
              template_id: 5,
              name: 'Balanced majors',
              description: 'EUR/USD/JPY rebalance',
              kind: 'REBALANCE',
              status: 'APPROVED',
            },
          ],
        },
      },
      'POST /api/v1/strategy-templates/5/instantiate': {
        status: 201,
        body: { strategy_id: 21, kind: 'REBALANCE', status: 'ACTIVE', template_id: 5 },
      },
      'POST /api/v1/strategy-templates': {
        status: 201,
        body: { template_id: 8, name: 'X', status: 'PENDING_APPROVAL' },
      },
    });
    renderApp(<MarketplacePanel />);
    const user = userEvent.setup();
    expect(await screen.findByText('Balanced majors')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Instantiate' }));
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('/strategy-templates/5/instantiate'))).toBe(true);
    });
    expect(await screen.findByText(/Strategy #21 created from template/)).toBeInTheDocument();

    await user.type(screen.getByLabelText(/^Name/), 'My template');
    await user.type(screen.getByLabelText(/Template description/), 'desc');
    fireEvent.change(screen.getByLabelText(/Config JSON/), { target: { value: '{"a":1}' } });
    await user.click(screen.getByRole('button', { name: 'Publish for review' }));
    await waitFor(() => {
      const c = calls.find((x) => x.method === 'POST' && x.url.endsWith('/strategy-templates'));
      expect(JSON.parse(c?.init?.body as string)).toMatchObject({
        name: 'My template',
        kind: 'DCA',
        config: { a: 1 },
      });
    });
  });

  it('creates an INCUBATING copy profile and requests listing', async () => {
    const calls = installFetchMock({
      'GET /api/v1/strategies': { body: { strategies: [] } },
      'GET /api/v1/strategy-templates': { body: { templates: [] } },
      'POST /api/v1/copy/strategies': {
        status: 201,
        body: {
          strategy_id: 44,
          display_name: 'G10 momentum',
          status: 'INCUBATING',
          incubating_since: '2026-05-19T00:00:00Z',
        },
      },
      'POST /api/v1/copy/strategies/44/list': {
        status: 422,
        body: {
          type: 'error',
          error: 'INCUBATION_INCOMPLETE',
          message: 'needs 30d track record',
          status: 422,
        },
      },
    });
    renderApp(<MarketplacePanel />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Display name/), 'G10 momentum');
    await user.type(screen.getByLabelText(/^Description/), 'desc');
    await user.type(screen.getByLabelText(/Profit share/), '10');
    await user.click(screen.getByRole('button', { name: 'Create profile' }));
    await waitFor(() => {
      const c = calls.find((x) => x.method === 'POST' && x.url.endsWith('/copy/strategies'));
      expect(JSON.parse(c?.init?.body as string)).toMatchObject({
        display_name: 'G10 momentum',
        currency: 'USD',
        instrument_class: 'FX_SPOT',
        profit_share_pct: '10',
      });
    });
    // created row renders INCUBATING with the list action
    const row = await screen.findByText(/G10 momentum/);
    const li = row.closest('li')!;
    await user.click(within(li).getByRole('button', { name: 'Request listing' }));
    await waitFor(() => {
      expect(calls.some((c) => c.url.includes('/copy/strategies/44/list'))).toBe(true);
    });
    // fail-closed refusal surfaces verbatim, not as success
    expect(await screen.findByRole('alert')).toHaveTextContent('INCUBATION_INCOMPLETE');
  });

  it('looks up a basket op and renders leg statuses', async () => {
    installFetchMock({
      'GET /api/v1/strategies': { body: { strategies: [] } },
      'GET /api/v1/strategy-templates': { body: { templates: [] } },
      'GET /api/v1/baskets/op-42': {
        body: {
          op_id: 'op-42',
          status: 'PARTIAL',
          leg_count: 2,
          legs_filled: 1,
          legs: [
            {
              leg_index: 0,
              order_id: 11,
              instrument_id: 1,
              order_status: 'FILLED',
              filled_qty: '100',
            },
            { leg_index: 1, order_id: 12, instrument_id: 2, order_status: 'WORKING' },
          ],
        },
      },
      'GET /api/v1/promotions/7': {
        body: {
          promotion: {
            promotion_id: 7,
            title: 'Spring rebate',
            slug: 'spring',
            channel: 'EMAIL',
            version: 2,
          },
        },
      },
    });
    renderApp(<MarketplacePanel />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Operation ID/), 'op-42');
    await user.click(screen.getByRole('button', { name: 'Look up' }));
    expect(await screen.findByText(/1\/2 filled/)).toBeInTheDocument();
    expect(screen.getByText('FILLED')).toBeInTheDocument();

    await user.type(screen.getByLabelText(/Promotion ID/), '7');
    await user.click(screen.getByRole('button', { name: 'View' }));
    expect(await screen.findByText(/Spring rebate/)).toBeInTheDocument();
  });
});
