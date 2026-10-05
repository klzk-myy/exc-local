import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const CaseDeskPage = lazy(() => import('./CaseDeskPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/surveillance', element: CaseDeskPage, title: 'Surveillance & AML' },
];
