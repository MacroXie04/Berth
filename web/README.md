# web

The Berth frontend: React 19 + Vite 8 + TypeScript, xterm.js for the terminal, plain CSS (no UI kit, no CSS-in-JS).
In production the Go backend serves the build same-origin at `/`, so every URL is root-relative and there is no CORS.

Pages (CLAUDE.md, "Frontend pages"): login / register, and the workspace page (status bar, "Power on" call to action,
progress while starting, and a resizable split of the xterm.js terminal and the neko iframe). The admin page is Milestone 5.

Requires Node >= 22.13. All dependencies are pinned to exact versions (`.npmrc` has `save-exact=true`); `npm audit` is clean.

## Scripts

| command             | what it does                                                                                      |
| ------------------- | ------------------------------------------------------------------------------------------------- |
| `npm ci`            | install from `package-lock.json`                                                                  |
| `npm run dev`       | Vite dev server on :5173; proxies `/api` and `/ws` (WebSocket included) to `http://127.0.0.1:18090` |
| `npm run typecheck` | `tsc --noEmit`                                                                                    |
| `npm test`          | vitest (jsdom)                                                                                    |
| `npm run build`     | production build into `../internal/webui/dist` (the Go package embeds it; `.gitkeep` is kept)     |
| `npm run e2e`       | browser smoke test against a running stack (see below)                                            |

The dev proxy has `changeOrigin` off on purpose: ttyd runs with `--check-origin` (the WebSocket `Origin` host must equal
`Host`), so `Host` must stay the dev server's own. Another stack: `BERTH_DEV_TARGET=http://host:port npm run dev`.

## Module layout (`src/`)

Pure modules (no DOM, no React) carry the logic and are unit-tested; components are thin.

```
api.ts              fetch client: Content-Type: application/json on every non-GET (also body-less ones), same-origin
                    cookies, {"error"} extraction, 401 -> onUnauthorized (not for login/register), response validation
types.ts            JSON shapes of the backend
ttyd.ts             ttyd 1.7 wire protocol: URL, handshake, INPUT/RESIZE/PAUSE/RESUME framing, server message
                    decoding (streaming UTF-8 across frames), flow control, reconnect backoff
ttydSession.ts      one terminal connection: WebSocket, handshake with the current size, reconnect (0.5 s doubling,
                    cap 10 s, reset after a stable connection), dispose = never reconnect
neko.ts             nekoUrl(username, nekoPassword): the ONE place that builds the browser iframe URL
split.ts            split-view maths: clamping, pointer -> ratio, WAI-ARIA splitter keys, localStorage (try/catch)
workspaceView.ts    state -> UI mapping (view, badge, banners, buttons), poll/heartbeat timing, error texts
useWorkspace.ts     the hook: status polling (2 s starting / 10 s running / none when off), heartbeat (30 s, visible
                    tab, starting/running), Power on / Power off, stale-answer protection
validation.ts       username / password rules (mirror of internal/auth; the server stays the authority)
format.ts           bytes, CPU, elapsed time
cspStyles.ts        makes xterm.js work under `style-src 'self'` (see "CSP")
App.tsx             /api/me decides login vs workspace; a 401 anywhere returns to login with "session expired"
components/         LoginPage, StatusBar, WorkspacePage, SplitView, TerminalPane, BrowserPane
styles.css          light/dark through prefers-color-scheme, 360 px up
```

Behaviour worth knowing:

- Login does not power on. The workspace view is driven by `GET /api/workspace`; a refresh keeps the user where they were.
- The panes mount only when the workspace is `running` and its health is not `starting` (the container healthcheck is the
  readiness signal; before that Traefik has no routes). `unhealthy` keeps the panes and shows a warning banner.
- Power off asks inline (no `window.confirm`), unmounts the panes first (so the terminal socket closes and nothing
  reconnects), then calls the API. A failed power off brings the panes back with the reason.
- The neko password (`neko_password` from `GET /api/workspace`) is used only in the iframe `src`
  (`/ws/<user>/browser/?usr=<user>&pwd=<password>`); it is not rendered, logged or stored. neko drops the query from the
  frame's location after it logs in. The iframe has no `sandbox` attribute (an opaque origin would stop the SameSite
  cookie), `referrerpolicy="no-referrer"` and `allow="clipboard-read; clipboard-write; fullscreen"`.
- Every terminal (re)connection is a new shell: ttyd starts one process per WebSocket and ends it when the socket closes.
  After a drop the client resets the screen and shows the fresh prompt. (`tmux` is in the image if sessions should survive.)
- Hidden tabs neither poll nor send heartbeats; on becoming visible again the page refreshes and sends one at once.

## CSP

The build needs no `'unsafe-inline'` / `'unsafe-eval'`: no inline `<script>` or `<style>`, no `data:` assets, no `eval`.
React sets styles through the CSSOM, which `style-src 'self'` allows. xterm.js is the exception: its DOM renderer creates
`<style>` elements (theme colours, row sizes) and sets `style` attributes on cells (true colour, the selection highlight),
which a strict policy blocks (measured: dark text on a dark background, no selection highlight). `cspStyles.ts` (installed in
`main.tsx`) redirects both to constructed stylesheets and per-property CSSOM writes, which the policy does not block. It needs
constructable stylesheets (Chrome 73, Firefox 101, Safari 16.4); without them the terminal still works if the policy allows
inline styles. The e2e run asserts zero `securitypolicyviolation` events and readable terminal colours.

Policy used in testing (`e2e/serve-dist.mjs`): `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self';
font-src 'self'; connect-src 'self'; frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`.
The app talks to its own origin only: `fetch` to `/api`, WebSocket to `/ws/<user>/terminal/ws`, iframe to `/ws/<user>/browser/`.

## Browser smoke test (`e2e/`)

`e2e/smoke.mjs` drives a real Chromium through Playwright (`playwright-core`, no browser download: it uses `$CHROMIUM`, else the
newest `chromium-*` under `$PLAYWRIGHT_BROWSERS_PATH` or `/opt/pw-browsers`).

```sh
npm run build                                   # the page under test
BASE_URL=http://127.0.0.1:18090 npm run e2e     # the Go server serving page + API + /ws (default URL)
SERVE_DIST=1 npm run e2e                        # backend without the page: serve dist locally with the strict CSP,
                                                #   forward /api and /ws to BASE_URL (e2e/serve-dist.mjs)
BASE_URL=http://127.0.0.1:5173 npm run e2e      # against `npm run dev`
```

| variable          | default                  | meaning                                                                 |
| ----------------- | ------------------------ | ----------------------------------------------------------------------- |
| `BASE_URL`        | `http://127.0.0.1:18090` | where the app (or, with `SERVE_DIST=1`, the stack) is                   |
| `E2E_USER`        | `alice`                  | an existing user with a **running** workspace (never powered off)       |
| `E2E_PASSWORD`    | `correct horse`          |                                                                         |
| `E2E_PARTS`       | `live,stub`              | `live` = real stack, `stub` = scripted `/api` and fake sockets          |
| `SCREENSHOT_DIR`  | (none)                   | write screenshots there                                                 |
| `SERVE_DIST`      | (off)                    | `1` = local static server with CSP in front of `BASE_URL`               |

What it checks, with the real stack: login form, deep link, tab order and focus rings, wrong password, login, usage numbers,
heartbeat 204, the ttyd handshake, typing `echo hello-$((6*7))` and reading `hello-42` from the xterm buffer, theme / true
colour / selection under the CSP, UTF-8, a 1 MB output flood, keyboard and mouse divider (dragging across the iframe,
persisted split, resulting terminal resize), reload, the neko iframe (attributes, `<video>`, no password outside the `src`),
reconnect (overlay, backoff, Reconnect button), the 360 px layout (and that flipping layouts neither reconnects the terminal
nor reloads the iframe), contrast >= 4.5:1 in light and dark, session expiry, logout. With stubs (no stack needed): offline
-> Power on (503, 409, success) -> progress -> panes, inline Power off and "no reconnect after power off", unhealthy banner,
poll / heartbeat cadence with a fake clock including hidden tabs and idle stop, 401 -> login with a message, register.
The run ends with "no uncaught page errors, console errors or CSP violations".

The live part types into the existing workspace's shell (echo, `stty size`, printf, `yes | head`) and nothing else; it
never calls start/stop.
