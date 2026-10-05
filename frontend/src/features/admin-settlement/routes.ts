import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const SettlementPage = lazy(() => import('./SettlementPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/settlement', element: SettlementPage, title: 'Settlement' },
];
