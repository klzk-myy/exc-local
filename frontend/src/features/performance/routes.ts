import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const PerformancePage = lazy(() => import('./PerformancePage'));

export const routes: FeatureRoute[] = [
  { path: 'performance', element: PerformancePage, title: 'Performance' },
];
