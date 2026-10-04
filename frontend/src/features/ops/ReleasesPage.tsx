/**
 * Release registry + promotion timeline (Phase-10 Task 10.3.20 item 2).
 *
 *   GET  /api/v1/admin/releases                 env-scoped list
 *   POST /api/v1/admin/releases/{id}/promote    {to_env, reason,
 *        approver_id} — direction-enforced (dev→staging→production),
 *        dual-control + §19.16.3 gate interlocks in production.
 *
 * Promotion targets follow the NEXT_ENV lattice; the production leg
 * requires an approver id up front (the server also validates it). Gate
 * evidence is rendered verbatim — never summarized away.
 */
import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError, type ApiClient } from '@/lib/api';
import {
  fetchReleases,
  parseGates,
  promoteRelease,
  registerRelease,
  type GateResult,
  type Release,
} from '@/lib/admin/api';
import {
  EnvSwitcher,
  EnvWatermark,
  EnvPill,
  NEXT_ENV,
  useBoundAdminApi,
  type BoundAdminApi,
} from '@/lib/env';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  hintTextCls,
  inputCls,
  labelCls,
  tableCls,
  tdCls,
  textareaCls,
  thCls,
  ErrorBox,
  Modal,
  StatusBadge,
} from '@/lib/ui';
import { AccessDeniedCard, RequireAdmin } from '@/features/admin/RequireAdmin';
import { isAccessDenied, useAdminRole } from '@/features/admin/adminRole';

function PromoteModal({
  release,
  busy,
  onClose,
  onSubmit,
}: {
  release: Release;
  busy: boolean;
  onClose: () => void;
  onSubmit: (toEnv: string, reason: string, approverId?: number) => void;
}) {
  const next = NEXT_ENV[release.env as 'dev' | 'staging'] ?? null;
  const [reason, setReason] = useState('');
  const [approver, setApprover] = useState('');
  const toProd = next === 'production';
  const approverId = Number(approver);
  const valid = next !== null && (!toProd || (Number.isInteger(approverId) && approverId > 0));
  return (
    <Modal open title={`Promote ${release.component} ${release.version}`} onClose={onClose}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (valid) onSubmit(next, reason.trim(), toProd ? approverId : undefined);
        }}
      >
        <p className="mb-3 text-sm text-neutral-400">
          Promotion direction is enforced server-side ({release.env} → {next ?? 'none'}).
          {toProd && ' Production promotes are dual-controlled — supply the approving admin id.'}
        </p>
        <label className={labelCls} htmlFor="pr-reason">
          Reason
        </label>
        <input
          id="pr-reason"
          className={`${inputCls} mb-3`}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
        {toProd && (
          <>
            <label className={labelCls} htmlFor="pr-approver">
              Approver admin id (four-eyes)
            </label>
            <input
              id="pr-approver"
              className={`${inputCls} mb-3`}
              value={approver}
              onChange={(e) => setApprover(e.target.value)}
            />
          </>
        )}
        <div className="flex justify-end gap-2">
          <button type="button" className={btnGhost} onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className={btnPrimary} disabled={!valid || busy}>
            {busy ? 'Promoting…' : `Promote to ${next ?? '—'}`}
          </button>
        </div>
      </form>
    </Modal>
  );
}

/** Evaluated §19.16.3 gate list — rendered verbatim on both the
 * EXECUTED and the BLOCKED/FORBIDDEN path so operators see exactly
 * which interlock fired (deploy window, open incidents, DR standby…). */
function GateList({ gates }: { gates: GateResult[] }) {
  if (gates.length === 0) return null;
  return (
    <ul className="mb-2 space-y-0.5 text-xs" data-testid="gate-list">
      {gates.map((g) => (
        <li key={g.name} className={g.passed ? 'text-emerald-400' : 'text-red-400'}>
          {g.passed ? '✓' : '✗'} {g.name}
          {g.detail !== undefined && g.detail !== '' && (
            <span className="text-neutral-500"> — {g.detail}</span>
          )}
        </li>
      ))}
    </ul>
  );
}

function RegisterReleaseForm({
  adminApi,
  onRegistered,
}: {
  adminApi: BoundAdminApi;
  onRegistered: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [component, setComponent] = useState('');
  const [version, setVersion] = useState('');
  const [artifactHash, setArtifactHash] = useState('');
  const [gateEvidence, setGateEvidence] = useState('');
  const [notes, setNotes] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      let evidence: unknown;
      if (gateEvidence.trim() !== '') {
        try {
          evidence = JSON.parse(gateEvidence) as unknown;
        } catch {
          setError(new Error('gate_evidence must be valid JSON'));
          setBusy(false);
          return;
        }
      }
      await registerRelease(adminApi, {
        component: component.trim(),
        version: version.trim(),
        artifactHash: artifactHash.trim(),
        gateEvidence: evidence,
        notes: notes.trim() === '' ? undefined : notes.trim(),
      });
      setComponent('');
      setVersion('');
      setArtifactHash('');
      setGateEvidence('');
      setNotes('');
      onRegistered();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  if (!open) {
    return (
      <button type="button" className={btnGhost} onClick={() => setOpen(true)}>
        Register release…
      </button>
    );
  }
  const valid = component.trim() !== '' && version.trim() !== '' && artifactHash.trim() !== '';
  return (
    <div className="mb-3 rounded border border-neutral-700 p-3" data-testid="register-release">
      <div className="grid gap-2 sm:grid-cols-3">
        <div>
          <label className={labelCls} htmlFor="rr-component">
            Component
          </label>
          <input
            id="rr-component"
            className={inputCls}
            value={component}
            placeholder="gateway"
            onChange={(e) => setComponent(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="rr-version">
            Version
          </label>
          <input
            id="rr-version"
            className={inputCls}
            value={version}
            placeholder="1.4.3"
            onChange={(e) => setVersion(e.target.value)}
          />
        </div>
        <div>
          <label className={labelCls} htmlFor="rr-hash">
            Artifact hash
          </label>
          <input
            id="rr-hash"
            className={inputCls}
            value={artifactHash}
            placeholder="sha256…"
            onChange={(e) => setArtifactHash(e.target.value)}
          />
        </div>
      </div>
      <div className="mt-2">
        <label className={labelCls} htmlFor="rr-evidence">
          Gate evidence (JSON, optional)
        </label>
        <textarea
          id="rr-evidence"
          className={textareaCls}
          value={gateEvidence}
          placeholder='{"soak":{"status":"PASS","age_h":24}}'
          onChange={(e) => setGateEvidence(e.target.value)}
        />
      </div>
      <div className="mt-2">
        <label className={labelCls} htmlFor="rr-notes">
          Notes (optional)
        </label>
        <input
          id="rr-notes"
          className={inputCls}
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
        />
      </div>
      <ErrorBox error={error} onDismiss={() => setError(null)} />
      <div className="mt-2 flex gap-2">
        <button
          type="button"
          className={btnPrimary}
          disabled={busy || !valid}
          onClick={() => void submit()}
        >
          {busy ? 'Registering…' : 'Register release'}
        </button>
        <button type="button" className={btnGhost} onClick={() => setOpen(false)}>
          Close
        </button>
      </div>
    </div>
  );
}

function ReleasesTable({ adminApi, role }: { adminApi: BoundAdminApi; role: string | null }) {
  const qc = useQueryClient();
  const [promoting, setPromoting] = useState<Release | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [resultGates, setResultGates] = useState<GateResult[]>([]);
  const [blockedGates, setBlockedGates] = useState<GateResult[]>([]);

  const query = useQuery({
    queryKey: ['ops', 'releases', adminApi.env],
    queryFn: () => fetchReleases(adminApi, { env: adminApi.env }),
    retry: false,
    refetchInterval: 30_000,
  });

  const canPromote = role === 'Super Admin';

  const doPromote = async (
    release: Release,
    toEnv: string,
    reason: string,
    approverId?: number,
  ) => {
    setBusy(true);
    setError(null);
    setNotice(null);
    setResultGates([]);
    setBlockedGates([]);
    try {
      const res = await promoteRelease(adminApi, release.id, { toEnv, reason, approverId });
      setNotice(
        `Promote ${res.status} for ${release.component} ${release.version} → ${toEnv}` +
          (res.promotionId !== undefined ? ` (promotion #${res.promotionId})` : ''),
      );
      setResultGates(res.gates);
      setPromoting(null);
      await qc.invalidateQueries({ queryKey: ['ops', 'releases'] });
    } catch (e) {
      // A blocked attempt returns FORBIDDEN with details.gates — the
      // §19.16.3 evaluation snapshot. Render it verbatim under the error.
      if (e instanceof ApiError) setBlockedGates(parseGates(e.details?.['gates']));
      setError(e);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className={cardCls} aria-label="Releases">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">Releases — {adminApi.env}</h2>
      {query.error !== null && isAccessDenied(query.error) && (
        <AccessDeniedCard detail="The release registry requires the Super Admin role." />
      )}
      {query.error !== null && !isAccessDenied(query.error) && <ErrorBox error={query.error} />}
      <ErrorBox error={error} onDismiss={() => setError(null)} />
      {blockedGates.length > 0 && (
        <div className="mb-2">
          <p className={hintTextCls}>Promotion blocked — evaluated gates:</p>
          <GateList gates={blockedGates} />
        </div>
      )}
      {notice !== null && (
        <p className="mb-2 text-xs text-emerald-400" role="status">
          {notice}
        </p>
      )}
      <GateList gates={resultGates} />
      {canPromote && (
        <RegisterReleaseForm
          adminApi={adminApi}
          onRegistered={() => void qc.invalidateQueries({ queryKey: ['ops', 'releases'] })}
        />
      )}
      {query.error === null && (
        <table className={tableCls}>
          <thead>
            <tr>
              <th className={thCls}>Component</th>
              <th className={thCls}>Version</th>
              <th className={thCls}>Artifact</th>
              <th className={thCls}>Env</th>
              <th className={thCls}>Status</th>
              <th className={thCls}>Gate evidence</th>
              <th className={thCls}>Promote</th>
            </tr>
          </thead>
          <tbody>
            {(query.data ?? []).map((r) => {
              const next = NEXT_ENV[r.env as 'dev' | 'staging'];
              return (
                <tr key={r.id}>
                  <td className={tdCls}>{r.component}</td>
                  <td className={tdCls}>{r.version}</td>
                  <td className={tdCls}>
                    <code className="text-xs" title={r.artifactHash}>
                      {r.artifactHash.slice(0, 12) || '—'}
                    </code>
                  </td>
                  <td className={tdCls}>{r.env}</td>
                  <td className={tdCls}>
                    <StatusBadge value={r.status.toUpperCase()} />
                  </td>
                  <td className={tdCls}>
                    {r.gateEvidence !== undefined ? (
                      <code className="text-xs" title={JSON.stringify(r.gateEvidence)}>
                        {JSON.stringify(r.gateEvidence).slice(0, 60)}
                      </code>
                    ) : (
                      '—'
                    )}
                  </td>
                  <td className={tdCls}>
                    {next !== undefined && canPromote ? (
                      <button
                        type="button"
                        className={btnGhost}
                        onClick={() => setPromoting(r)}
                        title={`Promote to ${next} — server enforces gates + dual control`}
                      >
                        → {next}
                      </button>
                    ) : next !== undefined ? (
                      <span className="text-xs text-neutral-600">Super Admin</span>
                    ) : (
                      <span className="text-xs text-neutral-600">terminal env</span>
                    )}
                  </td>
                </tr>
              );
            })}
            {(query.data ?? []).length === 0 && !query.isLoading && (
              <tr>
                <td className={tdCls} colSpan={7}>
                  No releases registered in {adminApi.env}.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      )}
      {promoting !== null && (
        <PromoteModal
          release={promoting}
          busy={busy}
          onClose={() => setPromoting(null)}
          onSubmit={(toEnv, reason, approverId) =>
            void doPromote(promoting, toEnv, reason, approverId)
          }
        />
      )}
    </section>
  );
}

export default function ReleasesPage({ api = apiClient }: { api?: ApiClient }) {
  return (
    <RequireAdmin>
      <Releases api={api} />
    </RequireAdmin>
  );
}

function Releases({ api }: { api: ApiClient }) {
  const adminApi = useBoundAdminApi(api);
  const role = useAdminRole();
  return (
    <div className="mx-auto max-w-6xl p-6">
      <EnvWatermark />
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold">Releases</h1>
        <div className="flex items-center gap-3">
          <EnvSwitcher />
          <EnvPill />
        </div>
      </div>
      <ReleasesTable adminApi={adminApi} role={role} />
    </div>
  );
}
