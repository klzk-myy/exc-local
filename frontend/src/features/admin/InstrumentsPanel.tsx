/**
 * Instrument management panel (Phase-10 Task 10.3.5) — the live
 * Phase-15 Task 15.3.2 admin surface: list incl. non-ACTIVE states,
 * create→DRAFT, parameter edits, and the §7.1 lifecycle transitions.
 *
 * Role gates mirror the route registry + service matrix (spec §7.2 —
 * UX hint only, the server re-authorizes):
 *
 *   list/edit/activate/restrict/halt/resume  Risk Manager+ (resume dual)
 *   cancel-only                              Risk Manager | Compliance
 *   suspend                                  Compliance Officer+
 *   create/delist                            Super Admin (dual-control)
 *
 * Four-eyes ops (create/resume/delist) resolve as 'pending' — rendered
 * as a submitted-for-approval notice, not an error. DRAFT/ACTIVE rows
 * expose Edit; DELISTED is terminal and shows no actions.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { ApiClient } from '@/lib/api';
import type { VenueAdminRole } from '@/lib/auth/session';
import type { BoundAdminApi } from '@/lib/env';
import { UnavailablePanel, isNotImplemented } from '@/lib/input-helpers';
import {
  ErrorBox,
  Field,
  Modal,
  StatusBadge,
  btnDanger,
  btnGhost,
  btnPrimary,
  cardCls,
  inputCls,
  selectCls,
  tableCls,
  tdCls,
  textareaCls,
  thCls,
} from '@/lib/ui';

import { AccessDeniedCard } from './RequireAdmin';
import { isAccessDenied, permitsAdminRole, useAdminRole } from './adminRole';
import {
  createAdminInstrument,
  listAdminInstruments,
  transitionAdminInstrument,
  updateAdminInstrument,
  type AdminInstrument,
  type AdminInstrumentCreateInput,
  type AdminInstrumentOp,
  type AdminInstrumentTransitionInput,
  type AdminInstrumentUpdateInput,
} from './api';

const RISK_MANAGER: readonly VenueAdminRole[] = ['Risk Manager'];
const SUPER_ADMIN: readonly VenueAdminRole[] = ['Super Admin'];

interface OpMeta {
  label: string;
  /** §7.2 role set — Super Admin satisfies every gate implicitly. */
  roles: readonly VenueAdminRole[];
  reasonRequired: boolean;
  dual: boolean;
  danger: boolean;
  blurb: string;
}

const OP_META: Record<AdminInstrumentOp, OpMeta> = {
  activate: {
    label: 'Activate',
    roles: RISK_MANAGER,
    reasonRequired: false,
    dual: false,
    danger: false,
    blurb: 'DRAFT → ACTIVE — opens the book for trading.',
  },
  'cancel-only': {
    label: 'Cancel-only',
    roles: ['Risk Manager', 'Compliance Officer'],
    reasonRequired: true,
    dual: false,
    danger: false,
    blurb: '→ CANCEL_ONLY — new orders rejected; resting orders remain cancellable.',
  },
  restrict: {
    label: 'Restrict',
    roles: RISK_MANAGER,
    reasonRequired: true,
    dual: false,
    danger: false,
    blurb: '→ RESTRICTED — limit-only trading; opens the 24h delist-notice window.',
  },
  suspend: {
    label: 'Suspend',
    roles: ['Compliance Officer'],
    reasonRequired: true,
    dual: false,
    danger: true,
    blurb: '→ SUSPENDED — 5-minute cancel-only grace, then all resting orders are mass-cancelled.',
  },
  halt: {
    label: 'Halt',
    roles: RISK_MANAGER,
    reasonRequired: true,
    dual: false,
    danger: false,
    blurb: '→ HALTED — matching stops; resting orders are preserved.',
  },
  resume: {
    label: 'Resume',
    roles: RISK_MANAGER,
    reasonRequired: false,
    dual: true,
    danger: false,
    blurb: '→ ACTIVE — resumes from HALTED/SUSPENDED run a reopening CALL auction unless skipped.',
  },
  delist: {
    label: 'Delist',
    roles: SUPER_ADMIN,
    reasonRequired: true,
    dual: true,
    danger: true,
    blurb: '→ DELISTED — terminal; opens the 30-day reduce-only close window.',
  },
};

/** §7.1 edge set — which lifecycle ops each state admits (mirrors
 * lifecycleTransitions + the DRAFT-activate / live-resume constraints
 * in internal/admin/instrument_lifecycle.go). */
const OPS_BY_STATE: Record<string, readonly AdminInstrumentOp[]> = {
  DRAFT: ['activate'],
  ACTIVE: ['cancel-only', 'restrict', 'suspend', 'halt', 'delist'],
  CANCEL_ONLY: ['resume', 'suspend', 'halt', 'delist'],
  RESTRICTED: ['resume', 'cancel-only', 'suspend', 'halt', 'delist'],
  SUSPENDED: ['resume', 'halt', 'delist'],
  HALTED: ['resume', 'suspend', 'delist'],
};

const GRACE_LABEL: Record<string, string> = {
  CANCEL_ONLY_WINDOW: 'cancel-only grace',
  DELIST_NOTICE: 'delist notice',
  CLOSE_ONLY: 'reduce-only close window',
};

const INSTRUMENT_TYPES = ['SPOT', 'FORWARD', 'SWAP', 'NDF', 'OPTION'] as const;
const SETTLEMENT_CYCLES = [
  { value: 0, label: 'Same-day (T+0)' },
  { value: 1, label: 'T+1' },
  { value: 2, label: 'T+2' },
] as const;

const EDITABLE_STATES: ReadonlySet<string> = new Set(['DRAFT', 'ACTIVE']);

function parseIntField(v: string): number | null {
  const n = Number(v.trim());
  return Number.isInteger(n) ? n : null;
}

// ---------------------------------------------------------------------------
// Transition confirmation dialog — one modal serves every op: reason
// capture (mandatory for most), reopening-auction options on resume,
// and the dual-control note for four-eyes ops.
// ---------------------------------------------------------------------------

function TransitionDialog({
  inst,
  op,
  busy,
  error,
  onCancel,
  onConfirm,
}: {
  inst: AdminInstrument;
  op: AdminInstrumentOp;
  busy: boolean;
  error: unknown;
  onCancel: () => void;
  onConfirm: (input: AdminInstrumentTransitionInput) => void;
}) {
  const meta = OP_META[op];
  const [reason, setReason] = useState('');
  const [skipAuction, setSkipAuction] = useState(false);
  const [auction, setAuction] = useState(false);
  const trimmed = reason.trim();
  const blocked = meta.reasonRequired && trimmed === '';
  return (
    <Modal open title={`${meta.label} ${inst.symbol}`} onClose={onCancel}>
      <p className="mb-3 text-sm text-neutral-300">{meta.blurb}</p>
      {meta.dual && (
        <p className="mb-3 rounded border border-amber-800/50 bg-amber-950/30 px-3 py-2 text-xs text-amber-200">
          §7.2 four-eyes — submits to the dual-control queue; a distinct approver executes the
          transition.
        </p>
      )}
      <ErrorBox error={error} />
      <Field
        label="Reason"
        required={meta.reasonRequired}
        hint="Recorded verbatim in the admin audit log."
      >
        {(id) => (
          <textarea
            id={id}
            className={textareaCls}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            placeholder={meta.reasonRequired ? 'Required' : 'Optional'}
          />
        )}
      </Field>
      {op === 'resume' && (inst.status === 'HALTED' || inst.status === 'SUSPENDED') && (
        <label className="mb-4 flex items-center gap-2 text-sm text-neutral-300">
          <input
            type="checkbox"
            className="h-6 w-6 accent-sky-500"
            checked={skipAuction}
            onChange={(e) => setSkipAuction(e.target.checked)}
          />
          Skip the reopening CALL auction (direct resume)
        </label>
      )}
      {op === 'resume' && (inst.status === 'RESTRICTED' || inst.status === 'CANCEL_ONLY') && (
        <label className="mb-4 flex items-center gap-2 text-sm text-neutral-300">
          <input
            type="checkbox"
            className="h-6 w-6 accent-sky-500"
            checked={auction}
            onChange={(e) => setAuction(e.target.checked)}
          />
          Run a reopening CALL auction
        </label>
      )}
      <div className="flex justify-end gap-2">
        <button type="button" className={btnGhost} onClick={onCancel} disabled={busy}>
          Cancel
        </button>
        <button
          type="button"
          className={meta.danger ? btnDanger : btnPrimary}
          disabled={busy || blocked}
          onClick={() =>
            onConfirm({
              reason: trimmed === '' ? undefined : trimmed,
              skipAuction: op === 'resume' && skipAuction ? true : undefined,
              auction: op === 'resume' && auction ? true : undefined,
            })
          }
        >
          {busy ? 'Working…' : meta.label}
        </button>
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Create form — Super Admin + four-eyes (OpInstrumentCreate). Server
// validates decimals/ISO currencies; the form only enforces presence.
// ---------------------------------------------------------------------------

const EMPTY_CREATE = {
  symbol: '',
  baseCurrency: '',
  quoteCurrency: '',
  instrumentType: 'SPOT',
  tickSize: '',
  lotSize: '',
  minOrderQty: '',
  maxOrderQty: '',
  priceBandPctUp: '',
  priceBandPctDown: '',
  settlementCycle: '1',
  maxLeverage: '',
  reason: '',
};

function CreateInstrumentModal({
  busy,
  error,
  onCancel,
  onSubmit,
}: {
  busy: boolean;
  error: unknown;
  onCancel: () => void;
  onSubmit: (input: AdminInstrumentCreateInput) => void;
}) {
  const [form, setForm] = useState(EMPTY_CREATE);
  const [formErr, setFormErr] = useState<string | null>(null);
  const set = (k: keyof typeof EMPTY_CREATE) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [k]: e.target.value }));

  const submit = () => {
    const missing = (
      [
        ['symbol', form.symbol],
        ['base currency', form.baseCurrency],
        ['quote currency', form.quoteCurrency],
        ['tick size', form.tickSize],
        ['lot size', form.lotSize],
        ['min order qty', form.minOrderQty],
        ['max order qty', form.maxOrderQty],
        ['max leverage', form.maxLeverage],
      ] as [string, string][]
    ).filter(([, v]) => v.trim() === '');
    if (missing.length > 0) {
      setFormErr(`Required: ${missing.map(([k]) => k).join(', ')}`);
      return;
    }
    const leverage = parseIntField(form.maxLeverage);
    const cycle = parseIntField(form.settlementCycle);
    if (leverage === null || leverage <= 0) {
      setFormErr('max leverage must be a positive integer');
      return;
    }
    if (cycle === null || cycle < 0 || cycle > 2) {
      setFormErr('settlement cycle must be 0, 1 or 2');
      return;
    }
    setFormErr(null);
    onSubmit({
      symbol: form.symbol.trim(),
      baseCurrency: form.baseCurrency.trim(),
      quoteCurrency: form.quoteCurrency.trim(),
      instrumentType: form.instrumentType,
      tickSize: form.tickSize.trim(),
      lotSize: form.lotSize.trim(),
      minOrderQty: form.minOrderQty.trim(),
      maxOrderQty: form.maxOrderQty.trim(),
      priceBandPctUp: form.priceBandPctUp.trim() || undefined,
      priceBandPctDown: form.priceBandPctDown.trim() || undefined,
      settlementCycle: cycle,
      maxLeverage: leverage,
      reason: form.reason.trim() || undefined,
    });
  };

  const text = (key: keyof typeof EMPTY_CREATE, label: string, required = false, hint?: string) => (
    <Field label={label} required={required} hint={hint}>
      {(id) => <input id={id} className={inputCls} value={form[key]} onChange={set(key)} />}
    </Field>
  );

  return (
    <Modal open title="New instrument (DRAFT)" onClose={onCancel}>
      <p className="mb-3 rounded border border-amber-800/50 bg-amber-950/30 px-3 py-2 text-xs text-amber-200">
        §7.2 four-eyes — submission enters the dual-control queue; a distinct Super Admin approver
        creates the DRAFT row.
      </p>
      {formErr !== null && (
        <p className="mb-3 text-xs text-red-400" role="alert">
          {formErr}
        </p>
      )}
      <ErrorBox error={error} />
      <div className="grid grid-cols-2 gap-x-3">
        {text('symbol', 'Symbol', true)}
        <Field label="Instrument type" required>
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={form.instrumentType}
              onChange={set('instrumentType')}
            >
              {INSTRUMENT_TYPES.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          )}
        </Field>
        {text('baseCurrency', 'Base currency', true, 'ISO 4217, e.g. EUR')}
        {text('quoteCurrency', 'Quote currency', true, 'ISO 4217, e.g. USD')}
        {text('tickSize', 'Tick size', true)}
        {text('lotSize', 'Lot size', true)}
        {text('minOrderQty', 'Min order qty', true)}
        {text('maxOrderQty', 'Max order qty', true)}
        {text('priceBandPctUp', 'Price band % up', false, 'blank → 2.00')}
        {text('priceBandPctDown', 'Price band % down', false, 'blank → 5.00')}
        <Field label="Settlement cycle" required>
          {(id) => (
            <select
              id={id}
              className={selectCls}
              value={form.settlementCycle}
              onChange={set('settlementCycle')}
            >
              {SETTLEMENT_CYCLES.map((s) => (
                <option key={s.value} value={s.value}>
                  {s.label}
                </option>
              ))}
            </select>
          )}
        </Field>
        {text('maxLeverage', 'Max leverage', true, 'e.g. 30')}
      </div>
      <Field label="Reason" hint="Recorded in the dual-control request + audit log.">
        {(id) => (
          <textarea id={id} className={textareaCls} value={form.reason} onChange={set('reason')} />
        )}
      </Field>
      <div className="flex justify-end gap-2">
        <button type="button" className={btnGhost} onClick={onCancel} disabled={busy}>
          Cancel
        </button>
        <button type="button" className={btnPrimary} onClick={submit} disabled={busy}>
          {busy ? 'Submitting…' : 'Submit for approval'}
        </button>
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Edit form — PUT /admin/instruments/{id}; DRAFT/ACTIVE only, Risk
// Manager+. Every field is optional: blank = unchanged server-side.
// Fields the list projection doesn't return start blank.
// ---------------------------------------------------------------------------

function EditInstrumentModal({
  inst,
  busy,
  error,
  onCancel,
  onSubmit,
}: {
  inst: AdminInstrument;
  busy: boolean;
  error: unknown;
  onCancel: () => void;
  onSubmit: (input: AdminInstrumentUpdateInput) => void;
}) {
  const [form, setForm] = useState({
    tickSize: inst.tickSize,
    lotSize: inst.lotSize,
    minOrderQty: inst.minOrderQty,
    maxOrderQty: inst.maxOrderQty,
    minNotional: '',
    minPrice: '',
    maxPrice: '',
    priceBandPctUp: '',
    priceBandPctDown: '',
    maxSpreadPips: '',
    maxOpenOrders: '',
    maxAlgoOrders: '',
    maxLeverage: String(inst.maxLeverage),
    settlementCycle: String(inst.settlementCycle),
    reason: '',
  });
  const [formErr, setFormErr] = useState<string | null>(null);
  const set = (k: keyof typeof form) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [k]: e.target.value }));

  const submit = () => {
    const input: AdminInstrumentUpdateInput = {};
    const strKeys = [
      'tickSize',
      'lotSize',
      'minOrderQty',
      'maxOrderQty',
      'minNotional',
      'minPrice',
      'maxPrice',
      'priceBandPctUp',
      'priceBandPctDown',
      'maxSpreadPips',
    ] as const;
    for (const key of strKeys) {
      const v = form[key].trim();
      if (v !== '') input[key] = v;
    }
    const ints: {
      key: 'maxOpenOrders' | 'maxAlgoOrders' | 'maxLeverage' | 'settlementCycle';
      label: string;
    }[] = [
      { key: 'maxOpenOrders', label: 'max open orders' },
      { key: 'maxAlgoOrders', label: 'max algo orders' },
      { key: 'maxLeverage', label: 'max leverage' },
      { key: 'settlementCycle', label: 'settlement cycle' },
    ];
    for (const { key, label } of ints) {
      const raw = form[key].trim();
      if (raw === '') continue;
      const n = parseIntField(raw);
      if (n === null) {
        setFormErr(`${label} must be an integer`);
        return;
      }
      input[key] = n;
    }
    if (Object.keys(input).length === 0) {
      setFormErr('No changes — every field is blank.');
      return;
    }
    const v = form.reason.trim();
    if (v !== '') input.reason = v;
    setFormErr(null);
    onSubmit(input);
  };

  const text = (key: keyof typeof form, label: string, hint?: string) => (
    <Field label={label} hint={hint}>
      {(id) => <input id={id} className={inputCls} value={form[key]} onChange={set(key)} />}
    </Field>
  );

  return (
    <Modal open title={`Edit ${inst.symbol} parameters`} onClose={onCancel}>
      <p className="mb-3 text-xs text-neutral-500">
        Blank fields stay unchanged. Edits are audit-logged; parameter lifecycle under control
        states is the maker-checker flow, not this form.
      </p>
      {formErr !== null && (
        <p className="mb-3 text-xs text-red-400" role="alert">
          {formErr}
        </p>
      )}
      <ErrorBox error={error} />
      <div className="grid grid-cols-2 gap-x-3">
        {text('tickSize', 'Tick size')}
        {text('lotSize', 'Lot size')}
        {text('minOrderQty', 'Min order qty')}
        {text('maxOrderQty', 'Max order qty')}
        {text('minNotional', 'Min notional', 'blank = unchanged')}
        {text('maxSpreadPips', 'Max spread (pips)', 'blank = unchanged')}
        {text('minPrice', 'Min price', 'blank = unchanged')}
        {text('maxPrice', 'Max price', 'blank = unchanged')}
        {text('priceBandPctUp', 'Price band % up')}
        {text('priceBandPctDown', 'Price band % down')}
        {text('maxOpenOrders', 'Max open orders', 'blank = unchanged')}
        {text('maxAlgoOrders', 'Max algo orders', 'blank = unchanged')}
        {text('maxLeverage', 'Max leverage')}
        {text('settlementCycle', 'Settlement cycle', '0 same-day · 1 T+1 · 2 T+2')}
      </div>
      <Field label="Reason" hint="Recorded in the admin audit log.">
        {(id) => (
          <textarea id={id} className={textareaCls} value={form.reason} onChange={set('reason')} />
        )}
      </Field>
      <div className="flex justify-end gap-2">
        <button type="button" className={btnGhost} onClick={onCancel} disabled={busy}>
          Cancel
        </button>
        <button type="button" className={btnPrimary} onClick={submit} disabled={busy}>
          {busy ? 'Saving…' : 'Save parameters'}
        </button>
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Panel
// ---------------------------------------------------------------------------

export function InstrumentsPanel({
  adminApi,
  api,
}: {
  adminApi: BoundAdminApi;
  /** Raw client — PUT update goes through it with a manual X-Admin-Env
   * stamp (BoundAdminApi exposes get/post only). */
  api: ApiClient;
}) {
  const qc = useQueryClient();
  const role = useAdminRole();
  const [showCreate, setShowCreate] = useState(false);
  const [dialog, setDialog] = useState<{ inst: AdminInstrument; op: AdminInstrumentOp } | null>(
    null,
  );
  const [editing, setEditing] = useState<AdminInstrument | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [actionError, setActionError] = useState<unknown>(null);

  const query = useQuery({
    queryKey: ['admin', 'instruments', adminApi.env],
    queryFn: () => listAdminInstruments(adminApi),
    retry: false,
    refetchInterval: 30_000,
  });

  const invalidate = async () => {
    await qc.invalidateQueries({ queryKey: ['admin', 'instruments'] });
    await qc.invalidateQueries({ queryKey: ['admin', 'dual-control'] });
  };

  const reportResult = async (res: {
    kind: 'applied' | 'pending';
    pending?: { dualControlId: number; operation: string; requiredApprover: string };
  }) => {
    if (res.kind === 'pending' && res.pending !== undefined) {
      setNotice(
        `Submitted for approval — ${res.pending.operation} request #${res.pending.dualControlId} ` +
          `awaits a distinct ${res.pending.requiredApprover} approver in the dual-control queue.`,
      );
    } else {
      setNotice('Instrument updated.');
    }
    await invalidate();
  };

  const transition = useMutation({
    mutationFn: (v: { id: number; op: AdminInstrumentOp; input: AdminInstrumentTransitionInput }) =>
      transitionAdminInstrument(adminApi, v.id, v.op, v.input),
    onSuccess: async (res) => {
      setActionError(null);
      setDialog(null);
      await reportResult(res);
    },
    onError: (e) => setActionError(e),
  });

  const create = useMutation({
    mutationFn: (input: AdminInstrumentCreateInput) => createAdminInstrument(adminApi, input),
    onSuccess: async (res) => {
      setActionError(null);
      setShowCreate(false);
      await reportResult(res);
    },
    onError: (e) => setActionError(e),
  });

  const update = useMutation({
    mutationFn: (v: { id: number; input: AdminInstrumentUpdateInput }) =>
      updateAdminInstrument(api, adminApi.env, v.id, v.input),
    onSuccess: async () => {
      setActionError(null);
      setEditing(null);
      setNotice('Instrument parameters updated.');
      await invalidate();
    },
    onError: (e) => setActionError(e),
  });

  const denied = query.error !== null && isAccessDenied(query.error);
  const stubbed = query.error !== null && isNotImplemented(query.error);
  const rows = query.data ?? [];
  const canCreate = permitsAdminRole(role, SUPER_ADMIN);
  const canEdit = permitsAdminRole(role, RISK_MANAGER);

  return (
    <section className={cardCls} aria-label="Instrument management">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-medium text-neutral-400">
          Instrument management
          {rows.length > 0 && <span className="ml-2 text-neutral-600">({rows.length})</span>}
        </h2>
        {canCreate && (
          <button type="button" className={btnGhost} onClick={() => setShowCreate(true)}>
            New instrument
          </button>
        )}
      </div>

      {denied && (
        <AccessDeniedCard detail="The instrument registry requires a Risk Manager (or Super Admin) binding." />
      )}
      {!denied && stubbed && (
        <UnavailablePanel feature="Instrument management" owner="Phase-15 Task 15.3.2" />
      )}
      {!denied && !stubbed && <ErrorBox error={query.error} />}
      <ErrorBox error={actionError} onDismiss={() => setActionError(null)} />
      {notice !== null && (
        <p
          className="mb-3 rounded border border-sky-800/50 bg-sky-950/30 px-3 py-2 text-xs text-sky-200"
          role="status"
        >
          {notice}
        </p>
      )}

      {!denied && !stubbed && query.error === null && (
        <div className="relative overflow-x-auto" tabIndex={0}>
          <table className={tableCls}>
            <thead>
              <tr>
                <th className={thCls}>Symbol</th>
                <th className={thCls}>Type</th>
                <th className={thCls}>Status</th>
                <th className={thCls}>Tick</th>
                <th className={thCls}>Lot</th>
                <th className={thCls}>Qty min–max</th>
                <th className={thCls}>Settle</th>
                <th className={thCls}>Lev</th>
                <th className={thCls}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((inst) => {
                const ops = (OPS_BY_STATE[inst.status] ?? []).filter((op) =>
                  permitsAdminRole(role, OP_META[op].roles),
                );
                return (
                  <tr key={inst.id} data-testid={`instrument-row-${inst.symbol}`}>
                    <td className={tdCls}>
                      <span className="font-medium">{inst.symbol}</span>
                      <span className="ml-1 text-xs text-neutral-500">
                        {inst.baseCurrency}/{inst.quoteCurrency}
                      </span>
                    </td>
                    <td className={tdCls}>{inst.instrumentType}</td>
                    <td className={tdCls}>
                      <StatusBadge value={inst.status} />
                      {inst.graceKind !== undefined && (
                        <p className="mt-1 text-xs text-neutral-500">
                          {GRACE_LABEL[inst.graceKind] ?? inst.graceKind}
                          {inst.graceDeadline !== undefined && ` until ${inst.graceDeadline}`}
                        </p>
                      )}
                    </td>
                    <td className={tdCls}>{inst.tickSize}</td>
                    <td className={tdCls}>{inst.lotSize}</td>
                    <td className={tdCls}>
                      {inst.minOrderQty}–{inst.maxOrderQty}
                    </td>
                    <td className={tdCls}>
                      {SETTLEMENT_CYCLES.find((s) => s.value === inst.settlementCycle)?.label ??
                        `T+${inst.settlementCycle}`}
                    </td>
                    <td className={tdCls}>{inst.maxLeverage}:1</td>
                    <td className={tdCls}>
                      <span className="flex flex-wrap gap-1">
                        {canEdit && EDITABLE_STATES.has(inst.status) && (
                          <button
                            type="button"
                            className={btnGhost}
                            onClick={() => setEditing(inst)}
                          >
                            Edit
                          </button>
                        )}
                        {ops.map((op) => (
                          <button
                            key={op}
                            type="button"
                            className={OP_META[op].danger ? btnDanger : btnGhost}
                            disabled={transition.isPending}
                            onClick={() => setDialog({ inst, op })}
                          >
                            {OP_META[op].label}
                          </button>
                        ))}
                        {ops.length === 0 && !(canEdit && EDITABLE_STATES.has(inst.status)) && (
                          <span className="text-xs text-neutral-600">—</span>
                        )}
                      </span>
                    </td>
                  </tr>
                );
              })}
              {rows.length === 0 && !query.isLoading && (
                <tr>
                  <td className={tdCls} colSpan={9}>
                    No instruments registered.
                  </td>
                </tr>
              )}
              {query.isLoading && (
                <tr>
                  <td className={tdCls} colSpan={9}>
                    Loading instruments…
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      )}

      {dialog !== null && (
        <TransitionDialog
          inst={dialog.inst}
          op={dialog.op}
          busy={transition.isPending}
          error={transition.error}
          onCancel={() => setDialog(null)}
          onConfirm={(input) => transition.mutate({ id: dialog.inst.id, op: dialog.op, input })}
        />
      )}
      {showCreate && (
        <CreateInstrumentModal
          busy={create.isPending}
          error={create.error}
          onCancel={() => setShowCreate(false)}
          onSubmit={(input) => create.mutate(input)}
        />
      )}
      {editing !== null && (
        <EditInstrumentModal
          inst={editing}
          busy={update.isPending}
          error={update.error}
          onCancel={() => setEditing(null)}
          onSubmit={(input) => update.mutate({ id: editing.id, input })}
        />
      )}
    </section>
  );
}
