import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

/**
 * Manifest contract (see frontend/README.md + src/app/manifest.ts):
 * `element` MUST be a `React.lazy` factory so the page ships as its own
 * chunk and the ≤300 kB initial-JS budget holds as features grow.
 */
const HomePage = lazy(() => import('./HomePage'));

export const routes: FeatureRoute[] = [{ index: true, element: HomePage, title: 'Dashboard' }];
