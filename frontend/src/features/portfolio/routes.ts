import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const PortfolioPage = lazy(() => import('./PortfolioPage'));

export const routes: FeatureRoute[] = [
  { path: 'portfolio', element: PortfolioPage, title: 'Portfolio' },
];
