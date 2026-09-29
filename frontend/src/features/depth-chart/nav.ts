import type { FeatureNavItem } from '@/app/manifest';

export const nav: FeatureNavItem[] = [
  { label: 'Depth', to: `/depth/${encodeURIComponent('EUR/USD')}`, section: 'Trade', order: 25 },
];
