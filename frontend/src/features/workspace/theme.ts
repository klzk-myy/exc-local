/**
 * Light/dark theme preference (Task 10.3.14 item 5) — feature-owned
 * store; the workspace root carries `data-theme` and `theme.css`
 * (imported lazily with the workspace chunk) remaps the neutral palette
 * inside the workspace subtree only, so shared chrome is untouched.
 */
import { create } from 'zustand';
import { persist } from 'zustand/middleware';

export type Theme = 'dark' | 'light';

interface ThemeState {
  theme: Theme;
  setTheme: (t: Theme) => void;
}

export const useTheme = create<ThemeState>()(
  persist(
    (set) => ({
      theme: 'dark',
      setTheme: (theme) => set({ theme }),
    }),
    { name: 'exc.theme.v1' },
  ),
);
