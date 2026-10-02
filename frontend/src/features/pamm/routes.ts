import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const PammPage = lazy(() => import('./PammPage'));

export const routes: FeatureRoute[] = [{ path: 'pamm', element: PammPage, title: 'PAMM pools' }];
