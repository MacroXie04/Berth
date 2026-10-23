// Small helpers for the browser smoke test.

import { existsSync, readdirSync } from 'node:fs'
import { join } from 'node:path'

/** Finds a Chromium: $CHROMIUM, else the newest chromium-*\/chrome-linux/chrome under $PLAYWRIGHT_BROWSERS_PATH or /opt/pw-browsers. */
export function findChromium() {
  if (process.env.CHROMIUM) return process.env.CHROMIUM
  const root = process.env.PLAYWRIGHT_BROWSERS_PATH ?? '/opt/pw-browsers'
  if (!existsSync(root)) return undefined // let playwright-core look in its own default location
  const dirs = readdirSync(root)
    .filter((d) => /^chromium-\d+$/.test(d))
    .sort((a, b) => Number(b.split('-')[1]) - Number(a.split('-')[1]))
  for (const d of dirs) {
    for (const rel of ['chrome-linux/chrome', 'chrome-linux64/chrome', 'chrome-mac/Chromium.app/Contents/MacOS/Chromium']) {
      const p = join(root, d, rel)
      if (existsSync(p)) return p
    }
  }
  return undefined
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

/** Polls `fn` until it returns a truthy value; throws with `what` on timeout. */
export async function waitFor(fn, { timeout = 15000, interval = 100, what = 'condition' } = {}) {
  const start = Date.now()
  let last
  for (;;) {
    try {
      last = await fn()
      if (last) return last
    } catch (e) {
      last = e
    }
    if (Date.now() - start > timeout) {
      throw new Error(`timed out after ${timeout} ms waiting for ${what}${last instanceof Error ? `: ${last.message}` : ''}`)
    }
    await sleep(interval)
  }
}

/** WCAG contrast ratio between two [r,g,b] colours (0-255). */
export function contrast(a, b) {
  const lum = ([r, g, b]) => {
    const f = (c) => {
      const s = c / 255
      return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
    }
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
  }
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

/**
 * Runs checks one by one, prints PASS/FAIL, remembers failures. A failing check
 * does not stop the run.
 */
export function createRunner() {
  const results = []
  return {
    results,
    async check(name, fn) {
      const t0 = Date.now()
      try {
        const detail = await fn()
        results.push({ name, ok: true })
        console.log(`PASS  ${name}${detail ? `  (${detail})` : ''}  [${Date.now() - t0} ms]`)
      } catch (e) {
        results.push({ name, ok: false, error: e })
        console.log(`FAIL  ${name}\n      ${e instanceof Error ? e.message.split('\n').join('\n      ') : e}`)
      }
    },
    note(msg) {
      console.log(`INFO  ${msg}`)
    },
    summary() {
      const failed = results.filter((r) => !r.ok)
      console.log(`\n${results.length - failed.length}/${results.length} checks passed${failed.length ? `, ${failed.length} FAILED` : ''}`)
      return failed.length === 0
    },
  }
}
