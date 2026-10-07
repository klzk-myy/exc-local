/**
 * Full-site rendered crawl audit for the Trader UI (docker nginx :3000).
 *
 * Crawls every feature route with a real Chromium renderer, captures:
 *   - console errors / pageerrors / failed HTTP responses
 *   - axe-core WCAG 2.x A/AA + best-practice violations
 *   - DOM a11y checks (labels, names, headings, landmarks, alt, tabindex)
 *   - horizontal overflow + offending elements
 *   - touch targets < 44px (mobile pass)
 *   - nav timing + resource stats
 *   - full-page screenshots
 *
 * Usage:
 *   node scripts/render-audit.mjs desktop   # authed taker, dark, 1440x900
 *   node scripts/render-audit.mjs mobile    # authed taker, dark, 390x844
 *   node scripts/render-audit.mjs light     # authed taker, light theme, key pages
 *   node scripts/render-audit.mjs public    # unauthenticated auth pages + guard check
 */
import { readFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const require = createRequire(new URL('../package.json', import.meta.url));
const { chromium } = require('playwright');
const AXE = readFileSync(
  fileURLToPath(new URL('../node_modules/axe-core/axe.min.js', import.meta.url)),
  'utf8',
);

const BASE = process.env.AUDIT_BASE ?? 'http://localhost:3000';
const HEADFUL = !!process.env.AUDIT_HEADFUL; // headed browser (needs DISPLAY)
const OUT = '/tmp/render-audit';
const MODE = process.argv[2] ?? 'desktop';
const TAKER = { email: 'e2e.taker@example.com', password: 'E2e-passphrase-9' };
const SYM = 'EUR%2FUSD';

const AUTHED_ROUTES = [
  ['home', '/'],
  ['workspace', '/workspace'],
  ['trade-redirect', `/trade/${SYM}`],
  ['advanced-redirect', `/advanced/${SYM}`],
  ['chart', `/chart/${SYM}`],
  ['depth', `/depth/${SYM}`],
  ['book', `/book/${SYM}`],
  ['calculator', '/calculator'],
  ['calculator-symbol', `/calculator/${SYM}`],
  ['orders', '/orders'],
  ['portfolio', '/portfolio'],
  ['performance', '/performance'],
  ['funding', '/funding'],
  ['kyc', '/kyc'],
  ['settings', '/settings'],
  ['sessions', '/account/sessions'],
  ['notifications', '/notifications'],
  ['support', '/support'],
  ['support-ticket', '/support/tickets/1'],
  ['webhooks', '/webhooks'],
  ['pamm', '/pamm'],
  ['copy-grid', '/copy-grid'],
  ['backtest', '/backtest'],
  ['discovery', '/discovery'],
  ['fee-tiers', '/fee-tiers'],
  ['reports', '/reports'],
  ['analytics', '/analytics'],
  ['explorer', '/explorer'],
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
  ['not-found', '/this-route-does-not-exist'],
];

const PUBLIC_ROUTES = [
  ['login', '/login'],
  ['register', '/register'],
  ['forgot-password', '/forgot-password'],
  ['reset-password', '/reset-password?token=probe'],
  ['verify-email', '/verify-email'],
  ['transparency', '/transparency'],
  ['status', '/status'],
  ['security', '/security'],
  ['guard-redirect', '/portfolio'], // must bounce to /login
];

const LIGHT_SUBSET = ['home', 'workspace', 'orders', 'settings', 'portfolio', 'discovery'];

const TOLERATED_404 = [/^\/api\/v1\/deposits\/[A-Z]{3}$/];

const DOM_CHECKS = `(() => {
  const q = (s, r = document) => [...r.querySelectorAll(s)];
  const vis = (el) => {
    const cs = getComputedStyle(el);
    const r = el.getBoundingClientRect();
    return cs.display !== 'none' && cs.visibility !== 'hidden' && r.width > 0 && r.height > 0;
  };
  const accName = (el) =>
    (el.getAttribute('aria-label') || '').trim() ||
    (el.getAttribute('aria-labelledby') || '').trim() ||
    (el.innerText || el.textContent || '').trim() ||
    (el.getAttribute('title') || '').trim() ||
    (el.getAttribute('alt') || '').trim();
  const sel = (el) => {
    const id = el.id ? '#' + el.id : '';
    const cls = (el.className && typeof el.className === 'string')
      ? '.' + el.className.trim().split(/\\s+/).slice(0, 2).join('.') : '';
    return el.tagName.toLowerCase() + id + cls;
  };

  // Headings
  const heads = q('h1,h2,h3,h4,h5,h6').filter(vis).map((h) => +h.tagName[1]);
  const skips = [];
  for (let i = 1; i < heads.length; i++)
    if (heads[i] - heads[i - 1] > 1) skips.push(\`h\${heads[i-1]}->h\${heads[i]}\`);

  // Images / icon graphics
  const imgNoAlt = q('img').filter((i) => !i.hasAttribute('alt')).map(sel);
  const svgNoTitle = q('svg').filter(vis).filter((s) =>
    !s.getAttribute('aria-hidden') && !s.getAttribute('role') && !s.querySelector('title')).map(sel);

  // Named interactives
  const btnNoName = q('button,[role=button]').filter(vis).filter((b) => !accName(b)).map(sel);
  const linkNoName = q('a[href]').filter(vis).filter((a) => !accName(a)).map(sel);

  // Form labels
  const fieldNoLabel = q('input:not([type=hidden]):not([type=submit]):not([type=button]),select,textarea')
    .filter(vis)
    .filter((f) => {
      if (f.getAttribute('aria-label') || f.getAttribute('aria-labelledby')) return false;
      if (f.id && document.querySelector(\`label[for="\${f.id}"]\`)) return false;
      if (f.closest('label')) return false;
      return true;
    })
    .map((f) => sel(f) + (f.placeholder ? \`[ph="\${f.placeholder}"]\` : ''));

  // Positive tabindex
  const tabPos = q('[tabindex]').filter((e) => +e.getAttribute('tabindex') > 0).map(sel);

  // Horizontal overflow
  const docOverflow = document.documentElement.scrollWidth - document.documentElement.clientWidth;
  const offenders = q('body *').filter(vis)
    .filter((e) => e.getBoundingClientRect().right > document.documentElement.clientWidth + 1)
    .slice(0, 8).map(sel);

  // Small targets (<24px any dimension) on visible interactives
  const smallTargets = q('a[href],button,[role=button],input,select,textarea,[tabindex]')
    .filter(vis)
    .filter((e) => {
      const r = e.getBoundingClientRect();
      return (r.width > 0 && r.width < 24) || (r.height > 0 && r.height < 24);
    })
    .map((e) => sel(e) + \` \${Math.round(e.getBoundingClientRect().width)}x\${Math.round(e.getBoundingClientRect().height)}\`)
    .slice(0, 15);

  // Mobile touch targets <44
  const touchTargets = q('a[href],button,[role=button],input,select,textarea')
    .filter(vis)
    .filter((e) => {
      const r = e.getBoundingClientRect();
      return r.width < 44 || r.height < 44;
    })
    .map((e) => sel(e) + \` \${Math.round(e.getBoundingClientRect().width)}x\${Math.round(e.getBoundingClientRect().height)}\`)
    .slice(0, 15);

  // Focus visibility probe: focus first 5 tabbables, check outline/ring
  const focusProbe = [];
  const tabbables = q('a[href],button,input,select,textarea,[tabindex]:not([tabindex="-1"])').filter(vis);
  for (const t of tabbables.slice(0, 5)) {
    t.focus();
    const cs = getComputedStyle(t);
    const hasIndicator =
      cs.outlineStyle !== 'none' || +cs.outlineWidth > 0 ||
      cs.boxShadow !== 'none' || cs.borderColor !== 'rgb(0, 0, 0)';
    focusProbe.push({ el: sel(t), visible: hasIndicator, outline: cs.outlineStyle + ' ' + cs.outlineWidth, ring: cs.boxShadow.slice(0, 60) });
  }
  document.activeElement && document.activeElement.blur();

  return {
    lang: document.documentElement.lang || null,
    h1Count: q('h1').filter(vis).length,
    headingSkips: skips,
    landmarks: {
      main: q('main,[role=main]').length,
      nav: q('nav,[role=navigation]').length,
      banner: q('header,[role=banner]').length,
      contentinfo: q('footer,[role=contentinfo]').length,
    },
    imgNoAlt, svgNoTitle, btnNoName, linkNoName, fieldNoLabel, tabPos,
    docOverflowPx: docOverflow, overflowOffenders: offenders,
    smallTargets, touchTargets, focusProbe,
    bodyText: (document.body.innerText || '').slice(0, 300),
    has404: /Page not found/.test(document.body.innerText || ''),
    stillLoading: /Loading…|Loading\\.\\.\\./.test(document.body.innerText || ''),
    insuffPerms: /Insufficient permissions/.test(document.body.innerText || ''),
  };
})()`;

function attachWatchers(page, rec) {
  const onPageError = (e) => rec.pageErrors.push(String(e).slice(0, 300));
  const onConsole = (m) => {
    if (m.type() === 'error' && !m.text().startsWith('Failed to load resource'))
      rec.consoleErrors.push(m.text().slice(0, 300));
    if (m.type() === 'warning') (rec.consoleWarnings ??= []).push(m.text().slice(0, 200));
  };
  const onResponse = (res) => {
    const s = res.status();
    if (s < 400) return;
    const p = new URL(res.url()).pathname;
    if (s === 404 && TOLERATED_404.some((re) => re.test(p))) return;
    rec.httpErrors.push(`${s} ${res.url().slice(0, 160)}`);
  };
  page.on('pageerror', onPageError);
  page.on('console', onConsole);
  page.on('response', onResponse);
  return () => {
    page.off('pageerror', onPageError);
    page.off('console', onConsole);
    page.off('response', onResponse);
  };
}

async function auditPage(page, name, url, shotDir) {
  const rec = {
    name,
    url,
    finalUrl: null,
    title: null,
    pageErrors: [],
    consoleErrors: [],
    consoleWarnings: [],
    httpErrors: [],
    dom: null,
    axe: null,
    perf: null,
    shot: null,
  };
  const detach = attachWatchers(page, rec);
  const t0 = Date.now();
  try {
    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 20000 });
    await page.waitForLoadState('networkidle', { timeout: 8000 }).catch(() => undefined);
    await page.waitForTimeout(900);
  } catch (e) {
    rec.pageErrors.push('NAV: ' + String(e).slice(0, 200));
  }
  rec.finalUrl = page.url();
  rec.title = await page.title().catch(() => null);
  rec.perf = await page
    .evaluate(() => {
      const n = performance.getEntriesByType('navigation')[0] || {};
      const res = performance.getEntriesByType('resource');
      return {
        ttfb: Math.round(n.responseStart ?? 0),
        dcl: Math.round(n.domContentLoadedEventEnd ?? 0),
        load: Math.round(n.loadEventEnd ?? 0),
        resources: res.length,
        transferKB: Math.round(res.reduce((a, r) => a + (r.transferSize || 0), 0) / 1024),
        wallMs: null,
      };
    })
    .catch(() => null);
  if (rec.perf) rec.perf.wallMs = Date.now() - t0;
  rec.dom = await page.evaluate(DOM_CHECKS).catch((e) => ({ evalError: String(e).slice(0, 200) }));
  rec.axe = await page
    .evaluate(
      `${AXE}; axe.run(document, { runOnly: { type: 'tag', values: ['wcag2a','wcag2aa','wcag21a','wcag21aa','best-practice'] } }).then(r => ({ violations: r.violations.map(v => ({ id: v.id, impact: v.impact, help: v.help, nodes: v.nodes.length, targets: v.nodes.slice(0,4).map(n => n.target.join(' ')) })) }))`,
    )
    .catch((e) => ({ axeError: String(e).slice(0, 200) }));
  const shot = `${shotDir}/${name}.png`;
  await page.screenshot({ path: shot, fullPage: true }).catch(() => (rec.shot = 'FAILED'));
  if (rec.shot !== 'FAILED') rec.shot = shot;
  detach();
  return rec;
}

async function uiLogin(page) {
  await page.goto('/login', { waitUntil: 'domcontentloaded' });
  await page.getByLabel('Email').fill(TAKER.email);
  await page.getByLabel('Password').fill(TAKER.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await page.waitForURL((u) => !u.pathname.includes('/login'), { timeout: 15000 });
}

async function main() {
  mkdirSync(`${OUT}/shots`, { recursive: true });
  const browser = await chromium.launch({ headless: !HEADFUL });
  const results = { mode: MODE, base: BASE, at: new Date().toISOString(), pages: [] };

  if (MODE === 'public') {
    mkdirSync(`${OUT}/shots/public`, { recursive: true });
    const ctx = await browser.newContext({ baseURL: BASE, viewport: { width: 1440, height: 900 } });
    const page = await ctx.newPage();
    for (const [name, url] of PUBLIC_ROUTES) {
      const rec = await auditPage(page, `public-${name}`, url, `${OUT}/shots/public`);
      rec.requested = url;
      results.pages.push(rec);
      console.log(
        `public ${name}: ${rec.title} | final=${rec.finalUrl} errs=${rec.pageErrors.length + rec.consoleErrors.length} http=${rec.httpErrors.length} axe=${rec.axe?.violations?.length ?? '?'}`,
      );
    }
    await ctx.close();
  } else {
    mkdirSync(`${OUT}/shots/${MODE}`, { recursive: true });
    const vp = MODE === 'mobile' ? { width: 390, height: 844 } : { width: 1440, height: 900 };
    const ctx = await browser.newContext({ baseURL: BASE, viewport: vp });
    const page = await ctx.newPage();
    const loginRec = {
      name: 'login-flow',
      url: '/login',
      pageErrors: [],
      consoleErrors: [],
      httpErrors: [],
    };
    attachWatchers(page, loginRec);
    try {
      await uiLogin(page);
      loginRec.ok = true;
      loginRec.landed = page.url();
    } catch (e) {
      loginRec.ok = false;
      loginRec.err = String(e).slice(0, 200);
    }
    results.login = loginRec;

    let routes = AUTHED_ROUTES;
    if (MODE === 'light') {
      routes = AUTHED_ROUTES.filter(([n]) => LIGHT_SUBSET.includes(n));
      await page.evaluate(() =>
        localStorage.setItem(
          'exc.theme.v1',
          JSON.stringify({ state: { theme: 'light' }, version: 0 }),
        ),
      );
    }
    if (MODE === 'desktop' || MODE === 'mobile')
      await page.evaluate(() =>
        localStorage.setItem(
          'exc.ui-mode.v1',
          JSON.stringify({ state: { mode: 'pro' }, version: 0 }),
        ),
      );

    for (const [name, url] of routes) {
      const rec = await auditPage(page, `${MODE}-${name}`, url, `${OUT}/shots/${MODE}`);
      // keep the requested route even after redirects
      rec.requested = url;
      results.pages.push(rec);
      console.log(
        `${MODE} ${name}: ${rec.title} | errs=${rec.pageErrors.length + rec.consoleErrors.length} http=${rec.httpErrors.length} axe=${rec.axe?.violations?.length ?? '?'} overflow=${rec.dom?.docOverflowPx ?? '?'}`,
      );
    }
    await ctx.close();
  }

  writeFileSync(`${OUT}/results-${MODE}.json`, JSON.stringify(results, null, 2));
  await browser.close();
  console.log(`DONE ${MODE} → ${OUT}/results-${MODE}.json`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
