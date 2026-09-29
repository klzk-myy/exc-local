/**
 * /support — ticket list (filters + cursor pagination) + submission form
 * + help pointer. Support Agent role holders get a link to the Phase-07
 * admin queue (the staff surface is NOT reimplemented here).
 */
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router';

import { apiClient } from '@/app/runtime';
import { RequireAuth } from '@/features/auth/guards';
import { useSessionStore } from '@/lib/auth/session';
import {
  ErrorBox,
  Field,
  StatusBadge,
  btnGhost,
  cardCls,
  formatCountdown,
  selectCls,
  tableCls,
  tdCls,
  thCls,
  useNow,
} from '@/lib/ui';

import * as api from './api';
import TicketForm from './TicketForm';

function SlaCell({ ticket }: { ticket: api.Ticket }) {
  const now = useNow(60_000);
  if (ticket.acknowledged_at !== undefined && ticket.acknowledged_at !== null) {
    return <span className="text-xs text-neutral-500">acknowledged</span>;
  }
  if (ticket.sla_due_at === undefined) return <span className="text-xs text-neutral-500">—</span>;
  const left = Date.parse(ticket.sla_due_at) - now;
  if (ticket.sla_breached === true || left <= 0) {
    return <span className="text-xs text-red-400">ack SLA breached</span>;
  }
  return (
    <span className="text-xs text-amber-300" role="timer">
      ack due in {formatCountdown(left)}
    </span>
  );
}

function TicketTable({ tickets }: { tickets: api.Ticket[] }) {
  return (
    <table className={tableCls}>
      <thead>
        <tr>
          <th className={thCls}>Subject</th>
          <th className={thCls}>Category</th>
          <th className={thCls}>Priority</th>
          <th className={thCls}>Status</th>
          <th className={thCls}>SLA</th>
          <th className={thCls}>Updated</th>
        </tr>
      </thead>
      <tbody>
        {tickets.map((t) => (
          <tr key={t.ticket_id}>
            <td className={tdCls}>
              <Link to={`/support/tickets/${t.ticket_id}`} className="text-sky-400 hover:underline">
                {t.subject}
              </Link>
            </td>
            <td className={tdCls}>{t.category}</td>
            <td className={tdCls}>{t.priority}</td>
            <td className={tdCls}>
              <StatusBadge value={t.status} />
            </td>
            <td className={tdCls}>
              <SlaCell ticket={t} />
            </td>
            <td className={tdCls}>{new Date(t.updated_at).toLocaleString()}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export default function SupportPage() {
  const [status, setStatus] = useState('');
  const [category, setCategory] = useState('');
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [newTicket, setNewTicket] = useState(false);
  const isSupportAgent = useSessionStore((s) => s.user?.roles?.includes('Support Agent') === true);

  const q = useQuery({
    queryKey: ['support', 'tickets', status, category, cursor],
    queryFn: () => api.listTickets(apiClient, { status, category, cursor }),
  });
  const tickets = q.data?.data ?? [];

  return (
    <RequireAuth>
      <div className="mx-auto max-w-3xl space-y-4 p-6">
        <div className="flex items-center justify-between">
          <h1 className="text-xl font-semibold">Support</h1>
          {isSupportAgent && (
            <Link to="/admin/support/tickets" className="text-sm text-sky-400 hover:underline">
              Staff queue →
            </Link>
          )}
        </div>

        <div className={cardCls}>
          <p className="text-sm text-neutral-400">
            We acknowledge tickets within <strong>8 business hours</strong>; formal complaints
            receive a final response within <strong>8 weeks</strong>. For account-security
            emergencies use{' '}
            <Link to="/settings?tab=safety" className="text-sky-400 hover:underline">
              emergency freeze
            </Link>
            .
          </p>
        </div>

        <div className={cardCls}>
          <div className="mb-3 flex flex-wrap items-end justify-between gap-2">
            <h2 className="text-sm font-semibold">Your tickets</h2>
            <div className="flex items-end gap-2">
              <Field label="Status">
                {(id, describedBy, invalid) => (
                  <select
                    id={id}
                    aria-describedby={describedBy}
                    aria-invalid={invalid}
                    className={selectCls}
                    value={status}
                    onChange={(e) => {
                      setCursor(undefined);
                      setStatus(e.target.value);
                    }}
                  >
                    <option value="">All</option>
                    {api.TICKET_STATUSES.map((s) => (
                      <option key={s} value={s}>
                        {s}
                      </option>
                    ))}
                  </select>
                )}
              </Field>
              <Field label="Category">
                {(id, describedBy, invalid) => (
                  <select
                    id={id}
                    aria-describedby={describedBy}
                    aria-invalid={invalid}
                    className={selectCls}
                    value={category}
                    onChange={(e) => {
                      setCursor(undefined);
                      setCategory(e.target.value);
                    }}
                  >
                    <option value="">All</option>
                    {api.TICKET_CATEGORIES.map((c) => (
                      <option key={c} value={c}>
                        {c}
                      </option>
                    ))}
                  </select>
                )}
              </Field>
              <button
                type="button"
                className={`${btnGhost} mb-4`}
                onClick={() => {
                  setNewTicket((v) => !v);
                }}
              >
                {newTicket ? 'Close form' : 'New ticket'}
              </button>
            </div>
          </div>
          <ErrorBox error={q.error} />
          {q.isPending ? (
            <p className="text-sm text-neutral-400">Loading tickets…</p>
          ) : tickets.length === 0 ? (
            <p className="text-sm text-neutral-400">No tickets match.</p>
          ) : (
            <TicketTable tickets={tickets} />
          )}
          {(q.data?.next_cursor ?? '') !== '' && (
            <button
              type="button"
              className={`${btnGhost} mt-3`}
              onClick={() => {
                if (q.data !== undefined) setCursor(q.data.next_cursor);
              }}
            >
              Load more
            </button>
          )}
        </div>

        {newTicket && <TicketForm />}
      </div>
    </RequireAuth>
  );
}
