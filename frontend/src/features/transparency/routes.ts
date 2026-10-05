import { lazy } from 'react';
import type { FeatureRoute } from '@/app/manifest';

const TransparencyPage = lazy(() => import('./TransparencyPage'));
const StatusPage = lazy(() => import('./StatusPage'));
const SecurityPage = lazy(() => import('./SecurityPage'));

export const routes: FeatureRoute[] = [
  { path: 'transparency', element: TransparencyPage, title: 'Venue transparency' },
  { path: 'status', element: StatusPage, title: 'System status' },
  { path: 'security', element: SecurityPage, title: 'Security' },
];
