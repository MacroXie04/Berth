// Real-stack test of the reaper from the user's side: what a real browser tab does (and
// does not do) to the idle clock, and what the page shows when the reaper powers a
// workspace off underneath it.
//
// Run it against a stack started with short timeouts and a fast reaper, for example
//
//   BERTH_IDLE_TIMEOUT=1m BERTH_RECLAIM_AFTER=10m BERTH_REAPER_INTERVAL=10s
//
//   BASE_URL=http://127.0.0.1:18090 E2E_USER=alice E2E_PASSWORD='correct horse' \
//     IDLE_SECONDS=60 node e2e/reaper.mjs
//
// It needs `docker` on this host (it reads the container's state directly: the API
// answers through the same reconciliation the reaper does, which would hide the
// difference). IDLE_SECONDS is the stack's BERTH_IDLE_TIMEOUT.
//
//  1. A visible tab keeps the workspace up for 2.5 idle timeouts.
//  2. A hidden tab (visibilityState faked, which is all the page can see) does too.
//  3. With the tab closed, the reaper stops the workspace after the timeout.
//  4. A tab that stays open but whose heartbeats stop getting through (a dead network,
//     a sleeping laptop's page) and whose remote-browser viewer is gone is powered off by
//     the reaper; the page notices at its next poll, drops the panes and offers Power on;
//     Power on brings it back. (A viewer that keeps streaming video is CPU load, and load
//     counts as use.)
//
// Exit code 0 when every check passed.

import { execFileSync } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { chromium } from 'playwright-core'
import { createRunner, findChromium, sleep, waitFor } from './lib.mjs'

const BASE = process.env.BASE_URL ?? 'http://127.0.0.1:18090'
const USER = process.env.E2E_USER ?? 'alice'
const PASS = process.env.E2E_PASSWORD ?? 'correct horse'
const IDLE = Number(process.env.IDLE_SECONDS ?? 60)
const SHOTS = process.env.SCREENSHOT_DIR
if (SHOTS) mkdirSync(SHOTS, { recursive: true })
const CONTAINER = `berth-ws-${USER}`

const { check, note, summary } = createRunner()

const docker = (...args) => {
  try {
    return execFileSync('docker', args, { encoding: 'utf8', timeout: 60000 }).trim()
  } catch {
    return null
  }
}
const containerState = () => docker('inspect', CONTAINER, '--format', '{{.State.Status}}')
const up = () => containerState() === 'running'

const browser = await chromium.launch({
  executablePath: findChromium(),
  args: ['--no-sandbox', '--autoplay-policy=no-user-gesture-required'],
  headless: true,
})
const problems = []

async function newPage(ctx) {
  const page = await ctx.newPage()
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message.slice(0, 200)}`))
  return page
}
const shot = (page, name) => (SHOTS ? page.screenshot({ path: join(SHOTS, `${name}.png`) }) : undefined)
const setVisibility = (page, v) =>
  page.evaluate((state) => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state })
    document.dispatchEvent(new Event('visibilitychange'))
  }, v)

async function login(page) {
  await page.goto(BASE + '/')
  await page.getByLabel('Username').fill(USER)
  await page.getByLabel('Password').fill(PASS)
  await page.getByRole('button', { name: 'Log in' }).click()
}

/** Logs in and brings the workspace to running with the panes mounted. */
async function openWorkspace(page) {
  await login(page)
  const power = page.getByRole('button', { name: 'Power on' })
  const panes = page.getByRole('separator')
  await Promise.race([power.waitFor({ timeout: 30000 }), panes.waitFor({ timeout: 30000 })])
  if (await power.isVisible()) await power.click()
  await panes.waitFor({ timeout: 180000 })
}

const MARKER = `reaper-marker-${Date.now()}`
const termText = (page) =>
  page.evaluate(() => Array.from(document.querySelectorAll('.xterm-rows > div'), (d) => d.textContent ?? '').join('\n'))
async function typeLine(page, text) {
  await page.locator('.term-host').click()
  await page.keyboard.type(text)
  await page.keyboard.press('Enter')
}

const watch = async (seconds, ok) => {
  // Polls every 5 s; returns the first second at which ok() failed, or null.
  const end = Date.now() + seconds * 1000
  while (Date.now() < end) {
    if (!ok()) return Math.round((seconds * 1000 - (end - Date.now())) / 1000)
    await sleep(5000)
  }
  return null
}

try {
  const ctx = await browser.newContext()
  const page = await newPage(ctx)

  await check('open the workspace in a real browser: terminal and remote browser mount', async () => {
    await openWorkspace(page)
    await page.locator('.xterm').waitFor({ timeout: 30000 })
    await page.locator('iframe').waitFor({ timeout: 30000 })
    if (!up()) throw new Error(`container is ${containerState()}`)
    // A file on the data volume, to find again after the reaper has stopped the workspace.
    await typeLine(page, `echo ${MARKER} > ~/reaper-marker.txt && echo written-ok`)
    await waitFor(async () => /written-ok/.test(await termText(page)), { what: 'the marker to be written', timeout: 15000 })
    await shot(page, 'r1-open')
  })

  await check(`a visible tab keeps the workspace up for ${Math.round(IDLE * 2.5)} s (idle timeout ${IDLE} s)`, async () => {
    const failedAt = await watch(Math.round(IDLE * 2.5), up)
    if (failedAt !== null) throw new Error(`the container stopped after ${failedAt} s with the tab open`)
  })

  await check(`a hidden tab keeps it up too (heartbeats go on, polling slows to once a minute)`, async () => {
    await setVisibility(page, 'hidden')
    const failedAt = await watch(Math.round(IDLE * 2.5), up)
    if (failedAt !== null) throw new Error(`the container stopped after ${failedAt} s with the tab hidden`)
    await setVisibility(page, 'visible')
  })

  await check(`with the tab closed, the reaper stops the workspace within about ${IDLE} s + a look`, async () => {
    await ctx.close() // no more heartbeats; the terminal and browser sockets close with the page
    const t0 = Date.now()
    await waitFor(() => !up(), { timeout: (IDLE + 60) * 1000, interval: 2000, what: 'the container to stop' })
    const took = Math.round((Date.now() - t0) / 1000)
    note(`stopped ${took} s after the tab was closed (idle timeout ${IDLE} s)`)
    if (took < IDLE * 0.4) throw new Error(`stopped after only ${took} s`)
    if (containerState() !== 'exited') throw new Error(`container is ${containerState()}, want exited (kept)`)
  })

  // A tab whose heartbeats stop getting through, with the page itself still open.
  const ctx2 = await browser.newContext()
  const page2 = await newPage(ctx2)
  await check('power on again from the call to action the page shows', async () => {
    await openWorkspace(page2)
    await page2.locator('.xterm').waitFor({ timeout: 30000 })
    if (!up()) throw new Error(`container is ${containerState()}`)
  })

  await check('the page stays open, but its heartbeats are blocked and its remote-browser viewer is gone: the reaper powers the workspace off', async () => {
    // A viewer streaming the remote browser is load (video encoding), and the reaper counts load as use; so the
    // viewer is detached too, which is also what a dead connection does. The terminal socket stays open and idle.
    await page2.route('**/api/workspace/heartbeat', (route) => route.abort())
    await page2.evaluate(() => document.querySelector('iframe')?.contentWindow?.location.replace('about:blank'))
    const t0 = Date.now()
    await waitFor(() => !up(), { timeout: (IDLE + 90) * 1000, interval: 2000, what: 'the container to stop' })
    note(`stopped ${Math.round((Date.now() - t0) / 1000)} s after the heartbeats and the viewer stopped`)
  })

  await check('the open page notices at its next poll: panes go, the call to action returns with the honest text', async () => {
    await page2.getByRole('button', { name: 'Power on' }).waitFor({ timeout: 30000 })
    if (await page2.locator('.xterm, iframe').count()) throw new Error('panes still mounted for a stopped workspace')
    const text = await page2.locator('main').innerText()
    if (!/powered off/i.test(text) || !/Programs that were running in it ended with it/.test(text) || !/Your files are kept/.test(text)) {
      throw new Error(`unexpected text: ${text.slice(0, 300)}`)
    }
    await shot(page2, 'r2-stopped-by-the-reaper')
  })

  await check('Power on brings it back; the terminal works again and the file is still there', async () => {
    await page2.unroute('**/api/workspace/heartbeat')
    await page2.getByRole('button', { name: 'Power on' }).click()
    await page2.locator('.xterm').waitFor({ timeout: 180000 })
    if (!up()) throw new Error(`container is ${containerState()}`)
    await waitFor(async () => /\$\s*$|[#>]\s*$/m.test(await termText(page2)), { what: 'a shell prompt', timeout: 30000 })
    await typeLine(page2, 'cat ~/reaper-marker.txt')
    await waitFor(async () => (await termText(page2)).includes(MARKER), {
      what: 'the marker file written before the stop (this terminal never saw the marker: it can only come from the file)',
      timeout: 15000,
    })
    await shot(page2, 'r3-back')
  })

  await check('no page errors', async () => {
    if (problems.length) throw new Error(problems.join('; '))
  })
  await ctx2.close()
} finally {
  await browser.close()
}
process.exit(summary() ? 0 : 1)
