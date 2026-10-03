/**
 * Dead-man switch (Task 10.3.27 item 4) —
 * POST /api/v1/orders/countdown-cancel-all: arm/renew with
 * countdown_ms ∈ [1000, 300000]; 0 disables (routes_v1.go).
 *
 * The route is live (Phase-05/Task-7 mount). Any error still reports
 * honestly inline, and the live countdown only runs while an arm call
 * actually succeeded (never a fake timer).
 */
import { useEffect, useRef, useState } from 'react';
import { useMutation } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ApiError } from '@/lib/api';
import { btnDanger, btnGhost, btnPrimary, cardCls } from '@/lib/ui';
import { formatCountdown, useNow } from '@/lib/ui';
import { InputField, isNotImplemented, useValidatedField } from '@/lib/input-helpers';
import { RULE_COUNTDOWN_MS } from '@/lib/input-helpers/validation';

import { COUNTDOWN_MAX_MS, COUNTDOWN_MIN_MS, armDeadman, disarmDeadman } from './api';

export function DeadmanSwitch() {
  const field = useValidatedField(RULE_COUNTDOWN_MS, '60000');
  const [armedUntil, setArmedUntil] = useState<number | null>(null);
  const [lastErr, setLastErr] = useState<unknown>(null);
  const now = useNow(250);
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  // Live countdown badge — runs only for a genuinely armed window.
  useEffect(() => {
    if (armedUntil === null) return;
    if (now >= armedUntil) {
      setArmedUntil(null);
    }
  }, [now, armedUntil]);
  useEffect(
    () => () => {
      if (timerRef.current !== null) clearInterval(timerRef.current);
    },
    [],
  );

  const arm = useMutation({
    mutationFn: (ms: number) => armDeadman(ms, apiClient),
    onError: setLastErr,
    onSuccess: (_d, ms) => {
      setLastErr(null);
      setArmedUntil(Date.now() + ms);
    },
  });
  const disarm = useMutation({
    mutationFn: () => disarmDeadman(apiClient),
    onError: setLastErr,
    onSuccess: () => {
      setLastErr(null);
      setArmedUntil(null);
    },
  });

  const remaining = armedUntil !== null ? Math.max(0, armedUntil - now) : null;
  const stub = isNotImplemented(lastErr);

  return (
    <section aria-label="Dead-man switch" className={cardCls}>
      <div className="flex items-start justify-between gap-4">
        <div>
          <h2 className="text-base font-semibold text-neutral-100">Dead-man switch</h2>
          <p className="mt-1 max-w-md text-xs text-neutral-500">
            Arms a countdown that cancels ALL open orders unless renewed before expiry (
            {COUNTDOWN_MIN_MS.toLocaleString()}–{COUNTDOWN_MAX_MS.toLocaleString()} ms). Fails
            closed: when the timer lapses the engine purges resting orders.
          </p>
        </div>
        {remaining !== null ? (
          <span
            role="status"
            aria-live="polite"
            className="rounded border border-amber-700 bg-amber-950/40 px-3 py-1 font-mono text-sm text-amber-300"
          >
            armed · {formatCountdown(remaining)}
          </span>
        ) : (
          <span className="rounded border border-neutral-700 px-3 py-1 text-sm text-neutral-500">
            disarmed
          </span>
        )}
      </div>

      <div className="mt-3 flex flex-wrap items-end gap-2">
        <InputField
          field={field}
          label="Countdown (ms)"
          unit="ms"
          inputMode="numeric"
          aria-label="Countdown milliseconds"
        />
        <button
          type="button"
          className={btnPrimary}
          disabled={!field.valid || arm.isPending}
          onClick={() => {
            if (field.validateNow()) arm.mutate(Number(field.value));
          }}
        >
          {armedUntil !== null ? 'Renew' : 'Arm'}
        </button>
        <button
          type="button"
          className={btnGhost}
          disabled={armedUntil === null || disarm.isPending}
          onClick={() => {
            disarm.mutate();
          }}
        >
          Disable
        </button>
        <button
          type="button"
          className={btnDanger}
          title="Arm with the minimum countdown (1s)"
          disabled={!field.valid}
          onClick={() => {
            field.setValue(String(COUNTDOWN_MIN_MS));
          }}
        >
          Min
        </button>
      </div>

      {stub ? (
        <p className="mt-3 rounded border border-amber-800 bg-amber-950/40 p-2 text-xs text-amber-300">
          Dead-man switch reported NOT_IMPLEMENTED (501) server-side — treat this as a regression
          signal (the route is live per Phase-05/Task-7). The countdown above runs only after a
          successful arm call; no orders were armed or cancelled.
        </p>
      ) : lastErr instanceof ApiError ? (
        <p className="mt-3 text-xs text-red-400">
          {lastErr.code}: {lastErr.message}
        </p>
      ) : null}
    </section>
  );
}
