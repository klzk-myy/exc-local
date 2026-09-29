import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const HistoryPage = lazy(() => import('./HistoryPage'));

export const routes: FeatureRoute[] = [{ path: 'orders', element: HistoryPage, title: 'Orders' }];
