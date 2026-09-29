import type { FeatureNavItem } from '@/app/manifest';

/** Nav manifest — optional per feature. `section` groups items in the
 * shell's primary nav; `order` sorts within the section. */
export const nav: FeatureNavItem[] = [{ label: 'Dashboard', to: '/', section: 'Trade', order: 0 }];
