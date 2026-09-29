import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const OpsBoardPage = lazy(() => import('./OpsBoardPage'));
const FleetPage = lazy(() => import('./FleetPage'));
const ReleasesPage = lazy(() => import('./ReleasesPage'));

export const routes: FeatureRoute[] = [
  { path: 'ops', element: OpsBoardPage, title: 'Ops Board' },
  { path: 'ops/fleet', element: FleetPage, title: 'Fleet' },
  { path: 'ops/releases', element: ReleasesPage, title: 'Releases' },
];
