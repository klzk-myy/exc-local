import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const WorkspacePage = lazy(() => import('./WorkspacePage'));

export const routes: FeatureRoute[] = [
  { path: 'workspace', element: WorkspacePage, title: 'Workspace' },
];
