import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const Customer360Page = lazy(() => import('./Customer360Page'));

export const routes: FeatureRoute[] = [
  { path: 'admin/customers', element: Customer360Page, title: 'Customer 360' },
];
