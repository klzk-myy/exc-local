import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const WorkspacePage = lazy(() => import('./WorkspacePage'));
const SymbolRedirect = lazy(() => import('./SymbolRedirect'));

export const routes: FeatureRoute[] = [
  { path: 'workspace', element: WorkspacePage, title: 'Workspace' },
  // Legacy trading surfaces fold into the workspace — the redirect
  // carries the symbol into the order draft (see SymbolRedirect).
  { path: 'trade/:symbol', element: SymbolRedirect, title: 'Workspace' },
  { path: 'advanced/:symbol?', element: SymbolRedirect, title: 'Workspace' },
];
