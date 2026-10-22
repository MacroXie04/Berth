# Berth

Single-host, multi-user "Linux workstation in the browser". One machine runs many Docker containers, one per user. Each container has a terminal (ttyd) and a remote browser (neko + Chromium). A user logs in on a web page and sees their own terminal and browser. Idle containers are stopped, later removed; user data lives on a dedicated volume and is restored on the next login.

**Scope guard: no Kubernetes, no message queue, no microservices.** The whole system is one Go backend process + Traefik + Docker. Do not over-engineer; prefer the simplest thing that satisfies this document.

## Working agreement

- Work in the milestones below, **one at a time**. After each milestone: list what was built, how it was verified, and the decisions that need confirmation — then **stop and wait for the go-ahead**.
- All configuration is via environment variables; every variable is documented in `.env.example` (add new ones in the same change that introduces them).
- Core logic (state transitions, idle detection) has unit tests. The Docker client is always behind an interface (`internal/docker`) so it can be mocked.
- No heavy frontend UI kits (Tailwind or plain CSS only).
- Develop on the branch the session designates; never push elsewhere.
- Verify against the real stack, not just unit tests: Traefik and Docker behave in ways the docs do not make obvious (see "Verified facts").

## Tech stack

- **Backend:** Go, `chi` router, Docker client (see Open decisions D5 for the module path), SQLite via `modernc.org/sqlite` (pure Go, **no cgo**). SQL is isolated behind the `store.Store` interface so Postgres can come later.
- **Proxy:** Traefik v3 (`traefik:v3.7`; Docker Engine 29 needs >= 3.6.1) with the Docker provider, routing from container labels. The backend never proxies user traffic itself.
- **Frontend:** React + Vite + TypeScript. Terminal = xterm.js; browser view = neko's own web UI in an iframe.
- **User image:** based on `ghcr.io/m1k1o/neko/chromium` (amd64/arm64) + ttyd, CLI tools, non-root user. Lives in `images/workspace/`.
- **Deploy:** `docker-compose.yml` brings up Traefik + backend + built frontend.

**Go version:** the spec says Go 1.22+. `go.mod` says `go 1.24` because that is the oldest Go the security-fixed `x/crypto` (>= v0.45.0) and a current SQLite (`modernc.org/sqlite` >= v1.46, bundling SQLite 3.51) support; Go 1.22 forced versions with published advisories. The Dockerfile builds on a supported release (`golang:1.26-alpine`). Build with `GOTOOLCHAIN=local` and never let `go get @latest` raise the `go` directive unreviewed.

## Layout

```
cmd/berth/main.go      backend entrypoint
internal/config/       env-var configuration
internal/api/          HTTP handlers, chi routes
internal/auth/         password hashing, usernames, session tokens
internal/store/        SQLite access behind the Store interface, migrations
internal/workspace/    container lifecycle: create/start/stop/remove, Traefik labels (M1: naming only)
internal/reaper/       periodic reclaim job (M4)
internal/docker/       thin, mockable wrapper over the docker client (M2)
web/                   React frontend (M3)
images/workspace/      user container Dockerfile (M2)
docker-compose.yml  Dockerfile  .env.example  README.md
```

## Data model (SQLite)

```
users(id, username UNIQUE, password_hash, is_admin, created_at)
sessions(token_hash PK, user_id, created_at, expires_at)
workspaces(user_id PK -> users, container_id NULL, state, volume_name UNIQUE,
           mem_limit_mb DEFAULT 2048, cpu_limit DEFAULT 2.0,
           last_active_at, last_stopped_at, updated_at)
```

One workspace per user. `state` ∈ `offline | starting | running | stopped` (CHECK-constrained). `container_id` NULL means the container was removed and only the volume remains. The first registered user is admin (decided atomically in the INSERT). Timestamps are UTC text `YYYY-MM-DD HH:MM:SS`; **never bind a `time.Time` directly** (the driver stores its monotonic-clock string) — use the store's `fmtTime`.

Schema changes are versioned migrations (`PRAGMA user_version`, contiguous `NNNN_name.sql`, each applied atomically); never edit a shipped migration. The store refuses a database newer than the binary. SQLite cannot `ADD COLUMN ... UNIQUE` or `NOT NULL` without a default: add nullable columns, backfill, then `CREATE UNIQUE INDEX` (partial, `WHERE ... IS NOT NULL`). Transactions are `BEGIN IMMEDIATE` (`_txlock=immediate`), so read-then-write sequences are safe; table-rebuild migrations need `PRAGMA foreign_keys=OFF` outside the transaction.

`workspaces` also gets a per-container non-overlapping UDP port range (neko `NEKO_EPR`), recorded in the table and published with `-p` — added by a migration in Milestone 2.

## Lifecycle state machine

```
offline ──login / "power on"──▶ starting ──container ready──▶ running
running ──idle timeout (default 30 min) or user "power off"──▶ stopped   (docker stop, container kept)
stopped ──user returns──▶ running                                          (docker start, seconds)
stopped ──idle again (default 7 days)──▶ offline                            (docker rm, volume kept)
```

- Volume `berth-data-<username>` is mounted at `/home/user` and **never deleted in any state**. (M1 only *records* the name at registration; creating the volume is Open decision D5c.)
- All timeouts are env-configurable (defaults above).
- **Idle** = ttyd AND neko WebSockets both closed for longer than the threshold, AND container CPU (docker stats) stayed below 5%. Both must hold so background jobs are not killed. How the backend learns about WebSocket state is Open decision D2.
- Frontend calls `POST /api/workspace/heartbeat` every 30 s to refresh `last_active_at`.

## Container spec

- Image `berth/workspace:latest`; name `berth-ws-<username>`; mount `berth-data-<username>:/home/user`.
- `--memory` / `--cpus` from the workspaces row; `--shm-size=2g` (Chromium).
- One private network per user `berth-net-<username>`. **How Traefik reaches it, and the shared `berth-proxy` network, is Open decision D1 — do not attach workspaces to a shared network without resolving it.**
- Env: neko password, resolution (default `1280x720@30`), `NEKO_EPR`; unique UDP range per container, published with `-p`.
- Traefik labels per user:
  - `<username>-term`: `PathPrefix("/ws/<username>/terminal")` → port 7681, StripPrefix middleware
  - `<username>-browser`: `PathPrefix("/ws/<username>/browser")` → port 8080
  - both routers use the shared ForwardAuth middleware `berth-auth@file` (defined in `traefik/dynamic.yml`, so it exists even while the backend is down: a dead backend then yields a fail-closed 5xx instead of a 404) → `/api/auth/check`. **List it before StripPrefix** (`middlewares=berth-auth@file,<u>-strip`): Traefik fills `X-Forwarded-Uri` from the URL at the moment ForwardAuth runs, so the other order shows the check a stripped path and every request gets 403.

## Backend API

```
M1  POST /api/auth/register     {username,password}
M1  POST /api/auth/login        {username,password} -> HttpOnly session cookie
M1  POST /api/auth/logout
M1  GET  /api/auth/check        Traefik ForwardAuth: cookie + username in path
M1  GET  /api/me                current user + workspace state
M1  GET  /healthz               liveness (compose healthcheck; not routed publicly)
M2  POST /api/workspace/start | /stop | /heartbeat     GET /api/workspace
M5  GET  /api/admin/workspaces  admin only (first registered user)
```

Errors are JSON `{"error": "..."}`. JSON bodies are required (`Content-Type: application/json`), size-capped, unknown fields rejected.

## Frontend pages

1. Login / register. 2. Workspace: status bar (state, mem/CPU, power-off) + resizable split of xterm.js (`/ws/<username>/terminal`) and the neko iframe (`/ws/<username>/browser`); "Power on" button when not running. 3. Admin: table of users + states with force-stop.

## Reaper

Goroutine, every minute: (1) for each `running` workspace, decide idleness (CPU + `last_active_at`), `docker stop` → `stopped` + `last_stopped_at`; (2) for each `stopped` older than 7 days, `docker rm` → `offline`, clear `container_id`; (3) reconcile all `berth-ws-*` containers against the DB (e.g. a container removed manually while the backend was down) — and, under D1's recommended design, re-attach Traefik to every user network.

## Milestones

1. **Skeleton** — layout, CLAUDE.md, compose (Traefik + backend), SQLite schema, register/login. `go run` works; curl can register and log in.
2. **Container management** — workspace image builds; `POST /api/workspace/start` launches a container with Traefik labels; `/ws/<username>/terminal` shows ttyd.
3. **Browser + frontend** — neko works; workspace page shows terminal and browser side by side.
4. **Reclaim** — reaper; verify with 1-minute timeouts that stop and rm both work and volume data survives.
5. **Admin page + polish** — admin endpoint + page; README with deployment steps.

## Security decisions (keep these invariant)

- **Usernames** are used in container/volume/network names, Traefik router names and URL paths, so they are restricted to `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, 3–32 chars (lowercase; no dots/underscores; no leading/trailing/double hyphen). Never relax this without auditing every place a username is interpolated.
- **Passwords:** bcrypt (default cost 12, configurable 10–15), 8–72 bytes (bcrypt ignores bytes past 72). Login always spends one bcrypt compare — unknown user, bad format, or overlong password included — and returns one generic error, so neither response nor timing reveals which usernames exist.
- **Sessions:** random 256-bit token in an `HttpOnly`, `SameSite=Lax` cookie (`Secure` when `BERTH_COOKIE_SECURE=true`); only `sha256(token)` is stored. Fixed TTL (`BERTH_SESSION_TTL`). Logout deletes the session, so the cookie is dead for every *new* request and WebSocket upgrade; **an already-open ttyd/neko stream is not terminated** (ForwardAuth runs once per request/upgrade).
- **ForwardAuth (`/api/auth/check`)** takes the original URI from `X-Forwarded-Uri` (Traefik sets it; keep `trustForwardHeader=false`) and accepts only `/ws/<user>/(terminal|browser)[/...]`: an encoded slash (`%2f`) is refused outright; the rest is percent-decoded and must have no empty, `.` or `..` segment, backslash or control character (so encoded backslashes and NULs are refused too). No/invalid session → 401; valid session for another user or any non-matching path → 403; a store error → 500. Fail closed. The endpoint is not routed publicly (hygiene — it only answers about the caller's own cookie).
- **Docker socket:** Traefik mounts `/var/run/docker.sock` (read-only does not limit the API); the backend will in M2 (needs `group_add` for the socket's GID). Both are root-equivalent on the host; keep the backend's attack surface small.
- Request IDs are server-generated; client `X-Request-Id` is ignored. Traefik drops header-alias spoofing (`aliasheadersstrategy=delete`).
- `BERTH_DEFAULT_CPUS` / `_MEM_MB` are finite and bounded; JSON responses are marshalled before the status line is written.

## Verified facts (against Traefik 3.7.16 + Docker 29.9, not assumptions)

- ForwardAuth is always a `GET` carrying only the headers in `authRequestHeaders` plus Traefik-set `X-Forwarded-{Method,Proto,Host,Uri,For}`; a client-sent `X-Forwarded-Uri` or `X_Forwarded_Uri` is overwritten/ignored. The check sees no `Upgrade`/`Origin`.
- Traefik withholds containers whose healthcheck is `starting`/unhealthy (they 404 until healthy) — a workspace `HEALTHCHECK` is a natural "container ready" signal for `starting → running`.
- Every container on a shared user-defined bridge can reach every other (name and IP). With per-user bridges, Docker's iptables isolation blocks cross-network traffic by IP too (tested). Default address pools allow only **~29 user networks** (`all predefined address pools have been fully subnetted`): set `default-address-pools` in `daemon.json` before onboarding users.
- A container attached to a network via `docker network connect` loses that attachment when it is recreated (e.g. Traefik after `docker compose up -d` with a new image): anything relying on it needs an idempotent ensure/reconcile step.

## Deviations from the spec (confirm at milestone review)

`users.is_admin` column (needed for "first user is admin"); `sessions` table (server-side sessions so logout truly revokes); `workspaces.volume_name UNIQUE` and CHECK constraints; extra package `internal/config`; `GET /healthz`; `/api/auth/check` returns **401** (no session) vs **403** (wrong user) where the spec says "otherwise 403"; register returns `{user, workspace}` and does not log in; Go floor 1.24 (above); hardening flags on Traefik (`encodedcharacters.*`, `aliasheadersstrategy`, ForwardAuth `maxResponseBodySize`); the ForwardAuth middleware is defined in `traefik/dynamic.yml` (file provider) rather than in container labels; the backend image is `berth-backend:local`.

## Open decisions (resolve with the user before/in the named milestone)

- **D1 (M2) Network isolation.** Spec has every workspace on shared `berth-proxy` *and* "containers must not reach each other": contradictory — demonstrated: alice's container opens bob's unauthenticated ttyd and the backend API directly. Recommended: workspaces join **only** `berth-net-<username>`; the backend `docker network connect`s the Traefik container to each user network (`BERTH_TRAEFIK_CONTAINER`), label `traefik.docker.network=berth-net-<username>`; `berth-proxy` holds only Traefik + backend; reconcile re-attaches Traefik after it is recreated; `cap_drop NET_RAW`; set `default-address-pools`.
- **D2 (M4) Idle signal.** The backend never sees WebSocket open/close. Proposed: heartbeat primary; `handleCheck` touches `last_active_at` (throttled ~1 write/min/user); CPU ≥ 5% also counts as activity; poll neko's stats API; ttyd via socket count or skip.
- **D3 (M1/M5) Admin bootstrap.** "First registered user is admin" is a takeover window on a fresh public deploy, and a DB reset re-opens it while `berth-data-*` volumes survive (a new registrant of a surviving name inherits its files). Proposed: optional `BERTH_ADMIN_USERNAME`/bootstrap token, `berth admin promote`, and a volume owner label.
- **D4 (M2) Cookie scope.** Traefik forwards the full `Cookie` (incl. `berth_session`) to ttyd/neko, so code in a workspace can read the owner's API token (admin → platform admin). Proposed: second cookie `berth_ws` (Path=/ws/, accepted only by `/api/auth/check`), `berth_session` narrowed to Path=/api. Also WebSocket `Origin` checks (ttyd `--check-origin`, neko allow-list, or pass `Origin` to the check).
- **D5 (M2) Docker plumbing.** (a) `github.com/docker/docker/client` (last release v28.5.2+incompatible) cannot be tidied on Go 1.24 (transitive deps need 1.26); successor `github.com/moby/moby/client` supports Go 1.24 — recommend it, with API version negotiation. (b) The backend container needs the socket mount + `group_add: [<docker gid>]`. (c) Volume at registration vs on first start: recommend registration stays DB-only and an idempotent ensure-volume runs at start and in reconcile.
- **D6 (M2) neko WebRTC.** Identity port mapping (`-p 52000-52007:52000-52007/udp`), `NAT1TO1`/public IP env (`BERTH_PUBLIC_IP`), pinned neko tag, small per-user range allocated from a dedicated range below 32768 at first start, `userland-proxy=false`.
- **D7 (M2) Container hardening.** `--pids-limit`, `no-new-privileges`, seccomp for Chromium instead of `SYS_ADMIN`, `--shm-size` ≤ half of memory, disk quota for `berth-data-*` (and keep `berth-state` off that disk).
- **D8 (M2/M3) "login / power on".** Explicit `POST /api/workspace/start` only; start is asynchronous (30 s server write timeout); guard double-start with a compare-and-swap `UPDATE ... WHERE state IN (...)` (the Store needs `TransitionWorkspace` with an `ErrStateConflict`).
- **D9 (M2) Image names.** `berth/…` is a Docker Hub namespace owned by someone else: the backend image is `berth-backend:local` with `pull_policy: build`. The spec's `berth/workspace:latest` must never be auto-pulled (`pull_policy: never`/build locally) or be renamed.
- **D10 (M5) Login rate limiting** (deliberately deferred) and open registration.
