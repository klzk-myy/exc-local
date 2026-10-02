/**
 * Notification bell — subscribes `private:notifications` on the shared
 * /ws/v1 socket for the shell's lifetime and badges unread count.
 * Mounts in AppShell (Task-8 notification surface).
 */
import { useCallback } from 'react';
import { Link } from 'react-router';

import { wsClient } from '@/app/runtime';
import { useChannel, useWsStatus } from '@/lib/ws';

import { pushNotification, useInbox } from './store';

export function NotificationBell() {
  const { unread } = useInbox();
  const status = useWsStatus(wsClient);

  const onMessage = useCallback((f: { type: string; data?: unknown }) => {
    if (f.type === 'event') pushNotification(f.data);
  }, []);
  // private:* channels require an authenticated session — subscribing
  // pre-handshake would force RESYNCING until the server acks a channel
  // the anonymous connection can't legally join.
  useChannel(
    wsClient,
    status.state === 'AUTHENTICATED' || status.state === 'STALE'
      ? 'private:notifications'
      : null,
    onMessage,
  );

  return (
    <Link
      to="/notifications"
      className="relative rounded px-2 py-1 text-neutral-400 hover:bg-neutral-900 hover:text-neutral-200"
      aria-label={unread > 0 ? `Notifications — ${unread} unread` : 'Notifications'}
    >
      <svg
        viewBox="0 0 20 20"
        fill="currentColor"
        aria-hidden="true"
        className="h-4 w-4"
      >
        <path d="M10 2a5 5 0 0 0-5 5v2.5L3.6 12a1 1 0 0 0 .9 1.5h11a1 1 0 0 0 .9-1.5L15 9.5V7a5 5 0 0 0-5-5Zm0 16a2.2 2.2 0 0 0 2.1-1.5H7.9A2.2 2.2 0 0 0 10 18Z" />
      </svg>
      {unread > 0 ? (
        <span
          data-testid="notif-badge"
          className="absolute -right-0.5 -top-0.5 rounded-full bg-sky-600 px-1 text-[10px] font-semibold text-white"
        >
          {unread > 99 ? '99+' : unread}
        </span>
      ) : null}
    </Link>
  );
}
