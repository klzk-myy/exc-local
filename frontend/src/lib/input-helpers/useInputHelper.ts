/**
 * `useInputHelper(route)` — the Task 10.3.29 entry point binding a form
 * to its generated route contract + bound field rules.
 *
 *   const h = useInputHelper('POST /api/v1/orders');
 *   h.contract            // generated RouteContract (or undefined)
 *   h.fields              // bound FieldRule[] for the route's body
 *   h.validate(values)    // FieldError[] — per-field + cross-field rules
 *   h.validateField(name, raw)
 *   h.errors              // last validate() result (state)
 *   h.isValid             // errors.length === 0 after validate()
 *   h.isStub              // registry hint (501 NOT_IMPLEMENTED expected)
 *   h.pathErrors(pathValues)  // path-param validation ("/{id}" segments)
 *
 * Forms in Tasks 10.3.3/10.3.7/10.3.21–10.3.28 consume this hook instead
 * of inline rules (per the task amendments).
 */
import { useCallback, useMemo, useState } from 'react';

import { routeContract, type RouteContract } from './generated/route-contracts';
import {
  ROUTE_FIELD_BINDINGS,
  validateField as runFieldRule,
  validateOrderSubmit,
  validateRecord,
  type FieldError,
  type FieldRule,
  type FieldValues,
} from './validation';

export interface InputHelper {
  readonly route: string;
  readonly contract: RouteContract | undefined;
  /** True when the route is registered in the OpenAPI document. */
  readonly registered: boolean;
  /** Registry hint that the handler is a stub (501 until its owning
   * phase lands). Runtime 501 detection is authoritative — this is for
   * badges/UX, never for refusing the call. */
  readonly isStub: boolean;
  /** True when the route requires the §8.10 dual-control flow. */
  readonly dualControl: boolean;
  readonly fields: readonly FieldRule[];
  readonly errors: readonly FieldError[];
  readonly isValid: boolean;
  readonly errorFor: (name: string) => string | null;
  readonly validateField: (name: string, raw: string | undefined) => FieldError | null;
  /** Validate every bound field plus route-level cross-field rules
   * (order submit carries the §22.1 relational rules). Updates `errors`. */
  readonly validate: (values: FieldValues) => FieldError[];
  /** Validate path-parameter substitutions for `{param}` segments. */
  readonly pathErrors: (pathValues: Record<string, string>) => FieldError[];
}

const ORDER_ROUTES = new Set(['POST /api/v1/orders', 'POST /api/v1/orders/test']);

export function useInputHelper(route: string): InputHelper {
  const contract = useMemo(() => routeContract(route), [route]);
  const fields = useMemo(() => ROUTE_FIELD_BINDINGS[route] ?? [], [route]);
  const [errors, setErrors] = useState<readonly FieldError[]>([]);

  const validate = useCallback(
    (values: FieldValues): FieldError[] => {
      const errs = validateRecord(fields, values);
      if (ORDER_ROUTES.has(route)) errs.push(...validateOrderSubmit(values));
      setErrors(errs);
      return errs;
    },
    [fields, route],
  );

  const validateField = useCallback(
    (name: string, raw: string | undefined): FieldError | null => {
      const rule = fields.find((f) => f.name === name);
      return rule ? runFieldRule(rule, raw) : null;
    },
    [fields],
  );

  const errorFor = useCallback(
    (name: string): string | null => errors.find((e) => e.field === name)?.message ?? null,
    [errors],
  );

  const pathErrors = useCallback(
    (pathValues: Record<string, string>): FieldError[] => {
      if (!contract) return [];
      const errs: FieldError[] = [];
      for (const p of contract.params) {
        if (p.in !== 'path') continue;
        const raw = pathValues[p.name];
        const value = raw?.trim() ?? '';
        if (p.required && value === '') {
          errs.push({ field: p.name, message: `${p.name} is required` });
          continue;
        }
        if (value === '') continue;
        if (p.enum && !p.enum.includes(value)) {
          errs.push({ field: p.name, message: `${p.name} must be one of ${p.enum.join(', ')}` });
        }
        if (p.type === 'integer' && !/^[+-]?\d+$/.test(value)) {
          errs.push({ field: p.name, message: `${p.name} must be an integer` });
        }
        if (p.type === 'number' && Number.isNaN(Number(value))) {
          errs.push({ field: p.name, message: `${p.name} must be a number` });
        }
      }
      return errs;
    },
    [contract],
  );

  return {
    route,
    contract,
    registered: contract !== undefined,
    isStub: contract?.status === 'stub',
    dualControl: contract?.dualControl === true,
    fields,
    errors,
    isValid: errors.length === 0,
    errorFor,
    validateField,
    validate,
    pathErrors,
  };
}
