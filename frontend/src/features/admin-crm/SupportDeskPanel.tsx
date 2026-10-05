/**
 * Support desk panel (Phase-10.5 Task 10.5.3.4 §3) — the account-
 * scoped ticket list, ticket detail with internal notes, the agent
 * note composer, and a complaint-register excerpt filtered to this
 * customer (MiFID complaint-handling record).
 */
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { fetchSupportTickets } from '@/lib/admin/api';
import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  StatusBadge,
  textareaCls,
} from '@/lib/ui';

import { AccessDeniedCard } from '../admin/RequireAdmin';
import { isAccessDenied } from '../admin/adminRole';
import { addTicketNote, fetchTicketDetail } from './api';

export function SupportDeskPanel({
  adminApi,
  accountId,
}: {
  adminApi: BoundAdminApi;
  accountId: number;
}) {
  const qc = useQueryClient();
  const [selected, setSelected] = useState<number | null>(null);
  const [noteBody, setNoteBody] = useState('');
  const [notePublic, setNotePublic] = useState(false);
  const [noteBusy, setNoteBusy] = useState(false);
  const [noteError, setNoteError] = useState<unknown>(null);

  const tickets = useQuery({
    queryKey: ['admin-crm', 'tickets', adminApi.env, accountId],
    queryFn: () => fetchSupportTickets(adminApi, { accountId, limit: 20 }),
    retry: false,
  });
  const complaints = useQuery({
    queryKey: ['admin-crm', 'complaints', adminApi.env, accountId],
    queryFn: () => adminApi.get<unknown>('/admin/support/complaints/register', { limit: 50 }),
    retry: false,
    select: (raw): unknown[] => {
      if (typeof raw !== 'object' || raw === null || !('data' in raw)) return [];
      const rows = Array.isArray(raw.data) ? raw.data : [];
      return rows.filter(
        (r) =>
          typeof r === 'object' &&
          r !== null &&
          (r as Record<string, unknown>)['account_id'] === accountId,
      );
    },
  });
  const detail = useQuery({
    queryKey: ['admin-crm', 'ticket', adminApi.env, selected],
    queryFn: () => fetchTicketDetail(adminApi, selected ?? 0),
    retry: false,
    enabled: selected !== null,
  });

  if (isAccessDenied(tickets.error)) {
    return <AccessDeniedCard detail="The support desk requires the Support Agent role." />;
  }

  const postNote = async () => {
    if (selected === null) return;
    setNoteBusy(true);
    setNoteError(null);
    try {
      await addTicketNote(adminApi, selected, noteBody.trim(), !notePublic);
      setNoteBody('');
      await qc.invalidateQueries({ queryKey: ['admin-crm', 'ticket'] });
    } catch (e) {
      setNoteError(e);
    } finally {
      setNoteBusy(false);
    }
  };

  return (
    <section className={cardCls} aria-label="Support desk">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Support desk</h2>
      <p className={hintTextCls}>
        Tickets and internal notes are audit-logged; notes default to internal (uncheck for a
        customer-visible reply).
      </p>

      {tickets.isError && <ErrorBox error={tickets.error} />}
      {tickets.isSuccess &&
        (tickets.data.data.length === 0 ? (
          <p className="text-sm text-neutral-500">No tickets on this account.</p>
        ) : (
          <ul className="max-h-40 space-y-1 overflow-y-auto text-sm">
            {tickets.data.data.map((t) => (
              <li key={t.ticketId}>
                <button
                  type="button"
                  className={btnGhost}
                  aria-pressed={selected === t.ticketId}
                  onClick={() => setSelected((cur) => (cur === t.ticketId ? null : t.ticketId))}
                >
                  #{t.ticketId} [{t.priority}] {t.subject} — {t.status}
                  {t.slaBreached && ' · SLA BREACHED'}
                </button>
              </li>
            ))}
          </ul>
        ))}

      {selected !== null && (
        <div className="mt-3 border-t border-neutral-800 pt-3">
          {detail.isError && <ErrorBox error={detail.error} />}
          {detail.isSuccess && (
            <>
              <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
                Notes — ticket #{selected}
              </h3>
              {detail.data.notes.length === 0 ? (
                <p className="text-xs text-neutral-500">No notes yet.</p>
              ) : (
                <ul className="mb-2 max-h-32 space-y-1 overflow-y-auto text-xs text-neutral-300">
                  {detail.data.notes.map((n) => (
                    <li key={n.noteId}>
                      <span className={n.internal ? 'text-amber-300' : 'text-sky-300'}>
                        {n.internal ? 'internal' : 'public'}
                      </span>{' '}
                      {n.createdAt.slice(0, 19)} — {n.body}
                    </li>
                  ))}
                </ul>
              )}
            </>
          )}
          <label className="sr-only" htmlFor="sd-note">
            Note body
          </label>
          <textarea
            id="sd-note"
            className={textareaCls}
            rows={2}
            placeholder="Add a note…"
            value={noteBody}
            onChange={(e) => setNoteBody(e.target.value)}
          />
          <div className="mt-1 flex items-center gap-3">
            <label className="flex items-center gap-2 text-xs text-neutral-300">
              <input
                type="checkbox"
                className="h-4 w-4"
                checked={notePublic}
                onChange={(e) => setNotePublic(e.target.checked)}
              />
              Customer-visible reply
            </label>
            <button
              type="button"
              className={btnPrimary}
              disabled={noteBusy || noteBody.trim() === ''}
              onClick={() => void postNote()}
            >
              Add note
            </button>
          </div>
          {noteError !== null && <ErrorBox error={noteError} />}
        </div>
      )}

      <div className="mt-3 border-t border-neutral-800 pt-3">
        <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
          Complaint register
        </h3>
        {complaints.isError && <ErrorBox error={complaints.error} />}
        {complaints.isSuccess &&
          (complaints.data.length === 0 ? (
            <p className="text-xs text-neutral-500">
              No COMPLAINT/DISPUTE entries for this account.
            </p>
          ) : (
            <ul className="space-y-1 text-xs text-neutral-300">
              {complaints.data.map((r, i) => {
                const row = r as Record<string, unknown>;
                const cell = (k: string): string => {
                  const v = row[k];
                  return typeof v === 'string' ? v : typeof v === 'number' ? String(v) : '';
                };
                return (
                  <li key={i}>
                    <StatusBadge value={cell('status')} /> #{cell('ticket_id')} — {cell('subject')}
                  </li>
                );
              })}
            </ul>
          ))}
      </div>
    </section>
  );
}
