/// <reference types="vitest/config" />
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';
import { defineConfig, type Plugin } from 'vite';

/**
 * Dev-mode CSP relaxation (Phase-10 Task 10.3.1 item 8).
 *
 * `index.html` ships the production meta Content-Security-Policy verbatim —
 * `default-src 'self'`, no `'unsafe-inline'`. `frame-ancestors` is absent
 * from the meta (browsers ignore it there); response headers own it.
 * That strict policy would break the Vite dev server (HMR injects `<style>`
 * tags and dials ws://localhost). During `vite serve` ONLY, this plugin
 * rewrites the meta content to a dev-relaxed policy. The built `dist/index.html`
 * keeps the strict production CSP untouched — verify with:
 *   grep -o 'http-equiv="Content-Security-Policy"[^>]*' dist/index.html
 */
function devCspRelaxation(): Plugin {
  const DEV_CSP = [
    "default-src 'self'",
    "script-src 'self'",
    // Vite dev injects Tailwind styles as <style> elements.
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    "font-src 'self'",
    // Same-origin API + HMR websocket (ws: is the http: scheme-equivalent
    // origin per CSP3 host-source matching, but explicit is clearer).
    "connect-src 'self' ws: wss:",
    "object-src 'none'",
    "base-uri 'self'",
  ].join('; ');
  return {
    name: 'exchange:dev-csp-relaxation',
    apply: 'serve',
    transformIndexHtml(html) {
      return html.replace(
        /(<meta\s+http-equiv="Content-Security-Policy"\s+content=")[^"]*(")/,
        `$1${DEV_CSP}$2`,
      );
    },
  };
}

/**
 * Dev-only XFF sharding for the gateway's §8.8 edge limiter — shared
 * policy with the docker frontend (deploy/docker/frontend.edge.js).
 *
 * The edge limiter buckets traffic per client IP and the dev gateway
 * runs EXC_TRUST_PROXY=1. Unsharded, every dev client collapses onto one
 * proxy IP and the bucket starves the SPA: page-mount bursts 429,
 * strikes arm the post-429 ban machinery, and the order-entry lock
 * trips mid-flow. The dev contract is that LAN clients are not
 * edge-rate-limited, so each proxied request gets its OWN synthetic
 * 10.90.x bucket (a monotonic counter — unique, no RNG). The 10.90.x
 * space is also what `seeddev -allowlist-ips` exempts from ban
 * escalation.
 */
let devEdgeSeq = 0;
function devEdgeIp(): string {
  devEdgeSeq = (devEdgeSeq + 1) % 65536;
  return `10.90.${(devEdgeSeq >>> 8) & 0xff}.${devEdgeSeq & 0xff}`;
}

export default defineConfig({
  plugins: [react(), tailwindcss(), devCspRelaxation()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5173,
    // Dev proxies keep the browser same-origin so the CSP `connect-src
    // 'self'` covers both REST and WS traffic in development. See
    // devEdgeIp above for the per-request XFF sharding rationale.
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: false,
        configure: (proxy) => {
          proxy.on('proxyReq', (proxyReq) => {
            proxyReq.setHeader('x-forwarded-for', devEdgeIp());
          });
        },
      },
      '/ws': {
        target: 'ws://localhost:8080',
        ws: true,
        changeOrigin: false,
        configure: (proxy) => {
          // Per-connection bucket — the gateway's ReconnectRate (10
          // upgrades /60s/IP) would 429 a SPA that opens a socket per
          // page mount if every upgrade shared one fixed proxy IP. WS
          // upgrades surface as proxyReqWs, not proxyReq.
          proxy.on('proxyReqWs', (proxyReq, req) => {
            const port = (req.socket.remotePort ?? 0) % 256;
            proxyReq.setHeader('x-forwarded-for', `10.90.255.${port}`);
          });
        },
      },
    },
  },
  build: {
    target: 'es2022',
    // Emits dist/.vite/manifest.json — scripts/check-bundle-size.mjs uses it
    // to measure the entry chunk + its static-import closure (initial JS).
    manifest: true,
    sourcemap: false,
    chunkSizeWarningLimit: 300,
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: false,
    exclude: ['e2e/**', 'node_modules/**', 'dist/**'],
    coverage: {
      reporter: ['text', 'html'],
      exclude: ['e2e/**', '**/*.test.*', 'src/test/**'],
    },
  },
});
