import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const FundingPage = lazy(() => import('./FundingPage'));

export const routes: FeatureRoute[] = [{ path: 'funding', element: FundingPage, title: 'Funding' }];
