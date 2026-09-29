import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const SupportPage = lazy(() => import('./SupportPage'));
const TicketDetailPage = lazy(() => import('./TicketDetailPage'));

export const routes: FeatureRoute[] = [
  { path: 'support', element: SupportPage, title: 'Support' },
  { path: 'support/tickets/:id', element: TicketDetailPage, title: 'Ticket' },
];
