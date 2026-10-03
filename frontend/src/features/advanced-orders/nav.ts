import type { FeatureNavItem } from '@/app/manifest';

/**
 * Sidebar entry for the advanced-orders cluster; fee tiers and the
 * workspace cockpit are reachable in-app but not primary nav items.
 */
export const nav: FeatureNavItem[] = [
  {
    label: 'Advanced',
    to: `/advanced/${encodeURIComponent('EUR/USD')}`,
    section: 'Trade',
    order: 35,
  },
];
