import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const SettingsPage = lazy(() => import('./SettingsPage'));

export const routes: FeatureRoute[] = [
  { path: 'settings', element: SettingsPage, title: 'Account settings' },
];
