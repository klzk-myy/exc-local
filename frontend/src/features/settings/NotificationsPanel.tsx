/**
 * Notifications tab — channel × event matrix (Task 10.3.22 item 10) plus
 * consent toggles (Task 10.3.22 item 9, Phase-14 Task 14.3.7).
 *   GET/PUT /account/notifications/preferences  {preferences: {event: {channel: bool}}}
 *   PUT     /account/consent                    {marketing, research, third_party}
 */
import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ErrorBox, btnPrimary, cardCls, tableCls, tdCls, thCls } from '@/lib/ui';

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
                          type="checkbox" className="h-6 w-6"
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
              type="checkbox" className="h-6 w-6"
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
    </div>
  );
}
