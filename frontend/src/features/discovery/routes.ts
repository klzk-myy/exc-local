import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const DiscoveryPage = lazy(() => import('./DiscoveryPage'));

export const routes: FeatureRoute[] = [
  { path: 'discovery', element: DiscoveryPage, title: 'Market Discovery' },
];
