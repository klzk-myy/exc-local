import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const IntegrityPage = lazy(() => import('./IntegrityPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/integrity', element: IntegrityPage, title: 'Integrity Console' },
];
