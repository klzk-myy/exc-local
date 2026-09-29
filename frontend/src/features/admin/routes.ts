import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const AdminPage = lazy(() => import('./AdminPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin', element: AdminPage, title: 'Admin Console' },
];
