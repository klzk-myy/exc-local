import type { FeatureNavItem } from '@/app/manifest';

export const nav: FeatureNavItem[] = [
  { label: 'Chart', to: `/chart/${encodeURIComponent('EUR/USD')}`, section: 'Trade', order: 30 },
];
