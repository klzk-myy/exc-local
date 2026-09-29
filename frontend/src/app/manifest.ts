/**
 * Feature manifest contract + auto-discovery (Phase-10 Task 10.3.1).
 *
 * Parallel agents add features WITHOUT touching shared files: drop a
 * directory under `src/features/<name>/` exporting these conventions and
 * Vite's `import.meta.glob` picks them up at build time:
 *
 *   routes.ts  → `export const routes: FeatureRoute[]`
 *   nav.ts     → `export const nav: FeatureNavItem[]`   (optional)
 *
 * Contract (full documentation in frontend/README.md):
 *   interface FeatureRoute {
 *     path?: string;            // route path, e.g. 'trade/:symbol'
 *     index?: boolean;          // index route for '/' (mutually exclusive)
 *     element: ElementType;     // pass lazy(() => import('./Page'))
 *     title: string;            // document.title + a11y route announcement
 *     children?: FeatureRoute[];
 *   }
 *   interface FeatureNavItem {
 *     label: string;            // en-US label (no i18n, spec §27 R5)
 *     to: string;               // absolute path matching a feature route
 *     icon?: string;            // icon token resolved by the shell
 *     section: string;          // nav grouping (e.g. 'Trade', 'Account')
 *     order?: number;           // sort key within the section
 *   }
 *
 * Elements are `React.lazy` factories → every feature page is its own
 * chunk; the glob only eagerly loads the tiny routes.ts manifests, which
 * is how the ≤300 kB initial-JS budget survives feature growth.
 */
import type { ElementType } from 'react';

export interface FeatureRoute {
  path?: string;
  index?: boolean;
  element: ElementType;
  title: string;
  children?: FeatureRoute[];
}

export interface FeatureNavItem {
  label: string;
  to: string;
  icon?: string;
  section: string;
  order?: number;
}

interface RoutesModule {
  routes?: FeatureRoute[] | { default?: FeatureRoute[] };
}
interface NavModule {
  nav?: FeatureNavItem[] | { default?: FeatureNavItem[] };
}

function unwrap<T>(v: T[] | { default?: T[] } | undefined): T[] {
  if (Array.isArray(v)) return v;
  if (v && Array.isArray(v.default)) return v.default;
  return [];
}

// Eager glob of the manifest files only — feature page components stay
// lazy chunks behind React.lazy in each routes.ts.
const routeModules = import.meta.glob<RoutesModule>('../features/*/routes.ts', { eager: true });
const navModules = import.meta.glob<NavModule>('../features/*/nav.ts', { eager: true });

export function collectFeatureRoutes(): { feature: string; route: FeatureRoute }[] {
  const out: { feature: string; route: FeatureRoute }[] = [];
  for (const [file, mod] of Object.entries(routeModules)) {
    const feature = file.replace('../features/', '').replace('/routes.ts', '');
    for (const route of unwrap(mod.routes)) out.push({ feature, route });
  }
  return out.sort((a, b) => a.feature.localeCompare(b.feature));
}

export function collectNavItems(): { feature: string; item: FeatureNavItem }[] {
  const out: { feature: string; item: FeatureNavItem }[] = [];
  for (const [file, mod] of Object.entries(navModules)) {
    const feature = file.replace('../features/', '').replace('/nav.ts', '');
    for (const item of unwrap(mod.nav)) out.push({ feature, item });
  }
  return out.sort(
    (a, b) =>
      a.item.section.localeCompare(b.item.section) ||
      (a.item.order ?? 0) - (b.item.order ?? 0) ||
      a.item.label.localeCompare(b.item.label),
  );
}

/** Nav items grouped by `section`, preserving the sorted order. */
export function navBySection(): Map<string, { feature: string; item: FeatureNavItem }[]> {
  const map = new Map<string, { feature: string; item: FeatureNavItem }[]>();
  for (const entry of collectNavItems()) {
    const list = map.get(entry.item.section) ?? [];
    list.push(entry);
    map.set(entry.item.section, list);
  }
  return map;
}
