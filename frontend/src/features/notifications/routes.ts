import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const InboxPage = lazy(() => import('./InboxPage'));

export const routes: FeatureRoute[] = [
  { path: 'notifications', element: InboxPage, title: 'Notifications' },
];
