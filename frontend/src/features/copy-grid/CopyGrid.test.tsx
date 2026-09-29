import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';
import { parseGridBot, parseStrategy } from './api';
import { StrategyBrowser } from './StrategyBrowser';
import { GridBotWizard } from './GridBotWizard';

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
      expect(screen.getByRole('status')).toHaveTextContent(/not yet live|not yet available/i);
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
    // POST lands on the (stubbed) route → honest 501 note inside the modal
    await waitFor(() => {
      expect(screen.getByText(/not yet live \(501/)).toBeInTheDocument();
    });
  });

  it('enforces the max-5 concurrent bot cap', () => {
    installFetchMock({ 'GET /api/v1/instruments': { status: 200, body: { data: [] } } });
    renderApp(<GridBotWizard activeBots={5} feeBps={null} />);
    expect(screen.getByText(/Maximum of 5 concurrent grid bots/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Create bot' })).toBeDisabled();
  });
});
