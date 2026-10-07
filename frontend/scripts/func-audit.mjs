/**
 * Functional-completeness audit crawler (distinct from the rendered a11y audit).
 *
 * For every authed route, exercised as a real user in Chromium:
 *   A. enumerate all interactive controls (buttons/tabs/links/selects/inputs)
 *      — classify disabled vs enabled, accessible name
 *   B. probe SAFE controls (tabs, dropdowns, open/create/filter/pagination/
 *      refresh toggles) — a click is observed via MutationObserver count,
 *      URL change, new network requests, dialog/popover appearing, or toast.
 *      Enabled + clicked + zero observable effect ⇒ dead-control candidate.
 *   C. stub detection — "coming soon"/"not implemented"/placeholder copy,
 *      error boundaries, stuck loading states, empty lists vs seeded data.
 *   D. HTTP layer — every response ≥400 recorded with route attribution;
 *      request count per page (coverage proxy).
 *
 * Real end-to-end flows live in func-flows.mjs (order ticket, support
 * ticket, webhook, settings persistence, sessions).
 *
 * Usage: node scripts/func-audit.mjs [--only route-substr] [--base URL]
 */
import { mkdirSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';

const require = createRequire(new URL('../package.json', import.meta.url));
const { chromium } = require('playwright');

const BASE = process.env.AUDIT_BASE ?? 'http://localhost:3000';
const HEADFUL = !!process.env.AUDIT_HEADFUL; // headed browser (needs DISPLAY)
const OUT = '/tmp/func-audit';
const ONLY = (() => {
  const i = process.argv.indexOf('--only');
  return i > -1 ? process.argv[i + 1] : null;
})();
const TAKER = { email: 'e2e.taker@example.com', password: 'E2e-passphrase-9' };
const SYM = 'EUR%2FUSD';

const ROUTES = [
  ['home', '/'],
  ['workspace', '/workspace'],
  ['trade', `/trade/${SYM}`],
  ['advanced', `/advanced/${SYM}`],
  ['chart', `/chart/${SYM}`],
  ['depth', `/depth/${SYM}`],
  ['book', `/book/${SYM}`],
  ['calculator', '/calculator'],
  ['orders', '/orders'],
  ['portfolio', '/portfolio'],
  ['performance', '/performance'],
  ['funding', '/funding'],
  ['kyc', '/kyc'],
  ['settings', '/settings'],
  ['sessions', '/account/sessions'],
  ['notifications', '/notifications'],
  ['support', '/support'],
  ['webhooks', '/webhooks'],
  ['pamm', '/pamm'],
  ['copy-grid', '/copy-grid'],
  ['backtest', '/backtest'],
  ['discovery', '/discovery'],
  ['fee-tiers', '/fee-tiers'],
  ['reports', '/reports'],
  ['analytics', '/analytics'],
  ['explorer', '/explorer'],
  ['transparency', '/transparency'],
  ['status', '/status'],
  ['security', '/security'],
  ['admin', '/admin'],
  ['admin-compliance', '/admin/compliance'],
  ['admin-conduct', '/admin/conduct'],
  ['admin-content', '/admin/content'],
  ['admin-customers', '/admin/customers'],
  ['admin-finance', '/admin/finance'],
  ['admin-funding-ops', '/admin/funding-ops'],
  ['admin-instruments', '/admin/instrument-governance'],
  ['admin-integrity', '/admin/integrity'],
  ['admin-mm', '/admin/market-making'],
  ['admin-ops-safety', '/admin/ops-safety'],
  ['admin-regreport', '/admin/regreporting'],
  ['admin-settlement', '/admin/settlement'],
  ['admin-surveillance', '/admin/surveillance'],
  ['admin-treasury', '/admin/treasury'],
  ['admin-venue', '/admin/venue'],
  ['ops', '/ops'],
  ['ops-fleet', '/ops/fleet'],
  ['ops-releases', '/ops/releases'],
];

// Controls we never click — destructive/irreversible/flow-breaking.
const SKIP_CLICK =
  /delete|revoke|remove|withdraw|close account|cancel all|kill|freeze|disable|reject|terminate|log ?out|sign ?out|unlink|reset|suspend|ban|halt|emergency|2fa|mfa|logout|destroy|purge|confirm.*(cancel|close|delete)|place order|submit|buy|sell|invest|redeem|pause|resume/i;
// We DO click these despite matching keywords above when clearly safe dialogs:
const SAFE_OVERRIDE =
  /^(new|create|add|open|filter|view|details|more|refresh|edit|columns|export|csv|download|help|info|customi[sz]e|layout|compare|share)/i;

const TOLERATED = [
  /^\/api\/v1\/deposits\/[A-Z]{3}$/,
  /\/api\/v1\/support\/tickets\/\d+$/, // seed ticket may not exist post-reset
  // Deterministic coded degradations under the dev seed — the panels
  // render the error verbatim (ErrorBox), so these are contract-correct
  // responses, not swallowed failures:
  /\/api\/v1\/market\/positioning/, // 422 INSUFFICIENT_COHORT — <100-account anonymity floor
  /\/api\/v1\/analytics\/long-short-ratio\//, // same anonymity floor
  /\/api\/v1\/market-data\/l3-snapshot\//, // 503 — L3 professional feed absent in dev seed
];

const ENUM_JS = `(() => {
  const q = (s) => [...document.querySelectorAll(s)];
  const vis = (el) => {
    const cs = getComputedStyle(el);
    const r = el.getBoundingClientRect();
    return cs.display !== 'none' && cs.visibility !== 'hidden' && r.width > 0 && r.height > 0;
  };
  const name = (el) =>
    (el.getAttribute('aria-label') || '').trim() ||
    (el.innerText || el.textContent || '').trim().slice(0, 80) ||
    (el.getAttribute('title') || '').trim() ||
    (el.getAttribute('placeholder') || '').trim();
  const path = (el) => {
    const parts = [];
    let n = el;
    while (n && n !== document.body && parts.length < 4) {
      const id = n.id ? '#' + n.id : '';
      const cls = typeof n.className === 'string' && n.className.trim()
        ? '.' + n.className.trim().split(/\\s+/).slice(0, 2).join('.') : '';
      parts.unshift(n.tagName.toLowerCase() + id + cls);
      n = n.parentElement;
    }
    return parts.join('>');
  };
  const controls = [];
  const byPath = new Map();
  for (const el of q('button,[role=button],[role=tab],a[href],select,input:not([type=hidden]),[role=switch],[role=combobox]')) {
    if (!vis(el)) continue;
    const p = path(el);
    const idx = byPath.get(p) || 0;
    byPath.set(p, idx + 1);
    controls.push({
      tag: el.tagName.toLowerCase(),
      role: el.getAttribute('role') || el.tagName.toLowerCase(),
      type: el.getAttribute('type') || '',
      name: name(el),
      disabled: el.disabled || el.getAttribute('aria-disabled') === 'true' || el.hasAttribute('disabled'),
      href: el.getAttribute('href') || null,
      selected: el.getAttribute('aria-selected') || el.getAttribute('aria-checked') || null,
      path: p,
      idx,
      tablist: !!el.closest('[role=tablist]'),
    });
  }
  // de-dup identical paths
  const seen = new Set();
  const out = [];
  for (const c of controls) {
    const k = c.path + '|' + c.name;
    if (seen.has(k)) continue;
    seen.add(k);
    out.push(c);
  }
  // stub/empty signals
  const txt = document.body.innerText || '';
  const stubs = [];
  for (const m of txt.matchAll(/(coming soon|not implemented|not available|under construction|placeholder|n\\/a required|feature unavailable|unsupported|temporarily unavailable)/gi))
    stubs.push(m[0]);
  const loading = /Loading…|Loading\\.\\.\\.|loading\\.\\.\\./.test(txt);
  const emptyStates = q('[class*=empty],[data-empty],[role=status]')
    .filter(vis).map((e) => (e.innerText || '').trim().slice(0, 120)).filter(Boolean).slice(0, 6);
  const errBoundary = /something went wrong|unexpected error|error boundary/i.test(txt);
  return { controls: out, stubs: [...new Set(stubs)], loading, emptyStates, errBoundary,
           mainText: txt.slice(0, 200) };
})()`;

async function uiLogin(page, creds) {
  await page.goto('/login', { waitUntil: 'domcontentloaded' });
  await page.getByLabel('Email').fill(creds.email);
  await page.getByLabel('Password').fill(creds.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL((u) => !u.pathname.includes('/login'), { timeout: 15000 });
}

function watch(page, rec) {
  const onPE = (e) => rec.pageErrors.push(String(e).slice(0, 250));
  const onCon = (m) => {
    if (m.type() === 'error' && !m.text().startsWith('Failed to load resource'))
      rec.consoleErrors.push(m.text().slice(0, 250));
  };
  const onRes = (res) => {
    const s = res.status();
    if (s < 400) return;
    const p = new URL(res.url()).pathname;
    if (TOLERATED.some((re) => re.test(p))) return;
    rec.httpErrors.push(`${s} ${p}${new URL(res.url()).search}`.slice(0, 160));
  };
  const onReq = (req) => {
    if (req.url().includes('/api/'))
      rec.apiCalls.push(req.method() + ' ' + new URL(req.url()).pathname);
  };
  page.on('pageerror', onPE);
  page.on('console', onCon);
  page.on('response', onRes);
  page.on('request', onReq);
  return () => {
    page.off('pageerror', onPE);
    page.off('console', onCon);
    page.off('response', onRes);
    page.off('request', onReq);
  };
}

async function probeControls(page, rec) {
  // Install a DOM mutation counter.
  await page.evaluate(`(() => {
    window.__mut = 0;
    if (window.__obs) window.__obs.disconnect();
    window.__obs = new MutationObserver((l) => { window.__mut += l.length; });
    window.__obs.observe(document.body, { childList: true, subtree: true, attributes: true, characterData: true });
  })()`);

  const controls = rec.enum.controls;
  const clickable = controls
    .filter((c) => {
      if (c.disabled) return false;
      if (c.tag === 'input' || c.tag === 'select') return false; // probed separately
      if (c.tag === 'a' && c.href && (c.href.startsWith('http') || c.href === '#')) return false;
      const label = c.name;
      if (SKIP_CLICK.test(label) && !SAFE_OVERRIDE.test(label)) {
        rec.skipped.push(label);
        return false;
      }
      return true;
    })
    .slice(0, 40); // cap per page

  for (const c of clickable) {
    const sel = c.path;
    const before = {
      url: page.url(),
      mut: await page.evaluate('window.__mut'),
      reqN: rec.apiCalls.length,
    };
    let outcome = null;
    try {
      const loc = page.locator(sel).nth(c.idx || 0);
      if (!(await loc.isVisible({ timeout: 800 }).catch(() => false))) {
        outcome = 'not-visible-at-click';
        continue;
      }
      const stateBefore = await loc
        .evaluate(
          (e) =>
            (e.getAttribute('aria-pressed') || '') +
            '|' +
            (e.getAttribute('aria-selected') || '') +
            '|' +
            e.className,
        )
        .catch(() => '');
      await loc.click({ timeout: 1500 });
      await page.waitForTimeout(450);
      const after = {
        url: page.url(),
        mut: await page.evaluate('window.__mut'),
        reqN: rec.apiCalls.length,
      };
      const stateAfter = await page
        .locator(sel)
        .nth(c.idx || 0)
        .evaluate(
          (e) =>
            (e.getAttribute('aria-pressed') || '') +
            '|' +
            (e.getAttribute('aria-selected') || '') +
            '|' +
            e.className,
        )
        .catch(() => '');
      const dialog = await page
        .locator(
          '[role=dialog],[role=menu],[role=listbox],[role=alertdialog],[popover],.toast,[role=status] >> visible=true',
        )
        .count()
        .catch(() => 0);
      const nav = after.url !== before.url;
      const dmut = after.mut - before.mut;
      const dreq = after.reqN - before.reqN;
      if (nav) outcome = `nav->${new URL(after.url).pathname}`;
      else if (dreq > 0) outcome = `requests:${dreq}`;
      else if (dialog > 0) outcome = 'dialog/menu';
      else if (stateAfter !== stateBefore) outcome = 'state-flip';
      else if (dmut > 2) outcome = `dom:${dmut}`;
      else outcome = 'DEAD';
      // Escape any open overlay; navigate back if we left the page.
      await page.keyboard.press('Escape').catch(() => undefined);
      if (nav) {
        await page.goBack({ waitUntil: 'domcontentloaded' }).catch(() => undefined);
        await page.waitForTimeout(300);
      }
    } catch (e) {
      outcome = 'click-failed:' + String(e).slice(0, 60);
    }
    rec.probes.push({ name: c.name || '(unnamed)', role: c.role, sel: sel.slice(-80), outcome });
  }
}

async function audit(page, name, url) {
  const rec = {
    name,
    url,
    pageErrors: [],
    consoleErrors: [],
    httpErrors: [],
    apiCalls: [],
    probes: [],
    skipped: [],
    enum: null,
  };
  const detach = watch(page, rec);
  try {
    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 20000 });
    await page.waitForTimeout(2200); // let queries settle
    rec.enum = await page.evaluate(ENUM_JS).catch((e) => ({ evalError: String(e).slice(0, 150) }));
    await probeControls(page, rec);
    // Re-enumerate stubs after probing (probing may reveal content)
    rec.final = await page.evaluate(ENUM_JS).catch(() => null);
  } catch (e) {
    rec.fatal = String(e).slice(0, 200);
  }
  detach();
  return rec;
}

async function main() {
  mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch({ headless: !HEADFUL });
  const ctx = await browser.newContext({ baseURL: BASE, viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  await uiLogin(page, TAKER);
  await page.evaluate(() =>
    localStorage.setItem('exc.ui-mode.v1', JSON.stringify({ state: { mode: 'pro' }, version: 0 })),
  );

  const results = [];
  for (const [name, url] of ROUTES) {
    if (ONLY && !name.includes(ONLY)) continue;
    const rec = await audit(page, name, url);
    results.push(rec);
    const dead = rec.probes.filter((p) => p.outcome === 'DEAD').length;
    console.log(
      `${name}: ctrl=${rec.enum?.controls?.length ?? '?'} probed=${rec.probes.length} dead=${dead} httpErr=${rec.httpErrors.length} jsErr=${rec.pageErrors.length + rec.consoleErrors.length} stubs=${(rec.enum?.stubs || []).join(',') || '-'} ${rec.fatal ?? ''}`,
    );
  }
  writeFileSync(`${OUT}/results.json`, JSON.stringify(results, null, 2));
  await browser.close();
  console.log(`DONE → ${OUT}/results.json`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
