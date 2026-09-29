import type { FeatureNavItem } from '@/app/manifest';

export const nav: FeatureNavItem[] = [
  { label: 'Book', to: `/book/${encodeURIComponent('EUR/USD')}`, section: 'Trade', order: 20 },
];
