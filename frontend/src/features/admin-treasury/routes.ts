import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const TreasuryPage = lazy(() => import('./TreasuryPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/treasury', element: TreasuryPage, title: 'Treasury' },
];
