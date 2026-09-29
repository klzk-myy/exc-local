import type { FeatureNavItem } from '@/app/manifest';

export const nav: FeatureNavItem[] = [
  { label: 'Trade', to: `/trade/${encodeURIComponent('EUR/USD')}`, section: 'Trade', order: 10 },
];
