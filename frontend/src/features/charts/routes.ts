import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const ChartPage = lazy(() => import('./ChartPage'));

export const routes: FeatureRoute[] = [
  { path: 'chart/:symbol', element: ChartPage, title: 'Chart' },
];
