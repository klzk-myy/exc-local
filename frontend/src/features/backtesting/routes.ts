import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const BacktestPage = lazy(() => import('./BacktestPage'));

export const routes: FeatureRoute[] = [
  { path: 'backtest', element: BacktestPage, title: 'Backtester' },
];
