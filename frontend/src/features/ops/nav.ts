import type { FeatureNavItem } from '@/app/manifest';

export const nav: FeatureNavItem[] = [
  { label: 'Ops Board', to: '/ops', section: 'Admin', order: 1 },
  { label: 'Fleet', to: '/ops/fleet', section: 'Admin', order: 2 },
  { label: 'Releases', to: '/ops/releases', section: 'Admin', order: 3 },
];
