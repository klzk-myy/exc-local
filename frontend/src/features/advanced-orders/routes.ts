import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const AdvancedOrdersPage = lazy(() => import('./AdvancedOrdersPage'));
const FeeTiersPage = lazy(() => import('./FeeTiersPage'));

/**
 * Advanced order types + sub-account switching + quick actions +
 * ADL + chart overlays (Phase-10 Tasks 10.3.7–10.3.15 cluster).
 * All routes lazy — nothing in the initial JS bundle.
 */
export const routes: FeatureRoute[] = [
  { path: 'advanced/:symbol?', element: AdvancedOrdersPage, title: 'Advanced orders' },
  { path: 'fee-tiers', element: FeeTiersPage, title: 'Fee tier assignment' },
];
