import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const ContentPage = lazy(() => import('./ContentPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/content', element: ContentPage, title: 'Content & Emergency' },
];
