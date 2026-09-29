/**
 * /logout — revokes the server session then clears local state (Task
 * 10.3.21: "logout clears all local state" — session store, persisted
 * tokens, and the React Query cache).
 */
import { useEffect } from 'react';
import { useNavigate } from 'react-router';
import { useQueryClient } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';

import * as api from './api';

export default function LogoutPage() {
  const navigate = useNavigate();
  const qc = useQueryClient();
  useEffect(() => {
    void api
      .logout(apiClient)
      .catch(() => undefined)
      .then(() => {
        qc.clear();
        void navigate('/login', { replace: true });
      });
  }, [navigate, qc]);
  return (
    <div className="flex h-64 items-center justify-center text-neutral-400" role="status">
      Signing out…
    </div>
  );
}
