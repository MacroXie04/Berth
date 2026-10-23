# Berth

Single-host, multi-user "Linux workstation in the browser". One machine runs many Docker containers, one per user. Each container has a terminal (ttyd) and a remote browser (neko + Chromium). A user logs in on a web page and sees their own terminal and browser. Idle containers are stopped, later removed; user data lives on a dedicated volume and is restored on the next login.

**Scope guard: no Kubernetes, no message queue, no microservices.** The whole system is one Go backend process + Traefik + Docker. Do not over-engineer; prefer the simplest thing that satisfies this document.

## Working agreement

- Work in the milestones below, **one at a time**. After each milestone: list what was built, how it was verified, and the decisions that need confirmation — then **stop and wait for the go-ahead**.
- All configuration is via environment variables; every variable is documented in `.env.example` (add new ones in the same change that introduces them).
- Core logic (state transitions, idle detection) has unit tests. The Docker client is always behind an interface (`internal/docker`) so it can be mocked.
- No heavy frontend UI kits (Tailwind or plain CSS only).
- Develop on the branch the session designates; never push elsewhere.
- Verify against the real stack, not just unit tests: Traefik and Docker behave in ways the docs do not make obvious (see "Verified facts"). Mutation-test security-relevant logic: a test that still passes when the check is deleted is not a test.

## Tech stack

- **Backend:** Go, `chi` router, `github.com/moby/moby/client` for Docker (the spec's `github.com/docker/docker/client` last released v28.5.2+incompatible and cannot be tidied on Go 1.24; the moby module is its maintained successor), SQLite via `modernc.org/sqlite` (pure Go, **no cgo**). SQL is isolated behind the `store.Store` interface so Postgres can come later.
- **Proxy:** Traefik v3 (`traefik:v3.7`; Docker Engine 29 needs >= 3.6.1) with the Docker provider for routes and the file provider (`traefik/dynamic.yml`) for the shared ForwardAuth middleware. The backend never proxies user traffic itself.
- **Frontend:** React + Vite + TypeScript. Terminal = xterm.js; browser view = neko's own web UI in an iframe.
- **User image:** `images/workspace/` — `ghcr.io/m1k1o/neko/chromium` (pinned, amd64/arm64) + ttyd, CLI tools, non-root `user` (uid 1000, home `/home/user`). `berth/workspace:latest` is only ever built locally, never pulled (the `berth/` Docker Hub namespace is not ours).
- **Deploy:** `docker-compose.yml` brings up Traefik + backend (+ the frontend in M3) and builds the workspace image.

**Go version:** the spec says Go 1.22+. `go.mod` says `go 1.24` because that is the oldest Go the security-fixed `x/crypto` (>= v0.45.0), the moby client and a current SQLite (`modernc.org/sqlite` >= v1.46, bundling SQLite 3.51) support. The Dockerfile builds on a supported release (`golang:1.26-alpine`). Build with `GOTOOLCHAIN=local` and never let `go get @latest` raise the `go` directive unreviewed.

## Layout

```
cmd/berth/main.go      backend entrypoint (wires store, docker, manager; keeps Traefik attached)
internal/config/       env-var configuration, validated at startup
internal/api/          HTTP handlers, chi routes
internal/auth/         password hashing, usernames, session tokens
internal/store/        SQLite access behind the Store interface, migrations
internal/workspace/    lifecycle manager, pure state machine, Traefik labels, container spec
internal/docker/       Client interface, moby implementation, in-memory Fake for tests
internal/reaper/       periodic reclaim job (M4)
web/                   React frontend (M3)
images/workspace/      user container image
traefik/dynamic.yml    ForwardAuth middleware (file provider)
docker-compose.yml  Dockerfile  .env.example  README.md
```

## Data model (SQLite)

```
users(id, username UNIQUE, password_hash, is_admin, created_at)
sessions(token_hash PK, ws_token_hash UNIQUE, user_id, created_at, expires_at)
workspaces(user_id PK -> users, container_id NULL, state, volume_name UNIQUE,
           mem_limit_mb DEFAULT 2048, cpu_limit DEFAULT 2.0,
           last_active_at, last_stopped_at, updated_at,
           udp_port_start NULL UNIQUE, udp_port_end NULL, neko_password NULL)
```

One workspace per user. `state` ∈ `offline | starting | running | stopped` (CHECK-constrained). `container_id` NULL means the container was removed and only the volume remains. The first registered user is admin (decided atomically in the INSERT). Timestamps are UTC text `YYYY-MM-DD HH:MM:SS`; **never bind a `time.Time` directly** (the driver stores its monotonic-clock string) — use the store's `fmtTime`.

Schema changes are versioned migrations (`PRAGMA user_version`, contiguous `NNNN_name.sql`, each applied atomically); never edit a shipped migration. The store refuses a database newer than the binary. SQLite cannot `ADD COLUMN ... UNIQUE` or `NOT NULL` without a default: add nullable columns, backfill, then `CREATE UNIQUE INDEX` (partial, `WHERE ... IS NOT NULL`). Transactions are `BEGIN IMMEDIATE` (`_txlock=immediate`), so read-then-write sequences are safe; table-rebuild migrations need `PRAGMA foreign_keys=OFF` outside the transaction.

**State changes are compare-and-swap:** `Store.TransitionWorkspace(userID, from[], to, opts...)` is one `UPDATE ... WHERE state IN (from)` that returns `ErrStateConflict` if someone else got there first. That is what makes a double-clicked "Power on" create exactly one container. Always derive `from` from `workspace.Sources(event)`, never hard-code it.

## Lifecycle state machine

```
offline ──"power on"──▶ starting ──container healthy──▶ running
running ──idle timeout (default 30 min) or user "power off"──▶ stopped   (docker stop, container kept)
stopped ──"power on"──▶ starting ──▶ running                              (docker start, seconds)
stopped ──idle again (default 7 days)──▶ offline                           (docker rm, volume kept)
any ──container vanished──▶ offline
```

`internal/workspace/state.go` is the pure state machine (`Event`, `Next`, `Sources`, `Observe`) and is exhaustively unit-tested. **Login does not power on**; the user presses "Power on" (`POST /api/workspace/start`). Start returns once the container is started; **readiness is the image's Docker healthcheck** — `GET /api/workspace` promotes `starting → running` when it turns healthy. `Status` also notices crashes, hand-removed containers and containers started behind the backend's back, so the database follows Docker. An unhealthy *running* workspace stays `running` (the API reports `health: unhealthy`).

- Volume `berth-data-<username>` is mounted at `/home/user` and **never deleted in any state**. Registration only *records* its name; the volume is created idempotently on every start.
- All timeouts are env-configurable (defaults above).
- **Idle** = ttyd AND neko WebSockets both closed for longer than the threshold, AND container CPU (docker stats) stayed below 5%. Both must hold so background jobs are not killed. How the backend learns about WebSocket state is Open decision D2.
- Frontend calls `POST /api/workspace/heartbeat` every 30 s to refresh `last_active_at`.

## Container spec

- Image `BERTH_WORKSPACE_IMAGE` (default `berth/workspace:latest`); name `berth-ws-<username>`; mount `berth-data-<username>:/home/user`.
- `--memory` / `--cpus` from the workspaces row; `--shm-size=2g` (Chromium); `--pids-limit 2048`; `--cap-drop NET_RAW`; `--security-opt no-new-privileges`; `--init`; stop timeout 15 s; no restart policy (the backend owns the lifecycle).
- **Networking (decision D1, implemented):** a workspace joins **only** its private network `berth-net-<username>`. The backend connects the Traefik container to that network (`BERTH_TRAEFIK_CONTAINER`) and labels the router `traefik.docker.network=berth-net-<username>`. The shared `berth-proxy` network holds only Traefik and the backend, so workspaces cannot reach each other or the backend. `keepProxyAttached` re-attaches Traefik to every managed network at startup and every minute, because Docker drops attachments when the Traefik container is recreated.
- Env: neko screen/passwords/sub-path/WebRTC range (`internal/workspace/neko.go`). **neko v3 names** — the spec's `NEKO_EPR` is a v2 name (it still parses but logs "deprecated V2 configuration"): `NEKO_WEBRTC_EPR`, `NEKO_WEBRTC_NAT1TO1`, `NEKO_MEMBER_MULTIUSER_{USER,ADMIN}_PASSWORD` (defaults are `neko`/`admin`, so always set both), `NEKO_SERVER_PATH_PREFIX=/ws/<user>/browser` (neko 404s everywhere else; load the page **with the trailing slash**, its assets are relative), plus `NEKO_SERVER_PROXY=true`, `NEKO_SESSION_IMPLICIT_HOSTING=true` and `NEKO_SERVER_METRICS=false` (otherwise `<prefix>/metrics` answers for any logged-in user). The UDP block is allocated at container creation inside an immediate transaction, recorded in the table, published port-for-port with `-p`, and released when the container is removed.
- Traefik labels per user:
  - `<username>-term`: `PathPrefix("/ws/<username>/terminal")` → port 7681, StripPrefix middleware
  - `<username>-browser`: `PathPrefix("/ws/<username>/browser")` → port 8080 (not stripped: neko is told its own prefix)
  - both routers use the shared ForwardAuth middleware `berth-auth@file` (`traefik/dynamic.yml`, so it exists even while the backend is down: a dead backend then yields a fail-closed 5xx instead of a 404) → `/api/auth/check`. **List it before StripPrefix** (`middlewares=berth-auth@file,<u>-term-strip`): Traefik fills `X-Forwarded-Uri` from the URL at the moment ForwardAuth runs, so the other order shows the check a stripped path and every request gets 403.

## Backend API

```
M1  POST /api/auth/register     {username,password}
M1  POST /api/auth/login        {username,password} -> sets berth_session (Path=/api) and berth_ws (Path=/ws)
M1  POST /api/auth/logout
M1  GET  /api/auth/check        Traefik ForwardAuth: berth_ws cookie + username in path
M1  GET  /api/me                current user + workspace state (database only)
M1  GET  /healthz               liveness (compose healthcheck; not routed publicly)
M2  GET  /api/workspace         refreshed state, health, usage (docker stats), neko password while up
M2  POST /api/workspace/start | /stop | /heartbeat
M5  GET  /api/admin/workspaces  admin only (first registered user)
```

Errors are JSON `{"error": "..."}`. JSON bodies are required (`Content-Type: application/json`), size-capped, unknown fields rejected. Workspace errors never echo Docker internals (503 for no capacity or an unreachable daemon, 409 when a start is mid-creation, otherwise a generic 500; details go to the log). `Deps.Workspaces == nil` means "no container backend": workspace endpoints answer 503 and accounts still work.

## Frontend pages

1. Login / register. 2. Workspace: status bar (state, mem/CPU, power-off) + resizable split of xterm.js (`/ws/<username>/terminal`) and the neko iframe (`/ws/<username>/browser`); "Power on" button when not running. 3. Admin: table of users + states with force-stop.

## Reaper

Goroutine, every minute: (1) for each `running` workspace, decide idleness (CPU + `last_active_at`), `docker stop` → `stopped` + `last_stopped_at`; (2) for each `stopped` older than 7 days, `docker rm` → `offline`, clear `container_id`, release the UDP block (events `EventStop`/`EventReclaim` already exist); (3) reconcile all `berth-ws-*` containers against the DB (e.g. a container removed manually while the backend was down) and fold in the Traefik re-attach.

## Milestones

1. **Skeleton** — layout, CLAUDE.md, compose (Traefik + backend), SQLite schema, register/login. *Done.*
2. **Container management** — workspace image builds; `POST /api/workspace/start` launches a container with Traefik labels; `/ws/<username>/terminal` shows ttyd. *Done (see the M2 report).*
3. **Browser + frontend** — neko works; workspace page shows terminal and browser side by side.
4. **Reclaim** — reaper; verify with 1-minute timeouts that stop and rm both work and volume data survives.
5. **Admin page + polish** — admin endpoint + page; README with deployment steps.

## Security decisions (keep these invariant)

- **Usernames** are used in container/volume/network names, Traefik router names and URL paths, so they are restricted to `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, 3–32 chars (lowercase; no dots/underscores; no leading/trailing/double hyphen). Never relax this without auditing every place a username is interpolated. The manager re-validates before using one (`checkName`), and config values that reach Traefik label keys (entrypoint, container name) are validated at startup.
- **Passwords:** bcrypt (default cost 12, configurable 10–15), 8–72 bytes (bcrypt ignores bytes past 72). Login always spends one bcrypt compare — unknown user, bad format, or overlong password included — and returns one generic error, so neither response nor timing reveals which usernames exist.
- **Sessions: two cookies, two tokens (decision D4, implemented).** A login creates one session row with two independent 256-bit tokens, stored only as sha256: `berth_session` (`Path=/api`, the API credential) and `berth_ws` (`Path=/ws`, accepted *only* by the ForwardAuth check). Both are `HttpOnly`, `SameSite=Lax` (`Secure` when `BERTH_COOKIE_SECURE=true`). Traefik forwards the browser's cookies into the workspace container, where code running as the user can read them; because the API token is never sent under `/ws`, such code cannot call the API (and so cannot reach admin endpoints). Fixed TTL (`BERTH_SESSION_TTL`). Logout deletes the row (both tokens die), so the cookies are dead for every *new* request and WebSocket upgrade; **an already-open ttyd/neko stream is not terminated** (ForwardAuth runs once per request/upgrade).
- **ForwardAuth (`/api/auth/check`)** takes the original URI from `X-Forwarded-Uri` (Traefik sets it; keep `trustForwardHeader=false`) and accepts only `/ws/<user>/(terminal|browser)[/...]`: an encoded slash (`%2f`) is refused outright; the rest is percent-decoded and must have no empty, `.` or `..` segment, backslash or control character (so encoded backslashes and NULs are refused too). No/invalid session → 401; valid session for another user or any non-matching path → 403; a store error → 500. Fail closed. The endpoint is not routed publicly (hygiene — it only answers about the caller's own cookie).
- **Docker socket:** Traefik and the backend mount `/var/run/docker.sock` (read-only does not limit the API); both are root-equivalent on the host. The backend runs unprivileged and joins the socket's group (`DOCKER_GID`, required). Keep the backend's attack surface small; Berth only touches objects it labelled (`berth.managed`) and never removes volumes.
- **Never pull or adopt blindly:** the API only creates containers from a local image. If a container named `berth-ws-<u>` exists but is not labelled as that user's, start refuses rather than adopting or removing it.
- Request IDs are server-generated; client `X-Request-Id` is ignored. Traefik drops header-alias spoofing (`aliasheadersstrategy=delete`).
- `BERTH_DEFAULT_CPUS` / `_MEM_MB` are finite and bounded; JSON responses are marshalled before the status line is written.

## Verified facts (against Traefik 3.7.16 + Docker 29.9, not assumptions)

- ForwardAuth is always a `GET` carrying only the headers in `authRequestHeaders` plus Traefik-set `X-Forwarded-{Method,Proto,Host,Uri,For}`; a client-sent `X-Forwarded-Uri` or `X_Forwarded_Uri` is overwritten/ignored. The check sees no `Upgrade`/`Origin`.
- Traefik withholds containers whose healthcheck is `starting`/unhealthy (they 404 until healthy) — the workspace `HEALTHCHECK` is the "container ready" signal for `starting → running`.
- Every container on a shared user-defined bridge can reach every other (name and IP). With per-user bridges, Docker's iptables isolation blocks cross-network traffic by IP too (tested). Default address pools allow only **~29 user networks** (`all predefined address pools have been fully subnetted`): set `default-address-pools` in `daemon.json` before onboarding users (README has the snippet).
- A container attached with `docker network connect` loses that attachment when it is recreated (e.g. Traefik after `docker compose up -d` with a new image), and its workspaces then 504. The backend's re-attach loop repairs this; the e2e run measures the recovery time.
- moby client: starting a running container and stopping a stopped one return "not modified", which the wrapper treats as success; the daemon normalises `NET_RAW` to `CAP_NET_RAW` when recording `CapDrop`; `ContainerStats` with a previous sample takes ~1 s.
- Workspace image (`images/workspace/README.md` has the evidence): every process runs as uid 1000 `user` (supervisord is root and drops privileges per program; never pass `--user`); the image `HEALTHCHECK` probes ttyd **and** neko under its prefix (neko's own `/health` check would report unhealthy under a prefix, so it is replaced); a killed ttyd/neko/Chromium is respawned in 1–3 s; `docker stop` takes ~1–2 s; files and the Chromium profile survive `docker rm`. Two things only broke on a real volume: neko seeds Chromium with session-only cookies (fixed by a managed policy), and a `docker kill` leaves `SingletonLock` pointing at the old hostname so Chromium refuses to open (the `berth-chromium` launcher removes it). The base image leaves `/var/log/neko` world-writable while supervisord (root) opens the log files, which let `user` make root create a file at an arbitrary path through a symlink — fixed in the Dockerfile, with a build-time assertion.
- ttyd runs with `--check-origin`: it refuses a WebSocket upgrade whose `Origin` differs from `Host`, **and one with no `Origin` at all**. neko does not check `Origin` (a valid token opens its WebSocket from anywhere), see D4b.
- A backend container that is unhealthy/restarting does not take workspace routes down: the middleware lives in the file provider, so ForwardAuth fails with a 500 instead of a 404.

## Deviations from the spec (confirm at milestone review)

`users.is_admin` column (needed for "first user is admin"); `sessions` table (server-side sessions so logout truly revokes) with **two tokens per login** (`berth_session`/`berth_ws`); `workspaces.volume_name UNIQUE` and CHECK constraints; extra package `internal/config`; `GET /healthz`; `/api/auth/check` returns **401** (no session) vs **403** (wrong user) where the spec says "otherwise 403"; register returns `{user, workspace}` and does not log in; Go floor 1.24 and the **moby client** instead of `github.com/docker/docker/client`; hardening flags on Traefik (`encodedcharacters.*`, `aliasheadersstrategy`, ForwardAuth `maxResponseBodySize`); the ForwardAuth middleware lives in `traefik/dynamic.yml`; the backend image is `berth-backend:local`; the **data volume is created at first start**, not at registration; workspaces join **only** their private network (not the shared `berth-proxy`); the backend needs `DOCKER_GID`; UDP block allocated at container creation and released at removal; extra container hardening (`--pids-limit`, `--cap-drop NET_RAW`, `no-new-privileges`, `--init`).

## Open decisions (resolve with the user before/in the named milestone)

- **D2 (M4) Idle signal.** The backend never sees WebSocket open/close. Proposed: heartbeat primary; `handleCheck` touches `last_active_at` (throttled ~1 write/min/user); CPU ≥ 5% also counts as activity; poll neko's stats API; ttyd via socket count or skip.
- **D3 (M5) Admin bootstrap.** "First registered user is admin" is a takeover window on a fresh public deploy, and a DB reset (`docker compose down -v`) re-opens it while `berth-data-*` volumes survive (a new registrant of a surviving name inherits its files). Proposed: optional `BERTH_ADMIN_USERNAME`/bootstrap token, `berth admin promote`, and a volume owner label.
- **D4b (M3) WebSocket `Origin`.** The check never sees `Origin`; same-site/different-origin pages could open a workspace WebSocket with the victim's `berth_ws` cookie. ttyd already runs with `--check-origin` (verified); neko does not check `Origin` at all (verified), so M3 must either pass `Origin` to the check or rely on neko's token being unguessable.
- **D6 (M3) neko WebRTC.** Port range and `BERTH_PUBLIC_IP` plumbing exist and neko logs the settings it received; the media path from a real browser (NAT1TO1, firewall, `userland-proxy=false` at scale) has **not** been tested and is verified in M3. Same for the `?usr=&pwd=` auto-login the neko client is said to support.
- **D11 (M2, unverified) Image provenance.** `BASE` defaults to `ghcr.io/m1k1o/neko/chromium:3.2.0` (tag exists, multi-arch, checked by registry metadata). In the sandbox ghcr layers and the Debian mirrors are unreachable, so the image was built and tested on the Docker Hub `m1k1o/neko:chromium` base with the static ttyd 1.7.7 binary instead of apt: the apt step and a build on the pinned ghcr tag have never run. The Dockerfile asserts the exact neko config lines it patches, so a mismatch fails the build rather than the container.
- **D7 (M5) More hardening.** seccomp profile for Chromium instead of broader caps, disk quota for `berth-data-*` (and keep `berth-state` off that disk), `--shm-size` ≤ half of memory (spec says 2g; with the default 2 GiB limit a full `/dev/shm` can OOM the container).
- **D10 (M5) Login rate limiting** (deliberately deferred), open registration, and a cap on concurrent `docker stats` calls (each `GET /api/workspace` on a running workspace costs ~1 s of daemon time).
