#!/usr/bin/env node
/**
 * check-route-coverage.mjs — Phase-10.5 Task 10.5.3.27.
 *
 * Fails CI when a mounted route from the route registry
 * (docs/openapi/openapi.json — derived, never hand-edited) has no UI
 * surface reference anywhere under src/. A route counts as referenced
 * when a string/template literal in the source spells its path (either
 * with the `/api/v1` prefix or the apiClient-relative suffix);
 * `{param}` segments match `${…}` interpolations and concrete ids.
 * Comments are stripped before matching — a docstring naming a route
 * is documentation, not coverage.
 *
 * Two registries live in route-coverage-exclusions.mjs:
 *   EXCLUSIONS — deliberately headless routes (test scaffolding,
 *     machine-to-machine, ops probes); each carries a reason comment.
 *   DISPATCHES — URL templates built through a typed-union verb segment
 *     (e.g. `/admin/sar/${id}/${action}`). The checker asserts the
 *     template literal exists in src and expands it: any mounted
 *     contract route whose path equals the template shape (each `${…}`
 *     covering exactly one segment) counts as covered.
 *
 * Usage: node scripts/check-route-coverage.mjs [--list-excluded]
 */
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { DISPATCHES, EXCLUSIONS } from './route-coverage-exclusions.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, '..');
const SPEC = resolve(ROOT, '../docs/openapi/openapi.json');
const SRC = resolve(ROOT, 'src');

const doc = JSON.parse(readFileSync(SPEC, 'utf8'));
const paths = doc.paths ?? {};

const files = [];
const walk = (dir) => {
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = resolve(dir, e.name);
    if (e.isDirectory()) {
      // src/test/ fixtures & mocks name routes without being a UI
      // surface — a mocked route is not coverage either.
      if (e.name === 'test' || e.name === '__tests__') continue;
      walk(p);
    } else if (
      /\.(ts|tsx)$/.test(e.name) &&
      !/\.(test|spec)\.(ts|tsx)$/.test(e.name) &&
      !p.includes('/generated/')
    ) {
      files.push(p);
    }
  }
};
walk(SRC);

/**
 * Strip comments while preserving string contents. Block comments go
 * wholesale; line comments only when `//` is not preceded by `:` (so
 * `wss://`, `https://` inside literals survive). A gate, not a parser —
 * the residue risk is a comment-looking literal, never a hidden route.
 */
const stripComments = (src) =>
  src.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/(^|[^:'"`])\/\/[^\n]*/g, '$1');
const corpus = files.map((f) => stripComments(readFileSync(f, 'utf8')));

const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// A `{param}` segment matches either a `${…}` interpolation or any
// non-slash literal run (concrete ids, enum values).
const PARAM_SEG = String.raw`(?:\$\{[^}]*\}|[^/'"'\s$` + '`' + String.raw`{}]+)`;
const INTERP = String.raw`\$\{[^}]*\}`;

/**
 * Literal matcher: contract literal segments must appear verbatim;
 * `{param}` contract segments match interp/concrete runs. Boundary:
 * the char after the path must close the string, start a query or
 * sub-path, open an interpolation (e.g. `${qs({…})}`), or end input.
 */
function pathMatcher(apiPath) {
  const suffix = apiPath.startsWith('/api/v1') ? apiPath.slice(7) : apiPath;
  const body = suffix
    .split('/')
    .map((part) => (part.startsWith('{') && part.endsWith('}') ? PARAM_SEG : esc(part)))
    .join('/');
  // Prefix: bare suffix, full `/api/v1`, or a base-var interpolation
  // (`${base}/security/policy` — base resolves to the api origin).
  return new RegExp(`['"\`](?:/api/v1|\\$\\{[^}]*\\})?${body}(?=['"\`?/$]|$)`);
}

/**
 * Dispatch signature: `/admin/sar/${id}/${action}` → match contract
 * paths of the same shape, each `${…}` covering one segment. Both
 * `/api/v1/...` and suffix forms are accepted for `src`.
 */
function dispatchSignature(template) {
  const suffix = template.startsWith('/api/v1') ? template.slice(7) : template;
  const body = suffix
    .split('/')
    .map((part) => (part.startsWith('${') ? '[^/]+' : esc(part)))
    .join('/');
  return {
    srcRe: new RegExp(
      `['"\`](?:/api/v1)?` +
        suffix
          .split('/')
          .map((p) => (p.startsWith('${') ? INTERP : esc(p)))
          .join('/'),
    ),
    contractRe: new RegExp(`^(?:/api/v1)?${body}$`),
  };
}

// Verify every declared dispatch template exists in the source corpus.
const dispatchMeta = DISPATCHES.map((template) => ({
  template,
  ...dispatchSignature(template),
}));
const missingTemplates = dispatchMeta.filter((d) => !corpus.some((c) => d.srcRe.test(c)));

const uncovered = [];
const stubOnly = [];
let checked = 0;
for (const [apiPath, ops] of Object.entries(paths)) {
  for (const [method, op] of Object.entries(ops)) {
    const key = `${method.toUpperCase()} ${apiPath}`;
    if (isExcluded(key)) continue;
    if (op?.['x-status'] === 'stub' || op?.status === 'stub') {
      stubOnly.push(key);
      continue;
    }
    checked++;
    const re = pathMatcher(apiPath);
    if (corpus.some((text) => re.test(text))) continue;
    if (dispatchMeta.some((d) => d.contractRe.test(apiPath))) continue;
    uncovered.push(key);
  }
}

// Exclusion keys support `*` for the method and a trailing `*` or
// mid-path `*` glob segment (`* /api/v1/test/*`).
function isExcluded(key) {
  return Object.keys(EXCLUSIONS).some((pat) => {
    const [pm, pp] = pat.split(' ');
    const [m, p] = key.split(' ');
    if (pm !== '*' && pm !== m) return false;
    // A trailing `*` matches the whole sub-tree (`/test/*` ⇒ one or
    // more segments); an interior `*` matches exactly one segment.
    const segs = pp.split('/');
    const trailingGlob = segs[segs.length - 1] === '*';
    const parts = segs.map((s, i) =>
      s === '*' ? (trailingGlob && i === segs.length - 1 ? '.+' : '[^/]+') : esc(s),
    );
    return new RegExp(`^${parts.join('/')}$`).test(p);
  });
}

// Stale registry entries are drift too — flag exclusions whose route
// vanished or gained coverage, and dispatch templates whose route no
// longer exists or is now literally referenced.
const stale = [];
for (const key of Object.keys(EXCLUSIONS)) {
  const [m, p] = key.split(' ');
  const mounted = m === '*' ? paths[p] !== undefined : paths[p]?.[m.toLowerCase()] !== undefined;
  const globbed = p.includes('*') || m === '*';
  if (!globbed && !mounted) stale.push(`EXCLUSION ${key} (route gone from registry)`);
  if (!globbed && mounted && corpus.some((text) => pathMatcher(p).test(text))) {
    stale.push(`EXCLUSION ${key} (now referenced — drop the exclusion)`);
  }
}
for (const d of missingTemplates) {
  stale.push(`DISPATCH ${d.template} (template not found in src)`);
}

if (process.argv.includes('--list-excluded')) {
  for (const k of Object.keys(EXCLUSIONS)) console.log(`excluded: ${k}`);
  for (const d of DISPATCHES) console.log(`dispatch: ${d}`);
  for (const k of stubOnly) console.log(`stub (not gated): ${k}`);
}

if (uncovered.length > 0 || stale.length > 0) {
  for (const k of uncovered) console.error(`UNCOVERED  ${k}`);
  for (const k of stale) console.error(`STALE  ${k}`);
  console.error(
    `\nroute-coverage: ${uncovered.length} mounted route(s) without a UI surface reference` +
      ` (${checked} checked, ${Object.keys(EXCLUSIONS).length} exclusions, ` +
      `${DISPATCHES.length} dispatch templates, ${stubOnly.length} stub)`,
  );
  process.exit(1);
}
console.log(
  `route-coverage: OK — ${checked} live routes referenced, ` +
    `${Object.keys(EXCLUSIONS).length} exclusions, ${DISPATCHES.length} dispatch templates, ` +
    `${stubOnly.length} stub`,
);
