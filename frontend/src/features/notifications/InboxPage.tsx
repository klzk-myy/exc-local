/**
 * Notifications inbox — this session's `private:notifications`
 * deliveries (push-only channel; no REST history exists). Delivery
 * preferences live under Settings → Notifications.
 */
import { useEffect } from 'react';
import { Link } from 'react-router';

import { markAllRead, useInbox } from './store';

function PayloadDetail({ payload }: { payload: unknown }) {
  if (payload === null || payload === undefined) return null;
  if (typeof payload === 'string') {
    return <p className="mt-1 text-xs text-neutral-400">{payload}</p>;
  }
  if (typeof payload === 'object') {
    const rec = payload as Record<string, unknown>;
    const pairs = Object.entries(rec).filter(
      ([, v]) => typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean',
    );
    if (pairs.length === 0) return null;
    return (
      <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs text-neutral-400">
        {pairs.slice(0, 6).map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="font-mono">{k}</dt>
            <dd className="font-mono">{String(v)}</dd>
          </div>
        ))}
      </dl>
    );
  }
  return null;
}

export default function InboxPage() {
  const { items, unread } = useInbox();

  // Viewing the inbox clears the unread badge.
  useEffect(() => {
    markAllRead();
  }, [items.length]);

  return (
    <div className="mx-auto max-w-3xl space-y-4 p-4">
      <header className="flex items-baseline justify-between">
        <div>
          <h1 className="text-xl font-semibold text-neutral-100">Notifications</h1>
          <p className="text-sm text-neutral-500">
            In-platform deliveries received this session. History is not retained across reloads —
            delivery preferences are under{' '}
            <Link to="/settings" className="text-sky-400 hover:underline">
              Settings
            </Link>
            .
          </p>
        </div>
        {unread > 0 ? (
          <span className="rounded bg-sky-600/20 px-2 py-0.5 text-xs text-sky-300">
            {unread} unread
          </span>
        ) : null}
      </header>

      {items.length === 0 ? (
        <p className="rounded border border-neutral-800 p-6 text-center text-sm text-neutral-500">
          No notifications this session.
        </p>
      ) : (
        <ul className="divide-y divide-neutral-800 rounded border border-neutral-800">
          {items.map((it, i) => (
            <li key={it.deliveryId ?? i} className="px-4 py-3">
              <div className="flex items-baseline justify-between gap-3">
                <p className="text-sm font-medium text-neutral-200">{it.subject}</p>
                <span className="shrink-0 font-mono text-xs text-neutral-500">
                  {new Date(it.sentAt).toLocaleString('en-US', { hour12: false })}
                </span>
              </div>
              <p className="mt-0.5 text-xs uppercase tracking-wide text-neutral-500">{it.event}</p>
              <PayloadDetail payload={it.payload} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
