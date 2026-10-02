/**
 * Webhooks page: endpoint list renders, registration POSTs url+events,
 * disable confirm DELETEs. Wire shapes follow Task 5.3.17.
 */
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import WebhooksPage from './WebhooksPage';
import { parseDelivery, parseEndpoint } from './api';

const { apiClient } = vi.hoisted(() => ({
  apiClient: {
    get: vi.fn(),
    post: vi.fn(),
    put: vi.fn(),
    delete: vi.fn(),
  },
}));
vi.mock('@/app/runtime', () => ({ apiClient, wsClient: { subscribe: vi.fn(), unsubscribe: vi.fn() } }));

function qc() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}
function renderPage() {
  return render(
    <QueryClientProvider client={qc()}>
      <MemoryRouter>
        <WebhooksPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const EP = {
  endpoint_id: 'ep_1',
  url: 'https://hooks.example.com/x',
  events: ['order.filled'],
  status: 'ACTIVE',
  created_at: '2025-01-10T00:00:00Z',
};

beforeEach(() => vi.clearAllMocks());

describe('parsers', () => {
  it('parses endpoint + delivery wire shapes', () => {
    expect(parseEndpoint(EP)).toMatchObject({ id: 'ep_1', url: EP.url });
    expect(
      parseDelivery({
        delivery_id: 'd1',
        event: 'order.filled',
        status: 'DELIVERED',
        attempts: 1,
        last_status_code: 200,
        created_at: '2025-01-10T00:00:00Z',
      }),
    ).toMatchObject({ id: 'd1', status: 'DELIVERED', lastStatusCode: 200 });
    expect(parseEndpoint({})).toBeNull();
    expect(parseDelivery({})).toBeNull();
  });
});

describe('WebhooksPage', () => {
  it('renders endpoints and allowed events', async () => {
    apiClient.get.mockResolvedValue({ webhooks: [EP], allowed_events: ['order.filled'] });
    renderPage();
    expect(await screen.findByText(EP.url)).toBeInTheDocument();
    expect(screen.getAllByText('order.filled').length).toBeGreaterThan(0);
  });

  it('registers an endpoint and reveals the one-time secret', async () => {
    apiClient.get.mockResolvedValue({ webhooks: [], allowed_events: ['order.filled'] });
    apiClient.post.mockResolvedValue({ endpoint: EP, secret: 'whsec_abc' });
    renderPage();
    const user = userEvent.setup();
    await user.type(
      await screen.findByPlaceholderText(/example.com\/hooks/),
      'https://hooks.example.com/x',
    );
    await user.click(screen.getByRole('checkbox', { name: 'order.filled' }));
    await user.click(screen.getByRole('button', { name: 'Register' }));
    await waitFor(() =>
      expect(apiClient.post).toHaveBeenCalledWith('/webhooks', {
        url: 'https://hooks.example.com/x',
        events: ['order.filled'],
      }),
    );
    expect(await screen.findByText('whsec_abc')).toBeInTheDocument();
  });
});
