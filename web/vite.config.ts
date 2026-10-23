import { existsSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import react from '@vitejs/plugin-react'
import type { Plugin } from 'vite'
import { defineConfig } from 'vitest/config'

// The Go backend serves the production build same-origin at "/". In development
// the dev server proxies the API and the workspace routes to a running stack.
// changeOrigin stays off on purpose: ttyd's --check-origin compares the
// WebSocket Origin host with the Host header, so Host must remain the dev
// server's own (the browser's Origin).
const target = process.env.BERTH_DEV_TARGET ?? 'http://127.0.0.1:18090'

const outDir = resolve(import.meta.dirname, '../internal/webui/dist')

// emptyOutDir wipes the directory, including the .gitkeep that keeps it in the
// repository (the Go package embeds it with //go:embed all:dist). Put it back.
function keepGitkeep(): Plugin {
  return {
    name: 'berth-keep-gitkeep',
    apply: 'build',
    closeBundle() {
      const f = resolve(outDir, '.gitkeep')
      if (!existsSync(f)) writeFileSync(f, '')
    },
  }
}

export default defineConfig({
  plugins: [react(), keepGitkeep()],
  server: {
    proxy: {
      '/api': { target },
      '/ws': { target, ws: true },
    },
  },
  build: {
    outDir,
    emptyOutDir: true,
    sourcemap: false,
    // xterm.js and React are most of the bundle; one file is fine for a same-origin app.
    chunkSizeWarningLimit: 700,
    // A strict CSP (img-src/font-src 'self') would block data: URIs.
    assetsInlineLimit: 0,
    // No inline polyfill script is needed for a single-entry app.
    modulePreload: { polyfill: false },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['src/test/setup.ts'],
    restoreMocks: true,
  },
})
