import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const OrderBookPage = lazy(() => import('./OrderBookPage'));

export const routes: FeatureRoute[] = [
  { path: 'book/:symbol', element: OrderBookPage, title: 'Order Book' },
];
