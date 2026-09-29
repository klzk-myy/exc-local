import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const DepthChartPage = lazy(() => import('./DepthChartPage'));

export const routes: FeatureRoute[] = [
  { path: 'depth/:symbol', element: DepthChartPage, title: 'Market depth' },
];
