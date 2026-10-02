import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const WebhooksPage = lazy(() => import('./WebhooksPage'));

export const routes: FeatureRoute[] = [
  { path: 'webhooks', element: WebhooksPage, title: 'Webhooks' },
];
