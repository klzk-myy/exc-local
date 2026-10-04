import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const OpsSafetyPage = lazy(() => import('./OpsSafetyPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/ops-safety', element: OpsSafetyPage, title: 'Ops Safety' },
];
