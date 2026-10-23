# Berth

A single-host, multi-user "Linux workstation in the browser". One machine runs one Docker container per user, each with a terminal ([ttyd](https://github.com/tsl0922/ttyd)) and a remote browser ([neko](https://github.com/m1k1o/neko) + Chromium). Users log in on a web page and see their own terminal and browser. Idle containers are stopped, later removed; user data lives on a dedicated volume and survives.

The whole system is one Go backend + [Traefik](https://traefik.io/) + Docker. See [CLAUDE.md](CLAUDE.md) for the design.

> **Status: Milestone 2 (container management).** Accounts, sessions, the ForwardAuth check and the workspace lifecycle (power on / off, status, heartbeat) work, and `/ws/<username>/terminal` serves a real terminal. The frontend, the reaper and the admin page arrive in later milestones.

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

Then, through Traefik on port 80:

```sh
curl -s -c jar localhost/api/auth/login -H 'Content-Type: application/json' -d '{"username":"alice","password":"correct horse"}'
curl -s -b jar -X POST localhost/api/workspace/start    # power on; poll GET /api/workspace until "state":"running"
curl -s -b jar localhost/ws/alice/terminal/             # the ttyd terminal (the cookie jar sends the workspace cookie)
curl -s -b jar -X POST localhost/api/workspace/stop     # power off; the container and the data volume are kept
```

`/api/auth/check` and `/healthz` are internal: Traefik and the container healthcheck use them, and they are deliberately not routed publicly.

> **Careful:** `docker compose down -v` deletes the `berth-state` volume, which holds every account. Per-user data volumes (`berth-data-*`) are never touched by Compose and would survive as orphans.

### Host requirements

- **Docker address pools.** Every user gets a private Docker network, and a default Docker daemon runs out of address space after about 30 networks (`all predefined address pools have been fully subnetted`). Before onboarding more users, give Docker smaller subnets in `/etc/docker/daemon.json` and restart it. This allows about 4000 networks:

  ```json
  { "default-address-pools": [{ "base": "10.200.0.0/16", "size": 28 }] }
  ```

- **UDP ports.** Each workspace publishes `BERTH_UDP_PORTS_PER_WORKSPACE` UDP ports (default 8) for the browser's WebRTC video, from `BERTH_UDP_PORT_MIN`-`MAX` (default 30000-30999, so 125 workspaces). Open that range in the firewall, and set `BERTH_PUBLIC_IP` when the host is behind NAT or in a cloud VM. Every published port costs a `docker-proxy` process; beyond a few dozen users set `"userland-proxy": false` in `daemon.json`.
- **The Docker socket.** The backend mounts `/var/run/docker.sock` to create containers, which is root-equivalent on the host. It runs unprivileged and joins the socket's group (`DOCKER_GID`).

## How access is controlled

A login sets two `HttpOnly`, `SameSite=Lax` cookies with separate tokens:

| Cookie | Path | Accepted by |
|---|---|---|
| `berth_session` | `/api` | the API |
| `berth_ws` | `/ws` | only Traefik's ForwardAuth check |

Traefik forwards the browser's cookies to the workspace container, where code running as the user could read them. Because the API token is never sent under `/ws`, such code cannot call the API.

Workspaces do not share a network: each container joins only its own `berth-net-<username>`, and the backend connects Traefik to that network. A workspace therefore cannot reach other workspaces or the backend. Logging out revokes both tokens, but an already-open terminal or browser stream stays connected until it closes.

## Configuration

Everything is an environment variable; see [.env.example](.env.example) for the full, commented list. `go run` does not read `.env` itself:

```sh
set -a; . ./.env; set +a; go run ./cmd/berth
```

## Development

```sh
go vet ./... && go test ./...                          # unit tests, no Docker needed
go test -tags integration ./internal/docker            # the Docker wrapper against a real daemon
```

Build with `GOTOOLCHAIN=local` so Go never silently downloads a newer toolchain. `go.mod` requires Go 1.24 (the spec asks for 1.22+): that is the oldest release the security-fixed `golang.org/x/crypto` and a current SQLite (`modernc.org/sqlite`) support. The Docker image builds with a current Go release.

## API

| Method | Path | |
|---|---|---|
| POST | `/api/auth/register` | `{username, password}` → 201 |
| POST | `/api/auth/login` | sets the `berth_session` and `berth_ws` cookies |
| POST | `/api/auth/logout` | revokes the session, clears both cookies |
| GET | `/api/auth/check` | Traefik ForwardAuth: 200 / 401 (no session) / 403 (not your workspace). Internal: not routed through Traefik |
| GET | `/api/me` | current user and workspace |
| GET | `/api/workspace` | state, health, resource usage (`cpu_percent` is `docker stats` style: 100 = one core), and the browser view's password while it is up |
| POST | `/api/workspace/start` | power on (idempotent); returns once the container is started, then poll `GET /api/workspace` for `"state":"running"` |
| POST | `/api/workspace/stop` | power off (idempotent); keeps the container and data |
| POST | `/api/workspace/heartbeat` | records activity; 204 |
| GET | `/healthz` | liveness. Internal: not routed through Traefik |
