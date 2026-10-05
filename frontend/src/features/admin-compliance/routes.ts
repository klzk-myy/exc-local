import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const CompliancePage = lazy(() => import('./CompliancePage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/compliance', element: CompliancePage, title: 'Compliance Ops' },
];
