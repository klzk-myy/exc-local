import type { FeatureNavItem } from '@/app/manifest';

export const nav: FeatureNavItem[] = [
  {
    label: 'Calculator',
    to: `/calculator/${encodeURIComponent('EUR/USD')}`,
    section: 'Trade',
    order: 60,
  },
];
