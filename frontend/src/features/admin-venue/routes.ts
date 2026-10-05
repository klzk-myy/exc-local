import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const VenuePage = lazy(() => import('./VenuePage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/venue', element: VenuePage, title: 'Venue Governance' },
];
