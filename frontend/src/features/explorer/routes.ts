import { lazy } from 'react';
import type { FeatureRoute } from '@/app/manifest';

const HistoryExplorerPage = lazy(() => import('./HistoryExplorerPage'));

export const routes: FeatureRoute[] = [
  { path: 'explorer', element: HistoryExplorerPage, title: 'History explorer' },
];
