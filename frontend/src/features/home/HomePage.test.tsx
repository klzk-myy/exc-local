import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import { Route, Routes } from 'react-router';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import HomePage from './HomePage';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

const BALANCES = {
  balances: [{ currency: 'USD', available: '10', locked: '2', total: '12' }],
};
const POSITIONS = {
  positions: [
    {
      id: 'pos-1',
      symbol: 'EUR/USD',
      side: 'LONG',
      quantity: '1000',
      entry_price: '1.0800',
      unrealized_pnl: '4.25',
      realized_pnl: '0',
      margin_used: '36',
    },
  ],
};
const ORDERS = { data: [], next_cursor: '', total: 0 };

function renderHome() {
  return renderApp(
    <Routes>
      <Route path="/" element={<HomePage />} />
    </Routes>,
    '/',
  );
}

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
});

describe('HomePage', () => {
  it('renders the WS state tile and exchange clock', async () => {
    installFetchMock({
      'GET /api/v1/time': { body: { server_time_ms: 1_700_000_000_000 } },
      'GET /api/v1/account/balances*': { body: BALANCES },
      'GET /api/v1/positions*': { body: POSITIONS },
      'GET /api/v1/orders*': { body: ORDERS },
    });
    renderHome();
    expect(screen.getByRole('heading', { name: 'Dashboard' })).toBeInTheDocument();
    expect(screen.getByTestId('ws-state')).toHaveTextContent('DISCONNECTED');
    await waitFor(() => expect(screen.getByTestId('server-time')).toHaveTextContent('2023-11-14T'));
  });

  it('shows the sign-in prompt instead of account data when logged out', () => {
    installFetchMock({
      'GET /api/v1/time': { body: { server_time_ms: 1_700_000_000_000 } },
    });
    renderHome();
    expect(screen.getByRole('link', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByText('Account snapshot')).not.toBeInTheDocument();
  });

  it('renders balances, positions, and quick links when signed in', async () => {
    signInForTests();
    installFetchMock({
      'GET /api/v1/time': { body: { server_time_ms: 1_700_000_000_000 } },
      'GET /api/v1/account/balances*': { body: BALANCES },
      'GET /api/v1/positions*': { body: POSITIONS },
      'GET /api/v1/orders*': { body: ORDERS },
    });
    renderHome();
    expect(await screen.findByText('Account snapshot')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('USD')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('EUR/USD')).toBeInTheDocument());
    expect(screen.getByRole('link', { name: 'Trade EUR/USD' })).toHaveAttribute(
      'href',
      '/trade/EUR%2FUSD',
    );
    expect(screen.getByRole('link', { name: 'Workspace' })).toHaveAttribute('href', '/workspace');
  });
});
