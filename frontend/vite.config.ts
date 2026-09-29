/// <reference types="vitest/config" />
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';
import { defineConfig, type Plugin } from 'vite';

/**
 * Dev-mode CSP relaxation (Phase-10 Task 10.3.1 item 8).
 *
 * `index.html` ships the production meta Content-Security-Policy verbatim —
 * `default-src 'self'`, no `'unsafe-inline'`, `frame-ancestors 'none'`.
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
    "frame-ancestors 'none'",
  ].join('; ');
  return {
    name: 'exchange:dev-csp-relaxation',
    apply: 'serve',
    transformIndexHtml(html) {
      return html.replace(
        /(<meta http-equiv="Content-Security-Policy" content=")[^"]*(")/,
        `$1${DEV_CSP}$2`,
      );
    },
  };
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
    // 'self'` covers both REST and WS traffic in development.
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: false },
      '/ws': { target: 'ws://localhost:8080', ws: true, changeOrigin: false },
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
