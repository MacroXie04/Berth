import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// The policy the browser tests run under (e2e/serve-dist.mjs) is a hand-kept copy of the one the Go server sends
// (internal/webui/webui.go). If they differ, the e2e run says nothing about production.

const webuiGo = readFileSync(resolve(import.meta.dirname, '../../internal/webui/webui.go'), 'utf8')

/** The Go string expression `csp = "..." + "..." ...` joined, with `connectSrc` filled in. */
function goPolicy(): string {
  const connectSrc = /connectSrc\s*=\s*"([^"]*)"/.exec(webuiGo)?.[1]
  const decl = /\n\tcsp\s*=\s*((?:"[^"\n]*"(?:\s*\+\s*(?:connectSrc|"[^"\n]*"))*\s*\+?\s*\n?\s*)+)/.exec(webuiGo)?.[1]
  if (connectSrc === undefined || decl === undefined) throw new Error('csp constant not found in internal/webui/webui.go')
  const parts = [...decl.matchAll(/"([^"\n]*)"|\bconnectSrc\b/g)].map((m) => (m[1] !== undefined ? m[1] : connectSrc))
  return parts.join('')
}

describe('Content-Security-Policy of the browser tests', () => {
  it('e2e/serve-dist.mjs sends the same policy as internal/webui/webui.go', async () => {
    const path = resolve(import.meta.dirname, '../e2e/serve-dist.mjs')
    const mod: { DEFAULT_CSP: string } = await import(/* @vite-ignore */ path)
    const directives = (policy: string) => policy.split(';').map((d) => d.trim()).filter(Boolean)
    expect(directives(mod.DEFAULT_CSP)).toEqual(directives(goPolicy()))
  })

  it('allows inline styles (xterm.js needs them) and no inline scripts', () => {
    const policy = goPolicy()
    expect(policy).toContain("style-src 'self' 'unsafe-inline'")
    expect(policy).toContain("script-src 'self';")
    expect(policy).not.toMatch(/script-src[^;]*unsafe/)
  })
})
