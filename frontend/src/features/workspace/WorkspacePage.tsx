/**
 * Customizable workspace (Task 10.3.14) + Pro/Lite shell (Task 10.3.9).
 *
 *   - movable/resizable panels on a 12-col × 56px-row grid — pointer
 *     drag on the title bar, corner handle to resize, and full keyboard
 *     control (focus the panel header → arrows move, Shift+arrows
 *     resize, each step announced via the header's aria-label);
 *   - named layouts: save / restore / delete / reset, persisted
 *     per-account per-device in localStorage (`exc.workspace.v1.<scope>`)
 *     — the spec exposes no workspace endpoint, so this is the honest
 *     fallback (documented in frontend/README.md);
 *   - safety-critical panels (order ticket, positions) warn before
 *     hiding (spec: never silently remove a risk surface);
 *   - light/dark theme scoped to the workspace subtree (theme.css).
 */
import {
  lazy,
  Suspense,
  useMemo,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from 'react';

import { Modal, ConfirmAction, btnGhost, inputCls, selectCls } from '@/lib/ui';
import { useScopeKey } from '@/lib/trading/queries';
import { useOrderDraft } from '@/lib/trading/orderDraft';
import { usePrivatePositionsFeed } from '@/lib/trading/positionFeed';
import { useMarketFeed } from '@/lib/trading/marketStore';
import { wsClient } from '@/app/runtime';

import {
  GRID_COLS,
  ROW_H,
  PANELS,
  clampPlacement,
  defaultPlacements,
  listLayouts,
  loadLayout,
  resetLayouts,
  saveLayout,
  type PanelId,
  type PanelPlacement,
  type Placements,
} from './layouts';
import { SubAccountSwitcher } from '@/features/advanced-orders/SubAccountSwitcher';
import { ChartPanel } from './ChartPanel';
import { TickerStrip } from './TickerStrip';

import { useUiMode, useUiModeStore } from './liteMode';
import { useTheme } from './theme';
import { ModeToggle } from './ModeToggle';
import { LiteDashboard } from './LiteDashboard';
import './theme.css';

// Heavy panels are their own lazy chunks — the workspace shell stays light
// and panel remounts never lose the order draft (it's a Zustand store).
const AdvancedOrderPanel = lazy(() =>
  import('@/features/advanced-orders/AdvancedOrderPanel').then((m) => ({
    default: m.AdvancedOrderPanel,
  })),
);
const BlotterPanel = lazy(() =>
  import('./BlotterPanel').then((m) => ({ default: m.BlotterPanel })),
);
const MarketTrades = lazy(() =>
  import('./MarketTrades').then((m) => ({ default: m.MarketTrades })),
);
const DepthChart = lazy(() =>
  import('@/features/depth-chart/DepthChart').then((m) => ({ default: m.DepthChart })),
);
const OrderBook = lazy(() =>
  import('@/features/order-book/OrderBook').then((m) => ({ default: m.OrderBook })),
);
const BalancesPanel = lazy(() =>
  import('./BalancesPanel').then((m) => ({ default: m.BalancesPanel })),
);

function PanelBody({ id, symbol }: { id: PanelId; symbol: string }) {
  const setDraft = useOrderDraft((s) => s.setDraft);
  switch (id) {
    case 'order':
      return <AdvancedOrderPanel />;
    case 'book':
      return (
        <OrderBook
          symbol={symbol}
          viewportHeight={360}
          onPriceClick={(price, side) =>
            setDraft({ price, side: side === 'ask' ? 'BUY' : 'SELL', symbol })
          }
        />
      );
    case 'positions':
      return <BlotterPanel />;
    case 'chart':
      return <ChartPanel symbol={symbol} />;
    case 'depth':
      return <DepthChart symbol={symbol} />;
    case 'tape':
      return <MarketTrades symbol={symbol} />;
    case 'balances':
      return <BalancesPanel />;
  }
}

interface DragState {
  id: PanelId;
  kind: 'move' | 'resize';
  startX: number;
  startY: number;
  orig: PanelPlacement;
}

function PanelFrame({
  id,
  title,
  safetyCritical,
  placement,
  containerRef,
  onMove,
  onHide,
  children,
}: {
  id: PanelId;
  title: string;
  safetyCritical: boolean;
  placement: PanelPlacement;
  containerRef: React.RefObject<HTMLDivElement | null>;
  onMove: (id: PanelId, p: PanelPlacement) => void;
  onHide: (id: PanelId, safetyCritical: boolean) => void;
  children: ReactNode;
}) {
  const [preview, setPreview] = useState<PanelPlacement | null>(null);
  const drag = useRef<DragState | null>(null);
  const shown = preview ?? placement;

  const pxDelta = (e: ReactPointerEvent, d: DragState) => {
    const rect = containerRef.current?.getBoundingClientRect();
    const colW = (rect?.width ?? GRID_COLS * 90) / GRID_COLS;
    return {
      dx: Math.round((e.clientX - d.startX) / colW),
      dy: Math.round((e.clientY - d.startY) / ROW_H),
    };
  };

  const startDrag = (kind: DragState['kind']) => (e: ReactPointerEvent) => {
    drag.current = { id, kind, startX: e.clientX, startY: e.clientY, orig: placement };
    e.currentTarget.setPointerCapture(e.pointerId);
  };
  const onPointerMove = (e: ReactPointerEvent) => {
    const d = drag.current;
    if (!d) return;
    const { dx, dy } = pxDelta(e, d);
    const next =
      d.kind === 'move'
        ? { ...d.orig, x: d.orig.x + dx, y: d.orig.y + dy }
        : { ...d.orig, w: d.orig.w + dx, h: d.orig.h + dy };
    setPreview(clampPlacement(next));
  };
  const endDrag = () => {
    if (drag.current && preview) onMove(id, preview);
    drag.current = null;
    setPreview(null);
  };

  const onKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = e.shiftKey ? 'resize' : 'move';
    const delta = e.key === 'ArrowLeft' ? -1 : e.key === 'ArrowRight' ? 1 : 0;
    const dy = e.key === 'ArrowUp' ? -1 : e.key === 'ArrowDown' ? 1 : 0;
    if (delta === 0 && dy === 0) return;
    e.preventDefault();
    const p = placement;
    const next =
      step === 'move'
        ? { ...p, x: p.x + delta, y: p.y + dy }
        : { ...p, w: p.w + delta, h: p.h + dy };
    onMove(id, clampPlacement(next));
  };

  return (
    <section
      aria-label={`${title} panel`}
      className="absolute flex flex-col overflow-hidden rounded-lg border border-neutral-700 bg-neutral-900 shadow"
      style={{
        left: `${(shown.x / GRID_COLS) * 100}%`,
        top: shown.y * ROW_H,
        width: `${(shown.w / GRID_COLS) * 100}%`,
        height: shown.h * ROW_H,
      }}
    >
      <div
        role="toolbar"
        tabIndex={0}
        aria-label={`${title} panel controls — arrow keys move, shift+arrows resize, position ${shown.x},${shown.y} size ${shown.w}×${shown.h}`}
        className="flex cursor-move touch-none items-center justify-between border-b border-neutral-800 bg-neutral-950 px-3 py-1.5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-sky-500"
        onPointerDown={startDrag('move')}
        onPointerMove={onPointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
        onKeyDown={onKeyDown}
      >
        <span className="select-none text-xs font-semibold uppercase tracking-wide text-neutral-400">
          {title}
          {safetyCritical && (
            <span
              className="ml-2 text-amber-500"
              title="Safety-critical panel — hiding requires confirmation"
            >
              ⚠
            </span>
          )}
        </span>
        <button
          type="button"
          aria-label={`Hide ${title} panel`}
          onPointerDown={(e) => e.stopPropagation()}
          onClick={() => onHide(id, safetyCritical)}
          className="rounded px-2 py-1 text-xs text-neutral-500 hover:bg-neutral-800 hover:text-neutral-200 focus-visible:ring-2 focus-visible:ring-sky-500"
        >
          hide
        </button>
      </div>
      <div className="min-h-0 flex-1 overflow-auto p-2">
        <Suspense fallback={<p className="p-4 text-xs text-neutral-500">Loading panel…</p>}>
          {children}
        </Suspense>
      </div>
      <button
        type="button"
        aria-label={`Resize ${title} panel — drag or use shift+arrows on the header`}
        className="absolute bottom-0 right-0 h-6 w-6 cursor-nwse-resize touch-none rounded-tl bg-neutral-700/60 focus-visible:ring-2 focus-visible:ring-sky-500"
        onPointerDown={startDrag('resize')}
        onPointerMove={onPointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
      />
    </section>
  );
}

export default function WorkspacePage() {
  const scope = useScopeKey();
  const mode = useUiMode();
  const { theme, setTheme } = useTheme();
  const draftSymbol = useOrderDraft((s) => s.draft.symbol);
  const symbol = draftSymbol !== '' ? draftSymbol : 'EUR/USD';

  useMarketFeed(symbol, wsClient, { depth: true });
  usePrivatePositionsFeed(wsClient);

  const [layoutName, setLayoutName] = useState('');
  const [placements, setPlacements] = useState<Placements>(() => {
    if (typeof window === 'undefined') return defaultPlacements(mode);
    return loadLayout(scope, mode).placements;
  });
  const [savedLayouts, setSavedLayouts] = useState<string[]>(() =>
    typeof window === 'undefined' ? [] : listLayouts(scope),
  );
  const [hideWarn, setHideWarn] = useState<PanelId | null>(null);
  const [saveAs, setSaveAs] = useState(false);
  const [newName, setNewName] = useState('');
  const [notice, setNotice] = useState<string | null>(null);
  const containerRef = useRef<HTMLDivElement>(null);

  // Reload when the account scope OR the ui mode switches — placements
  // are resolved per mode, so a lite→pro toggle must not keep the lite
  // geometry (and its hidden-panel set) on the Pro grid.
  const [loadedAt, setLoadedAt] = useState({ scope, mode });
  if (loadedAt.scope !== scope || loadedAt.mode !== mode) {
    setLoadedAt({ scope, mode });
    const loaded = loadLayout(scope, mode);
    setPlacements(loaded.placements);
    setLayoutName(loaded.name);
    setSavedLayouts(listLayouts(scope));
  }

  const move = (id: PanelId, p: PanelPlacement) => setPlacements((prev) => ({ ...prev, [id]: p }));

  const hide = (id: PanelId) =>
    setPlacements((prev) => ({ ...prev, [id]: { ...prev[id], visible: false } }));

  const show = (id: PanelId) =>
    setPlacements((prev) => ({ ...prev, [id]: { ...prev[id], visible: true } }));

  const onHide = (id: PanelId, safetyCritical: boolean) => {
    if (safetyCritical) setHideWarn(id);
    else hide(id);
  };

  const hidden = PANELS.filter((p) => !placements[p.id].visible);
  const maxY = useMemo(
    () =>
      PANELS.reduce(
        (m, p) =>
          placements[p.id].visible ? Math.max(m, placements[p.id].y + placements[p.id].h) : m,
        6,
      ),
    [placements],
  );

  if (mode === 'lite') {
    return (
      <div className="ws-root min-h-full" data-theme={theme}>
        <div className="flex items-center justify-between border-b border-neutral-800 px-4 py-2">
          <h1 className="text-lg font-semibold">Workspace</h1>
          <div className="flex items-center gap-2">
            <SubAccountSwitcher />
            <ModeToggle />
          </div>
        </div>
        <LiteDashboard />
      </div>
    );
  }

  return (
    <div className="ws-root flex min-h-full flex-col" data-theme={theme}>
      <div className="flex flex-wrap items-center gap-2 border-b border-neutral-800 px-4 py-2">
        <h1 className="mr-2 text-lg font-semibold">Workspace</h1>
        <ModeToggle />
        <SubAccountSwitcher />
        <label className="sr-only" htmlFor="ws-layout">
          Layout
        </label>
        <select
          id="ws-layout"
          value={layoutName}
          onChange={(e) => {
            const name = e.target.value;
            if (name === '') return;
            const l = loadLayout(scope, mode, name);
            setPlacements(l.placements);
            setLayoutName(l.name === name ? name : l.name);
            setNotice(`Layout "${name}" loaded`);
          }}
          className={`${selectCls} w-auto py-1 text-xs`}
        >
          <option value="">{layoutName !== '' ? layoutName : '— layouts —'}</option>
          {savedLayouts.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
        <button
          type="button"
          className={btnGhost}
          onClick={() => {
            setNewName('');
            setSaveAs(true);
          }}
        >
          Save as…
        </button>
        {layoutName !== '' && (
          <button
            type="button"
            className={btnGhost}
            onClick={() => {
              saveLayout(scope, layoutName, placements);
              setNotice(`Layout "${layoutName}" saved`);
            }}
          >
            Save
          </button>
        )}
        <button
          type="button"
          className={btnGhost}
          aria-label="Reset workspace layout"
          onClick={() => {
            resetLayouts(scope);
            setPlacements(defaultPlacements(mode));
            setSavedLayouts([]);
            setLayoutName('');
            setNotice('Workspace reset to the Pro default layout');
          }}
        >
          Reset
        </button>
        <button
          type="button"
          className={btnGhost}
          aria-pressed={theme === 'light'}
          onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
        >
          {theme === 'dark' ? 'Light theme' : 'Dark theme'}
        </button>
        {hidden.length > 0 && (
          <select
            aria-label="Show a hidden panel"
            value=""
            onChange={(e) => {
              const meta = PANELS.find((p) => p.id === e.target.value);
              if (meta) show(meta.id);
            }}
            className={`${selectCls} w-auto py-1 text-xs`}
          >
            <option value="">＋ show panel…</option>
            {hidden.map((p) => (
              <option key={p.id} value={p.id}>
                {p.title}
              </option>
            ))}
          </select>
        )}
        <span className="ml-auto text-xs text-neutral-500" aria-live="polite">
          {notice ?? `scope: ${scope}`}
        </span>
      </div>

      {/* Symbol context strip — MT5 Market Watch / Binance symbol bar
          role. Fixed chrome, not a grid panel: price context for the
          order ticket must never be hideable. */}
      <div className="border-b border-neutral-800 px-4 py-1.5">
        <TickerStrip symbol={symbol} />
      </div>

      {/* The Pro grid needs ≥640px — tell narrow viewports instead of
          silently scrolling; Lite mode is the small-screen surface. */}
      <p className="border-b border-neutral-800 px-4 py-1 text-xs text-amber-500 sm:hidden">
        Workspace grid is wider than this screen — scroll sideways or{' '}
        <button
          type="button"
          className="underline underline-offset-2"
          onClick={() => useUiModeStore.getState().setMode('lite')}
        >
          switch to Lite mode
        </button>
        .
      </p>

      <div
        ref={containerRef}
        className="relative m-2"
        style={{ height: maxY * ROW_H, minWidth: 640 }}
        role="application"
        aria-label="Workspace grid — focus a panel header to move it with arrow keys"
      >
        {PANELS.filter((p) => placements[p.id].visible).map((p) => (
          <PanelFrame
            key={p.id}
            id={p.id}
            title={p.title}
            safetyCritical={p.safetyCritical}
            placement={placements[p.id]}
            containerRef={containerRef}
            onMove={move}
            onHide={onHide}
          >
            <PanelBody id={p.id} symbol={symbol} />
          </PanelFrame>
        ))}
      </div>

      {/* safety-critical hide warning */}
      <Modal
        open={hideWarn !== null}
        title="Hide safety-critical panel?"
        onClose={() => setHideWarn(null)}
      >
        <ConfirmAction
          message={
            <>
              <strong>{PANELS.find((p) => p.id === hideWarn)?.title}</strong> is a safety-critical
              surface — hiding it removes your view of open risk controls. You can restore it from
              “show panel…”.
            </>
          }
          confirmLabel="Hide anyway"
          onConfirm={() => {
            if (hideWarn) hide(hideWarn);
            setHideWarn(null);
          }}
          onCancel={() => setHideWarn(null)}
        />
      </Modal>

      {/* save-as dialog */}
      <Modal open={saveAs} title="Save layout as" onClose={() => setSaveAs(false)}>
        <label htmlFor="ws-save-name" className="mb-1 block text-sm text-neutral-300">
          Layout name
        </label>
        <input
          id="ws-save-name"
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
          className={inputCls}
          placeholder="e.g. Scalping desk"
        />
        <div className="mt-4 flex justify-end gap-2">
          <button type="button" className={btnGhost} onClick={() => setSaveAs(false)}>
            Cancel
          </button>
          <button
            type="button"
            className={btnGhost}
            disabled={newName.trim() === ''}
            onClick={() => {
              const name = newName.trim();
              saveLayout(scope, name, placements);
              setSavedLayouts(listLayouts(scope));
              setLayoutName(name);
              setSaveAs(false);
              setNotice(`Layout "${name}" saved (this device, ${scope})`);
            }}
          >
            Save
          </button>
        </div>
      </Modal>
    </div>
  );
}
