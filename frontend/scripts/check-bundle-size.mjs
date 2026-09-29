#!/usr/bin/env node
/**
 * Bundle budget gate (Phase-10 Task 10.3.1 item 7 — size-limit equivalent).
 *
 * "Initial JS payload" = the index.html entry chunk plus its full
 * static-import closure, resolved from dist/.vite/manifest.json (emitted by
 * `vite build` with `build.manifest: true`). Route-level lazy chunks loaded
 * through `import.meta.glob`/`React.lazy` are dynamic imports and therefore
 * excluded — TradingView Lightweight Charts, the backtester, and feature
 * pages must stay lazy so the initial payload stays under the 300 kB
 * gzipped budget.
 *
 * Exits non-zero on breach → wired into `npm run build` so the gate fails
 * the build, per the task's acceptance requirement.
 */
import { gzipSync } from 'node:zlib';
import { readFileSync, readdirSync, existsSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const LIMIT_BYTES = 300 * 1024; // 300 kB gzipped (spec-pinned budget)
const root = dirname(dirname(fileURLToPath(import.meta.url)));
const distDir = join(root, 'dist');
const manifestPath = join(distDir, '.vite', 'manifest.json');

if (!existsSync(manifestPath)) {
  console.error('bundle-size: dist/.vite/manifest.json missing — run `vite build` first');
  process.exit(1);
}

/** @type {Record<string, {file: string, isEntry?: boolean, imports?: string[], css?: string[]}>} */
const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));

/** Collect the entry chunk + transitive *static* imports (dynamicImports excluded). */
const initial = new Set();
const stack = Object.keys(manifest).filter((k) => manifest[k]?.isEntry);
if (stack.length === 0) {
  console.error('bundle-size: no entry chunk found in manifest');
  process.exit(1);
}
while (stack.length > 0) {
  const key = stack.pop();
  if (!key || initial.has(key)) continue;
  initial.add(key);
  for (const dep of manifest[key]?.imports ?? []) stack.push(dep);
}

let total = 0;
const rows = [];
for (const key of initial) {
  const file = manifest[key]?.file;
  if (!file || !file.endsWith('.js')) continue;
  const bytes = readFileSync(join(distDir, file));
  const gz = gzipSync(bytes).length;
  total += gz;
  rows.push({ file, gz });
}

// If manifest resolution somehow found no JS, fall back to measuring every
// non-lazy-looking entry file directly (defensive — fails closed on drift).
if (rows.length === 0) {
  for (const f of readdirSync(join(distDir, 'assets'))) {
    if (!f.endsWith('.js')) continue;
    const gz = gzipSync(readFileSync(join(distDir, 'assets', f))).length;
    total += gz;
    rows.push({ file: `assets/${f}`, gz });
  }
}

const fmt = (n) => `${(n / 1024).toFixed(1)} kB`;
for (const r of rows.sort((a, b) => b.gz - a.gz)) {
  console.log(`  ${fmt(r.gz).padStart(10)}  ${r.file}`);
}
console.log(`initial JS: ${fmt(total)} gzipped / budget ${fmt(LIMIT_BYTES)}`);

if (total > LIMIT_BYTES) {
  console.error('bundle-size: BUDGET BREACH — initial JS exceeds 300 kB gzipped');
  process.exit(1);
}
console.log('bundle-size: OK');
