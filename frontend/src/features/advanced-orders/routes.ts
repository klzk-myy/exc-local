import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const FeeTiersPage = lazy(() => import('./FeeTiersPage'));

/**
 * Advanced order types + sub-account switching + quick actions +
 * ADL + chart overlays (Phase-10 Tasks 10.3.7–10.3.15 cluster).
 * `/advanced/:symbol` redirects to the workspace (registered there);
 * the panels live on as workspace components, not standalone pages.
 * All routes lazy — nothing in the initial JS bundle.
 */
export const routes: FeatureRoute[] = [
  { path: 'fee-tiers', element: FeeTiersPage, title: 'Fee tier assignment' },
];
