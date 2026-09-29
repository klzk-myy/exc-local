import { lazy } from 'react';

import type { FeatureRoute } from '@/app/manifest';

const LoginPage = lazy(() => import('./LoginPage'));
const RegisterPage = lazy(() => import('./RegisterPage'));
const ForgotPassword = lazy(() =>
  import('./ForgotPasswordPage').then((m) => ({ default: m.ForgotPasswordPage })),
);
const ResetPassword = lazy(() =>
  import('./ForgotPasswordPage').then((m) => ({ default: m.ResetPasswordPage })),
);
const LogoutPage = lazy(() => import('./LogoutPage'));
const SessionsPage = lazy(() => import('./SessionList'));

export const routes: FeatureRoute[] = [
  { path: 'login', element: LoginPage, title: 'Sign in' },
  { path: 'register', element: RegisterPage, title: 'Create account' },
  { path: 'forgot-password', element: ForgotPassword, title: 'Reset password' },
  { path: 'reset-password', element: ResetPassword, title: 'New password' },
  { path: 'logout', element: LogoutPage, title: 'Sign out' },
  { path: 'account/sessions', element: SessionsPage, title: 'Sessions' },
];
