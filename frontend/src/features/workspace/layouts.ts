/**
 * Workspace layout model + persistence (Task 10.3.14).
 *
 * Grid: 12 columns × 56px rows, panels placed absolutely. Layouts are
 * named, saved per ACCOUNT SCOPE (localStorage is inherently per-device —
 * the spec exposes no workspace-layout endpoint, so `exc.workspace.v1.
 * {scopeKey}` is the documented, honestly-scoped fallback).
 *
 * Safety: `order` and `positions` are safety-critical — the UI warns
 * before hiding them (the flag is data here, the modal lives in
 * WorkspacePage).
 */
import type { UiMode } from './liteMode';

export type PanelId = 'order' | 'book' | 'positions' | 'chart' | 'depth' | 'balances' | 'tape';

export interface PanelMeta {
  id: PanelId;
  title: string;
  /** Hiding a safety-critical panel requires an explicit warning. */
  safetyCritical: boolean;
}

export const PANELS: readonly PanelMeta[] = [
  { id: 'order', title: 'Order ticket', safetyCritical: true },
  { id: 'book', title: 'Order book', safetyCritical: false },
  { id: 'positions', title: 'Blotter (positions/orders/history)', safetyCritical: true },
  { id: 'chart', title: 'Chart & overlays', safetyCritical: false },
  { id: 'depth', title: 'Market depth', safetyCritical: false },
  { id: 'tape', title: 'Market trades', safetyCritical: false },
  { id: 'balances', title: 'Balances', safetyCritical: false },
] as const;

export interface PanelPlacement {
  /** 0..11 column, 0.. row index. */
  x: number;
  y: number;
  /** 1..12 columns, ≥1 rows. */
  w: number;
  h: number;
  visible: boolean;
}

export type Placements = Record<PanelId, PanelPlacement>;

export interface WorkspaceLayout {
  name: string;
  mode: UiMode;
  placements: Placements;
}

export const GRID_COLS = 12;
export const ROW_H = 64;

/** Binance spot-classic arrangement, mapped onto the 12-col grid:
 * full-height order-book column left, chart-over-ticket center,
 * full-height market-trades column right, and the orders/positions
 * blotter strip along the bottom with balances beside it. Binance
 * renders market depth as a chart-mode tab rather than a separate
 * panel — our chart has no depth mode, so the depth panel ships
 * hidden and is one "show panel…" pick away. */
export const PRO_DEFAULT: Placements = {
  book: { x: 0, y: 0, w: 2, h: 20, visible: true },
  chart: { x: 2, y: 0, w: 7, h: 13, visible: true },
  order: { x: 2, y: 13, w: 7, h: 7, visible: true },
  tape: { x: 9, y: 0, w: 3, h: 20, visible: true },
  depth: { x: 9, y: 13, w: 3, h: 7, visible: false },
  positions: { x: 0, y: 20, w: 9, h: 8, visible: true },
  balances: { x: 9, y: 20, w: 3, h: 8, visible: true },
};

export const LITE_DEFAULT: Placements = {
  order: { x: 0, y: 0, w: 6, h: 8, visible: true },
  positions: { x: 6, y: 0, w: 6, h: 8, visible: true },
  balances: { x: 0, y: 8, w: 12, h: 4, visible: true },
  book: { x: 0, y: 12, w: 12, h: 6, visible: false },
  chart: { x: 0, y: 12, w: 12, h: 6, visible: false },
  depth: { x: 0, y: 12, w: 12, h: 6, visible: false },
  tape: { x: 0, y: 12, w: 12, h: 6, visible: false },
};

export function defaultPlacements(mode: UiMode): Placements {
  const src = mode === 'lite' ? LITE_DEFAULT : PRO_DEFAULT;
  return structuredClone(src);
}

/** Name of the shipped default per mode. 'def' is the canonical Pro
 * default — it shows in the layout picker and is what loads with
 * nothing saved; a stored layout of the same name overrides it. */
export function builtinLayoutName(mode: UiMode): string {
  return mode === 'lite' ? 'Lite default' : 'def';
}

// -- persistence ---------------------------------------------------------------

export interface WorkspaceStoreShape {
  activeName: string;
  /** Stored layouts may predate newer panels — they validate as Partial
   * and loadLayout merges the missing panels in from the mode default. */
  layouts: Record<string, Partial<Placements>>;
}

export function workspaceStorageKey(scopeKey: string): string {
  return `exc.workspace.v1.${scopeKey}`;
}

function readStore(scopeKey: string, storage: Storage): WorkspaceStoreShape {
  try {
    const raw = storage.getItem(workspaceStorageKey(scopeKey));
    if (raw === null) return { activeName: '', layouts: {} };
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== 'object' || parsed === null) return { activeName: '', layouts: {} };
    const r = parsed as Record<string, unknown>;
    const layouts: Record<string, Partial<Placements>> = {};
    if (typeof r['layouts'] === 'object' && r['layouts'] !== null) {
      for (const [name, v] of Object.entries(r['layouts'] as Record<string, unknown>)) {
        if (isPlacements(v)) layouts[name] = v;
      }
    }
    return { activeName: typeof r['activeName'] === 'string' ? r['activeName'] : '', layouts };
  } catch {
    return { activeName: '', layouts: {} };
  }
}

function isPlacements(v: unknown): v is Partial<Placements> {
  if (typeof v !== 'object' || v === null) return false;
  const r = v as Record<string, unknown>;
  // Entries are per-panel; a stored layout may legitimately lack panels
  // added after it was saved (defaults merge in at load). Unknown panel
  // keys are dropped rather than invalidating the whole layout.
  return Object.entries(r).every(([key, c]) => {
    if (!PANELS.some((p) => p.id === key)) return true;
    if (typeof c !== 'object' || c === null) return false;
    const pl = c as Record<string, unknown>;
    return (
      typeof pl['x'] === 'number' &&
      typeof pl['y'] === 'number' &&
      typeof pl['w'] === 'number' &&
      typeof pl['h'] === 'number' &&
      typeof pl['visible'] === 'boolean'
    );
  });
}

function writeStore(scopeKey: string, store: WorkspaceStoreShape, storage: Storage): void {
  try {
    storage.setItem(workspaceStorageKey(scopeKey), JSON.stringify(store));
  } catch {
    // quota/private mode — the workspace stays functional in-memory.
  }
}

export function listLayouts(
  scopeKey: string,
  mode: UiMode,
  storage: Storage = localStorage,
): string[] {
  const builtin = builtinLayoutName(mode);
  const stored = Object.keys(readStore(scopeKey, storage).layouts).sort();
  return [builtin, ...stored.filter((n) => n !== builtin)];
}

/** Load named layout; falls back to the mode default (never fabricates
 * a stored layout that doesn't exist). */
export function loadLayout(
  scopeKey: string,
  mode: UiMode,
  name?: string,
  storage: Storage = localStorage,
): WorkspaceLayout {
  const store = readStore(scopeKey, storage);
  const want = name ?? store.activeName;
  const stored = store.layouts[want];
  return {
    name: stored ? want : builtinLayoutName(mode),
    mode,
    placements: stored ? mergeWithDefaults(stored, mode) : defaultPlacements(mode),
  };
}

/** Fill panels missing from a stored layout with the mode default so a
 * saved layout survives the panel set growing (unknown keys dropped). */
function mergeWithDefaults(stored: Partial<Placements>, mode: UiMode): Placements {
  const out = defaultPlacements(mode);
  for (const p of PANELS) {
    const s = stored[p.id];
    if (s !== undefined) out[p.id] = structuredClone(s);
  }
  return out;
}

export function saveLayout(
  scopeKey: string,
  name: string,
  placements: Placements,
  storage: Storage = localStorage,
): void {
  const store = readStore(scopeKey, storage);
  store.layouts[name] = structuredClone(placements);
  store.activeName = name;
  writeStore(scopeKey, store, storage);
}

/** Mark a layout as the default for this scope — the name resolves
 * through the normal chain (stored layout wins, otherwise the builtin
 * of that name falls back to the mode default). */
export function setActiveLayout(
  scopeKey: string,
  name: string,
  storage: Storage = localStorage,
): void {
  const store = readStore(scopeKey, storage);
  store.activeName = name;
  writeStore(scopeKey, store, storage);
}

export function deleteLayout(
  scopeKey: string,
  name: string,
  storage: Storage = localStorage,
): void {
  const store = readStore(scopeKey, storage);
  store.layouts = Object.fromEntries(Object.entries(store.layouts).filter(([k]) => k !== name));
  if (store.activeName === name) store.activeName = '';
  writeStore(scopeKey, store, storage);
}

/** Reset = drop every persisted layout for this scope → defaults return. */
export function resetLayouts(scopeKey: string, storage: Storage = localStorage): void {
  try {
    storage.removeItem(workspaceStorageKey(scopeKey));
  } catch {
    /* ignore */
  }
}

// -- geometry helpers ------------------------------------------------------------

export function clampPlacement(p: PanelPlacement): PanelPlacement {
  const w = Math.max(2, Math.min(GRID_COLS, Math.round(p.w)));
  const x = Math.max(0, Math.min(GRID_COLS - w, Math.round(p.x)));
  const h = Math.max(2, Math.min(20, Math.round(p.h)));
  const y = Math.max(0, Math.round(p.y));
  return { x, y, w, h, visible: p.visible };
}
