import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const FinancePage = lazy(() => import('./FinancePage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/finance', element: FinancePage, title: 'Finance' },
];
