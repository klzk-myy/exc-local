/**
 * Trading-parameters & account-data panel (Phase-10.5 Task 10.5.3.17) —
 * the account-scoped trading settings and read-only introspection
 * surfaces:
 *
 *   Leverage      — POST /account/leverage {symbol?, leverage}
 *   Margin mode   — POST /account/margin-mode {mode} (409 while positions
 *                   open — MARGIN_MODE_SWITCH_BLOCKED)
 *   Swap-free     — GET /account/swap-free + POST …/request
 *                   {attestation_ref} (PENDING verification)
 *   Appropriateness — GET/POST /account/appropriateness (server derives
 *                   PASS/FAIL from the pass mark; expiry 12 months;
 *                   SPOT is exempt)
 *   Limits        — GET /account/risk-limits + GET /account/rate-limits
 *                   (read-only)
 *   Family        — GET /account/sub-accounts/aggregate (master only)
 *   Symbol specs  — GET /account/filters/{symbol},
 *                   GET /account/commission/{symbol},
 *                   GET /account/cost-preview (ex-ante quote)
 *   Liquidations  — GET /account/liquidations
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import { apiClient } from '@/app/runtime';
import {
  btnGhost,
  btnPrimary,
  cardCls,
  ErrorBox,
  hintTextCls,
  inputCls,
  selectCls,
  StatusBadge,
  tableCls,
  tdCls,
  thCls,
} from '@/lib/ui';

const rec = (v: unknown): Record<string, unknown> =>
  typeof v === 'object' && v !== null ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === 'string' ? v : '');
const num = (v: unknown): number => (typeof v === 'number' ? v : Number(v ?? 0));
const listOf = <T,>(v: unknown, fn: (x: unknown) => T): T[] => (Array.isArray(v) ? v.map(fn) : []);

// ---------- generic read-only JSON projection ---------------------------

function kvRows(v: unknown, prefix = ''): [string, string][] {
  const out: [string, string][] = [];
  for (const [k, val] of Object.entries(rec(v))) {
    if (val === null || val === undefined) continue;
    if (typeof val === 'object' && !Array.isArray(val)) {
      out.push(...kvRows(val, `${prefix}${k}.`));
    } else if (!Array.isArray(val)) {
      out.push([
        `${prefix}${k}`,
        typeof val === 'string' || typeof val === 'number' || typeof val === 'boolean'
          ? String(val)
          : JSON.stringify(val),
      ]);
    }
  }
  return out;
}

function KvTable({ value }: { value: unknown }) {
  const rows = kvRows(value);
  if (rows.length === 0)
    return <p className="py-2 text-center text-xs text-neutral-500">No data.</p>;
  return (
    <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs sm:grid-cols-3">
      {rows.map(([k, v]) => (
        <div key={k} className="flex justify-between gap-2">
          <dt className="text-neutral-500">{k}</dt>
          <dd className="truncate text-neutral-200" title={v}>
            {v}
          </dd>
        </div>
      ))}
    </dl>
  );
}

// ---------- panel -------------------------------------------------------

const MARGIN_MODES = ['CROSS', 'ISOLATED', 'PORTFOLIO'];
const APP_CLASSES = ['FORWARD', 'SWAP', 'NDF', 'OPTION'];

export default function TradingPanel() {
  const qc = useQueryClient();
  const [lev, setLev] = useState({ symbol: '', leverage: '20' });
  const [mode, setMode] = useState('CROSS');
  const [attestation, setAttestation] = useState('');
  const [assess, setAssess] = useState({ instrumentClass: 'FORWARD', score: '0' });
  const [sym, setSym] = useState('EUR/USD');
  const [costQ, setCostQ] = useState({ symbol: 'EUR/USD', side: 'BUY', qty: '100000' });
  const [notice, setNotice] = useState<string | null>(null);

  const swapFree = useQuery({
    queryKey: ['account', 'swap-free'],
    queryFn: () => apiClient.get('/account/swap-free'),
    retry: false,
  });
  const approp = useQuery({
    queryKey: ['account', 'appropriateness'],
    queryFn: () => apiClient.get('/account/appropriateness'),
    retry: false,
  });
  const riskLimits = useQuery({
    queryKey: ['account', 'risk-limits'],
    queryFn: () => apiClient.get('/account/risk-limits'),
    retry: false,
  });
  const rateLimits = useQuery({
    queryKey: ['account', 'rate-limits'],
    queryFn: () => apiClient.get('/account/rate-limits'),
    retry: false,
  });
  const family = useQuery({
    queryKey: ['account', 'sub-accounts', 'aggregate'],
    queryFn: () => apiClient.get('/account/sub-accounts/aggregate'),
    retry: false,
  });
  const liquidations = useQuery({
    queryKey: ['account', 'liquidations'],
    queryFn: () => apiClient.get('/account/liquidations'),
    retry: false,
  });
  const filters = useQuery({
    queryKey: ['account', 'filters', sym],
    queryFn: () => apiClient.get(`/account/filters/${encodeURIComponent(sym)}`),
    enabled: sym !== '',
    retry: false,
  });
  const commission = useQuery({
    queryKey: ['account', 'commission', sym],
    queryFn: () => apiClient.get(`/account/commission/${encodeURIComponent(sym)}`),
    enabled: sym !== '',
    retry: false,
  });

  const onErr = (e: unknown) => setNotice(e instanceof Error ? e.message : String(e));

  const levMut = useMutation({
    mutationFn: () =>
      apiClient.post('/account/leverage', {
        leverage: Number(lev.leverage),
        ...(lev.symbol !== '' ? { symbol: lev.symbol } : {}),
      }),
    onSuccess: () => setNotice('Leverage updated.'),
    onError: onErr,
  });
  const modeMut = useMutation({
    mutationFn: () => apiClient.post('/account/margin-mode', { mode }),
    onSuccess: () => setNotice(`Margin mode → ${mode}.`),
    onError: onErr,
  });
  const swapFreeMut = useMutation({
    mutationFn: () =>
      apiClient.post('/account/swap-free/request', { attestation_ref: attestation }),
    onSuccess: () => {
      setNotice('Swap-free verification opened (PENDING).');
      void qc.invalidateQueries({ queryKey: ['account', 'swap-free'] });
    },
    onError: onErr,
  });
  const appropMut = useMutation({
    mutationFn: () =>
      apiClient.post('/account/appropriateness', {
        instrument_class: assess.instrumentClass,
        score: Number(assess.score),
      }),
    onSuccess: () => {
      setNotice('Assessment recorded — outcome derived server-side from the pass mark.');
      void qc.invalidateQueries({ queryKey: ['account', 'appropriateness'] });
    },
    onError: onErr,
  });
  const costPreview = useMutation({
    mutationFn: () =>
      apiClient.get(
        `/account/cost-preview?symbol=${encodeURIComponent(costQ.symbol)}&side=${costQ.side}&qty=${costQ.qty}`,
      ),
    onSuccess: () => setNotice(null),
    onError: onErr,
  });

  return (
    <section className={cardCls} aria-label="Trading parameters">
      <h2 className="mb-2 text-sm font-medium text-neutral-400">
        Trading parameters &amp; account data
      </h2>
      {notice !== null && (
        <p className="mb-2 rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs text-neutral-300">
          {notice}
        </p>
      )}

      <div className="grid gap-4 lg:grid-cols-2">
        <div>
          <h3 className="mb-1 text-xs font-medium text-neutral-400">Leverage</h3>
          <form
            className="grid gap-2 sm:grid-cols-3"
            onSubmit={(e) => {
              e.preventDefault();
              levMut.mutate();
            }}
          >
            <label className="block">
              <span className="sr-only">Leverage symbol</span>
              <input
                aria-label="Leverage symbol"
                className={inputCls}
                placeholder="symbol (blank = account default)"
                value={lev.symbol}
                onChange={(e) => setLev({ ...lev, symbol: e.target.value })}
              />
            </label>
            <label className="block">
              <span className="sr-only">leverage</span>
              <input
                aria-label="leverage"
                className={inputCls}
                placeholder="leverage"
                value={lev.leverage}
                onChange={(e) => setLev({ ...lev, leverage: e.target.value })}
              />
            </label>
            <button type="submit" className={btnPrimary} disabled={levMut.isPending}>
              Set leverage
            </button>
          </form>
          <ErrorBox error={levMut.error} />

          <h3 className="mt-4 mb-1 text-xs font-medium text-neutral-400">Margin mode</h3>
          <div className="flex gap-2">
            <select
              aria-label="Margin mode"
              className={selectCls}
              value={mode}
              onChange={(e) => setMode(e.target.value)}
            >
              {MARGIN_MODES.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
            <button
              type="button"
              className={btnGhost}
              disabled={modeMut.isPending}
              onClick={() => modeMut.mutate()}
            >
              Switch
            </button>
          </div>
          <ErrorBox error={modeMut.error} />
          <p className={hintTextCls}>Switching is refused (409) while positions are open.</p>

          <h3 className="mt-4 mb-1 text-xs font-medium text-neutral-400">Swap-free</h3>
          <ErrorBox error={swapFree.error} />
          {swapFree.data !== undefined && <KvTable value={swapFree.data} />}
          <form
            className="mt-2 flex gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              swapFreeMut.mutate();
            }}
          >
            <label className="block flex-1">
              <span className="sr-only">attestation_ref</span>
              <input
                aria-label="attestation_ref"
                className={inputCls}
                placeholder="attestation_ref (kyc_documents row)"
                value={attestation}
                onChange={(e) => setAttestation(e.target.value)}
              />
            </label>
            <button
              type="submit"
              className={btnGhost}
              disabled={swapFreeMut.isPending || attestation === ''}
            >
              Request
            </button>
          </form>
          <ErrorBox error={swapFreeMut.error} />

          <h3 className="mt-4 mb-1 text-xs font-medium text-neutral-400">
            Appropriateness assessment
          </h3>
          <ErrorBox error={approp.error} />
          {approp.data !== undefined && <KvTable value={approp.data} />}
          <form
            className="mt-2 grid gap-2 sm:grid-cols-3"
            onSubmit={(e) => {
              e.preventDefault();
              appropMut.mutate();
            }}
          >
            <label className="block">
              <span className="sr-only">instrument_class</span>
              <select
                aria-label="instrument_class"
                className={selectCls}
                value={assess.instrumentClass}
                onChange={(e) => setAssess({ ...assess, instrumentClass: e.target.value })}
              >
                {APP_CLASSES.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            </label>
            <label className="block">
              <span className="sr-only">score</span>
              <input
                aria-label="score"
                className={inputCls}
                placeholder="score"
                value={assess.score}
                onChange={(e) => setAssess({ ...assess, score: e.target.value })}
              />
            </label>
            <button type="submit" className={btnGhost} disabled={appropMut.isPending}>
              Submit assessment
            </button>
          </form>
          <ErrorBox error={appropMut.error} />
          <p className={hintTextCls}>
            Outcome derives from the pass mark server-side — clients never self-declare PASS. Valid
            12 months; SPOT is exempt.
          </p>
        </div>

        <div>
          <h3 className="mb-1 text-xs font-medium text-neutral-400">Risk limits</h3>
          <ErrorBox error={riskLimits.error} />
          {riskLimits.data !== undefined && <KvTable value={riskLimits.data} />}

          <h3 className="mt-4 mb-1 text-xs font-medium text-neutral-400">Rate limits</h3>
          <ErrorBox error={rateLimits.error} />
          {rateLimits.data !== undefined && <KvTable value={rateLimits.data} />}

          <h3 className="mt-4 mb-1 text-xs font-medium text-neutral-400">
            Family aggregate (master view)
          </h3>
          <ErrorBox error={family.error} />
          {family.data !== undefined && <KvTable value={family.data} />}
        </div>
      </div>

      <h3 className="mt-6 mb-2 text-sm font-medium text-neutral-400">Symbol specs &amp; costs</h3>
      <div className="mb-2 flex gap-2">
        <label className="block">
          <span className="sr-only">Spec symbol</span>
          <input
            aria-label="Spec symbol"
            className={inputCls}
            placeholder="symbol"
            value={sym}
            onChange={(e) => setSym(e.target.value)}
          />
        </label>
      </div>
      <ErrorBox error={filters.error ?? commission.error} />
      <div className="grid gap-4 lg:grid-cols-2">
        {filters.data !== undefined && (
          <KvTable value={rec(filters.data)['filters'] ?? filters.data} />
        )}
        {commission.data !== undefined && (
          <KvTable value={rec(commission.data)['commission'] ?? commission.data} />
        )}
      </div>
      <form
        className="mt-3 grid gap-2 sm:grid-cols-4"
        onSubmit={(e) => {
          e.preventDefault();
          costPreview.mutate();
        }}
      >
        <label className="block">
          <span className="sr-only">Cost symbol</span>
          <input
            aria-label="Cost symbol"
            className={inputCls}
            placeholder="symbol"
            value={costQ.symbol}
            onChange={(e) => setCostQ({ ...costQ, symbol: e.target.value })}
          />
        </label>
        <label className="block">
          <span className="sr-only">Cost side</span>
          <select
            aria-label="Cost side"
            className={selectCls}
            value={costQ.side}
            onChange={(e) => setCostQ({ ...costQ, side: e.target.value })}
          >
            <option value="BUY">BUY</option>
            <option value="SELL">SELL</option>
          </select>
        </label>
        <label className="block">
          <span className="sr-only">Cost qty</span>
          <input
            aria-label="Cost qty"
            className={inputCls}
            placeholder="qty"
            value={costQ.qty}
            onChange={(e) => setCostQ({ ...costQ, qty: e.target.value })}
          />
        </label>
        <button type="submit" className={btnGhost} disabled={costPreview.isPending}>
          Cost preview
        </button>
      </form>
      <ErrorBox error={costPreview.error} />
      {costPreview.data !== undefined && <KvTable value={costPreview.data} />}

      <h3 className="mt-6 mb-2 text-sm font-medium text-neutral-400">Liquidation history</h3>
      <ErrorBox error={liquidations.error} />
      <table className={tableCls}>
        <thead>
          <tr>
            <th className={thCls}>ID</th>
            <th className={thCls}>Symbol</th>
            <th className={thCls}>Kind</th>
            <th className={thCls}>Side</th>
            <th className={thCls}>Qty</th>
            <th className={thCls}>Price</th>
            <th className={thCls}>Insurance</th>
            <th className={thCls}>When</th>
          </tr>
        </thead>
        <tbody>
          {listOf(rec(liquidations.data)['liquidations'], (x) => rec(x)).map((l) => (
            <tr key={num(l['id'])}>
              <td className={tdCls}>{num(l['id'])}</td>
              <td className={tdCls}>{str(l['symbol'])}</td>
              <td className={tdCls}>
                <StatusBadge value={str(l['kind'])} />
              </td>
              <td className={tdCls}>{str(l['side'])}</td>
              <td className={tdCls}>{str(l['quantity'])}</td>
              <td className={tdCls}>{str(l['price'])}</td>
              <td className={tdCls}>{str(l['insurance_fund_contribution'])}</td>
              <td className={tdCls}>{str(l['created_at']).slice(0, 16)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {liquidations.data !== undefined &&
        listOf(rec(liquidations.data)['liquidations'], (x) => x).length === 0 && (
          <p className="py-2 text-center text-xs text-neutral-500">No liquidations.</p>
        )}
    </section>
  );
}
