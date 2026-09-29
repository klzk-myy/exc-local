/**
 * Validated-field primitives (Task 10.3.29) — `useValidatedField` runs a
 * FieldRule against a raw string value; `<InputField>` renders label +
 * input + inline error + optional help tooltip with the WCAG wiring
 * (aria-invalid, aria-describedby) every surface shares.
 */
import { useId, useMemo, useState, type ChangeEvent, type InputHTMLAttributes } from 'react';

import { validateField, type FieldRule } from './validation';

export interface ValidatedField {
  readonly value: string;
  readonly error: string | null;
  readonly touched: boolean;
  readonly valid: boolean;
  /** Props to spread on the <input>/<select>. */
  readonly inputProps: {
    value: string;
    onChange: (e: ChangeEvent<HTMLInputElement | HTMLSelectElement>) => void;
    onBlur: () => void;
    'aria-invalid': boolean | undefined;
  };
  setValue: (v: string) => void;
  /** Force validation now (e.g. on submit) and return validity. */
  validateNow: () => boolean;
  reset: (v?: string) => void;
}

/** Validate-on-blur / revalidate-on-change field state for one rule. */
export function useValidatedField(rule: FieldRule, initial = ''): ValidatedField {
  const [value, setValue] = useState(initial);
  const [touched, setTouched] = useState(false);
  const error = useMemo(
    () => (touched ? (validateField(rule, value)?.message ?? null) : null),
    [rule, value, touched],
  );
  return {
    value,
    error,
    touched,
    valid: validateField(rule, value) === null,
    inputProps: {
      value,
      onChange: (e) => {
        setValue(e.target.value);
      },
      onBlur: () => {
        setTouched(true);
      },
      'aria-invalid': error !== null || undefined,
    },
    setValue: (v) => {
      setValue(v);
    },
    validateNow: () => {
      setTouched(true);
      return validateField(rule, value) === null;
    },
    reset: (v = '') => {
      setValue(v);
      setTouched(false);
    },
  };
}

export interface InputFieldProps extends Omit<
  InputHTMLAttributes<HTMLInputElement>,
  'value' | 'onChange' | 'id'
> {
  field: ValidatedField;
  label: string;
  /** Optional help tooltip body (Task 10.3.29 item 7 consumer). */
  help?: string;
  /** Optional trailing unit label inside the input ("USD", "lots"). */
  unit?: string;
}

/** Standard labelled input with inline validation error. */
export function InputField({ field, label, help, unit, className, ...rest }: InputFieldProps) {
  const id = useId();
  const errId = `${id}-err`;
  const helpId = `${id}-help`;
  return (
    <div className={`flex flex-col gap-1 ${className ?? ''}`}>
      <div className="flex items-baseline justify-between gap-2">
        <label htmlFor={id} className="text-xs font-medium text-neutral-400">
          {label}
        </label>
        {help ? (
          <span id={helpId} className="text-xs text-neutral-500">
            {help}
          </span>
        ) : null}
      </div>
      <div className="relative">
        <input
          id={id}
          {...rest}
          {...field.inputProps}
          aria-invalid={field.error !== null ? true : undefined}
          aria-describedby={field.error ? errId : help ? helpId : undefined}
          className="w-full rounded border border-neutral-700 bg-neutral-900 px-3 py-1.5 text-sm text-neutral-100 outline-none focus:border-sky-600 disabled:opacity-50"
        />
        {unit ? (
          <span className="pointer-events-none absolute inset-y-0 right-3 flex items-center text-xs text-neutral-500">
            {unit}
          </span>
        ) : null}
      </div>
      {field.error ? (
        <p id={errId} role="alert" className="text-xs text-red-400">
          {field.error}
        </p>
      ) : null}
    </div>
  );
}
