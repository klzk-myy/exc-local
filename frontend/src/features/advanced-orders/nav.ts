import type { FeatureNavItem } from '@/app/manifest';

/**
 * Workspace is the primary cockpit; these are the power-user /
 * admin deep links for the advanced-orders cluster.
 */
export const nav: FeatureNavItem[] = [
  {
    label: 'Advanced',
    to: `/advanced/${encodeURIComponent('EUR/USD')}`,
    section: 'Trade',
    order: 30,
  },
  { label: 'Fee tiers', to: '/fee-tiers', section: 'Account', order: 50 },
];
