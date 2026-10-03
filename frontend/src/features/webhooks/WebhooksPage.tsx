/**
 * Webhook endpoint management — register, rotate secret, disable, and a
 * per-endpoint delivery log (Task 5.3.17). The signing secret is shown
 * exactly once at registration/rotation; DLQ admin review lives under
 * the admin console.
 */
import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { ConfirmModal, UnavailablePanel } from '@/lib/input-helpers';

import {
  disableWebhook,
  listDeliveries,
  listWebhooks,
  registerWebhook,
  rotateSecret,
  type WebhookEndpoint,
} from './api';

function isUnavailable(err: unknown): boolean {
  return err instanceof ApiError && err.status >= 500;
}

function SecretReveal({ secret, onClose }: { secret: string; onClose: () => void }) {
  return (
    <div className="rounded border border-amber-700/50 bg-amber-950/30 p-3" role="alert">
      <p className="text-xs font-medium text-amber-300">
        Signing secret — shown once. Verify deliveries with{' '}
        <code>
          X-Webhook-Signature = hex(HMAC-SHA256(secret, "&lt;X-Webhook-Timestamp&gt;.&lt;body&gt;"))
        </code>
      </p>
      <div className="mt-2 flex items-center gap-2">
        <code className="flex-1 break-all rounded bg-neutral-950 px-2 py-1 font-mono text-xs text-neutral-200">
          {secret}
        </code>
        <button
          type="button"
          onClick={onClose}
          className="rounded bg-neutral-800 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-700"
        >
          Dismiss
        </button>
      </div>
    </div>
  );
}

function DeliveriesPanel({ endpointId }: { endpointId: string }) {
  const q = useQuery({
    queryKey: ['webhooks', 'deliveries', endpointId],
    queryFn: () => listDeliveries(apiClient, endpointId),
  });
  if (q.isLoading) return <p className="p-3 text-xs text-neutral-500">Loading deliveries…</p>;
  if (q.error) {
    return isUnavailable(q.error) ? (
      <UnavailablePanel
        feature="Webhook deliveries"
        owner="Phase-05 Task 5.3.17"
        note="The delivery log is temporarily unavailable."
      />
    ) : (
      <p className="p-3 text-xs text-red-400">{q.error.message}</p>
    );
  }
  const rows = q.data ?? [];
  if (rows.length === 0) {
    return <p className="p-3 text-xs text-neutral-500">No deliveries yet.</p>;
  }
  return (
    <table className="w-full text-xs">
      <thead>
        <tr className="border-b border-neutral-800 text-left text-neutral-500">
          <th className="px-3 py-1.5 font-medium">Event</th>
          <th className="px-3 py-1.5 font-medium">Status</th>
          <th className="px-3 py-1.5 font-medium">Attempts</th>
          <th className="px-3 py-1.5 font-medium">Last HTTP</th>
          <th className="px-3 py-1.5 font-medium">Error</th>
          <th className="px-3 py-1.5 font-medium">Delivered</th>
        </tr>
      </thead>
      <tbody className="divide-y divide-neutral-800">
        {rows.map((d) => (
          <tr key={d.id}>
            <td className="px-3 py-1.5 font-mono text-neutral-300">{d.event}</td>
            <td
              className={`px-3 py-1.5 font-mono ${
                d.status === 'DELIVERED'
                  ? 'text-emerald-400'
                  : d.status === 'DEAD_LETTERED'
                    ? 'text-red-400'
                    : 'text-neutral-400'
              }`}
            >
              {d.status}
            </td>
            <td className="px-3 py-1.5 font-mono text-neutral-400">{d.attempts}</td>
            <td className="px-3 py-1.5 font-mono text-neutral-400">{d.lastStatusCode ?? '—'}</td>
            <td className="max-w-48 truncate px-3 py-1.5 text-neutral-500">{d.lastError ?? '—'}</td>
            <td className="px-3 py-1.5 font-mono text-neutral-500">
              {d.deliveredAt
                ? new Date(d.deliveredAt).toLocaleString('en-US', { hour12: false })
                : '—'}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function EndpointRow({ ep }: { ep: WebhookEndpoint }) {
  const queryClient = useQueryClient();
  const [expanded, setExpanded] = useState(false);
  const [confirmDisable, setConfirmDisable] = useState(false);
  const [confirmRotate, setConfirmRotate] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);

  const disable = useMutation({
    mutationFn: () => disableWebhook(apiClient, ep.id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['webhooks', 'list'] }),
  });
  const rotate = useMutation({
    mutationFn: () => rotateSecret(apiClient, ep.id, 3600),
    onSuccess: (s) => {
      setSecret(s);
      void queryClient.invalidateQueries({ queryKey: ['webhooks', 'list'] });
    },
  });

  return (
    <li className="divide-y divide-neutral-800 rounded border border-neutral-800">
      <div className="flex items-center justify-between gap-3 px-4 py-3">
        <div className="min-w-0">
          <p className="truncate font-mono text-sm text-neutral-200">{ep.url}</p>
          <p className="mt-0.5 text-xs text-neutral-500">
            {ep.events.join(', ')} ·{' '}
            <span className={ep.status === 'ACTIVE' ? 'text-emerald-400' : 'text-neutral-500'}>
              {ep.status}
            </span>
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="rounded bg-neutral-800 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-700"
          >
            {expanded ? 'Hide deliveries' : 'Deliveries'}
          </button>
          {ep.status === 'ACTIVE' ? (
            <>
              <button
                type="button"
                onClick={() => setConfirmRotate(true)}
                className="rounded bg-neutral-800 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-700"
              >
                Rotate secret
              </button>
              <button
                type="button"
                onClick={() => setConfirmDisable(true)}
                className="rounded bg-red-900/40 px-2 py-1 text-xs text-red-300 hover:bg-red-900/60"
              >
                Disable
              </button>
            </>
          ) : null}
        </div>
      </div>
      {secret ? <SecretReveal secret={secret} onClose={() => setSecret(null)} /> : null}
      {expanded ? <DeliveriesPanel endpointId={ep.id} /> : null}
      <ConfirmModal
        open={confirmDisable}
        severity="MEDIUM"
        title="Disable webhook"
        confirmLabel="Disable"
        busy={disable.isPending}
        onCancel={() => setConfirmDisable(false)}
        onConfirm={() => {
          setConfirmDisable(false);
          disable.mutate();
        }}
      >
        <p className="text-sm">Stop deliveries to {ep.url}? Pending deliveries are abandoned.</p>
      </ConfirmModal>
      <ConfirmModal
        open={confirmRotate}
        severity="MEDIUM"
        title="Rotate signing secret"
        confirmLabel="Rotate"
        busy={rotate.isPending}
        onCancel={() => setConfirmRotate(false)}
        onConfirm={() => {
          setConfirmRotate(false);
          rotate.mutate();
        }}
      >
        <p className="text-sm">
          Rotate the signing secret for {ep.url}? The previous secret remains valid for a 1-hour
          overlap window via X-Webhook-Signature-Prev.
        </p>
      </ConfirmModal>
    </li>
  );
}

function CreateForm({ allowedEvents }: { allowedEvents: string[] }) {
  const queryClient = useQueryClient();
  const [url, setUrl] = useState('');
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [secret, setSecret] = useState<string | null>(null);
  const create = useMutation({
    mutationFn: () => registerWebhook(apiClient, url, [...selected]),
    onSuccess: (r) => {
      setSecret(r.secret);
      setUrl('');
      setSelected(new Set());
      void queryClient.invalidateQueries({ queryKey: ['webhooks', 'list'] });
    },
  });

  return (
    <form
      className="space-y-3 rounded border border-neutral-800 p-4"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate();
      }}
    >
      <h2 className="text-sm font-medium text-neutral-200">Register endpoint</h2>
      {secret ? <SecretReveal secret={secret} onClose={() => setSecret(null)} /> : null}
      <input
        type="url"
        required
        value={url}
        onChange={(e) => setUrl(e.target.value)}
        placeholder="https://example.com/hooks/exchange"
        className="w-full rounded border border-neutral-700 bg-neutral-950 px-3 py-1.5 text-sm text-neutral-200"
      />
      <fieldset className="grid grid-cols-2 gap-1 sm:grid-cols-3">
        <legend className="sr-only">Events</legend>
        {allowedEvents.map((ev) => (
          <label key={ev} className="flex items-center gap-1.5 text-xs text-neutral-400">
            <input
              type="checkbox"
              checked={selected.has(ev)}
              onChange={(e) => {
                const next = new Set(selected);
                if (e.target.checked) next.add(ev);
                else next.delete(ev);
                setSelected(next);
              }}
            />
            <span className="font-mono">{ev}</span>
          </label>
        ))}
      </fieldset>
      {create.error ? <p className="text-xs text-red-400">{create.error.message}</p> : null}
      <button
        type="submit"
        disabled={create.isPending || selected.size === 0}
        className="rounded bg-sky-600 px-3 py-1.5 text-sm text-white hover:bg-sky-500 disabled:opacity-50"
      >
        {create.isPending ? 'Registering…' : 'Register'}
      </button>
    </form>
  );
}

export default function WebhooksPage() {
  const q = useQuery({ queryKey: ['webhooks', 'list'], queryFn: () => listWebhooks(apiClient) });

  if (q.isLoading) {
    return <p className="p-4 text-sm text-neutral-500">Loading webhooks…</p>;
  }
  if (q.error) {
    return isUnavailable(q.error) ? (
      <UnavailablePanel
        feature="Webhook endpoints"
        owner="Phase-05 Task 5.3.17"
        note="Webhook management is temporarily unavailable."
      />
    ) : (
      <p className="p-4 text-sm text-red-400">{q.error.message}</p>
    );
  }
  const { endpoints, allowedEvents } = q.data ?? { endpoints: [], allowedEvents: [] };

  return (
    <div className="mx-auto max-w-4xl space-y-4 p-4">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">Webhooks</h1>
        <p className="text-sm text-neutral-500">
          Signed event delivery to your endpoints. Secrets are shown once at registration or
          rotation.
        </p>
      </header>
      <CreateForm allowedEvents={allowedEvents} />
      {endpoints.length === 0 ? (
        <p className="rounded border border-neutral-800 p-6 text-center text-sm text-neutral-500">
          No webhook endpoints registered.
        </p>
      ) : (
        <ul className="space-y-3">
          {endpoints.map((ep) => (
            <EndpointRow key={ep.id} ep={ep} />
          ))}
        </ul>
      )}
    </div>
  );
}
