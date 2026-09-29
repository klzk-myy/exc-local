/**
 * Field — labeled form control wrapper with per-field error surfacing
 * (`aria-invalid` + `aria-describedby`, Task 10.3.14 WCAG 2.1 AA). Works
 * with any control via `children` render or the `fieldId` contract.
 */
import { useId, type ReactNode } from 'react';

import { errorTextCls, hintTextCls, labelCls } from './classes';

export interface FieldProps {
  label: string;
  error?: string | null;
  hint?: string;
  required?: boolean;
  children: (id: string, describedBy: string | undefined, invalid: boolean) => ReactNode;
}

export function Field({ label, error, hint, required, children }: FieldProps) {
  const id = useId();
  const errId = `${id}-err`;
  const hintId = `${id}-hint`;
  const describedBy = [error ? errId : null, hint ? hintId : null]
    .filter((v): v is string => v !== null)
    .join(' ');
  return (
    <div className="mb-4">
      <label htmlFor={id} className={labelCls}>
        {label}
        {required === true && (
          <span className="ml-1 text-red-400" aria-hidden="true">
            *
          </span>
        )}
      </label>
      {children(id, describedBy === '' ? undefined : describedBy, error != null)}
      {error != null && (
        <p id={errId} className={errorTextCls} role="alert">
          {error}
        </p>
      )}
      {hint !== undefined && (
        <p id={hintId} className={hintTextCls}>
          {hint}
        </p>
      )}
    </div>
  );
}
