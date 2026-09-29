import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const KycPage = lazy(() => import('./KycPage'));

export const routes: FeatureRoute[] = [
  { path: 'kyc', element: KycPage, title: 'Identity verification' },
];
