# web

The Berth frontend: React 19 + Vite 8 + TypeScript, xterm.js for the terminal, plain CSS (no UI kit, no CSS-in-JS).
In production the Go backend serves the build same-origin at `/`, so every URL is root-relative and there is no CORS.

Pages (CLAUDE.md, "Frontend pages"): login / register, and the workspace page (status bar, "Power on" call to action,
progress while starting, and a resizable split of the xterm.js terminal and the neko iframe). The admin page is Milestone 5.

Requires Node >= 22.13. All dependencies are pinned to exact versions (`.npmrc` has `save-exact=true`); `npm audit` is clean.

## Scripts

| command             | what it does                                                                                        |
| ------------------- | --------------------------------------------------------------------------------------------------- |
| `npm ci`            | install from `package-lock.json`                                                                    |
| `npm run dev`       | Vite dev server on :5173; proxies `/api` and `/ws` (WebSocket included) to `$BERTH_DEV_TARGET`      |
| `npm run typecheck` | `tsc --noEmit`                                                                                      |
| `npm test`          | vitest (jsdom)                                                                                      |
| `npm run build`     | production build into `../internal/webui/dist` (the Go package embeds it; `.gitkeep` is kept)       |
| `npm run e2e`       | browser smoke test against a running stack (`e2e/smoke.mjs`, see below)                             |

The dev proxy has `changeOrigin` off on purpose: ttyd runs with `--check-origin` (the WebSocket `Origin` host must equal
`Host`), so `Host` must stay the dev server's own.

Dev-only environment variables (none of them is read by the backend or the production build):

| variable                                           | used by                | default                  | meaning                                        |
| -------------------------------------------------- | ---------------------- | ------------------------ | ---------------------------------------------- |
| `BERTH_DEV_TARGET`                                 | `vite.config.ts`       | `http://127.0.0.1:18090` | the stack `npm run dev` forwards `/api`, `/ws` |
| `BASE_URL`, `E2E_USER`, `E2E_PASSWORD`, ...        | `e2e/*.mjs`            | see "Browser tests"      | which stack and account the browser tests use  |
| `PORT`, `TARGET`, `CSP`                            | `e2e/serve-dist.mjs`   | `4173`, `http://127.0.0.1:18090`, the policy of `webui.go` | the local stand-in for the Go server (run on its own; `SERVE_DIST=1` starts it for `smoke.mjs`) |
| `CHROMIUM`, `PLAYWRIGHT_BROWSERS_PATH`             | `e2e/lib.mjs`          | newest `chromium-*` under `/opt/pw-browsers` | which browser binary Playwright starts |

## Module layout (`src/`)

Pure modules (no DOM, no React) carry the logic and are unit-tested; components are thin.

```
api.ts              fetch client: Content-Type: application/json on every non-GET (also body-less ones), same-origin
                    cookies, {"error"} extraction, 401 -> onUnauthorized (not for login/register), response validation;
                    reads the top-level "username" of the workspace answers (absent from an older backend)
types.ts            JSON shapes of the backend
ttyd.ts             ttyd 1.7 wire protocol: URL, handshake, INPUT/RESIZE/PAUSE/RESUME framing, server message
                    decoding (streaming UTF-8 across frames), flow-control bookkeeping, reconnect backoff
ttydSession.ts      one terminal connection: WebSocket, handshake with the current size, reconnect (0.5 s doubling,
                    cap 10 s, reset after a stable connection), a terminal write that throws is contained,
                    dispose = never reconnect
terminal.ts         createTerminal(): xterm.js with the fit and Unicode 11 addons, the colour theme, and the
                    Shift+Escape chord that leaves the terminal (isLeaveKey)
neko.ts             nekoUrl(username, nekoPassword): the ONE place that builds the browser iframe URL
split.ts            split-view maths: clamping, pointer -> ratio, WAI-ARIA splitter keys, localStorage (try/catch)
workspaceView.ts    state -> UI mapping (view, badge, banners, buttons, call-to-action text), poll/heartbeat
                    timing, error texts
useWorkspace.ts     the hook: status polling (2 s starting / 10 s running / none when off, every 5 s while a request
                    has failed; a hidden tab every 60 s), heartbeat (30 s, starting/running, visible or hidden tab),
                    Power on / Power off, stale-answer
                    protection (an answer is dropped when an action overtook it or a newer request was issued),
                    account check (see below)
validation.ts       username / password rules (mirror of internal/auth; the server stays the authority)
format.ts           bytes, CPU, elapsed time
App.tsx             /api/me decides login vs workspace; a 401 anywhere returns to login with "session expired"
components/         LoginPage, StatusBar, WorkspacePage, SplitView, TerminalPane, BrowserPane
styles.css          light/dark through prefers-color-scheme, 360 px up
```

Behaviour worth knowing:

- Login does not power on. The workspace view is driven by `GET /api/workspace`; a refresh keeps the user where they were.
- The panes mount only when the workspace is `running` and its health is not `starting` (the container healthcheck is the
  readiness signal; before that Traefik has no routes). `unhealthy` keeps the panes and shows a warning banner.
- The call to action says what is true: a workspace that was never started (no `last_active_at`, no `last_stopped_at`)
  says so; any other "offline" says only that there is no running container; "stopped" says it was stopped. Files are kept
  in all three.
- Power off asks inline (no `window.confirm`), unmounts the panes first (so the terminal socket closes and nothing
  reconnects), then calls the API. A failed power off brings the panes back with the reason. The question closes by itself
  if powering off stops being possible (the workspace stopped meanwhile).
- The neko password (`neko_password` from `GET /api/workspace`) is used only in the iframe `src`,
  `/ws/<user>/browser/?usr=<user>&pwd=<password>&embed=1`; it is not rendered, logged or stored. neko removes `usr` and
  `pwd` from its address after logging in; `?embed=1` (neko's chrome-less mode) stays. The iframe has no `sandbox`
  attribute (an opaque origin would stop the SameSite cookie), `referrerpolicy="no-referrer"` and
  `allow="clipboard-write; fullscreen"` (clipboard-read is denied on purpose, see the Permissions-Policy in
  `internal/webui/webui.go`).
- **Which account.** All tabs of a browser share the session cookie, so a tab that was opened for alice keeps talking to
  whoever logged in last. Every workspace answer carries the top-level `"username"` of the account it was for; when it is not
  the page's user, nothing is applied, the panes and the heartbeat stop, and the app starts over from `/api/me`. (A Power on
  or off that was already sent has acted on the cookie's account: the check keeps the page from continuing, it cannot take
  the request back. A tab that becomes visible refreshes at once, which is usually the first thing a user does in a stale
  tab.) An answer without `username` (an older backend) is accepted as it is.
- **One shell per terminal socket.** ttyd starts a `bash -l` for every WebSocket and sends it SIGHUP when the socket closes,
  so every reconnect, reload, power-off or logout ends the shell (use `tmux`, which is in the image, for sessions that must
  survive). Known limitation: a bash that is sitting at readline's "Display all N possibilities? (y or n)" prompt (Tab, Tab
  at an empty prompt) ignores that SIGHUP and stays in the container, under ttyd, until the workspace is stopped; every
  further reconnect from that state adds another. The page cannot prevent this.
- **Terminal flow control.** The client counts bytes handed to xterm and sends PAUSE / RESUME like ttyd's own client, but
  ttyd 1.7.7 (the image's version) ignores both, so a page that renders slower than the output arrives is not slowed
  down by the server. What keeps it alive instead: the image starts ttyd with `--ping-interval 30` (ttyd hangs up on a
  client that has not answered a ping within twice that; the default 5 s would drop a busy page after 10 s, and the
  reconnect would be a new shell), and a write that xterm refuses ("write data discarded", its queue is capped at 50 MB)
  is contained: that chunk is lost, the socket handler does not throw, and the flow-control count stays right.
- **Character widths.** xterm.js 6 counts emoji and other newer wide characters as one cell (Unicode 6); the shell counts two
  (glibc `wcwidth`). The terminal loads `@xterm/addon-unicode11` and switches to version 11, so readline's cursor tracking and
  column alignment agree with it.
- **Keyboard.** xterm.js sends Tab, Shift+Tab and Escape to the shell (completion, vim), which would trap keyboard focus in
  the terminal (WCAG 2.1.2). **Shift+Esc** is the way out: it is not sent to the shell, the terminal gives up focus and the
  pane divider (`role=separator`) takes it. From there Tab continues to the browser pane, Shift+Tab comes back to the
  terminal, and the arrow keys move the divider. The hint ("Shift+Esc leaves the terminal") is in the terminal's pane bar
  and is the accessible description of the pane and of the terminal input. A plain Escape and Tab still go to the shell.
  (Chromium on Windows and Linux uses Shift+Esc for its own task manager; the page calls `preventDefault()` on it, which a
  browser normally honours for a key that is not reserved. Not checked in a headed browser.)
- **Layout.** The panes are stacked when the viewport is at most 900 px wide AND at least 480 px tall; a phone held sideways
  (667x375, 740x360) keeps them side by side, because two stacked panes would not fit under the status bar and the page
  would scroll. The query is one constant, `STACK_QUERY` in `components/SplitView.tsx`; `styles.css` repeats it for the flex
  direction (CSS cannot import it) and `SplitView.test.tsx` fails if the two differ.
- **Login form.** While a request is under way the inputs stay enabled but read-only (`aria-busy`); only the submit button
  is disabled. A disabled input cannot take focus, and the page moves it to the password field after a failed login and to
  the username field after "taken".
- A failed status request is retried every 5 s whatever the state (offline and stopped included; a hidden tab: every 60 s) until it works, and a fresh
  answer from Power on / Power off clears the "Not up to date" flag.
- A hidden tab keeps sending heartbeats (an open page holds open terminal and browser connections, and the server powers
  a workspace off when nobody has used it for the idle timeout) and polls the status once a minute instead of every 2 or
  10 s, so it still learns that its workspace was powered off or that another account logged in, and stops beating. On
  becoming visible again the page refreshes at once. A closed tab, a sleeping laptop or a lost connection send nothing:
  that is what lets the server tell an abandoned workspace from a used one. (Chrome may hold a hidden tab's timers to one
  a minute, which is why the idle timeout should not be set below a few minutes.)

## CSP

The server sends (`internal/webui/webui.go`, mirrored in `e2e/serve-dist.mjs`):

```
default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self';
connect-src 'self'; frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
```

Scripts are strict: no inline `<script>`, no `eval`. Styles allow `'unsafe-inline'` because xterm.js needs it: its DOM
renderer creates `<style>` elements (theme colours, row sizes) and sets `style` attributes on cells (true colour, the
selection highlight), and a strict `style-src` leaves a terminal with dark text on a dark background and no selection
(measured). The page renders no HTML from the server, so injected CSS has no way in. `img-src` also allows `data:`, as
the Go constant does (nothing in this build uses a data URI; the build inlines no assets). The app talks to its own origin only: `fetch` to `/api`, WebSocket to
`/ws/<user>/terminal/ws`, iframe to `/ws/<user>/browser/`. `src/csp.test.ts` fails if the policy in `e2e/serve-dist.mjs`
and the Go constant differ.

## Browser tests (`e2e/`)

Both scripts drive a real Chromium through Playwright (`playwright-core`, no browser download: it uses `$CHROMIUM`, else the
newest `chromium-*` under `$PLAYWRIGHT_BROWSERS_PATH` or `/opt/pw-browsers`), print PASS/FAIL per check and exit 0 only if
every check passed.

### `e2e/smoke.mjs`: does not power anything off

```sh
npm run build                                   # the page under test
BASE_URL=http://127.0.0.1:18090 npm run e2e     # the Go server serving page + API + /ws (default URL)
SERVE_DIST=1 npm run e2e                        # backend without the page: serve dist locally with the CSP above,
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
| `CHROMIUM`        | (auto)                   | path to a Chromium binary                                               |

With the real stack: login form, deep link, tab order and focus rings, wrong password (and focus on the password field),
login, usage numbers, heartbeat 204, the ttyd handshake, typing `echo hello-$((6*7))` and reading `hello-42` from the xterm
buffer, theme / true colour / selection under the CSP above, UTF-8, an emoji advancing the cursor by two cells (editing a
prompt line around it), Shift+Esc out of the terminal and Tab / Shift+Tab around the divider, a 1 MB output flood, keyboard
and mouse divider (dragging across the iframe, persisted split, resulting terminal resize), reload, the neko iframe
(attributes, `<video>`, no password outside the `src`), reconnect (overlay, backoff, Reconnect button), the 360 px layout
(and that flipping layouts neither reconnects the terminal nor reloads the iframe), side-by-side panes without page scroll
at 667x375 and 740x360 and stacked at 360x740, contrast >= 4.5:1 in light and dark, session expiry, logout. With stubs (no
stack needed): offline -> first-run text -> Power on (503, 409, success) -> progress -> panes, inline Power off and "no
reconnect after power off", unhealthy banner, Escape / Tab reaching the shell and Shift+Esc not, cursor columns after a
letter, an emoji and a CJK character (read back with a DSR cursor report), poll / heartbeat cadence with a fake clock
including hidden tabs and idle stop, an answer for another account sending the page back to `/api/me`, 401 -> login with a
message, register (rules, "taken" focus). The run ends with "no uncaught page errors, console errors or CSP violations".

The live part types into the existing workspace's shell (echo, `stty size`, printf, `yes | head`, one emoji it then cancels
with Ctrl+C) and nothing else; it never calls start/stop.

### `e2e/lifecycle.mjs`: powers the workspace off and on

Drives a COLD start, the terminal, the neko remote browser (WebRTC video, click and key chord), two tabs, power off / power
on with the files surviving, and logout. It stops the workspace, so run it against a stack you can disturb.

```sh
BASE_URL=http://127.0.0.1:18090 E2E_USER=alice E2E_PASSWORD='correct horse' E2E_DOCKER=1 node e2e/lifecycle.mjs
```

| variable          | default                  | meaning                                                                                   |
| ----------------- | ------------------------ | ----------------------------------------------------------------------------------------- |
| `BASE_URL`        | `http://127.0.0.1:18090` | the app (SPA + `/api` + `/ws` behind Traefik)                                             |
| `E2E_USER`        | `alice`                  | an existing account (its container is `berth-ws-<user>`)                                  |
| `E2E_PASSWORD`    | `correct horse`          |                                                                                           |
| `E2E_DOCKER`      | (off)                    | `1` = the test may call `docker` on this host (see below)                                 |
| `SCREENSHOT_DIR`  | (none)                   | write screenshots there                                                                   |
| `CHROMIUM`        | (auto)                   | path to a Chromium binary                                                                 |

`E2E_DOCKER=1` enables what a browser cannot see: it removes the workspace container behind the backend's back before the
first step (so the run starts from "offline", and the test asserts that `/api/workspace` and the status badge say
`offline`), inspects the container's published UDP port and environment (`NEKO_WEBRTC_UDPMUX`, one port), reads the UDP
sockets bound inside the container (exactly one for the multiplexed port, and the full set the same after a second tab came
and went), checks that a click and a key chord made in the video reach the remote Chromium (`xdotool` in the container), and
that the container is stopped after Power off. Without it only what the browser sees is checked.

### `e2e/reaper.mjs`: what a tab does to the idle clock

Needs a stack with a fast reaper and a short idle timeout, and `docker` on this host (it reads the container's state
directly: the API reconciles by itself, which would hide what the reaper did):

```sh
# .env of the stack: BERTH_IDLE_TIMEOUT=1m  BERTH_RECLAIM_AFTER=10m  BERTH_REAPER_INTERVAL=10s
BASE_URL=http://127.0.0.1:18090 E2E_USER=alice E2E_PASSWORD='correct horse' IDLE_SECONDS=60 node e2e/reaper.mjs
```

A visible tab keeps the workspace up for 2.5 idle timeouts; so does a hidden one (`visibilityState` faked: the only thing the
page can see); with the tab closed the container is stopped after about one timeout and kept (`exited`); a tab that stays
open but whose heartbeats are blocked is powered off by the reaper, notices at its next poll, drops the panes, shows the
"powered off" text (programs ended, files kept) and Power on brings the workspace back. `IDLE_SECONDS` must equal the stack's
`BERTH_IDLE_TIMEOUT`; `SCREENSHOT_DIR` writes screenshots.
