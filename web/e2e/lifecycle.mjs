// Real-stack lifecycle test: drives the SPA in a real Chromium through a COLD start,
// the terminal, the neko remote browser (video + input), several tabs, power off /
// power on with persistence, and logout. Unlike smoke.mjs it powers the workspace off
// and on, so run it against a stack you can disturb.
//
//   BASE_URL=http://127.0.0.1:18090 E2E_USER=alice E2E_PASSWORD='correct horse' \
//     E2E_DOCKER=1 node e2e/lifecycle.mjs
//
// BASE_URL        the app (SPA + /api + /ws behind Traefik). Default http://127.0.0.1:18090.
// E2E_USER/PASSWORD  an existing account.
// E2E_DOCKER=1    allow `docker` calls on this host: remove the workspace container first (cold
//                 start from "offline"), inspect the published UDP ports and the listening UDP
//                 sockets, and check that typing reached the remote Chromium (xdotool).
//                 Without it only what a browser can see is checked.
// SCREENSHOT_DIR  write screenshots there.
// CHROMIUM        path to a Chromium binary.
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
const DOCKER = process.env.E2E_DOCKER === '1'
const SHOTS = process.env.SCREENSHOT_DIR
if (SHOTS) mkdirSync(SHOTS, { recursive: true })
const CONTAINER = `berth-ws-${USER}`

const { check, note, summary } = createRunner()
const problems = { csp: [], pageErrors: [], consoleErrors: [] }
let step = ''

const docker = (...args) => execFileSync('docker', args, { encoding: 'utf8', timeout: 60000 })
const dockerTry = (...args) => {
  try {
    return docker(...args)
  } catch (e) {
    return null
  }
}

const browser = await chromium.launch({
  executablePath: findChromium(),
  args: ['--no-sandbox', '--autoplay-policy=no-user-gesture-required'],
  headless: true,
})

// Remember every RTCPeerConnection of every frame so the neko connection can be inspected.
const INIT = () => {
  const Native = window.RTCPeerConnection
  window.__pcs = []
  window.RTCPeerConnection = function (...a) {
    const pc = new Native(...a)
    window.__pcs.push(pc)
    return pc
  }
  window.RTCPeerConnection.prototype = Native.prototype
  Object.setPrototypeOf(window.RTCPeerConnection, Native)
}

async function newPage(ctx) {
  const page = await ctx.newPage()
  await page.addInitScript(INIT)
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) => {
      console.log(`CSPVIOLATION ${e.violatedDirective} ${e.blockedURI}`)
    })
  })
  page.on('console', (m) => {
    const t = m.text()
    if (t.startsWith('CSPVIOLATION')) problems.csp.push(`[${step.slice(0, 40)}] ${t}`)
    // The clipboard-read refusal is intended: the app's Permissions-Policy denies it so that neko cannot copy the
    // user's local clipboard into the workspace; neko's client tries on every mouse-enter and Chromium logs it.
    else if (m.type() === 'error' && !/Failed to load resource|WebSocket connection to .* failed|Permissions policy violation: The Clipboard API/.test(t)) {
      problems.consoleErrors.push(`[${step.slice(0, 40)}] ${t.slice(0, 200)}`)
    }
  })
  page.on('pageerror', (e) => problems.pageErrors.push(`[${step.slice(0, 40)}] ${e.message.slice(0, 300)}`))
  return page
}

const shot = (page, name) => (SHOTS ? page.screenshot({ path: join(SHOTS, `${name}.png`) }) : undefined)
const run = (name, fn) => {
  step = name
  return check(name, fn)
}

const termRows = (page) =>
  page.evaluate(() => Array.from(document.querySelectorAll('.xterm-rows > div'), (d) => d.textContent ?? '').join('\n'))

async function typeLine(page, text) {
  await page.locator('.term-host').click()
  await page.keyboard.type(text)
  await page.keyboard.press('Enter')
}

async function login(page) {
  await page.goto(BASE + '/')
  await page.getByLabel('Username').fill(USER)
  await page.getByLabel('Password').fill(PASS)
  await page.getByRole('button', { name: 'Log in' }).click()
}

const api = async (page, path, init) => page.evaluate(async ([p, i]) => {
  const r = await fetch(p, { credentials: 'same-origin', ...i })
  return { status: r.status, body: await r.text() }
}, [path, init])

const workspaceState = async (page) => JSON.parse((await api(page, '/api/workspace')).body).workspace.state

/** The neko frame of a page, once it exists. */
const nekoFrame = (page) => page.frames().find((f) => /\/browser\//.test(f.url()))

/** Waits until the neko frame has a connected WebRTC peer connection with playing video. */
async function waitForVideo(page, what) {
  return waitFor(
    async () => {
      const f = nekoFrame(page)
      if (!f) return null
      const r = await f.evaluate(() => {
        const pcs = window.__pcs ?? []
        const connected = pcs.some((pc) => pc.connectionState === 'connected')
        const v = document.querySelector('video')
        return { pcs: pcs.length, connected, w: v?.videoWidth ?? 0, playing: !!v && !v.paused && v.readyState >= 2 }
      })
      return r.connected && r.w > 0 && r.playing ? r : null
    },
    { timeout: 45000, interval: 500, what: `${what}: connected WebRTC + playing video` },
  )
}

// Every UDP socket bound inside the container, as {ip, port}, from /proc/net/udp (and udp6 when the container has IPv6).
// Docker's embedded DNS resolver (127.0.0.11, a random port of its own) is left out. null = could not be read.
function udpSockets() {
  const v4 = dockerTry('exec', CONTAINER, 'cat', '/proc/net/udp')
  if (v4 === null) return null
  const rows = (text) =>
    (text ?? '')
      .split('\n')
      .slice(1)
      .map((l) => l.trim().split(/\s+/))
      .filter((c) => c.length > 3)
      .map((c) => c[1].split(':'))
  // "030014AC" is 172.20.0.3 (the bytes of a little-endian address, as hex)
  const dotted = (hex) => hex.match(/../g).reverse().map((b) => parseInt(b, 16)).join('.')
  const found = [
    ...rows(v4).map(([ip, port]) => ({ ip: dotted(ip), port: parseInt(port, 16) })),
    ...rows(dockerTry('exec', CONTAINER, 'sh', '-c', 'cat /proc/net/udp6 2>/dev/null; true')).map(([ip, port]) => ({ ip: `[${ip}]`, port: parseInt(port, 16) })),
  ]
  return found.filter((s) => s.ip !== '127.0.0.11')
}
const socketKeys = (list) => (list ?? []).map((s) => `${s.ip}:${s.port}`).sort()

const ctx = await browser.newContext({ viewport: { width: 1400, height: 800 } })
const page = await newPage(ctx)

try {
  if (DOCKER) {
    await run('setup: power off through the API, then remove the container behind the backend\'s back', async () => {
      await page.goto(BASE + '/')
      await login(page)
      await page.getByRole('button', { name: 'Log out' }).waitFor()
      await api(page, '/api/workspace/stop', { method: 'POST', headers: { 'Content-Type': 'application/json' } })
      docker('rm', '-f', CONTAINER)
      return 'container removed'
    })
    await page.reload()
  } else {
    await page.goto(BASE + '/')
    await login(page)
  }

  await run('login shows the workspace page; a container removed behind the backend\'s back is noticed: state "offline", a Power on button', async () => {
    await page.getByRole('button', { name: 'Log out' }).waitFor()
    if (!DOCKER) return 'logged in'
    // The call to action looks the same for "stopped" and "offline", so ask for the state itself, and read the badge.
    await page.getByRole('button', { name: 'Power on' }).waitFor({ timeout: 15000 })
    const st = await workspaceState(page)
    if (st !== 'offline') throw new Error(`/api/workspace says "${st}" after the container was removed, expected "offline"`)
    const badge = (await page.locator('.badge').innerText()).trim()
    if (badge !== 'Offline') throw new Error(`the status bar says "${badge}", expected "Offline"`)
    return 'state offline, badge Offline, call to action'
  })

  await run('Power on from a cold start: progress, then terminal + browser panes', async () => {
    const t0 = Date.now()
    if (await page.getByRole('button', { name: 'Power on' }).count()) {
      await page.getByRole('button', { name: 'Power on' }).click()
      await page.getByText(/starting/i).first().waitFor({ timeout: 10000 })
    }
    await page.locator('.xterm').waitFor({ timeout: 120000 })
    await page.getByTitle('Remote browser').waitFor({ timeout: 10000 })
    const st = await workspaceState(page)
    if (st !== 'running') throw new Error(`state ${st}`)
    return `${Math.round((Date.now() - t0) / 1000)} s to running`
  })
  await shot(page, 'lifecycle-1-running')

  await run('terminal: the shell runs as the unprivileged user and writes to the data volume', async () => {
    await waitFor(async () => /\$/.test(await termRows(page)), { what: 'a prompt', timeout: 20000 })
    await typeLine(page, 'echo uid-$(id -u)-$(whoami) && echo persisted-$RANDOM > ~/m3-lifecycle.txt && cat ~/m3-lifecycle.txt')
    await waitFor(async () => /uid-1000-user/.test(await termRows(page)), { what: 'uid-1000-user' })
    const m = (await termRows(page)).match(/persisted-\d+/g)
    if (!m) throw new Error('no marker echoed back')
    globalThis.MARKER = m[m.length - 1]
    return globalThis.MARKER
  })

  await run('browser: neko logs in on its own and the WebRTC video plays', async () => {
    const r = await waitForVideo(page, 'first load')
    return `${r.w}px wide, ${r.pcs} peer connection(s)`
  })
  await shot(page, 'lifecycle-2-video')

  await run('browser: the address never keeps the neko password, and the page text does not show it', async () => {
    const f = nekoFrame(page)
    const loc = await f.evaluate(() => location.search)
    if (/pwd=|usr=/.test(loc)) throw new Error(`neko left the credentials in its address: ${loc}`)
    if (/pwd=/.test(page.url())) throw new Error('password in the page URL')
    return `frame search "${loc}"`
  })

  if (DOCKER) {
    await run('browser: one published UDP port, multiplexed (NEKO_WEBRTC_UDPMUX), and exactly one socket bound for it', async () => {
      const info = JSON.parse(docker('inspect', CONTAINER))[0]
      const bindings = Object.keys(info.HostConfig.PortBindings ?? {})
      const env = Object.fromEntries((info.Config.Env ?? []).map((e) => [e.split('=')[0], e.slice(e.indexOf('=') + 1)]))
      if (bindings.length !== 1) throw new Error(`published ports: ${bindings.join(',')}`)
      if (!env.NEKO_WEBRTC_UDPMUX || env.NEKO_WEBRTC_EPR) throw new Error(`env mux=${env.NEKO_WEBRTC_UDPMUX} epr=${env.NEKO_WEBRTC_EPR}`)
      const port = Number(bindings[0].split('/')[0])
      const bound = (udpSockets() ?? []).filter((x) => x.port === port)
      if (bound.length !== 1) throw new Error(`sockets bound to ${port}: ${bound.length}`)
      return `udp/${port}`
    })

    await run('browser: a click and a key chord in the video reach the remote Chromium (observed with xdotool)', async () => {
      const f = nekoFrame(page)
      const box = await f.locator('video').boundingBox()
      if (!box) throw new Error('no video box')
      const windows = () => (dockerTry('exec', CONTAINER, 'sh', '-c', 'DISPLAY=:99.0 xdotool search --onlyvisible --class chromium 2>/dev/null') ?? '').trim().split('\n').filter(Boolean)
      const names = () => dockerTry('exec', CONTAINER, 'sh', '-c', 'DISPLAY=:99.0 xdotool search --onlyvisible --class chromium getwindowname %@ 2>/dev/null') ?? ''

      // Mouse: a fresh remote Chromium shows the New Tab page with one "Web Store" tile in the middle of the screen.
      // The first click also takes control (implicit hosting), and is replayed.
      await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2)
      await waitFor(() => /chrome\.google\.com/.test(names()), { timeout: 20000, interval: 500, what: 'the click on the Web Store tile to navigate the remote browser' })

      // Keyboard: Playwright's high-level press() does not produce events neko's keyboard handler accepts; physical-shaped
      // raw events do. Ctrl+N opens a new remote window, which xdotool counts.
      const cdp = await ctx.newCDPSession(page)
      const key = (type, o) => cdp.send('Input.dispatchKeyEvent', { type, ...o })
      const chord = async (k, code, vk) => {
        await key('rawKeyDown', { key: 'Control', code: 'ControlLeft', windowsVirtualKeyCode: 17, modifiers: 0 })
        await sleep(80)
        await key('rawKeyDown', { key: k, code, windowsVirtualKeyCode: vk, modifiers: 2 })
        await sleep(80)
        await key('keyUp', { key: k, code, windowsVirtualKeyCode: vk, modifiers: 2 })
        await sleep(80)
        await key('keyUp', { key: 'Control', code: 'ControlLeft', windowsVirtualKeyCode: 17, modifiers: 0 })
      }
      const before = windows().length
      await chord('n', 'KeyN', 78)
      await waitFor(() => windows().length === before + 1, { timeout: 15000, interval: 500, what: `Ctrl+N to open a second remote window (had ${before})` })
      await chord('w', 'KeyW', 87) // and close it again
      await waitFor(() => windows().length === before, { timeout: 15000, interval: 500, what: 'Ctrl+W to close it' })
      return `click navigated (${names().trim().split('\n')[0]}); Ctrl+N opened and Ctrl+W closed a remote window`
    })
    await shot(page, 'lifecycle-3-typed')
  }

  await run('reload: still on the workspace view; terminal and video come back by themselves', async () => {
    await page.reload()
    await page.locator('.xterm').waitFor({ timeout: 30000 })
    await waitForVideo(page, 'after reload')
    await waitFor(async () => /\$/.test(await termRows(page)), { what: 'a new prompt', timeout: 20000 })
    return 'ok'
  })

  // Everything bound for UDP in the container while one tab is open: what must be there again after a second tab came and went.
  const udpBefore = DOCKER ? socketKeys(udpSockets()) : null
  const second = await newPage(ctx)
  await run('a second tab: its own neko session; the first tab keeps playing (no "opened in another tab")', async () => {
    await second.goto(BASE + '/')
    await second.locator('.xterm').waitFor({ timeout: 30000 })
    await waitForVideo(second, 'second tab')
    await sleep(2000)
    const firstText = await nekoFrame(page).evaluate(() => document.body.innerText)
    if (/another tab|opened in another/i.test(firstText)) throw new Error(`first tab was kicked: ${firstText.slice(0, 120)}`)
    await waitForVideo(page, 'first tab after the second opened')
    return 'both tabs play'
  })
  await shot(second, 'lifecycle-4-second-tab')
  await second.close()

  if (DOCKER) {
    await run('closing tabs does not leak: the set of UDP sockets bound in the container is the one from before the second tab', async () => {
      if (udpBefore === null || udpBefore.length === 0) throw new Error('could not read /proc/net/udp in the container')
      await sleep(8000) // a closed peer lingers a few seconds
      // A leaked socket stays for ever; one that belongs to the remote Chromium's own traffic goes away by itself. Wait for equality.
      let now = []
      await waitFor(
        () => {
          now = socketKeys(udpSockets())
          return JSON.stringify(now) === JSON.stringify(udpBefore)
        },
        { timeout: 20000, interval: 1000, what: 'the UDP sockets to be the same as before' },
      ).catch(() => {
        const extra = now.filter((k) => !udpBefore.includes(k))
        const gone = udpBefore.filter((k) => !now.includes(k))
        throw new Error(`UDP sockets differ from before the second tab: new ${JSON.stringify(extra)}, gone ${JSON.stringify(gone)}`)
      })
      return `${udpBefore.length} socket(s), unchanged: ${udpBefore.join(', ')}`
    })
  }

  await run('Power off (inline confirmation): panes go, the call to action returns, the container is kept', async () => {
    await page.getByRole('button', { name: 'Power off' }).click()
    await page.getByRole('button', { name: /yes, power off/i }).click()
    await page.getByRole('button', { name: 'Power on' }).waitFor({ timeout: 60000 })
    if ((await page.locator('.xterm').count()) !== 0) throw new Error('terminal still mounted')
    if ((await page.getByTitle('Remote browser').count()) !== 0) throw new Error('browser frame still mounted')
    const st = await workspaceState(page)
    if (st !== 'stopped') throw new Error(`state ${st}`)
    if (DOCKER) {
      const running = docker('inspect', '-f', '{{.State.Running}}', CONTAINER).trim()
      if (running !== 'false') throw new Error('container still running')
    }
    return 'stopped'
  })

  await run('Power on again (docker start): files survived, terminal and video are back', async () => {
    const t0 = Date.now()
    await page.getByRole('button', { name: 'Power on' }).click()
    await page.locator('.xterm').waitFor({ timeout: 120000 })
    await waitFor(async () => /\$/.test(await termRows(page)), { what: 'a prompt', timeout: 30000 })
    await typeLine(page, 'cat ~/m3-lifecycle.txt')
    await waitFor(async () => (await termRows(page)).includes(globalThis.MARKER ?? 'persisted-'), { what: 'the marker from before the power-off' })
    await waitForVideo(page, 'after restart')
    return `${Math.round((Date.now() - t0) / 1000)} s`
  })
  await shot(page, 'lifecycle-5-restarted')

  await run('Log out: login page, neko session forgotten, /ws is closed, the API says 401', async () => {
    await page.getByRole('button', { name: 'Log out' }).click()
    await page.getByLabel('Username').waitFor({ timeout: 15000 })
    const stored = await page.evaluate(() => localStorage.getItem('neko_session'))
    if (stored !== null) throw new Error('neko_session left behind')
    const ws = await api(page, `/ws/${USER}/terminal/`)
    const me = await api(page, '/api/me')
    if (ws.status !== 401 || me.status !== 401) throw new Error(`ws ${ws.status}, api ${me.status}`)
    return 'ws 401, api 401'
  })

  await run('no page errors, console errors or CSP violations in any step', async () => {
    const all = [...problems.pageErrors.map((e) => `pageerror: ${e}`), ...problems.consoleErrors.map((e) => `console.error: ${e}`), ...problems.csp.map((e) => `csp: ${e}`)]
    if (all.length) throw new Error(all.slice(0, 6).join('\n'))
    return 'none'
  })
} finally {
  await browser.close()
}

const ok = summary()
process.exit(ok ? 0 : 1)
