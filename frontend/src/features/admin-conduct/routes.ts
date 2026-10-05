import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const ConductPage = lazy(() => import('./ConductPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/conduct', element: ConductPage, title: 'Conduct & Governance' },
];
