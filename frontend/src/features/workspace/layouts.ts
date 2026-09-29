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

export type PanelId = 'order' | 'positions' | 'chart' | 'depth' | 'balances';

export interface PanelMeta {
  id: PanelId;
  title: string;
  /** Hiding a safety-critical panel requires an explicit warning. */
  safetyCritical: boolean;
}

export const PANELS: readonly PanelMeta[] = [
  { id: 'order', title: 'Order ticket', safetyCritical: true },
  { id: 'positions', title: 'Positions & quick actions', safetyCritical: true },
  { id: 'chart', title: 'Chart & overlays', safetyCritical: false },
  { id: 'depth', title: 'Market depth', safetyCritical: false },
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

export const PRO_DEFAULT: Placements = {
  order: { x: 0, y: 0, w: 4, h: 12, visible: true },
  chart: { x: 4, y: 0, w: 8, h: 7, visible: true },
  depth: { x: 4, y: 7, w: 8, h: 5, visible: true },
  positions: { x: 0, y: 12, w: 12, h: 5, visible: true },
  balances: { x: 0, y: 17, w: 12, h: 3, visible: false },
};

export const LITE_DEFAULT: Placements = {
  order: { x: 0, y: 0, w: 6, h: 8, visible: true },
  positions: { x: 6, y: 0, w: 6, h: 8, visible: true },
  balances: { x: 0, y: 8, w: 12, h: 4, visible: true },
  chart: { x: 0, y: 12, w: 12, h: 6, visible: false },
  depth: { x: 0, y: 12, w: 12, h: 6, visible: false },
};

export function defaultPlacements(mode: UiMode): Placements {
  const src = mode === 'lite' ? LITE_DEFAULT : PRO_DEFAULT;
  return structuredClone(src);
}

// -- persistence ---------------------------------------------------------------

export interface WorkspaceStoreShape {
  activeName: string;
  layouts: Record<string, Placements>;
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
    const layouts: Record<string, Placements> = {};
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

function isPlacements(v: unknown): v is Placements {
  if (typeof v !== 'object' || v === null) return false;
  const r = v as Record<string, unknown>;
  return PANELS.every((p) => {
    const pl = r[p.id];
    if (typeof pl !== 'object' || pl === null) return false;
    const c = pl as Record<string, unknown>;
    return (
      typeof c['x'] === 'number' &&
      typeof c['y'] === 'number' &&
      typeof c['w'] === 'number' &&
      typeof c['h'] === 'number' &&
      typeof c['visible'] === 'boolean'
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

export function listLayouts(scopeKey: string, storage: Storage = localStorage): string[] {
  return Object.keys(readStore(scopeKey, storage).layouts).sort();
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
  const placements = store.layouts[want];
  return {
    name: placements ? want : `${mode === 'lite' ? 'Lite' : 'Pro'} default`,
    mode,
    placements: placements ? structuredClone(placements) : defaultPlacements(mode),
  };
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
