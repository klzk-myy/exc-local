import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const MarketMakingPage = lazy(() => import('./MarketMakingPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/market-making', element: MarketMakingPage, title: 'Market Making' },
];
