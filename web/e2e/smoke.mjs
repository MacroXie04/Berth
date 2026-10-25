// Browser smoke test for the Berth frontend (Playwright + a real Chromium).
//
//   BASE_URL=http://127.0.0.1:18090 node e2e/smoke.mjs        # `npm run e2e`
//
// BASE_URL        where the app is served (SPA + /api + /ws). Default http://127.0.0.1:18090.
// SERVE_DIST=1    instead serve internal/webui/dist locally with the CSP of the Go server (e2e/serve-dist.mjs)
//                 and forward /api and /ws to BASE_URL. For a stack whose backend does not serve
//                 the web page yet. With the Go server serving the page, leave it off.
// E2E_USER, E2E_PASSWORD   an existing user whose workspace is RUNNING (default alice / "correct horse").
// E2E_PARTS       "live,stub" (default), or just one. "live" drives the real stack and needs that
//                 running workspace (it never powers it off); "stub" stubs /api and the sockets
//                 in the browser and only needs the app to be served.
// SCREENSHOT_DIR  write screenshots there.
// CHROMIUM        path to a Chromium binary (default: newest under /opt/pw-browsers).
//
// Exit code 0 when every check passed.

import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { chromium } from 'playwright-core'
import { contrast, createRunner, findChromium, sleep, waitFor } from './lib.mjs'
import { startServer } from './serve-dist.mjs'

const TARGET = process.env.BASE_URL ?? 'http://127.0.0.1:18090'
const USER = process.env.E2E_USER ?? 'alice'
const PASS = process.env.E2E_PASSWORD ?? 'correct horse'
const SHOTS = process.env.SCREENSHOT_DIR
const PARTS = (process.env.E2E_PARTS ?? 'live,stub').split(',')
if (SHOTS) mkdirSync(SHOTS, { recursive: true })

const runner = createRunner()
const { note, summary } = runner
let currentCheck = ''
const check = (name, fn) => {
  currentCheck = name
  return runner.check(name, fn)
}

let stand = null
if (process.env.SERVE_DIST === '1') stand = await startServer({ target: TARGET })
const APP = stand ? stand.url : TARGET
note(`app ${APP}${stand ? ` (local dist + CSP stand-in, API -> ${TARGET})` : ''}; user ${USER}; parts ${PARTS.join(',')}`)

const browser = await chromium.launch({ executablePath: findChromium(), args: ['--no-sandbox'], headless: true })

// ---------- instrumentation shared by all pages ----------

const problems = { csp: [], pageErrors: [], consoleErrors: [] }
let harnessNoise = 0

async function newPage(opts = {}) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 800 }, colorScheme: 'light', ...opts })
  await ctx.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) => {
      const t = e.target
      const el = t && t.tagName ? `${t.tagName.toLowerCase()}.${t.className}` : ''
      console.log(`CSPVIOLATION ${e.violatedDirective} blocked=${e.blockedURI || 'inline'} ${el} at ${e.sourceFile}:${e.lineNumber}`)
    })
  })
  const page = await ctx.newPage()
  page.on('console', (m) => {
    const t = m.text()
    if (t.startsWith('CSPVIOLATION') || /Content Security Policy/i.test(t)) problems.csp.push(`[${currentCheck.slice(0, 50)}] ${t.slice(0, 200)}`)
    else if (m.type() === 'error' && !/Failed to load resource/.test(t)) problems.consoleErrors.push(t.slice(0, 300))
  })
  page.on('pageerror', (e) => {
    // Playwright's own WebSocket mock (routeWebSocket) throws this inside the page when the node side
    // forwards a frame after the page closed its socket. It is not thrown by the app.
    if (/^WebSocket is already in CLOSING or CLOSED state\.$/.test(e.message)) harnessNoise++
    else problems.pageErrors.push(`[${currentCheck.slice(0, 50)}] ${e.message.slice(0, 300)}`)
  })
  return { ctx, page }
}

const shot = (page, name) => (SHOTS ? page.screenshot({ path: join(SHOTS, `${name}.png`) }) : undefined)

const termRows = (page) =>
  page.evaluate(() => Array.from(document.querySelectorAll('.xterm-rows > div'), (d) => d.textContent ?? '').join('\n'))
const termHas = async (page, re) => re.test(await termRows(page))
const termLine = async (page, text) => (await termRows(page)).split('\n').some((l) => l.trim() === text)

async function typeLine(page, text) {
  await page.locator('.term-host').click()
  await page.keyboard.type(text)
  await page.keyboard.press('Enter')
}

const sepValue = async (page) => Number(await page.getByRole('separator').getAttribute('aria-valuenow'))

const bytes = (m) => (typeof m === 'string' ? Buffer.from(m) : Buffer.from(m))
const firstChar = (m) => bytes(m).subarray(0, 1).toString()

// ---------- LIVE: the real stack ----------

async function live() {
  const { ctx, page } = await newPage()

  // Observe (and, for the reconnect checks, disturb) the terminal socket by proxying it.
  const ws = { connections: 0, toServer: [], live: [], refuse: false }
  await page.routeWebSocket(/\/ws\/[^/]+\/terminal\/ws$/, (route) => {
    ws.connections++
    if (ws.refuse) {
      route.close({ code: 1011, reason: 'e2e: refused' })
      return
    }
    const server = route.connectToServer()
    let closed = false
    route.onMessage((m) => {
      ws.toServer.push(bytes(m))
      if (!closed) server.send(m)
    })
    server.onMessage((m) => {
      if (!closed) route.send(m)
    })
    route.onClose(() => {
      closed = true
      server.close()
    })
    server.onClose(() => {
      if (!closed) {
        closed = true
        route.close()
      }
    })
    ws.live.push({ route, server })
  })

  const heartbeats = []
  page.on('response', (r) => {
    if (r.url().endsWith('/api/workspace/heartbeat')) heartbeats.push(r.status())
  })

  await check('anonymous visitor gets the login form (and a CSP header if the server sets one)', async () => {
    const resp = await page.goto(`${APP}/`)
    await page.getByLabel('Username').waitFor()
    if ((await page.title()) !== 'Berth') throw new Error(`title is ${await page.title()}`)
    const csp = resp.headers()['content-security-policy']
    note(`main document CSP: ${csp ?? '(none: the server in front sets no CSP)'}`)
    return csp ? 'CSP present' : 'no CSP header'
  })

  await check('a deep link falls back to the app', async () => {
    const r = await page.goto(`${APP}/some/deep/link`)
    if (r.status() !== 200) throw new Error(`HTTP ${r.status()} for a deep link`)
    await page.getByLabel('Username').waitFor()
    await page.goto(`${APP}/`)
  })

  await check('keyboard: tab order on the login page is tabs (one stop), username, password, button, each with a visible focus ring', async () => {
    const seen = []
    await page.locator('body').click({ position: { x: 5, y: 5 } })
    for (let i = 0; i < 4; i++) {
      await page.keyboard.press('Tab')
      seen.push(
        await page.evaluate(() => {
          const el = document.activeElement
          const cs = getComputedStyle(el)
          return `${el.tagName.toLowerCase()}:${el.getAttribute('role') ?? el.getAttribute('name') ?? el.type ?? ''}:${cs.outlineStyle}/${cs.outlineWidth}`
        }),
      )
    }
    const expectOrder = ['button:tab', 'input:username', 'input:password', 'button:submit']
    seen.forEach((s, i) => {
      if (!s.startsWith(expectOrder[i] ?? '?')) throw new Error(`tab stop ${i + 1} is ${s}, expected ${expectOrder[i]}`)
      if (!/solid\/[2-9]px/.test(s)) throw new Error(`no visible focus ring on tab stop ${i + 1}: ${s}`)
    })
    // arrow keys move between the two tabs
    await page.getByRole('tab', { name: 'Log in' }).focus()
    await page.keyboard.press('ArrowRight')
    if ((await page.getByRole('tab', { name: 'Register' }).getAttribute('aria-selected')) !== 'true') throw new Error('ArrowRight did not select Register')
    await page.keyboard.press('ArrowLeft')
    if ((await page.getByRole('tab', { name: 'Log in' }).getAttribute('aria-selected')) !== 'true') throw new Error('ArrowLeft did not select Log in')
    return seen.join(' > ')
  })

  await check('wrong password: inline server error, still on the login page, no "session expired"', async () => {
    await page.getByLabel('Username').fill(USER)
    await page.getByLabel('Password').fill('definitely wrong password')
    await page.getByRole('button', { name: 'Log in' }).click()
    await page.getByRole('alert').filter({ hasText: 'invalid username or password' }).waitFor()
    if (await page.getByText(/session expired/i).count()) throw new Error('shows "session expired" for a wrong password')
    // The page moves focus to the (emptied) password field; inputs are read-only, not disabled, while the request runs.
    const focused = await page.evaluate(() => document.activeElement?.getAttribute('name') ?? document.activeElement?.tagName)
    if (focused !== 'password') throw new Error(`focus is on ${focused} after a failed login, expected the password field`)
  })

  await check('login shows the running workspace: status bar, terminal and browser panes', async () => {
    await page.getByLabel('Password').fill(PASS)
    await page.getByRole('button', { name: 'Log in' }).click()
    await page.getByRole('separator').waitFor({ timeout: 30000 })
    await page.locator('.badge', { hasText: 'Running' }).waitFor()
    await page.locator('iframe[title="Remote browser"]').waitFor()
    await page.locator('.xterm').waitFor()
    await shot(page, '01-running')
  })

  await check('status bar shows memory and CPU usage', async () => {
    const t = await page.locator('.usage').innerText()
    if (!/Memory\s+\d+(\.\d)? (MiB|GiB)\s+\/\s+\d+(\.\d)? (MiB|GiB)/.test(t) || !/CPU\s+\d+(\.\d)?%/.test(t)) throw new Error(`usage text: ${JSON.stringify(t)}`)
    return t.replace(/\s+/g, ' ')
  })

  await check('heartbeat: the first one goes out at once and is accepted (204)', async () => {
    await waitFor(() => heartbeats.length > 0, { what: 'a heartbeat', timeout: 5000 })
    if (heartbeats[0] !== 204) throw new Error(`heartbeat answered ${heartbeats[0]}`)
  })

  await check('terminal: ttyd handshake (JSON with columns/rows) is the first frame', async () => {
    await waitFor(() => /\$\s*$|\$ /.test(''), { timeout: 1 }).catch(() => {})
    await waitFor(async () => termHas(page, /\$/), { what: 'a shell prompt', timeout: 20000 })
    const first = ws.toServer[0]
    if (!first || first.toString()[0] !== '{') throw new Error(`first frame: ${first?.toString()}`)
    const j = JSON.parse(first.toString())
    if (!(j.columns > 10 && j.rows > 3) || j.AuthToken !== '') throw new Error(`handshake ${first}`)
    return `${j.columns}x${j.rows}`
  })

  await check('terminal: type "echo hello-$((6*7))" and read hello-42 back from the xterm buffer', async () => {
    await typeLine(page, 'echo hello-$((6*7))')
    await waitFor(() => termLine(page, 'hello-42'), { what: 'a line "hello-42"', timeout: 15000 })
    if (!ws.toServer.some((m) => m.toString().startsWith('0e'))) throw new Error('no INPUT frame ("0" + data) was sent')
    await shot(page, '02-terminal')
  })

  await check('terminal: theme colours, true colour and the selection highlight are applied (under the served CSP, which allows inline styles for xterm.js)', async () => {
    const rgb = (c) => c.match(/\d+(\.\d+)?/g).slice(0, 3).map(Number)
    const fg = rgb(await page.evaluate(() => getComputedStyle(document.querySelector('.xterm-rows')).color))
    const ratio = contrast(fg, [20, 23, 31])
    if (ratio < 7) throw new Error(`terminal text colour ${fg} on the dark background has contrast ${ratio.toFixed(1)}:1 (theme style not applied?)`)
    await typeLine(page, "printf '\\e[38;2;255;60;60mTRUECOLOR\\e[0m \\e[48;2;0;90;200mTRUEBG\\e[0m\\n'")
    const spanStyle = (text) =>
      page.evaluate((t) => {
        const el = Array.from(document.querySelectorAll('.xterm-rows span')).filter((e) => e.textContent === t).pop()
        if (!el) return null
        const cs = getComputedStyle(el)
        return { color: cs.color, bg: cs.backgroundColor }
      }, text)
    await waitFor(async () => (await spanStyle('TRUECOLOR'))?.color === 'rgb(255, 60, 60)', { what: 'true-colour text' })
    await waitFor(async () => (await spanStyle('TRUEBG'))?.bg === 'rgb(0, 90, 200)', { what: 'true-colour background' })
    // double-click the word: the selection highlight is a per-cell background
    const box = await page.evaluate(() => {
      const el = Array.from(document.querySelectorAll('.xterm-rows span')).filter((e) => e.textContent === 'TRUECOLOR').pop()
      const r = el.getBoundingClientRect()
      return { x: r.x + r.width / 2, y: r.y + r.height / 2 }
    })
    await page.mouse.dblclick(box.x, box.y)
    await waitFor(
      async () => {
        const st = await spanStyle('TRUECOLOR')
        return st && st.bg !== 'rgba(0, 0, 0, 0)' && st.bg !== 'rgb(0, 90, 200)'
      },
      { what: 'a selection highlight on the double-clicked word' },
    )
    await shot(page, '02b-colours')
    return `text ${fg.join(',')} (${ratio.toFixed(1)}:1)`
  })

  await check('terminal: UTF-8 output (é € emoji) arrives intact', async () => {
    await typeLine(page, 'printf "caf\\xc3\\xa9 \\xe2\\x82\\xac \\xf0\\x9f\\x99\\x82\\n"')
    await waitFor(() => termLine(page, 'café € 🙂'), { what: 'the UTF-8 line', timeout: 15000 })
  })

  await check('terminal: an emoji takes two cells like in the shell (Unicode 11), so editing around it does not eat the prompt', async () => {
    // bash/readline counts U+1F600 as 2 columns. If the page counted 1, moving left over the emoji would land one column
    // too far and the next character would overwrite the space after the prompt ("$X😀" instead of "$ X😀").
    await page.locator('.term-host').click()
    await page.keyboard.insertText('\u{1F600}')
    await page.keyboard.press('ArrowLeft')
    await page.keyboard.type('X')
    const line = await waitFor(
      async () => (await termRows(page)).split('\n').map((l) => l.trimEnd()).filter((l) => /\$ ?X/.test(l)).pop(),
      { what: 'the edited prompt line' },
    )
    await page.keyboard.press('Control+C') // abandon the line: a fresh, empty prompt
    if (!/\$ X😀$/.test(line)) throw new Error(`prompt line is ${JSON.stringify(line)}`)
    return line
  })

  await check('keyboard: Shift+Esc leaves the terminal (nothing is sent to the shell); Tab reaches the browser pane, Shift+Tab comes back', async () => {
    await page.locator('.term-host').click()
    const hint = await page.locator('.pane__hint').innerText()
    if (hint !== 'Shift+Esc leaves the terminal') throw new Error(`hint is ${JSON.stringify(hint)}`)
    const described = await page.evaluate(() => {
      const ta = document.querySelector('.xterm-helper-textarea')
      const byId = (id) => document.getElementById(id ?? '')?.textContent
      return { input: byId(ta?.getAttribute('aria-describedby')), pane: byId(document.querySelector('.pane--terminal')?.getAttribute('aria-describedby')) }
    })
    if (described.input !== hint || described.pane !== hint) throw new Error(`accessible description: ${JSON.stringify(described)}`)
    const where = () =>
      page.evaluate(() => {
        const a = document.activeElement
        return a?.getAttribute('role') ?? `${a?.tagName.toLowerCase()}${a?.getAttribute('title') ? `[${a.getAttribute('title')}]` : ''}.${a?.className}`
      })
    if ((await where()) !== 'textarea.xterm-helper-textarea') throw new Error(`focus starts on ${await where()}`)
    const sent = ws.toServer.length
    await page.keyboard.press('Shift+Escape')
    if ((await where()) !== 'separator') throw new Error(`after Shift+Esc focus is on ${await where()}`)
    await sleep(300)
    if (ws.toServer.length !== sent) throw new Error(`Shift+Esc reached the shell: ${ws.toServer.slice(sent).map((m) => JSON.stringify(m.toString()))}`)
    await page.keyboard.press('Tab')
    if ((await where()) !== 'iframe[Remote browser].browser') throw new Error(`Tab from the divider goes to ${await where()}`)
    await page.keyboard.press('Shift+Tab')
    if ((await where()) !== 'separator') throw new Error(`Shift+Tab goes to ${await where()}`)
    await page.keyboard.press('Shift+Tab')
    if ((await where()) !== 'textarea.xterm-helper-textarea') throw new Error(`Shift+Tab again goes to ${await where()}`)
  })

  await check('terminal: 1 MB of multi-byte output keeps the connection and ends cleanly, no character is split', async () => {
    const before = ws.connections
    await typeLine(page, "yes 'é€🙂' | head -n 100000; echo flood-done-$((10*10))")
    await waitFor(() => termLine(page, 'flood-done-100'), { what: 'the end of the flood', timeout: 60000 })
    if (ws.connections !== before) throw new Error('the terminal reconnected during the flood')
    const rows = await termRows(page)
    if (rows.includes('�')) throw new Error('replacement characters on screen: a character was split')
    const pause = ws.toServer.filter((m) => m.length === 1 && m.toString() === '2').length
    const resume = ws.toServer.filter((m) => m.length === 1 && m.toString() === '3').length
    return `PAUSE x${pause}, RESUME x${resume}`
  })

  await check('divider (keyboard): arrows, Shift, Home/End, double-click reset', async () => {
    const sep = page.getByRole('separator')
    await sep.focus()
    const v0 = await sepValue(page)
    await page.keyboard.press('ArrowRight')
    if ((await sepValue(page)) !== v0 + 5) throw new Error(`ArrowRight: ${v0} -> ${await sepValue(page)}`)
    await page.keyboard.press('Shift+ArrowLeft')
    if ((await sepValue(page)) !== v0 - 5) throw new Error(`Shift+ArrowLeft -> ${await sepValue(page)}`)
    await page.keyboard.press('Home')
    const min = Number(await sep.getAttribute('aria-valuemin'))
    if ((await sepValue(page)) !== min) throw new Error(`Home -> ${await sepValue(page)}, min ${min}`)
    await page.keyboard.press('End')
    const max = Number(await sep.getAttribute('aria-valuemax'))
    if ((await sepValue(page)) !== max) throw new Error(`End -> ${await sepValue(page)}, max ${max}`)
    await sep.dblclick()
    if ((await sepValue(page)) !== 50) throw new Error(`double-click -> ${await sepValue(page)}`)
    const ring = await sep.evaluate((el) => getComputedStyle(el).outlineStyle)
    if (ring !== 'solid') throw new Error('no focus ring on the separator after keyboard use')
    return `range ${min}..${max}`
  })

  let colsBefore = 0
  const ttySize = async (tag) => {
    await typeLine(page, `echo ${tag}-$(stty size | tr ' ' x)`)
    const re = new RegExp(`^${tag}-(\\d+)x(\\d+)$`)
    const line = await waitFor(
      async () => (await termRows(page)).split('\n').map((l) => l.trim()).filter((l) => re.test(l)).pop(),
      { what: `${tag} size line` },
    )
    const [, rows, cols] = line.match(re)
    return { rows: Number(rows), cols: Number(cols) }
  }

  await check('divider (mouse): drag across the iframe follows the pointer, persists, and the terminal is resized', async () => {
    colsBefore = (await ttySize('sizea')).cols
    const box = await page.getByRole('separator').boundingBox()
    const y = box.y + box.height / 2
    const x0 = box.x + box.width / 2
    const resizeFrames = ws.toServer.filter((m) => m.toString().startsWith('1{')).length
    await page.mouse.move(x0, y)
    await page.mouse.down()
    if ((await page.locator('.split__shield').count()) !== 1) throw new Error('no drag shield while the button is down')
    await page.mouse.move(x0 + 150, y, { steps: 6 })
    await page.mouse.move(1380, y, { steps: 8 }) // far over the browser iframe
    const max = Number(await page.getByRole('separator').getAttribute('aria-valuemax'))
    const during = await sepValue(page)
    await page.mouse.up()
    if (during !== max) throw new Error(`dragging over the iframe stopped at ${during}, expected the maximum ${max}`)
    if ((await page.locator('.split__shield').count()) !== 0) throw new Error('shield still there after release')
    const stored = await page.evaluate(() => localStorage.getItem('berth.split'))
    if (!stored || Math.abs(Number(stored) - max / 100) > 0.01) throw new Error(`localStorage berth.split = ${stored}`)
    const after = await waitFor(
      async () => {
        const s = await ttySize('sizeb')
        return s.cols > colsBefore ? s : null
      },
      { what: 'a wider terminal', timeout: 15000, interval: 500 },
    )
    const resizeAfter = ws.toServer.filter((m) => m.toString().startsWith('1{')).length
    if (resizeAfter <= resizeFrames) throw new Error('no RESIZE frame (1{...}) reached the server')
    // The drag produced one debounced burst, not a message per pixel.
    if (resizeAfter - resizeFrames > 8) throw new Error(`${resizeAfter - resizeFrames} resize frames for one drag`)
    await shot(page, '03-dragged')
    return `cols ${colsBefore} -> ${after.cols}, ${resizeAfter - resizeFrames} resize frame(s)`
  })

  await check('reload keeps the user on the workspace view, with the saved split', async () => {
    const saved = await sepValue(page)
    await page.reload()
    await page.getByRole('separator').waitFor({ timeout: 30000 })
    if ((await sepValue(page)) !== saved) throw new Error(`split ${saved} -> ${await sepValue(page)} after reload`)
    if (await page.getByLabel('Username').count()) throw new Error('login page after reload')
    await page.getByRole('separator').dblclick()
  })

  await check('browser pane: neko loads inside the iframe under /ws/<user>/browser/ and shows its UI', async () => {
    const el = page.locator('iframe[title="Remote browser"]')
    const attrs = await el.evaluate((f) => ({
      src: f.getAttribute('src'),
      allow: f.getAttribute('allow'),
      referrerpolicy: f.getAttribute('referrerpolicy'),
      sandbox: f.hasAttribute('sandbox'),
    }))
    if (!attrs.src.startsWith(`/ws/${USER}/browser/?`)) throw new Error(`iframe src ${attrs.src.replace(/pwd=[^&]*/, 'pwd=***')}`)
    if (attrs.allow !== 'clipboard-write; fullscreen' || attrs.referrerpolicy !== 'no-referrer' || attrs.sandbox) {
      throw new Error(`iframe attributes ${JSON.stringify({ ...attrs, src: '...' })}`)
    }
    const frame = page.frames().find((f) => f.url().includes(`/ws/${USER}/browser/`))
    if (!frame) throw new Error('no frame on /ws/<user>/browser/')
    await frame.waitForSelector('video', { timeout: 40000 })
    const info = await frame.evaluate(() => ({ title: document.title, text: document.body.innerText.slice(0, 80) }))
    const pwd = new URL(attrs.src, 'http://x').searchParams.get('pwd')
    if (pwd) {
      const leaks = await page.evaluate((secret) => {
        const where = []
        if (document.body.innerText.includes(secret)) where.push('page text')
        for (const el of document.querySelectorAll('*')) {
          for (const a of el.attributes) {
            if (a.value.includes(secret) && !(el.tagName === 'IFRAME' && a.name === 'src')) where.push(`${el.tagName}[${a.name}]`)
          }
        }
        for (const store of [localStorage, sessionStorage]) {
          for (let i = 0; i < store.length; i++) if ((store.getItem(store.key(i)) ?? '').includes(secret)) where.push('web storage')
        }
        return where
      }, pwd)
      if (leaks.length) throw new Error(`neko password appears outside the iframe src: ${leaks.join(', ')}`)
    }
    await shot(page, '04-neko')
    return `neko title ${JSON.stringify(info.title)}`
  })

  await check('terminal reconnects by itself after a dropped connection (overlay, backoff, manual Reconnect)', async () => {
    // 1. the server side goes away: overlay appears, a new (real) connection is made by itself
    const n0 = ws.connections
    for (const c of ws.live) {
      c.route.close()
      c.server.close()
    }
    ws.live.length = 0
    await page.getByText('Disconnected, reconnecting…').waitFor({ timeout: 5000 })
    await waitFor(() => ws.connections > n0, { what: 'an automatic reconnect', timeout: 5000 })
    await page.getByText('Disconnected, reconnecting…').waitFor({ state: 'hidden', timeout: 10000 })
    await waitFor(async () => termHas(page, /\$/), { what: 'a fresh prompt' })
    // 2. connections keep failing: attempts back off, the overlay stays, Reconnect tries at once
    ws.refuse = true
    for (const c of ws.live) {
      c.route.close()
      c.server.close()
    }
    ws.live.length = 0
    await page.getByText('Disconnected, reconnecting…').waitFor()
    const n1 = ws.connections
    await sleep(3200)
    const attempts = ws.connections - n1
    // 0.5 s, 1 s, 2 s of backoff: about 3 attempts in 3.2 s, far fewer than a hot loop
    if (attempts < 2 || attempts > 5) throw new Error(`${attempts} attempts in 3.2 s (expected backoff, 2-5)`)
    const n2 = ws.connections
    await page.getByRole('button', { name: 'Reconnect' }).click()
    await waitFor(() => ws.connections > n2, { what: 'an immediate attempt after Reconnect', timeout: 1000, interval: 20 })
    // 3. the server is back: Reconnect succeeds
    ws.refuse = false
    await page.getByRole('button', { name: 'Reconnect' }).click().catch(() => {})
    await page.getByText('Disconnected, reconnecting…').waitFor({ state: 'hidden', timeout: 15000 })
    await typeLine(page, 'echo back-$((1+1))')
    await waitFor(() => termLine(page, 'back-2'), { what: 'a working shell after reconnect' })
    return `${attempts} attempts while refused`
  })

  await check('layout at 360 px: stacked panes, horizontal separator, no sideways scroll, controls reachable', async () => {
    const connectionsBefore = ws.connections
    const neko = page.frames().find((f) => f.url().includes(`/ws/${USER}/browser/`))
    await neko.evaluate(() => (window.__e2eMarker = 'still-here')) // lost if the iframe reloads
    await page.setViewportSize({ width: 360, height: 720 })
    await page.waitForTimeout(400)
    const sep = page.getByRole('separator')
    if ((await sep.getAttribute('aria-orientation')) !== 'horizontal') throw new Error('separator is not horizontal when stacked')
    const m = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }))
    if (m.sw > m.cw) throw new Error(`page scrolls sideways: scrollWidth ${m.sw} > ${m.cw}`)
    const t = await page.locator('.pane--terminal').boundingBox()
    const b = await page.locator('.pane--browser').boundingBox()
    if (!(t.y + t.height <= b.y + 1)) throw new Error(`terminal ${JSON.stringify(t)} is not above browser ${JSON.stringify(b)}`)
    if (t.height < 130 || b.height < 130) throw new Error(`panes too small: ${t.height}, ${b.height}`)
    for (const name of ['Power off', 'Log out']) {
      const bx = await page.getByRole('button', { name }).boundingBox()
      if (!bx || bx.x < 0 || bx.x + bx.width > 360) throw new Error(`${name} is outside the 360 px viewport: ${JSON.stringify(bx)}`)
    }
    await sep.focus()
    const v = await sepValue(page)
    await page.keyboard.press('ArrowRight')
    if ((await sepValue(page)) !== v) throw new Error('Left/Right must not move a horizontal separator')
    await page.keyboard.press('ArrowDown')
    if ((await sepValue(page)) !== v + 5) throw new Error('ArrowDown did not move the horizontal separator')
    await page.keyboard.press('Home')
    await shot(page, '05-narrow')
    // drag with a touch-like pointer along the vertical axis
    const box = await sep.boundingBox()
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    await page.mouse.down()
    await page.mouse.move(box.x + box.width / 2, box.y + 150, { steps: 5 })
    await page.mouse.up()
    if ((await sepValue(page)) <= 25) throw new Error('vertical drag did not move the separator')
    await page.getByRole('separator').dblclick()
    await page.setViewportSize({ width: 1400, height: 800 })
    await page.waitForTimeout(400)
    // flipping between the two layouts must not reconnect the terminal or reload the browser frame
    if (ws.connections !== connectionsBefore) throw new Error('the terminal reconnected when the layout changed')
    if ((await neko.evaluate(() => window.__e2eMarker).catch(() => null)) !== 'still-here') throw new Error('the browser iframe reloaded when the layout changed')
    return `terminal ${Math.round(t.height)} px, browser ${Math.round(b.height)} px`
  })

  await check('layout on a phone held sideways stays side by side without scrolling the page; a tall phone stacks', async () => {
    const connectionsBefore = ws.connections
    const out = []
    for (const [w, h, stacked] of [
      [667, 375, false],
      [740, 360, false],
      [360, 740, true],
      [800, 480, true],
    ]) {
      await page.setViewportSize({ width: w, height: h })
      await page.waitForTimeout(400)
      const m = await page.evaluate(() => {
        const box = (sel) => {
          const b = document.querySelector(sel).getBoundingClientRect()
          return { x: b.x, y: b.y, w: b.width, h: b.height, right: b.right, bottom: b.bottom }
        }
        return {
          scrollH: document.documentElement.scrollHeight,
          scrollW: document.documentElement.scrollWidth,
          innerH: window.innerHeight,
          innerW: window.innerWidth,
          orientation: document.querySelector('[role=separator]').getAttribute('aria-orientation'),
          term: box('.pane--terminal'),
          browser: box('.pane--browser'),
        }
      })
      const at = `${w}x${h}`
      if (m.scrollH > m.innerH || m.scrollW > m.innerW) throw new Error(`${at}: the page scrolls (${m.scrollW}x${m.scrollH} in ${m.innerW}x${m.innerH})`)
      if (m.orientation !== (stacked ? 'horizontal' : 'vertical')) throw new Error(`${at}: separator is ${m.orientation}`)
      for (const [name, b] of [['terminal', m.term], ['browser', m.browser]]) {
        if (b.right > m.innerW + 1 || b.bottom > m.innerH + 1) throw new Error(`${at}: the ${name} pane is cut off: ${JSON.stringify(b)}`)
        if (b.h < 140 || b.w < 240) throw new Error(`${at}: the ${name} pane is too small: ${Math.round(b.w)}x${Math.round(b.h)}`)
      }
      const sideBySide = m.term.right <= m.browser.x + 1
      if (sideBySide === stacked) throw new Error(`${at}: expected ${stacked ? 'stacked' : 'side by side'} panes, terminal ${JSON.stringify(m.term)} browser ${JSON.stringify(m.browser)}`)
      out.push(`${at} ${stacked ? 'stacked' : 'row'} ${Math.round(m.term.h)}px`)
      if (w === 667) await shot(page, '05b-landscape')
    }
    await page.setViewportSize({ width: 1400, height: 800 })
    await page.waitForTimeout(400)
    if (ws.connections !== connectionsBefore) throw new Error('the terminal reconnected when the layout changed')
    return out.join(', ')
  })

  await check('contrast of text is at least 4.5:1 in light and dark mode', async () => {
    const out = []
    for (const scheme of ['light', 'dark']) {
      await page.emulateMedia({ colorScheme: scheme })
      await page.waitForTimeout(150)
      const pairs = await page.evaluate(() => {
        const parse = (c) => {
          const m = c.match(/rgba?\(([^)]+)\)/)
          if (!m) return null
          const p = m[1].split(/[ ,/]+/).filter(Boolean).map(Number)
          return { rgb: p.slice(0, 3), a: p[3] ?? 1 }
        }
        const bgOf = (el) => {
          for (let e = el; e; e = e.parentElement) {
            const c = parse(getComputedStyle(e).backgroundColor)
            if (c && c.a > 0.99) return c.rgb
          }
          return [255, 255, 255]
        }
        const sel = ['.statusbar__brand', '.statusbar__user', '.badge', '.health--healthy', '.usage dt', '.usage dd', '.pane__title', '.pane__sub', '.pane__hint', '.btn:not(:disabled)', '.btn--quiet']
        return sel
          .map((s) => document.querySelector(s))
          .map((el, i) => {
            if (!el) return null
            const c = parse(getComputedStyle(el).color)
            return { sel: sel[i], fg: c.rgb, bg: bgOf(el) }
          })
          .filter(Boolean)
      })
      for (const p of pairs) {
        const r = contrast(p.fg, p.bg)
        if (r < 4.5) throw new Error(`${scheme}: ${p.sel} contrast ${r.toFixed(2)} (fg ${p.fg}, bg ${p.bg})`)
        out.push(r)
      }
      await shot(page, `06-${scheme}`)
    }
    await page.emulateMedia({ colorScheme: 'light' })
    return `min ${Math.min(...out).toFixed(1)}:1 over ${out.length} samples`
  })

  await check('session expiry: dropping the API cookie sends the user back to the login page with a message', async () => {
    await ctx.clearCookies({ name: 'berth_session' })
    await page.getByText('Your session expired. Please log in again.').waitFor({ timeout: 25000 })
    await page.getByLabel('Username').waitFor()
    const n = ws.connections
    await sleep(4000)
    if (ws.connections !== n) throw new Error('the terminal kept reconnecting after the session ended')
  })

  await check('log out ends the session: login page, API answers 401, no reconnect loop', async () => {
    await page.getByLabel('Username').fill(USER)
    await page.getByLabel('Password').fill(PASS)
    await page.getByRole('button', { name: 'Log in' }).click()
    await page.getByRole('separator').waitFor({ timeout: 30000 })
    await page.getByRole('button', { name: 'Log out' }).click()
    await page.getByRole('button', { name: 'Log in' }).waitFor()
    const me = await ctx.request.get(`${APP}/api/me`)
    if (me.status() !== 401) throw new Error(`GET /api/me after logout: ${me.status()}`)
    const n = ws.connections
    await sleep(3000)
    if (ws.connections !== n) throw new Error('terminal reconnect attempts after logout')
  })

  await ctx.close()
}

// ---------- STUB: the browser with a scripted API and fake sockets ----------

// Advance the fake clock in small steps with real pauses in between: the app's status poll is a
// timer chain whose next timer is armed only when the (real) network answer has arrived.
async function tick(page, ms) {
  for (let left = ms; left > 0; left -= 500) {
    await page.clock.runFor(Math.min(500, left))
    await sleep(20)
  }
  await sleep(150)
}

function makeStub(initial = {}) {
  const s = {
    state: 'offline',
    health: 'none',
    startError: null, // {status, error}
    pollsWhileStarting: 0,
    requests: [],
    heartbeats: 0,
    expired: false,
    user: 'alice', // whose session cookie the "browser" holds: /api/me and every workspace answer name him
    registerError: null, // {status, error}
    wsConnections: 0,
    wsClosed: 0,
    wsFrames: [],
    wsUrls: [],
    sockets: [],
    ...initial,
  }
  // Server -> terminal: an OUTPUT frame on the newest fake ttyd socket.
  s.say = (text) => s.sockets[s.sockets.length - 1].send(Buffer.concat([Buffer.from('0'), Buffer.from(text)]))
  s.workspace = () => ({
    state: s.state,
    health: s.health,
    mem_limit_mb: 2048,
    cpu_limit: 2,
    last_active_at: null,
    last_stopped_at: null,
    usage: s.state === 'running' ? { cpu_percent: 3.2, mem_bytes: 300 * 2 ** 20, mem_limit_bytes: 2 * 2 ** 30, pids: 20 } : null,
    ...(s.state === 'running' || s.state === 'starting' ? { neko_password: 'stub-neko-pw' } : {}),
  })
  // What the real backend answers: the account the cookie belongs to, and its workspace.
  s.reply = () => ({ username: s.user, workspace: s.workspace() })
  return s
}

async function wireStub(page, s) {
  const json = (route, status, body) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  await page.route('**/api/**', async (route) => {
    const req = route.request()
    const path = new URL(req.url()).pathname
    s.requests.push({ method: req.method(), path, contentType: req.headers()['content-type'] ?? null, body: req.postData() })
    if (path === '/api/auth/register') {
      if (s.registerError) return json(route, s.registerError.status, { error: s.registerError.error })
      return json(route, 201, { user: { id: 2, username: 'bob', is_admin: false }, workspace: {} })
    }
    if (path === '/api/auth/login') {
      s.user = 'bob'
      return json(route, 200, { user: { id: 2, username: 'bob', is_admin: false } })
    }
    if (path === '/api/auth/logout') return route.fulfill({ status: 204 })
    if (s.expired) return json(route, 401, { error: 'not logged in' })
    if (path === '/api/me') return json(route, 200, { user: { id: 1, username: s.user, is_admin: false }, workspace: { state: s.state, mem_limit_mb: 2048, cpu_limit: 2, last_active_at: null } })
    if (path === '/api/workspace/heartbeat') {
      s.heartbeats++
      return route.fulfill({ status: 204 })
    }
    if (path === '/api/workspace/start') {
      if (s.startError) return json(route, s.startError.status, { error: s.startError.error })
      s.state = 'starting'
      s.pollsWhileStarting = 0
      return json(route, 200, s.reply())
    }
    if (path === '/api/workspace/stop') {
      s.state = 'stopped'
      s.health = 'none'
      return json(route, 200, s.reply())
    }
    if (path === '/api/workspace') {
      if (s.state === 'starting' && ++s.pollsWhileStarting >= 2) {
        s.state = 'running'
        s.health = 'healthy'
      }
      return json(route, 200, s.reply())
    }
    return json(route, 404, { error: 'not found' })
  })
  await page.route('**/ws/*/browser/**', (route) =>
    route.fulfill({ contentType: 'text/html', body: '<!doctype html><meta charset=utf-8><title>fake neko</title><p>fake neko</p>' }),
  )
  // A tiny ttyd: handshake, then echo input back as output.
  await page.routeWebSocket(/\/ws\/[^/]+\/terminal\/ws$/, (route) => {
    s.wsConnections++
    s.wsUrls.push(route.url())
    s.sockets.push(route)
    let greeted = false
    route.onClose(() => s.wsClosed++)
    route.onMessage((m) => {
      const b = bytes(m)
      s.wsFrames.push(b)
      if (!greeted && b[0] === 0x7b) {
        greeted = true
        route.send(Buffer.concat([Buffer.from('1'), Buffer.from('fake ttyd')]))
        route.send(Buffer.concat([Buffer.from('0'), Buffer.from('fake$ ')]))
      } else if (b[0] === 0x30) {
        route.send(Buffer.concat([Buffer.from('0'), b.subarray(1)]))
      }
    })
  })
}

async function stub() {
  await check('stub: offline workspace shows the call to action; Power on -> progress -> panes', async () => {
    const s = makeStub({ state: 'offline' })
    const { ctx, page } = await newPage()
    await wireStub(page, s)
    await page.goto(`${APP}/`)
    // The app asks /api/me first; the stub says logged in.
    await page.getByRole('button', { name: 'Power on' }).waitFor()
    if (!(await page.getByText(/your files are kept/i).count())) throw new Error('no data-kept sentence')
    // The stub's workspace was never started (no timestamps): the text must not claim a container was removed.
    if (!(await page.getByText('Your workspace has not been started yet.', { exact: false }).count())) throw new Error('no first-run text')
    if (await page.getByText(/removed/i).count()) throw new Error('claims that a container was removed')
    if (await page.locator('iframe').count()) throw new Error('iframe mounted while offline')
    await shot(page, 's1-cta')

    // 503 first: readable message, button stays
    s.startError = { status: 503, error: 'the server has no capacity for another workspace' }
    await page.getByRole('button', { name: 'Power on' }).click()
    await page.getByRole('alert').filter({ hasText: /no capacity/ }).waitFor()
    await page.getByRole('button', { name: 'Power on' }).waitFor()
    // 409: busy
    s.startError = { status: 409, error: 'busy' }
    await page.getByRole('button', { name: 'Power on' }).click()
    await page.getByRole('alert').filter({ hasText: /busy/i }).waitFor()
    // then it works
    s.startError = null
    await page.getByRole('button', { name: 'Power on' }).click()
    await page.getByRole('heading', { name: /Starting your workspace/ }).waitFor()
    if (await page.locator('iframe').count()) throw new Error('iframe mounted while starting')
    await sleep(1200)
    await shot(page, 's2-starting')
    await page.getByRole('separator').waitFor({ timeout: 15000 })

    const start = s.requests.filter((r) => r.path === '/api/workspace/start')
    if (start.length !== 3 || start.some((r) => r.method !== 'POST' || r.contentType !== 'application/json')) {
      throw new Error(`start requests: ${JSON.stringify(start)}`)
    }
    const src = await page.locator('iframe[title="Remote browser"]').getAttribute('src')
    if (src !== `/ws/alice/browser/?usr=alice&pwd=stub-neko-pw&embed=1`) throw new Error(`iframe src ${src}`)
    if ((await page.locator('body').innerText()).includes('stub-neko-pw')) throw new Error('neko password visible in the page text')
    const frame = page.frames().find((f) => f.url().includes('/ws/alice/browser/'))
    if (!frame) throw new Error('iframe did not navigate')

    // fake ttyd: handshake JSON, title, echo
    await waitFor(() => s.wsFrames.length > 0, { what: 'handshake' })
    const hs = JSON.parse(s.wsFrames[0].toString())
    if (!(hs.columns > 10 && hs.rows > 3)) throw new Error(`handshake ${JSON.stringify(hs)}`)
    await page.getByText('fake ttyd').waitFor()
    await waitFor(() => termLine(page, 'fake$'), { what: 'fake prompt' })
    await typeLine(page, 'abc')
    await waitFor(() => termHas(page, /abc/), { what: 'echoed input' })
    await shot(page, 's3-panes')

    // Power off: inline confirmation (no dialog), Cancel keeps everything
    let dialogs = 0
    page.on('dialog', (d) => {
      dialogs++
      d.dismiss()
    })
    await page.getByRole('button', { name: 'Power off' }).click()
    await page.getByRole('group', { name: /confirm power off/i }).waitFor()
    await page.getByRole('button', { name: 'Cancel' }).click()
    if (await page.getByRole('group', { name: /confirm power off/i }).count()) throw new Error('confirmation still open after Cancel')
    if (s.requests.some((r) => r.path === '/api/workspace/stop')) throw new Error('stop was sent by Cancel')
    await page.getByRole('button', { name: 'Power off' }).click()
    await page.getByRole('button', { name: /yes, power off/i }).click()
    await page.getByRole('button', { name: 'Power on' }).waitFor()
    if (dialogs) throw new Error('window.confirm was used')
    if (await page.locator('iframe, .xterm').count()) throw new Error('panes still mounted after power off')
    await waitFor(() => s.wsClosed >= 1, { what: 'terminal socket closed' })
    const conns = s.wsConnections
    await sleep(4000)
    if (s.wsConnections !== conns) throw new Error('terminal reconnected after the user powered off')
    const stop = s.requests.filter((r) => r.path === '/api/workspace/stop')
    if (stop.length !== 1 || stop[0].contentType !== 'application/json') throw new Error(`stop requests ${JSON.stringify(stop)}`)
    await ctx.close()
  })

  await check('stub: running but unhealthy -> warning banner, panes stay', async () => {
    const s = makeStub({ state: 'running', health: 'unhealthy' })
    const { ctx, page } = await newPage()
    await wireStub(page, s)
    await page.goto(`${APP}/`)
    await page.getByRole('separator').waitFor()
    await page.getByRole('alert').filter({ hasText: /health check is failing/i }).waitFor()
    if (!(await page.locator('iframe').count()) || !(await page.locator('.xterm').count())) throw new Error('panes missing')
    await shot(page, 's4-unhealthy')
    await ctx.close()
  })

  await check('stub: polling, heartbeat cadence, hidden tab, stop by the reaper and session expiry (fake clock)', async () => {
    const s = makeStub({ state: 'running', health: 'healthy' })
    const { ctx, page } = await newPage()
    await page.clock.install()
    await wireStub(page, s)
    await page.goto(`${APP}/`)
    await page.getByRole('separator').waitFor()
    await waitFor(() => s.heartbeats >= 1, { what: 'the first heartbeat' })
    const polls = () => s.requests.filter((r) => r.path === '/api/workspace' && r.method === 'GET').length
    const p0 = polls()
    const h0 = s.heartbeats
    await tick(page, 31_000)
    if (s.heartbeats !== h0 + 1) throw new Error(`heartbeats after 31 s: ${s.heartbeats - h0} (expected 1)`)
    if (polls() - p0 < 2 || polls() - p0 > 4) throw new Error(`status polls in 31 s: ${polls() - p0} (expected ~3 at 10 s)`)

    // hidden tab: heartbeats go on (an open page is use), polling slows to once a
    // minute; visible again: one poll at once and no extra heartbeat
    await page.evaluate(() => {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' })
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await sleep(100)
    const h1 = s.heartbeats
    const p1 = polls()
    await tick(page, 61_000)
    const beats = s.heartbeats - h1
    const hiddenPolls = polls() - p1
    if (beats < 1 || beats > 3) throw new Error(`a hidden tab sent ${beats} heartbeats in 61 s (expected 2, one per 30 s)`)
    if (hiddenPolls < 1 || hiddenPolls > 2) throw new Error(`a hidden tab polled ${hiddenPolls} times in 61 s (expected 1, once a minute)`)
    await page.evaluate(() => {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' })
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await waitFor(() => polls() === p1 + hiddenPolls + 1, { what: 'a poll on becoming visible' })
    await sleep(300)
    if (s.heartbeats !== h1 + beats) throw new Error('becoming visible must not add a heartbeat')

    // the reaper stops it: the next poll notices, panes go, call to action returns, no more heartbeats
    s.state = 'stopped'
    s.health = 'none'
    await tick(page, 10_500)
    await page.getByRole('button', { name: 'Power on' }).waitFor({ timeout: 10000 })
    if (await page.locator('iframe, .xterm').count()) throw new Error('panes stay after the workspace stopped')
    const h2 = s.heartbeats
    const p2 = polls()
    await tick(page, 60_000)
    if (s.heartbeats !== h2 || polls() !== p2) throw new Error('heartbeats or polls continue while stopped')

    await ctx.close()
  })

  await check('stub: Escape and Tab still reach the shell; Shift+Esc does not, and moves focus to the divider', async () => {
    const s = makeStub({ state: 'running', health: 'healthy' })
    const { ctx, page } = await newPage()
    await wireStub(page, s)
    await page.goto(`${APP}/`)
    await page.getByRole('separator').waitFor()
    await waitFor(() => termLine(page, 'fake$'), { what: 'fake prompt' })
    await page.locator('.term-host').click()
    const frames = () => s.wsFrames.map((f) => f.toString('latin1'))
    const active = () => page.evaluate(() => document.activeElement?.getAttribute('role') ?? document.activeElement?.tagName.toLowerCase())
    const n = s.wsFrames.length
    await page.keyboard.press('Escape')
    await page.keyboard.press('Tab')
    await waitFor(() => frames().slice(n).join('|') === '0\x1b|0\t', { what: 'Escape and Tab as INPUT frames' })
    if ((await active()) !== 'textarea') throw new Error(`Tab/Escape moved focus to ${await active()}`)
    const m = s.wsFrames.length
    await page.keyboard.press('Shift+Escape')
    if ((await active()) !== 'separator') throw new Error(`Shift+Esc: focus is on ${await active()}`)
    await sleep(300)
    if (s.wsFrames.length !== m) throw new Error(`Shift+Esc reached the shell: ${JSON.stringify(frames().slice(m))}`)
    await page.keyboard.press('Tab')
    if ((await active()) !== 'iframe') throw new Error(`Tab from the divider: focus is on ${await active()}`)
    await ctx.close()
  })

  await check('stub: an emoji advances the cursor by two cells (Unicode 11); a letter by one', async () => {
    const s = makeStub({ state: 'running', health: 'healthy' })
    const { ctx, page } = await newPage()
    await wireStub(page, s)
    await page.goto(`${APP}/`)
    await page.getByRole('separator').waitFor()
    await waitFor(() => termLine(page, 'fake$'), { what: 'fake prompt' })
    // Print on a fresh line, then ask the terminal where its cursor is (DSR, ESC[6n): it answers ESC[row;colR as input.
    const column = async (text) => {
      const n = s.wsFrames.length
      s.say(`\r\n${text}\x1b[6n`)
      const frame = await waitFor(() => s.wsFrames.slice(n).map((f) => f.toString('latin1')).find((f) => /^0\x1b\[\d+;\d+R$/.test(f)), { what: 'a cursor position report' })
      return Number(/;(\d+)R$/.exec(frame)[1])
    }
    const letter = await column('x')
    const emoji = await column('\u{1F600}')
    const cjk = await column('\u6f22')
    if (letter !== 2) throw new Error(`cursor column after a letter: ${letter}, expected 2`)
    if (emoji !== 3) throw new Error(`cursor column after an emoji: ${emoji}, expected 3 (it took ${emoji - 1} cell)`)
    if (cjk !== 3) throw new Error(`cursor column after a CJK character: ${cjk}, expected 3`)
    await ctx.close()
  })

  await check('stub: a status answer for another account (another tab logged in as someone else) sends the page back to /api/me', async () => {
    const s = makeStub({ state: 'running', health: 'healthy' })
    const { ctx, page } = await newPage()
    await page.clock.install()
    await wireStub(page, s)
    await page.goto(`${APP}/`)
    await page.getByRole('separator').waitFor()
    await page.locator('.statusbar__user', { hasText: 'alice' }).waitFor()
    const meBefore = s.requests.filter((r) => r.path === '/api/me').length
    const socketsBefore = s.wsUrls.length
    s.user = 'bob' // the other tab logged in as bob: the cookie both tabs share now belongs to him
    await tick(page, 10_500)
    await page.locator('.statusbar__user', { hasText: 'bob' }).waitFor({ timeout: 10000 })
    await page.getByRole('separator').waitFor()
    const meAfter = s.requests.filter((r) => r.path === '/api/me').length
    if (meAfter !== meBefore + 1) throw new Error(`/api/me calls: ${meBefore} -> ${meAfter}, expected one more`)
    const src = await page.locator('iframe[title="Remote browser"]').getAttribute('src')
    if (!src.startsWith('/ws/bob/browser/')) throw new Error(`iframe src ${src}`)
    const opened = s.wsUrls.slice(socketsBefore)
    if (opened.length === 0 || opened.some((u) => !u.includes('/ws/bob/terminal/'))) throw new Error(`terminal sockets opened after the swap: ${JSON.stringify(opened)}`)
    if ((await page.locator('.xterm').count()) !== 1) throw new Error('more than one terminal')
    await ctx.close()
  })

  await check('stub: a 401 from the API returns to the login page with a message', async () => {
    const s = makeStub({ state: 'running', health: 'healthy' })
    const { ctx, page } = await newPage()
    await page.clock.install()
    await wireStub(page, s)
    await page.goto(`${APP}/`)
    await page.getByRole('separator').waitFor()
    s.expired = true
    await tick(page, 10_500)
    await page.getByText('Your session expired. Please log in again.').waitFor({ timeout: 10000 })
    await page.getByLabel('Username').waitFor()
    const n = s.wsConnections
    const reqs = s.requests.length
    await tick(page, 60_000)
    if (s.wsConnections !== n) throw new Error('the terminal reconnected after the session ended')
    if (s.requests.length - reqs > 1) throw new Error(`${s.requests.length - reqs} API calls after the session ended`)
    await shot(page, 's5-expired')
    await ctx.close()
  })

  await check('stub: register creates the account and then logs in; rules are enforced before any request', async () => {
    const s = makeStub({ state: 'stopped' })
    s.expired = true // /api/me answers 401: the app starts on the login page
    const { ctx, page } = await newPage()
    await wireStub(page, s)
    await page.goto(`${APP}/`)
    await page.getByRole('tab', { name: 'Register' }).click()
    await page.getByLabel('Username').fill('Bad_Name')
    await page.getByLabel('Password').fill('short')
    await page.getByRole('button', { name: 'Create account' }).click()
    await page.locator('.field__error', { hasText: /lowercase letters, digits and single hyphens/ }).waitFor()
    await page.locator('.field__error', { hasText: /at least 8 bytes/ }).waitFor()
    if (s.requests.some((r) => r.path.startsWith('/api/auth/'))) throw new Error('a request was sent with invalid fields')
    await page.getByLabel('Username').fill('bob')
    await page.getByLabel('Password').fill('correct horse battery')
    // a taken name: reported on the username field, and focus goes there (the inputs are read-only, not disabled, meanwhile)
    s.registerError = { status: 409, error: 'username already taken' }
    await page.getByLabel('Password').focus()
    await page.getByRole('button', { name: 'Create account' }).click()
    await page.getByText('That username is already taken.').waitFor()
    const focused = await page.evaluate(() => document.activeElement?.getAttribute('name') ?? document.activeElement?.tagName)
    if (focused !== 'username') throw new Error(`focus is on ${focused} after "taken", expected the username field`)
    s.registerError = null
    s.expired = false
    await page.getByRole('button', { name: 'Create account' }).click()
    await page.getByRole('button', { name: 'Log out' }).waitFor()
    const auth = s.requests.filter((r) => r.path.startsWith('/api/auth/')).map((r) => `${r.method} ${r.path} ${r.contentType}`)
    const want = ['POST /api/auth/register application/json', 'POST /api/auth/register application/json', 'POST /api/auth/login application/json']
    if (JSON.stringify(auth) !== JSON.stringify(want)) throw new Error(`auth calls: ${JSON.stringify(auth)}`)
    await ctx.close()
  })
}

// ---------- run ----------

try {
  if (PARTS.includes('live')) await live()
  if (PARTS.includes('stub')) await stub()
} finally {
  await browser.close()
  if (stand) await stand.close()
}

await check('no uncaught page errors, console errors or CSP violations in any page', async () => {
  const all = [...problems.pageErrors.map((e) => `pageerror: ${e}`), ...problems.consoleErrors.map((e) => `console.error: ${e}`), ...problems.csp.map((e) => `csp: ${e}`)]
  if (all.length) throw new Error(`${all.length} problem(s):\n${[...new Set(all)].slice(0, 12).join('\n')}`)
  return harnessNoise ? `none; ${harnessNoise} Playwright WebSocket-mock message(s) ignored` : 'none'
})

process.exit(summary() ? 0 : 1)
