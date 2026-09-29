import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const CalculatorPage = lazy(() => import('./CalculatorPage'));

export const routes: FeatureRoute[] = [
  { path: 'calculator/:symbol?', element: CalculatorPage, title: 'Position calculator' },
];
