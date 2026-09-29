import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const CopyGridPage = lazy(() => import('./CopyGridPage'));

export const routes: FeatureRoute[] = [
  { path: 'copy-grid', element: CopyGridPage, title: 'Copy trading & grid bots' },
];
