/**
 * axe-core audit helper — Phase-10 Task 10.3.14 / §24 a11y checkpoints.
 *
 * jest-axe wraps axe-core; `configureAxe` splits options into
 * `globalOptions` (axe.configure) and the rest (axe.run). We pin the
 * ruleset to the WCAG 2.1 AA conformance tags per the Task 10.3.14
 * baseline.
 *
 * jsdom caveat: rules that need real layout/style computation (notably
 * `color-contrast`) cannot evaluate under jsdom — axe reports them as
 * `incomplete`, not `violations`. `auditA11y` surfaces the incomplete
 * list in the failure message so genuinely unevaluable rules are
 * visible in CI output instead of silently passing.
 *
 * Tag caveat: in axe-core the literal `wcag21aa` tag selects only the
 * handful of rules added *by* WCAG 2.1 AA — not the full conformance
 * set. WCAG 2.1 AA = 2.0 A + 2.0 AA + 2.1 A + 2.1 AA, so the ruleset
 * below is the union of those four tags (same convention as
 * playwright's AxeBuilder.withTags WCAG 2.1 AA docs).
 */
import { configureAxe } from 'jest-axe';
import type { Result } from 'axe-core';
import { expect } from 'vitest';

export const axe = configureAxe({
  runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'] },
});

function formatNode(r: Result): string {
  return r.nodes
    .map((n) => `    ${n.target.join(' ')}\n      ${n.failureSummary ?? ''}`)
    .join('\n');
}

export function formatViolations(violations: Result[]): string {
  return violations
    .map((v) => `[${v.impact ?? 'unknown'}] ${v.id} — ${v.help}\n${formatNode(v)}`)
    .join('\n\n');
}

export function formatIncomplete(incomplete: Result[]): string {
  return incomplete.map((r) => `${r.id} (${r.nodes.length} node(s) need review)`).join(', ');
}

/**
 * Runs axe (WCAG 2.1 AA) over a rendered container and asserts zero
 * violations. Returns the incomplete-rule summary for reporting.
 */
export async function expectNoAxeViolations(container: Element): Promise<string> {
  const results = await axe(container);
  const incompleteNote = formatIncomplete(results.incomplete);
  expect(
    results.violations,
    `axe-core wcag21aa violations:\n${formatViolations(results.violations)}`,
  ).toEqual([]);
  return incompleteNote;
}
