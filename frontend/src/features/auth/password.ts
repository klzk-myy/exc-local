/**
 * Password-strength heuristic for the registration form — kept out of
 * RegisterPage.tsx so that module stays component-only for react-refresh.
 */

/** 0–4 heuristic strength score (display only — the server enforces the
 * real policy). Longer + mixed classes score higher. */
export function passwordStrength(pw: string): { score: number; label: string } {
  let score = 0;
  if (pw.length >= 12) score += 1;
  if (pw.length >= 8) score += 1;
  if (/[a-z]/.test(pw) && /[A-Z]/.test(pw)) score += 1;
  if (/\d/.test(pw)) score += 1;
  if (/[^A-Za-z0-9]/.test(pw)) score += 1;
  score = Math.min(4, score);
  const labels = ['Very weak', 'Weak', 'Fair', 'Strong', 'Very strong'] as const;
  return { score, label: labels[score] ?? 'Very weak' };
}
