// A stand-in for the Go backend's static serving, for browser tests: serves the
// production build (internal/webui/dist) with the Content-Security-Policy the Go
// server sends (DEFAULT_CSP below; src/csp.test.ts keeps the two equal),
// falls back to index.html for unknown non-asset GET paths, and forwards /api
// and /ws (WebSocket upgrades included) to a running Berth stack.
//
// Host and Origin are forwarded untouched (like the dev server without
// changeOrigin), because ttyd compares the WebSocket Origin host with Host.
//
//   node e2e/serve-dist.mjs                       # PORT=4173 TARGET=http://127.0.0.1:18090
//   TARGET=http://127.0.0.1:18090 PORT=4173 CSP="..." node e2e/serve-dist.mjs

import { createReadStream, existsSync, statSync } from 'node:fs'
import http from 'node:http'
import net from 'node:net'
import { dirname, extname, join, normalize, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))

export const DEFAULT_DIST = resolve(here, '../../internal/webui/dist')

/** The policy of internal/webui/webui.go, copied; src/csp.test.ts fails if they differ. Override with CSP=... */
export const DEFAULT_CSP = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data:",
  "font-src 'self'",
  "connect-src 'self'",
  "frame-src 'self'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
].join('; ')

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.json': 'application/json',
  '.map': 'application/json',
  '.ico': 'image/x-icon',
  '.png': 'image/png',
}

function safeFile(dist, urlPath) {
  let p
  try {
    p = decodeURIComponent(urlPath)
  } catch {
    return null
  }
  const full = normalize(join(dist, p))
  if (!full.startsWith(dist)) return null
  try {
    if (existsSync(full) && statSync(full).isFile()) return full
  } catch {
    // fall through
  }
  return null
}

export function startServer({ port = 0, target = 'http://127.0.0.1:18090', dist = DEFAULT_DIST, csp = DEFAULT_CSP } = {}) {
  const t = new URL(target)

  const server = http.createServer((req, res) => {
    const url = new URL(req.url ?? '/', 'http://x')
    if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/ws/') || url.pathname === '/healthz') {
      const up = http.request(
        { host: t.hostname, port: t.port, method: req.method, path: req.url, headers: req.headers },
        (r) => {
          res.writeHead(r.statusCode ?? 502, r.headers)
          r.pipe(res)
        },
      )
      up.on('error', () => {
        res.writeHead(502).end('Bad Gateway')
      })
      req.pipe(up)
      return
    }
    if (req.method !== 'GET' && req.method !== 'HEAD') {
      res.writeHead(405).end()
      return
    }
    let file = safeFile(dist, url.pathname === '/' ? '/index.html' : url.pathname)
    const isAsset = extname(url.pathname) !== ''
    if (!file && !isAsset) file = safeFile(dist, '/index.html') // SPA fallback
    if (!file) {
      res.writeHead(404, { 'Content-Type': 'text/plain' }).end('not found')
      return
    }
    const headers = { 'Content-Type': TYPES[extname(file)] ?? 'application/octet-stream' }
    if (extname(file) === '.html') {
      headers['Content-Security-Policy'] = csp
      headers['Cache-Control'] = 'no-cache'
      headers['X-Content-Type-Options'] = 'nosniff'
    }
    res.writeHead(200, headers)
    if (req.method === 'HEAD') res.end()
    else createReadStream(file).pipe(res)
  })

  server.on('upgrade', (req, socket, head) => {
    if (!(req.url ?? '').startsWith('/ws/')) {
      socket.destroy()
      return
    }
    const up = net.connect(Number(t.port), t.hostname, () => {
      let raw = `${req.method} ${req.url} HTTP/1.1\r\n`
      for (let i = 0; i < req.rawHeaders.length; i += 2) raw += `${req.rawHeaders[i]}: ${req.rawHeaders[i + 1]}\r\n`
      up.write(`${raw}\r\n`)
      if (head?.length) up.write(head)
      socket.pipe(up)
      up.pipe(socket)
    })
    up.on('error', () => socket.destroy())
    socket.on('error', () => up.destroy())
  })

  return new Promise((resolveListen) => {
    server.listen(port, '127.0.0.1', () => {
      const addr = server.address()
      resolveListen({ server, url: `http://127.0.0.1:${addr.port}`, close: () => new Promise((r) => server.close(() => r())) })
    })
  })
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const { url } = await startServer({
    port: Number(process.env.PORT ?? 4173),
    target: process.env.TARGET ?? 'http://127.0.0.1:18090',
    csp: process.env.CSP ?? DEFAULT_CSP,
  })
  console.log(`serving ${DEFAULT_DIST} at ${url} (API and /ws -> ${process.env.TARGET ?? 'http://127.0.0.1:18090'})`)
}
