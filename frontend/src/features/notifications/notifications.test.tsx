/**
 * Notifications store + bell: `private:notifications` frames land in the
 * session ring, badge counts unread, inbox renders deliveries.
 */
import { render, screen } from '@testing-library/react';
import { act } from 'react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';

import InboxPage from './InboxPage';
import { NotificationBell } from './NotificationBell';
import { markAllRead, pushNotification, resetNotifications } from './store';

vi.mock('@/app/runtime', () => {
  const subs = new Map<string, unknown>();
  const wsStatus = {
    state: 'AUTHENTICATED',
    attempt: 0,
    orderEntryEnabled: true,
    subscriptions: [],
    health: {},
    lastError: null,
  };
  return {
    wsClient: {
      subscribe: vi.fn((channel: string, cb: unknown) => {
        subs.set(channel, cb);
        return () => subs.delete(channel);
      }),
      unsubscribe: vi.fn(),
      onStatusChange: vi.fn(() => () => undefined),
      getStatus: () => wsStatus,
      __subs: subs,
    },
    api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

afterEach(() => resetNotifications());

function delivery(subject: string, event = 'order.filled') {
  return {
    delivery_id: Math.floor(Math.random() * 1e6),
    event,
    subject,
    payload: { order_id: '42', qty: '1.5' },
    sent_at: '2025-01-15T10:00:00Z',
  };
}

describe('pushNotification', () => {
  it('stores well-formed deliveries', () => {
    pushNotification(delivery('Order filled'));
    render(
      <MemoryRouter>
        <InboxPage />
      </MemoryRouter>,
    );
    expect(screen.getByText('Order filled')).toBeInTheDocument();
    expect(screen.getByText('order.filled')).toBeInTheDocument();
    expect(screen.getByText('order_id')).toBeInTheDocument();
    expect(screen.getByText('42')).toBeInTheDocument();
  });

  it('drops malformed payloads', () => {
    pushNotification(null);
    pushNotification('garbage');
    pushNotification({ delivery_id: 1 }); // no subject/event
    render(
      <MemoryRouter>
        <InboxPage />
      </MemoryRouter>,
    );
    expect(screen.getByText('No notifications this session.')).toBeInTheDocument();
  });
});

describe('NotificationBell', () => {
  it('badges unread count and subscribes private:notifications', async () => {
    const { wsClient } = await import('@/app/runtime');
    render(
      <MemoryRouter>
        <NotificationBell />
      </MemoryRouter>,
    );
    expect(wsClient.subscribe).toHaveBeenCalledWith(
      'private:notifications',
      expect.any(Function),
    );
    act(() => {
      pushNotification(delivery('Fill A'));
      pushNotification(delivery('Fill B'));
    });
    expect(screen.getByTestId('notif-badge')).toHaveTextContent('2');
    act(() => markAllRead());
    expect(screen.queryByTestId('notif-badge')).not.toBeInTheDocument();
  });
});
