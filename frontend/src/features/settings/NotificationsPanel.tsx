/**
 * Notifications tab — channel × event matrix (Task 10.3.22 item 10) plus
 * consent toggles (Task 10.3.22 item 9, Phase-14 Task 14.3.7).
 *   GET/PUT /account/notifications/preferences  {preferences: {event: {channel: bool}}}
 *   PUT     /account/consent                    {marketing, research, third_party}
 */
import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import {
  ErrorBox,
  StatusBadge,
  btnGhost,
  btnPrimary,
  cardCls,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

import * as api from './api';

const EVENT_LABELS: Record<(typeof api.NOTIFICATION_EVENTS)[number], string> = {
  order_fill: 'Order fills',
  funding: 'Deposits & withdrawals',
  security: 'Security alerts',
  margin: 'Margin warnings',
  announcements: 'Platform announcements',
};

export default function NotificationsPanel() {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ['account', 'notification-prefs'],
    queryFn: () => api.getNotificationPrefs(apiClient),
  });
  const [prefs, setPrefs] = useState<api.NotificationPrefs>({});
  const [consent, setConsent] = useState<api.ConsentState>({
    marketing: false,
    research: false,
    third_party: false,
  });
  const [consentSaved, setConsentSaved] = useState(false);

  useEffect(() => {
    if (q.data !== undefined) setPrefs(q.data);
  }, [q.data]);

  const save = useMutation({
    mutationFn: () => api.putNotificationPrefs(apiClient, prefs),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['account', 'notification-prefs'] });
    },
  });
  const saveConsent = useMutation({
    mutationFn: () => api.putConsent(apiClient, consent),
    onSuccess: () => {
      setConsentSaved(true);
    },
  });

  const toggle = (event: string, channel: string) => {
    setPrefs((p) => ({
      ...p,
      [event]: { ...p[event], [channel]: p[event]?.[channel] !== true },
    }));
  };

  return (
    <div className="space-y-4">
      <div className={cardCls}>
        <h3 className="mb-2 text-sm font-semibold">Notification preferences</h3>
        <p className="mb-3 text-sm text-neutral-400">
          Choose which events reach each channel. Security alerts cannot be fully disabled.
        </p>
        {q.isError && <ErrorBox error={q.error} />}
        {q.isPending ? (
          <p className="text-sm text-neutral-400">Loading preferences…</p>
        ) : (
          <>
            <table className={tableCls}>
              <thead>
                <tr>
                  <th className={thCls}>Event</th>
                  {api.NOTIFICATION_CHANNELS.map((c) => (
                    <th key={c} className={thCls}>
                      {c.toUpperCase()}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {api.NOTIFICATION_EVENTS.map((ev) => (
                  <tr key={ev}>
                    <td className={tdCls}>{EVENT_LABELS[ev]}</td>
                    {api.NOTIFICATION_CHANNELS.map((ch) => (
                      <td key={ch} className={tdCls}>
                        <input
                          type="checkbox"
                          className="h-6 w-6"
                          aria-label={`${EVENT_LABELS[ev]} via ${ch}`}
                          checked={prefs[ev]?.[ch] === true}
                          onChange={() => {
                            toggle(ev, ch);
                          }}
                        />
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
            <ErrorBox error={save.error} />
            <button
              type="button"
              className={`${btnPrimary} mt-3`}
              disabled={save.isPending}
              onClick={() => {
                save.mutate();
              }}
            >
              {save.isPending ? 'Saving…' : 'Save preferences'}
            </button>
            {save.isSuccess && (
              <p className="mt-2 text-sm text-emerald-400" role="status">
                Preferences saved.
              </p>
            )}
          </>
        )}
      </div>

      <div className={cardCls}>
        <h3 className="mb-2 text-sm font-semibold">Consents</h3>
        <p className="mb-3 text-sm text-neutral-400">
          Optional consents — trading terms and execution-policy acknowledgements are part of
          registration and cannot be revoked here.
        </p>
        {consentSaved && (
          <p className="mb-2 text-sm text-emerald-400" role="status">
            Consent preferences saved.
          </p>
        )}
        <ErrorBox error={saveConsent.error} />
        {(
          [
            ['marketing', 'Product & marketing emails'],
            ['research', 'Anonymous usage research'],
            ['third_party', 'Third-party partner offers'],
          ] as const
        ).map(([key, lbl]) => (
          <label key={key} className="mb-2 flex items-center gap-2 text-sm text-neutral-200">
            <input
              type="checkbox"
              className="h-6 w-6"
              checked={consent[key]}
              onChange={(e) => {
                setConsentSaved(false);
                setConsent((c) => ({ ...c, [key]: e.target.checked }));
              }}
            />
            {lbl}
          </label>
        ))}
        <button
          type="button"
          className={`${btnPrimary} mt-2`}
          disabled={saveConsent.isPending}
          onClick={() => {
            saveConsent.mutate();
          }}
        >
          {saveConsent.isPending ? 'Saving…' : 'Save consents'}
        </button>
      </div>

      <GdprConsentPanel />
    </div>
  );
}

/**
 * GDPR consent registry (Task 10.5.3.27 gate-coverage wiring,
 * Phase-25 Task 25.3.21) — purpose-keyed consents with granted_at /
 * revoked_at audit stamps from the registry (distinct from the
 * marketing-research-third_party block above).
 */
export function GdprConsentPanel() {
  const qc = useQueryClient();
  const [notice, setNotice] = useState<string | null>(null);
  const gdpr = useQuery({
    queryKey: ['account', 'gdpr-consents'],
    queryFn: () => api.fetchGdprConsents(apiClient),
    retry: false,
  });
  const setConsent = useMutation({
    mutationFn: ({ purpose, granted }: { purpose: string; granted: boolean }) =>
      api.putGdprConsent(apiClient, purpose, granted),
    onSuccess: async (_v, { purpose, granted }) => {
      setNotice(`${purpose}: ${granted ? 'granted' : 'revoked'}`);
      await qc.invalidateQueries({ queryKey: ['account', 'gdpr-consents'] });
    },
    onError: (e) => setNotice(e instanceof Error ? e.message : 'Consent update failed'),
  });

  return (
    <div className={`${cardCls} mt-4`}>
      <h3 className="mb-1 text-sm font-semibold">GDPR consent registry</h3>
      <p className="mb-3 text-sm text-neutral-400">
        Purpose-level lawful-basis consents with grant/revoke timestamps. Revocation is effective
        immediately and recorded in the audit trail.
      </p>
      {notice !== null && (
        <p role="status" className="mb-2 text-xs text-sky-300">
          {notice}
        </p>
      )}
      {gdpr.isError && <ErrorBox error={gdpr.error} />}
      {gdpr.isPending ? (
        <p className="text-sm text-neutral-400">Loading consent registry…</p>
      ) : (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Purpose</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>Granted</th>
              <th className={thCls}>Revoked</th>
              <th className={thCls} />
            </tr>
          </thead>
          <tbody>
            {(gdpr.data ?? []).map((c) => (
              <tr key={c.purpose}>
                <td className={tdCls}>{c.purpose}</td>
                <td className={tdCls}>
                  <StatusBadge value={c.granted ? 'GRANTED' : 'REVOKED'} />
                </td>
                <td className={tdCls}>{c.grantedAt ?? '—'}</td>
                <td className={tdCls}>{c.revokedAt ?? '—'}</td>
                <td className={tdCls}>
                  <button
                    type="button"
                    className={btnGhost}
                    disabled={setConsent.isPending}
                    onClick={() => setConsent.mutate({ purpose: c.purpose, granted: !c.granted })}
                  >
                    {c.granted ? 'Revoke' : 'Grant'}
                  </button>
                </td>
              </tr>
            ))}
            {(gdpr.data ?? []).length === 0 && (
              <tr>
                <td className={`${tdCls} text-neutral-500`} colSpan={5}>
                  No consent purposes registered.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      )}
    </div>
  );
}
