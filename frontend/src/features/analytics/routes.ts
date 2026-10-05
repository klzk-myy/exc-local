import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const AnalyticsPage = lazy(() => import('./AnalyticsPage'));

export const routes: FeatureRoute[] = [
  { path: 'analytics', element: AnalyticsPage, title: 'Market Analytics' },
];
