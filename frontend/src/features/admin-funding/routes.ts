import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const FundingOpsPage = lazy(() => import('./FundingOpsPage'));

export const routes: FeatureRoute[] = [
  { path: 'admin/funding-ops', element: FundingOpsPage, title: 'Funding Ops' },
];
