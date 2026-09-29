import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const TradePage = lazy(() => import('./TradePage'));

export const routes: FeatureRoute[] = [
  { path: 'trade/:symbol', element: TradePage, title: 'Trade' },
];
