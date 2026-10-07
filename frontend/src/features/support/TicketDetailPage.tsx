/**
 * /support/tickets/:id — conversation thread + status + SLA surfacing
 * (Task 10.3.25 item 3). Notes are the public thread (internal notes are
 * never returned to the client endpoint). RESOLVED/CLOSED tickets show a
 * user-side reopen action.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useParams } from 'react-router';

import { apiClient } from '@/app/runtime';
import { RequireAuth } from '@/features/auth/guards';
import { ErrorBox, StatusBadge, btnGhost, cardCls, formatCountdown, useNow } from '@/lib/ui';

import * as api from './api';

const TRANSITIONS = ['OPEN', 'PENDING', 'IN_PROGRESS', 'RESOLVED', 'CLOSED'] as const;

function StatusTimeline({ status }: { status: string }) {
  const idx = TRANSITIONS.indexOf(status as (typeof TRANSITIONS)[number]);
  return (
    <ol className="mb-4 flex flex-wrap items-center gap-1" aria-label="Ticket lifecycle">
      {TRANSITIONS.map((s, i) => (
        <li key={s} className="flex items-center">
          <span
            className={`rounded px-2 py-0.5 text-xs ${
              idx >= 0 && i <= idx ? 'bg-sky-600/60 text-white' : 'bg-neutral-800 text-neutral-500'
            }`}
            aria-current={i === idx ? 'step' : undefined}
          >
            {s}
          </span>
          {i < TRANSITIONS.length - 1 && (
            <span className="mx-1 h-px w-4 bg-neutral-700" aria-hidden="true" />
          )}
        </li>
      ))}
    </ol>
  );
}

function SlaBlock({ ticket }: { ticket: api.Ticket }) {
  const now = useNow(60_000);
  const isComplaint = ticket.category === 'COMPLAINT' || ticket.type === 'COMPLAINT';
  return (
    <dl className="mb-4 grid grid-cols-2 gap-2 text-xs">
      <div className="rounded border border-neutral-800 p-2">
        <dt className="text-neutral-500">Acknowledgement SLA (8 business hours)</dt>
        <dd>
          {ticket.acknowledged_at !== undefined && ticket.acknowledged_at !== null ? (
            <span className="text-emerald-400">
              acknowledged {new Date(ticket.acknowledged_at).toLocaleString()}
            </span>
          ) : ticket.sla_due_at !== undefined ? (
            ticket.sla_breached === true || Date.parse(ticket.sla_due_at) <= now ? (
              <span className="text-red-400">breached</span>
            ) : (
              <span className="text-amber-300" role="timer">
                due in {formatCountdown(Date.parse(ticket.sla_due_at) - now)}
              </span>
            )
          ) : (
            '—'
          )}
        </dd>
      </div>
      {isComplaint && (
        <div className="rounded border border-neutral-800 p-2">
          <dt className="text-neutral-500">Final response due (8 weeks, MiFID)</dt>
          <dd>
            {ticket.final_response_due_at !== undefined ? (
              ticket.final_sla_breached === true ? (
                <span className="text-red-400">breached</span>
              ) : (
                new Date(ticket.final_response_due_at).toLocaleDateString()
              )
            ) : (
              '—'
            )}
          </dd>
        </div>
      )}
    </dl>
  );
}

export default function TicketDetailPage() {
  const { id = '' } = useParams();
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ['support', 'ticket', id],
    queryFn: () => api.getTicket(apiClient, id),
    enabled: id !== '',
  });
  const reopen = useMutation({
    mutationFn: () => api.reopenTicket(apiClient, Number(id)),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['support', 'ticket', id] });
    },
  });

  const t = q.data?.ticket;
  const reopenable = t !== undefined && (t.status === 'RESOLVED' || t.status === 'CLOSED');

  return (
    <RequireAuth>
      <div className="mx-auto max-w-3xl p-6">
        <Link to="/support" className="text-sm text-sky-400 hover:underline">
          ← All tickets
        </Link>
        <h1 className="mt-4 text-2xl font-semibold">
          {t !== undefined ? t.subject : `Ticket ${id}`}
        </h1>
        {q.isPending && <p className="mt-4 text-sm text-neutral-400">Loading…</p>}
        {q.isError && <ErrorBox error={q.error} />}
        {t !== undefined && (
          <div className={`${cardCls} mt-4`}>
            <div className="mb-2 flex items-start justify-between">
              <h2 className="text-lg font-semibold">Details</h2>
              <StatusBadge value={t.status} />
            </div>
            <StatusTimeline status={t.status} />
            <SlaBlock ticket={t} />
            {t.body !== undefined && t.body !== '' && (
              <div className="mb-4 rounded border border-neutral-800 p-3">
                <p className="mb-1 text-xs text-neutral-500">Your report</p>
                <p className="whitespace-pre-wrap text-sm text-neutral-200">{t.body}</p>
              </div>
            )}
            <h2 className="mb-2 text-sm font-semibold">Conversation</h2>
            <ul className="space-y-2">
              {(q.data?.notes ?? []).map((n) => (
                <li
                  key={n.note_id}
                  className={`rounded border p-3 text-sm ${
                    n.author_kind === 'ADMIN'
                      ? 'border-sky-800/50 bg-sky-950/20'
                      : 'border-neutral-800'
                  }`}
                >
                  <p className="mb-1 text-xs text-neutral-500">
                    {n.author_kind === 'ADMIN' ? 'Support' : 'You'} ·{' '}
                    {new Date(n.created_at).toLocaleString()}
                  </p>
                  <p className="whitespace-pre-wrap text-neutral-200">{n.body}</p>
                </li>
              ))}
              {(q.data?.notes ?? []).length === 0 && (
                <li className="text-sm text-neutral-400">
                  No replies yet — we acknowledge tickets within 8 business hours.
                </li>
              )}
            </ul>
            {reopenable && (
              <div className="mt-4">
                <ErrorBox error={reopen.error} />
                <button
                  type="button"
                  className={btnGhost}
                  disabled={reopen.isPending}
                  onClick={() => {
                    reopen.mutate();
                  }}
                >
                  {reopen.isPending ? 'Reopening…' : 'Reopen this ticket'}
                </button>
              </div>
            )}
          </div>
        )}
      </div>
    </RequireAuth>
  );
}
