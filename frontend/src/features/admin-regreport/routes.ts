import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const RegReportingPage = lazy(() => import('./RegReportingPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/regreporting', element: RegReportingPage, title: 'Reg Reporting' },
];
