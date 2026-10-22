# Berth

A single-host, multi-user "Linux workstation in the browser". One machine runs one Docker container per user, each with a terminal ([ttyd](https://github.com/tsl0922/ttyd)) and a remote browser ([neko](https://github.com/m1k1o/neko) + Chromium). Users log in on a web page and see their own terminal and browser. Idle containers are stopped, later removed; user data lives on a dedicated volume and survives.

The whole system is one Go backend + [Traefik](https://traefik.io/) + Docker. See [CLAUDE.md](CLAUDE.md) for the design.

> **Status: Milestone 1 (skeleton).** Accounts, sessions and the ForwardAuth endpoint work. Containers, the frontend, the reaper and the admin page arrive in later milestones.

## Quick start (backend only)

Requires Go 1.24+.

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

## Quick start (Traefik + backend)

Requires Docker with Compose >= 2.24.

```sh
cp .env.example .env        # optional; every setting has a default
docker compose up -d --build
curl -s localhost/api/auth/login ...   # same API, through Traefik on port 80
```

`/api/auth/check` and `/healthz` are internal: Traefik and the container healthcheck use them, and they are deliberately not routed publicly.

> **Careful:** `docker compose down -v` deletes the `berth-state` volume, which holds every account. Per-user data volumes (`berth-data-*`) are never touched by Compose and would survive as orphans.

## Configuration

Everything is an environment variable; see [.env.example](.env.example) for the full, commented list. `go run` does not read `.env` itself:

```sh
set -a; . ./.env; set +a; go run ./cmd/berth
```

## Development

```sh
go vet ./... && go test ./...
```

Build with `GOTOOLCHAIN=local` so Go never silently downloads a newer toolchain. `go.mod` requires Go 1.24 (the spec asks for 1.22+): that is the oldest release the security-fixed `golang.org/x/crypto` and a current SQLite (`modernc.org/sqlite`) support. The Docker image builds with a current Go release.

## API (Milestone 1)

| Method | Path | |
|---|---|---|
| POST | `/api/auth/register` | `{username, password}` → 201 |
| POST | `/api/auth/login` | sets the `berth_session` cookie (HttpOnly, SameSite=Lax) |
| POST | `/api/auth/logout` | revokes the session, clears the cookie |
| GET | `/api/auth/check` | Traefik ForwardAuth: 200 / 401 (no session) / 403 (not your workspace). Internal: not routed through Traefik |
| GET | `/api/me` | current user and workspace |
| GET | `/healthz` | liveness. Internal: not routed through Traefik |
