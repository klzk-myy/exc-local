/**
 * useNow — re-render tick for countdown surfaces (withdrawal 15-minute
 * confirmation window, ticket SLA timers, cooling-off remaining time).
 * The interval stops ticking while the component is unmounted.
 */
import { useEffect, useState } from 'react';

export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = window.setInterval(() => {
      setNow(Date.now());
    }, intervalMs);
    return () => {
      window.clearInterval(t);
    };
  }, [intervalMs]);
  return now;
}

/** mm:ss (or h:mm:ss beyond 60min) countdown text; clamps at zero. */
export function formatCountdown(msLeft: number): string {
  const totalSec = Math.max(0, Math.floor(msLeft / 1000));
  const h = Math.floor(totalSec / 3600);
  const m = Math.floor((totalSec % 3600) / 60);
  const s = totalSec % 60;
  const mm = String(m).padStart(2, '0');
  const ss = String(s).padStart(2, '0');
  return h > 0 ? `${String(h)}:${mm}:${ss}` : `${mm}:${ss}`;
}

/** Business-day-aware human duration for SLA copy ("8 business hours"). */
export function formatDurationMs(ms: number): string {
  const totalMin = Math.floor(ms / 60000);
  if (totalMin < 60) return `${String(totalMin)}m`;
  const h = Math.floor(totalMin / 60);
  if (h < 48) return `${String(h)}h ${String(totalMin % 60)}m`;
  const d = Math.floor(h / 24);
  return `${String(d)}d ${String(h % 24)}h`;
}
