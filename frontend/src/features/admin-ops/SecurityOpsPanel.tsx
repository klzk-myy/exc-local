/**
 * Security ops panel (Task 10.5.3.27 gate-coverage wiring) — the admin
 * side of the VDP (Phase-13.5 Task 13.5.3.8/9): disclosure register
 * (list/detail/triage/update/internal intake) plus the secrets
 * inventory (list/upsert/mark-rotated, Task 9.3.29).
 */
import { useMutation, useQuery } from '@tanstack/react-query';
import { useState } from 'react';

import type { BoundAdminApi } from '@/lib/env';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  hintTextCls,
  inputCls,
  labelCls,
  selectCls,
  StatusBadge,
} from '@/lib/ui';

const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null;
const str = (v: unknown): string => (typeof v === 'string' ? v : '');

interface DisclosureRow {
  id: number;
  title: string;
  severity: string;
  status: string;
  source: string;
}
const parseDisclosure = (v: unknown): DisclosureRow => {
  const r = isRecord(v) ? v : {};
  return {
    id: typeof r['id'] === 'number' ? r['id'] : 0,
    title: str(r['title']),
    severity: str(r['severity']),
    status: str(r['status']),
    source: str(r['source']),
  };
};

export function SecurityOpsPanel({ adminApi }: { adminApi: BoundAdminApi }) {
  const [notice, setNotice] = useState<string | null>(null);
  const [detail, setDetail] = useState<number | null>(null);
  const [detailBody, setDetailBody] = useState<unknown>(null);
  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : 'Action failed');

  const [statusFilter, setStatusFilter] = useState('');
  const disclosures = useQuery({
    queryKey: ['admin-security', 'disclosures', statusFilter],
    queryFn: async () => {
      const raw = await adminApi.get<unknown>('/admin/security/disclosures', {
        limit: '50',
        ...(statusFilter !== '' ? { status: statusFilter } : {}),
      });
      const items =
        isRecord(raw) && Array.isArray(raw['items'])
          ? raw['items']
          : isRecord(raw) && Array.isArray(raw['disclosures'])
            ? (raw['disclosures'] as unknown[])
            : [];
      return items.map(parseDisclosure);
    },
    retry: false,
  });

  const showDetail = useMutation({
    mutationFn: (id: number) => adminApi.get<unknown>(`/admin/security/disclosures/${id}`),
    onSuccess: (raw, id) => {
      setDetail(id);
      setDetailBody(raw);
    },
    onError: onErr,
  });

  const [triage, setTriage] = useState({ id: '', severity: 'MEDIUM', cvss: '', assignToMe: true });
  const doTriage = useMutation({
    mutationFn: () =>
      adminApi.post<unknown>(`/admin/security/disclosures/${triage.id}/triage`, {
        severity: triage.severity,
        cvss_score: triage.cvss === '' ? undefined : Number(triage.cvss),
        assign_to_me: triage.assignToMe,
      }),
    onSuccess: () => {
      setNotice(`Disclosure #${triage.id} triaged ${triage.severity}`);
      void disclosures.refetch();
    },
    onError: onErr,
  });

  const [upd, setUpd] = useState({ id: '', status: '', bulletinRef: '', summary: '' });
  const doUpdate = useMutation({
    mutationFn: () =>
      adminApi.put<unknown>(`/admin/security/disclosures/${upd.id}`, {
        status: upd.status,
        bulletin_ref: upd.bulletinRef === '' ? undefined : upd.bulletinRef,
        resolution_summary: upd.summary === '' ? undefined : upd.summary,
      }),
    onSuccess: () => {
      setNotice(`Disclosure #${upd.id} updated`);
      void disclosures.refetch();
    },
    onError: onErr,
  });

  const [intake, setIntake] = useState({ reportId: '', source: 'INTERNAL', title: '' });
  const doIntake = useMutation({
    mutationFn: () =>
      adminApi.post<unknown>('/admin/security/disclosures/intake', {
        report_id: intake.reportId,
        source: intake.source,
        title: intake.title,
      }),
    onSuccess: () => {
      setNotice('Internal disclosure filed');
      void disclosures.refetch();
    },
    onError: onErr,
  });

  const secrets = useQuery({
    queryKey: ['admin-security', 'secrets'],
    queryFn: async () => {
      const raw = await adminApi.get<unknown>('/admin/security/secrets-inventory');
      const rows =
        isRecord(raw) && Array.isArray(raw['rows'])
          ? raw['rows']
          : isRecord(raw) && Array.isArray(raw['secrets'])
            ? (raw['secrets'] as unknown[])
            : [];
      return rows.filter(isRecord);
    },
    retry: false,
  });
  const [secForm, setSecForm] = useState({
    name: '',
    cls: '',
    owner: '',
    ttl: '',
    procedure: '',
  });
  const upsertSecret = useMutation({
    mutationFn: () =>
      adminApi.put<unknown>('/admin/security/secrets-inventory', {
        secret_name: secForm.name,
        secret_class: secForm.cls,
        owner: secForm.owner,
        ttl_seconds: secForm.ttl === '' ? undefined : Number(secForm.ttl),
        rotation_procedure: secForm.procedure,
      }),
    onSuccess: () => {
      setNotice(`Secret ${secForm.name} upserted`);
      void secrets.refetch();
    },
    onError: onErr,
  });
  const [mark, setMark] = useState({ name: '', emergency: false, incidentRef: '' });
  const markRotated = useMutation({
    mutationFn: () =>
      adminApi.post<unknown>(
        `/admin/security/secrets-inventory/${encodeURIComponent(mark.name)}/mark-rotated`,
        {
          emergency: mark.emergency,
          incident_ref: mark.incidentRef,
        },
      ),
    onSuccess: () => {
      setNotice(`Secret ${mark.name} marked rotated`);
      void secrets.refetch();
    },
    onError: onErr,
  });

  return (
    <section className={cardCls} aria-label="Security ops">
      <h2 className="mb-2 text-sm font-semibold">Security — disclosures &amp; secrets</h2>
      {notice !== null && (
        <p role="status" className="mb-2 text-xs text-sky-300">
          {notice}
        </p>
      )}

      <div className="grid gap-4 md:grid-cols-2">
        <div>
          <div className="mb-2 flex items-end gap-2">
            <h3 className="text-xs font-semibold uppercase tracking-wide text-neutral-500">
              VDP disclosures
            </h3>
            <label className={labelCls}>
              <span className="sr-only">Status filter</span>
              <select
                className={selectCls}
                value={statusFilter}
                onChange={(e) => setStatusFilter(e.target.value)}
                aria-label="Status filter"
              >
                <option value="">all</option>
                {['NEW', 'TRIAGED', 'ASSIGNED', 'RESOLVED', 'DISPUTED'].map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
            </label>
          </div>
          {disclosures.isPending && <p className={hintTextCls}>Loading…</p>}
          {disclosures.isError && (
            <p role="alert" className="text-xs text-rose-300">
              {disclosures.error.message}
            </p>
          )}
          <ul className="divide-y divide-neutral-800 text-xs">
            {(disclosures.data ?? []).map((d) => (
              <li key={d.id}>
                <button
                  type="button"
                  className="flex w-full items-center justify-between py-1.5 text-left"
                  onClick={() => showDetail.mutate(d.id)}
                  aria-expanded={detail === d.id}
                >
                  <span>
                    #{d.id} {d.title}
                  </span>
                  <StatusBadge value={d.status || d.severity} />
                </button>
                {detail === d.id && detailBody !== null && (
                  <pre
                    className="mb-2 max-h-40 overflow-y-auto rounded border border-neutral-800 p-2 font-mono text-neutral-300"
                    tabIndex={0}
                  >
                    {JSON.stringify(detailBody, null, 2)}
                  </pre>
                )}
              </li>
            ))}
            {disclosures.data?.length === 0 && <li className={hintTextCls}>No disclosures.</li>}
          </ul>

          <form
            className="mt-2 grid gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              doTriage.mutate();
            }}
          >
            <h4 className="text-xs font-semibold text-neutral-400">Triage</h4>
            <div className="flex flex-wrap gap-2">
              <label className={labelCls}>
                Id
                <input
                  className={inputCls}
                  value={triage.id}
                  onChange={(e) => setTriage({ ...triage, id: e.target.value })}
                  required
                  inputMode="numeric"
                />
              </label>
              <label className={labelCls}>
                Severity
                <select
                  className={selectCls}
                  value={triage.severity}
                  onChange={(e) => setTriage({ ...triage, severity: e.target.value })}
                >
                  {['LOW', 'MEDIUM', 'HIGH', 'CRITICAL'].map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              </label>
              <label className={labelCls}>
                CVSS (optional)
                <input
                  className={inputCls}
                  value={triage.cvss}
                  onChange={(e) => setTriage({ ...triage, cvss: e.target.value })}
                  inputMode="decimal"
                />
              </label>
            </div>
            <label className="flex items-center gap-2 text-xs text-neutral-300">
              <input
                type="checkbox"
                checked={triage.assignToMe}
                onChange={(e) => setTriage({ ...triage, assignToMe: e.target.checked })}
              />
              Assign to me
            </label>
            <button type="submit" className={btnPrimary} disabled={doTriage.isPending}>
              Apply triage
            </button>
          </form>

          <form
            className="mt-2 grid gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              doUpdate.mutate();
            }}
          >
            <h4 className="text-xs font-semibold text-neutral-400">Update status</h4>
            <div className="flex flex-wrap gap-2">
              <label className={labelCls}>
                Id
                <input
                  className={inputCls}
                  value={upd.id}
                  onChange={(e) => setUpd({ ...upd, id: e.target.value })}
                  required
                  inputMode="numeric"
                />
              </label>
              <label className={labelCls}>
                Status
                <input
                  className={inputCls}
                  value={upd.status}
                  onChange={(e) => setUpd({ ...upd, status: e.target.value })}
                  required
                />
              </label>
              <label className={labelCls}>
                Bulletin ref
                <input
                  className={inputCls}
                  value={upd.bulletinRef}
                  onChange={(e) => setUpd({ ...upd, bulletinRef: e.target.value })}
                />
              </label>
            </div>
            <button type="submit" className={btnGhost} disabled={doUpdate.isPending}>
              Update disclosure
            </button>
          </form>

          <form
            className="mt-2 grid gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              doIntake.mutate();
            }}
          >
            <h4 className="text-xs font-semibold text-neutral-400">Internal intake</h4>
            <div className="flex flex-wrap gap-2">
              <label className={labelCls}>
                Report id
                <input
                  className={inputCls}
                  value={intake.reportId}
                  onChange={(e) => setIntake({ ...intake, reportId: e.target.value })}
                  required
                />
              </label>
              <label className={labelCls}>
                Source
                <select
                  className={selectCls}
                  value={intake.source}
                  onChange={(e) => setIntake({ ...intake, source: e.target.value })}
                >
                  <option value="INTERNAL">Internal</option>
                  <option value="PENTEST">Pentest</option>
                </select>
              </label>
              <label className={labelCls}>
                Title
                <input
                  className={inputCls}
                  value={intake.title}
                  onChange={(e) => setIntake({ ...intake, title: e.target.value })}
                  required
                />
              </label>
            </div>
            <button type="submit" className={btnGhost} disabled={doIntake.isPending}>
              File disclosure
            </button>
          </form>
        </div>

        <div>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
            Secrets inventory
          </h3>
          {secrets.isPending && <p className={hintTextCls}>Loading…</p>}
          {secrets.isError && (
            <p role="alert" className="text-xs text-rose-300">
              {secrets.error.message}
            </p>
          )}
          <ul
            className="mb-3 max-h-56 divide-y divide-neutral-800 overflow-y-auto text-xs"
            tabIndex={0}
          >
            {(secrets.data ?? []).map((s, i) => (
              <li key={i} className="py-1 font-mono text-neutral-300">
                {str(s['secret_name']) || `#${i}`} — {str(s['secret_class'])} {str(s['owner'])}
              </li>
            ))}
            {secrets.data?.length === 0 && <li className={hintTextCls}>No secrets registered.</li>}
          </ul>

          <form
            className="grid gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              upsertSecret.mutate();
            }}
          >
            <h4 className="text-xs font-semibold text-neutral-400">Upsert secret</h4>
            {(
              [
                ['name', 'Secret name'],
                ['cls', 'Class'],
                ['owner', 'Owner'],
                ['ttl', 'TTL seconds'],
                ['procedure', 'Rotation procedure'],
              ] as const
            ).map(([k, lab]) => (
              <label key={k} className={labelCls}>
                {lab}
                <input
                  className={inputCls}
                  value={secForm[k]}
                  onChange={(e) => setSecForm({ ...secForm, [k]: e.target.value })}
                  required={k !== 'ttl'}
                />
              </label>
            ))}
            <button type="submit" className={btnPrimary} disabled={upsertSecret.isPending}>
              Upsert
            </button>
          </form>

          <form
            className="mt-2 grid gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              markRotated.mutate();
            }}
          >
            <h4 className="text-xs font-semibold text-neutral-400">Mark rotated</h4>
            <label className={labelCls}>
              Secret name
              <input
                className={inputCls}
                value={mark.name}
                onChange={(e) => setMark({ ...mark, name: e.target.value })}
                required
              />
            </label>
            <label className="flex items-center gap-2 text-xs text-neutral-300">
              <input
                type="checkbox"
                checked={mark.emergency}
                onChange={(e) => setMark({ ...mark, emergency: e.target.checked })}
              />
              Emergency (leak-triggered — incident ref required)
            </label>
            {mark.emergency && (
              <label className={labelCls}>
                Incident ref
                <input
                  className={inputCls}
                  value={mark.incidentRef}
                  onChange={(e) => setMark({ ...mark, incidentRef: e.target.value })}
                  required
                />
              </label>
            )}
            <button type="submit" className={btnGhost} disabled={markRotated.isPending}>
              Mark rotated
            </button>
          </form>
        </div>
      </div>
    </section>
  );
}
