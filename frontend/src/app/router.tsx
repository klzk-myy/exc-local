/**
 * Router — auto-discovers feature routes via `import.meta.glob` over
 * `features/<name>/routes.ts`. Per-route code splitting comes from each
 * feature declaring `element: lazy(() => import('./Page'))`; this module
 * only adds Suspense + the document-title side effect.
 */
import { Suspense, useEffect, type ReactNode } from 'react';
import { createBrowserRouter, useLocation, type RouteObject } from 'react-router';

import { AppShell } from '@/components/AppShell';
import { collectFeatureRoutes, type FeatureRoute } from './manifest';

function RouteFallback() {
  return (
    <div className="flex h-full items-center justify-center text-neutral-400" role="status">
      Loading…
    </div>
  );
}

function RouteTitle({ title, children }: { title: string; children: ReactNode }) {
  const loc = useLocation();
  useEffect(() => {
    document.title = `${title} — Exchange`;
  }, [title, loc.pathname]);
  return <>{children}</>;
}

function toRouteObject(route: FeatureRoute): RouteObject {
  const Element = route.element;
  const element = (
    <RouteTitle title={route.title}>
      <Suspense fallback={<RouteFallback />}>
        <Element />
      </Suspense>
    </RouteTitle>
  );
  // IndexRouteObject forbids `path`/`children` — keep the union narrow.
  if (route.index === true) return { index: true, element };
  return { path: route.path, element, children: route.children?.map(toRouteObject) };
}

function NotFound() {
  return (
    <RouteTitle title="Not found">
      <div className="flex h-full flex-col items-center justify-center gap-2 p-6 text-center">
        <h1 className="text-2xl font-semibold">404</h1>
        <p className="text-neutral-400">Page not found.</p>
      </div>
    </RouteTitle>
  );
}

export function buildRouter(): ReturnType<typeof createBrowserRouter> {
  const featureRoutes = collectFeatureRoutes().map(({ route }) => toRouteObject(route));
  return createBrowserRouter([
    {
      element: <AppShell />,
      children: [...featureRoutes, { path: '*', element: <NotFound /> }],
    },
  ]);
}
