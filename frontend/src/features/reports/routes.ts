import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const ReportsPage = lazy(() => import('./ReportsPage'));

export const routes: FeatureRoute[] = [
  { path: 'reports', element: ReportsPage, title: 'Reports & transparency' },
];
