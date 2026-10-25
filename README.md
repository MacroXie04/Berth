# Berth

A single-host, multi-user "Linux workstation in the browser". One machine runs one Docker container per user, each with a terminal ([ttyd](https://github.com/tsl0922/ttyd)) and a remote browser ([neko](https://github.com/m1k1o/neko) + Chromium). Users log in on a web page and see their own terminal and browser. Idle containers are stopped, later removed; user data lives on a dedicated volume and survives.

The whole system is one Go backend + [Traefik](https://traefik.io/) + Docker. See [CLAUDE.md](CLAUDE.md) for the design.

> **Status: Milestone 4 (reclaim).** Log in at `http://<host>/`, power the workspace on, and use a terminal and a remote Chromium side by side in the browser. Accounts, sessions, the ForwardAuth check and the workspace lifecycle work, and the backend stops workspaces nobody uses and removes the containers of long-stopped ones (see "What happens to a workspace nobody uses"). The admin page arrives in the next milestone.

## Quick start (backend only)

Requires Go 1.24+. Without Docker the account endpoints work and the workspace endpoints answer `503`.

```sh
go run ./cmd/berth                       # listens on :8080, database in ./data/berth.db
```

```sh
curl -s localhost:8080/api/auth/register -H 'Content-Type: application/json' \
     -d '{"username":"alice","password":"correct horse"}'
curl -s -c jar localhost:8080/api/auth/login -H 'Content-Type: application/json' \
     -d '{"username":"alice","password":"correct horse"}'
curl -s -b jar localhost:8080/api/me
```

The first registered user is the admin. Usernames are 3-32 characters of lowercase letters, digits and single hyphens, starting with a letter; passwords are 8-72 bytes.

## Quick start (the full stack)

Requires Docker with Compose >= 2.24 and the host settings below.

```sh
cp .env.example .env
echo "DOCKER_GID=$(stat -c %g /var/run/docker.sock)" >> .env   # required
docker compose up -d --build        # builds the backend and the workspace image (large first build)
```

Then open `http://localhost/` (or `http://<host>/`): register, log in, press **Power on**, and the terminal and the remote browser appear side by side once the workspace is up (a cold start takes about 15 s). The divider between them can be dragged or moved with the arrow keys.

The same thing from the command line, through Traefik on port 80:

```sh
curl -s -c jar localhost/api/auth/login -H 'Content-Type: application/json' -d '{"username":"alice","password":"correct horse"}'
curl -s -b jar -X POST -H 'Content-Type: application/json' localhost/api/workspace/start   # power on; poll GET /api/workspace until "state":"running"
curl -s -b jar localhost/ws/alice/terminal/             # the ttyd terminal (the cookie jar sends the workspace cookie)
curl -s -b jar -X POST -H 'Content-Type: application/json' localhost/api/workspace/stop    # power off; the container and the data volume are kept
```

`/api/auth/check` and `/healthz` are internal: Traefik and the container healthcheck use them, and they are deliberately not routed publicly.

> **Careful:** `docker compose down -v` deletes the `berth-state` volume, which holds every account. Per-user data volumes (`berth-data-*`) are never touched by Compose and would survive as orphans.

### Upgrading

`docker compose up -d --build` rebuilds the backend. Two things it does not do by itself:

- Compose now mounts the whole `traefik/` directory and Traefik watches it (`--providers.file.directory`), so a later `git pull` that replaces `traefik/dynamic.yml` is picked up without a restart. The first upgrade to this layout changes Traefik's service spec, so `docker compose up -d` recreates it by itself (users are reconnected within `BERTH_PROXY_ATTACH_INTERVAL`, 10 s by default). If you run Traefik some other way, point its file provider at the directory or the file and make sure it re-reads it: a file bind-mounted on its own keeps the old inode when it is replaced, and the running Traefik then never sees the new content.
- Since Milestone 4 the backend stops workspaces nobody uses (30 minutes by default) and removes the containers of workspaces that stayed stopped for a week; read "What happens to a workspace nobody uses" below before upgrading a deployment with long-running sessions. Nobody is stopped until a full `BERTH_IDLE_TIMEOUT` after the new backend starts. The page now keeps sending heartbeats from a hidden tab, so rebuild (`--build`) to ship the new page together with the backend: an old page in a hidden tab would not count as in use. Set `BERTH_IDLE_TIMEOUT` and `BERTH_RECLAIM_AFTER` to something very long (`87600h`) to switch the reaper's stopping and removing off in effect.
- A workspace container keeps the routing labels it was created with, so a change to the labels reaches an existing workspace only when its container is recreated (for one user: `docker rm -f berth-ws-<username>`; the data volume is untouched and the next power-on creates a fresh container with the same password and data). Changes to `traefik/dynamic.yml` (for example the `/ws` response headers below) need no such step, because the routers only name `berth-auth@file` and everything else lives in that file.

### Host requirements

- **Docker Engine 28 or newer.** When the backend attaches Traefik to a user's network it sets a low gateway priority, which Engine 28 (API 1.48) introduced; older engines ignore it, and the first power-on of every new user then resets the connections of everyone using Traefik. The backend logs an error at startup if it finds an older engine.
- **Docker address pools.** Every user gets a private Docker network, and a default Docker daemon runs out of address space after about 30 networks (`all predefined address pools have been fully subnetted`). Before onboarding more users, give Docker smaller subnets in `/etc/docker/daemon.json` and restart it. This allows about 4000 networks:

  ```json
  { "default-address-pools": [{ "base": "10.200.0.0/16", "size": 28 }] }
  ```

- **UDP ports and `BERTH_PUBLIC_IP`.** The remote browser's video is WebRTC. Each workspace publishes `BERTH_UDP_PORTS_PER_WORKSPACE` UDP ports (default 1: neko multiplexes every viewer of the workspace over one port; a larger block gives each viewer its own port, but closed tabs were seen to keep their port for 5-10 s and some stayed bound until neko restarted) from `BERTH_UDP_PORT_MIN`-`MAX` (default 30000-30999, so up to 1000 workspaces). Open that range in the firewall. **Set `BERTH_PUBLIC_IP` to the address browsers use to reach this host** (its LAN address on a LAN, its public address on the internet): it is required for every browser that is not on the Docker host itself, because without it neko offers only the workspace's private bridge address and the video never connects (the backend logs a warning at startup when it is empty). Every published port costs a `docker-proxy` process; beyond a few dozen users set `"userland-proxy": false` in `daemon.json`.
- **The Docker socket.** The backend mounts `/var/run/docker.sock` to create containers, which is root-equivalent on the host. It runs unprivileged and joins the socket's group (`DOCKER_GID`).

## What happens to a workspace nobody uses

```
running ──unused for BERTH_IDLE_TIMEOUT (30 min)──▶ stopped ──for BERTH_RECLAIM_AFTER (7 days)──▶ offline
         docker stop: programs end,                  docker rm: the container goes,
         container and files stay                    the data volume stays
```

A background job in the backend (the "reaper") looks at every workspace each `BERTH_REAPER_INTERVAL` (1 minute). A workspace counts as **in use** while

- its web page is open: the page sends a heartbeat every 30 seconds, from a visible or a hidden tab (an open page holds open terminal and browser connections). A closed tab, a sleeping laptop or a lost connection stop the heartbeats; or
- it is **busy**: its average CPU use since the reaper's previous look was at least `BERTH_IDLE_CPU_PERCENT` (5, in percent of one core), so a build or a long job is not killed. An idle workspace measures well under 1% (see CLAUDE.md, "Verified facts").

Details that matter to an operator:

- **A stop ends every process in the workspace**, including `nohup`/`tmux` jobs that sit idle (they are under the CPU threshold). Files in the home directory are untouched. The page says so when it shows "Power on".
- A stop happens at the first look after the timeout, so the real idle time is the timeout plus up to one interval. After a backend restart (or a wall-clock jump, for example a resumed laptop) nobody is stopped for one full idle timeout, because the heartbeats of the downtime were missed. Timeouts below 5 minutes make the backend log a warning: they are for testing.
- Power-on after a stop starts the kept container (seconds). Power-on after the container was removed creates a new one on the same volume: the same files and browser password, a slower start, and `/tmp` and anything else outside the home directory is gone.
- **Data volumes are never deleted by Berth, so the volumes of abandoned accounts accumulate** until you remove them (there is no quota yet). After a removal the volume has no container, so do not run `docker volume prune -a` or `docker system prune -a --volumes` on the host; to prune anyway, keep Berth's volumes with `docker volume prune -a --filter 'label!=berth.managed'`.
- A stopped workspace keeps its UDP port until its container is removed, so `BERTH_UDP_PORT_MIN`-`MAX` bounds the number of users active within `BERTH_RECLAIM_AFTER`, not only those running. Each user who has ever started a workspace keeps an empty private network `berth-net-<username>` (the address-pool setting above is the sizing answer).
- The reaper also records what Docker did behind the backend's back (a crashed container is stopped, one removed by hand is offline and frees its port) and logs, once, any container carrying Berth's labels that the database does not know. It never removes those.

## How access is controlled

A login sets two `HttpOnly`, `SameSite=Lax` cookies with separate tokens:

| Cookie | Path | Accepted by |
|---|---|---|
| `berth_session` | `/api` | the API |
| `berth_ws` | `/ws` | only Traefik's ForwardAuth check |

Traefik forwards the browser's cookies to the workspace container, where code running as the user could read them. Because the API token is never sent under `/ws`, such code cannot steal it. It is not a full isolation: the workspaces share the site's origin with the API, so a page served by a hostile workspace can still make the visitor's browser call `/api` as that visitor. Serving workspaces from their own origin closes this (CLAUDE.md, decision D4c).

Every `/ws` route (both sections and the catch-all) runs `berth-auth@file`, a chain defined in `traefik/dynamic.yml` of the `berth-ws-headers` response headers and then the ForwardAuth check, so the 401 and 403 the check itself writes carry the headers too. Responses are `X-Frame-Options: SAMEORIGIN` with `frame-ancestors 'self'`, so only Berth's own page can frame a terminal or remote browser (a page on another port or subdomain of the same site could otherwise, with the visitor's cookies), and `Service-Worker-Allowed` is removed, so a workspace page cannot register a service worker for the whole site.

Workspaces do not share a network: each container joins only its own `berth-net-<username>`, and the backend connects Traefik to that network. A workspace therefore cannot reach other workspaces or the backend. Logging out revokes both tokens, but an already-open terminal or browser stream stays connected until it closes.

## Configuration

Everything is an environment variable; see [.env.example](.env.example) for the full, commented list. `go run` does not read `.env` itself:

```sh
set -a; . ./.env; set +a; go run ./cmd/berth
```

## Development

The web app lives in `web/` (React, Vite, TypeScript, xterm.js; see [web/README.md](web/README.md)). The Go binary embeds the build from `internal/webui/dist` (git-ignored except `.gitkeep`), which the Docker build produces in a Node stage:

```sh
cd web && npm ci && npm run build      # writes ../internal/webui/dist
cd .. && go run ./cmd/berth            # serves the app at / and the API at /api
cd web && npm run dev                  # or: Vite dev server with /api and /ws proxied to a running stack on :18090
cd web && npm test                     # unit tests; npm run e2e / node e2e/lifecycle.mjs drive a real Chromium against a stack
```

Without a build `GET /` answers `503 frontend not built` while the API keeps working.

```sh
go vet ./... && go test ./...                          # unit tests, no Docker needed
go test -tags integration ./internal/docker            # the Docker wrapper against a real daemon
BERTH_TEST_TRAEFIK=/path/to/traefik go test -tags integration -run Traefik ./internal/workspace   # the /ws routes through a real Traefik
scripts/e2e-reaper.sh                                  # the reaper on the real stack with 1-minute timeouts (about 12 minutes; deletes the stack's state)
```

`scripts/e2e-reaper.sh` brings the stack up with an idle timeout and a reclaim age of one minute and a reaper that looks every 10 seconds, then checks on a real workspace container that a heartbeat keeps it up, that without one it is stopped (container kept) and later removed (the data volume kept, the files and the password back after the next power-on), that CPU load keeps it up, that a crash or a removal by hand is picked up on its own, and that nobody is stopped for one timeout after the backend restarts. `web/e2e/reaper.mjs` does the user's side with a real browser (see `web/README.md`).

Build with `GOTOOLCHAIN=local` so Go never silently downloads a newer toolchain. `go.mod` requires Go 1.24 (the spec asks for 1.22+): that is the oldest release the security-fixed `golang.org/x/crypto` and a current SQLite (`modernc.org/sqlite`) support. The Docker image builds with a current Go release.

## API

| Method | Path | |
|---|---|---|
| POST | `/api/auth/register` | `{username, password}` → 201 |
| POST | `/api/auth/login` | sets the `berth_session` and `berth_ws` cookies |
| POST | `/api/auth/logout` | revokes the session, clears both cookies |
| GET | `/api/auth/check` | Traefik ForwardAuth: 200 / 401 (no session) / 403 (not your workspace). Internal: not routed through Traefik |
| GET | `/api/me` | current user and workspace |
| GET | `/api/workspace` | `{username, workspace}`: state, health, resource usage (`cpu_percent` is `docker stats` style: 100 = one core), and the browser view's password while it is up |
| POST | `/api/workspace/start` | power on (idempotent); answers like `GET /api/workspace` once the container is started, then poll that for `"state":"running"` |
| POST | `/api/workspace/stop` | power off (idempotent); keeps the container and data; same answer |
| POST | `/api/workspace/heartbeat` | records activity; 204 |
| GET | `/healthz` | liveness. Internal: not routed through Traefik |
