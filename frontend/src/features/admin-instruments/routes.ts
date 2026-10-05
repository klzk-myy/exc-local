import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const InstrumentGovernancePage = lazy(() => import('./InstrumentGovernancePage'));

export const routes: FeatureRoute[] = [
  {
    path: 'admin/instrument-governance',
    element: InstrumentGovernancePage,
    title: 'Instrument Governance',
  },
];
