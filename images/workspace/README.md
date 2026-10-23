# images/workspace

The per-user workspace image `berth/workspace:latest`: [neko](https://github.com/m1k1o/neko) v3 (WebRTC remote
Chromium) + [ttyd](https://github.com/tsl0922/ttyd) (web terminal) + CLI tools, all running as the non-root user
`user` (uid/gid 1000, home `/home/user`). One container per Berth user; the backend creates it, Traefik routes to it.

```
docker build -t berth/workspace:latest images/workspace
docker build --build-arg BASE=<other neko v3 chromium image> -t berth/workspace:latest images/workspace
```

`BASE` defaults to `ghcr.io/m1k1o/neko/chromium:3.2.0`. That tag exists (checked 2026-10-23 with registry metadata
calls: it resolves to index `sha256:270face8b8111e639744fb7bcdbbf0deed227900bb89d58b2f8adfc12af8c41e` with linux/amd64
and linux/arm64 entries; 3.2.0 is the newest stable release, 2026-10-15). The build needs the network (apt, Debian
trixie). Nothing in the Dockerfile is architecture specific. Bump `BASE` deliberately: step 3 patches neko's own
supervisord/Chromium config and **fails the build** if that config no longer has the expected shape.

## What is inside

| What | How |
|---|---|
| Processes | `supervisord` (root, PID 1 under `--init`) starts neko's programs (`x-server`, `pulseaudio`, `openbox`, `chromium`, `neko`) and ours (`ttyd`) as `user`; every program has `autorestart=true` |
| ttyd | `/usr/bin/ttyd` (distro package), port **7681**, `--writable --check-origin --ping-interval 30`, no base path, cwd `/home/user`, runs `/bin/bash -l`; config `rootfs/etc/neko/supervisord/ttyd.conf` |
| neko | port **8080**, serves under `NEKO_SERVER_PATH_PREFIX` (set per user at runtime) |
| Chromium | neko's own flags (incl. `--no-sandbox`), profile `--user-data-dir=/home/user/.config/chromium` |
| Tools | git curl wget vim nano tmux htop jq less unzip openssh-client procps iputils-ping ca-certificates python3 bash-completion |
| User | `user` uid/gid 1000, shell bash, groups audio/video/pulse. This is neko's own `neko` user renamed in place (`usermod -l`), so the files neko/Chromium/Xorg/pulse need are already owned by it. No sudo, no root inside the terminal |
| Dotfiles | `/home/user/.bashrc` (prompt `user@berth:dir$`, red `!N` after a failed command, big shared history) and `.profile`; copies in `/etc/berth/skel` to restore defaults |
| Healthcheck | `berth-healthcheck`: ttyd `GET http://127.0.0.1:7681/` **and** neko `GET http://127.0.0.1:8080<prefix>/health`; `--interval=5s --timeout=3s --start-period=40s --retries=3` |
| Logs | per-program files in `/var/log/neko/*.log` (rotated); `docker logs` only has supervisord's own events (spawn/exit/FATAL) |

## Runtime contract (what the backend must do)

Flags this image was tested with (nothing else is needed): `--memory 2g --cpus 2 --shm-size 2g --pids-limit 2048
--cap-drop NET_RAW --security-opt no-new-privileges --init --stop-timeout 15`, no restart policy, not privileged, no
`--cap-add`, default seccomp, a user-defined bridge network, and a named volume at `/home/user`.

* **Do not pass `--user`/`-u`.** PID 1 (supervisord) must start as root and drops to `user` per program.
* **Volume**: mount the named volume at `/home/user`. Docker copies the image's `/home/user` (dotfiles, Chromium
  seed Preferences) into a *new empty* named volume with the right ownership; an existing volume is never touched by
  a newer image (so dotfile fixes in a new image do not reach existing users; they can copy from `/etc/berth/skel`).
* **Ready** = Docker health `healthy` (about 6 to 15 s after start in the sandbox; Traefik withholds the container
  until then).
* **Routing**: ttyd is plain HTTP + WebSocket at `/` and `/ws` (Traefik must `StripPrefix /ws/<user>/terminal`).
  neko must **not** be stripped: it serves everything under its own prefix.
* **Load the browser at `/ws/<user>/browser/` with the trailing slash.** neko answers the slash-less URL too, but its
  `index.html` uses relative asset URLs (`./assets/...`), which then resolve to `/ws/<user>/assets/...` and 404.
* **ttyd ignores PAUSE and hangs up on slow pingers.** ttyd 1.7.7 does not honour the client's flow-control PAUSE (6 MB
  of output arrived within 1 s after a PAUSE) and closes a client that has not answered a ping within twice the ping
  interval (10 s at the default 5 s). A page that is busy rendering a flood of output therefore lost its shell
  (the client reconnects into a new one); `--ping-interval 30` in `ttyd.conf` stretches that to 60 s.
* **A closed terminal can leave a `bash -l` behind.** If the socket closes while bash waits at readline's
  "Display all N possibilities? (y or n)" prompt, bash ignores the SIGHUP ttyd sends and keeps running (ttyd logs
  `killing process` but never `process killed with signal 1`) until the container stops. Closing at an idle prompt, after
  `echo hi`, or after clearing the line with Ctrl-U ended the shell as expected. CLAUDE.md, decision D12.
* **ttyd `--check-origin`** rejects a WebSocket upgrade whose `Origin` host differs from `Host`, and also one with
  no `Origin` at all (non-browser clients must send `Origin` equal to `Host`). Traefik keeps `Host`, so browsers work.
* **neko login without a second prompt**: the neko client (3.2.0) auto-logs in from `?usr=<name>&pwd=<password>` and
  then removes both from the URL (verified in Chromium 141: `POST api/login` 200, the frame's `location.search` is
  `?embed=1` afterwards). `embed=1` hides neko's own header, side panel and bars and keeps the video with its
  Fullscreen and Picture-in-Picture buttons (`cast=1` also hides those two). Berth uses the member password
  (`neko_password` from the API); the admin role can change resolution and settings, the member role cannot.
  neko keeps its session token in the page origin's `localStorage` key `neko_session`: a stored token makes the next
  load take over the previous session (a second tab kicks the first), so the Berth SPA clears it before creating the
  frame and on logout.

### neko environment variables (v3 names)

Names come from `neko serve --help` of the image (`NEKO_` + flag name, upper case, `.` to `_`).

| Variable | Value | Notes |
|---|---|---|
| `NEKO_SERVER_PATH_PREFIX` | `/ws/<user>/browser` | **required**. Verified: `/ws/<user>/browser/`, `.../health`, `.../api/*` and the WebSocket `.../api/ws` answer; bare `/` and `/health` are 404. neko's own image HEALTHCHECK (`/health`) would therefore report unhealthy; this image replaces it |
| `NEKO_MEMBER_MULTIUSER_USER_PASSWORD` | random secret | **required**: the default is `neko`. Role: regular user |
| `NEKO_MEMBER_MULTIUSER_ADMIN_PASSWORD` | another random secret | **required**: the default is `admin`. Role: admin |
| `NEKO_DESKTOP_SCREEN` | `1280x720@30` | already neko's default; verified through `GET api/room/screen` |
| `NEKO_WEBRTC_UDPMUX` | e.g. `30000` | **Berth's default** (`BERTH_UDP_PORTS_PER_WORKSPACE=1`, a block of one port): one UDP socket serves every viewer of the workspace. Must equal the published port (`-p 30000:30000/udp`, identity mapping). Berth sets this **instead of** `NEKO_WEBRTC_EPR` whenever the workspace's recorded block is a single port |
| `NEKO_WEBRTC_EPR` | e.g. `52000-52007` | each viewer takes one port of the range. Berth sets this **instead of** `NEKO_WEBRTC_UDPMUX` whenever the recorded block has two or more ports (`BERTH_UDP_PORTS_PER_WORKSPACE` above 1, or a workspace created while the default was 8: a block keeps its size). Must equal the published range (`-p 52000-52007:52000-52007/udp`, identity mapping) |
| `NEKO_WEBRTC_NAT1TO1` | the host's public IP | address put into the ICE candidates |
| `NEKO_SERVER_PROXY` | `true` | trust Traefik's `X-Forwarded-*` |
| `NEKO_SESSION_IMPLICIT_HOSTING` | `true` | the single user gets control on first click (verified in neko's `system/init` settings) |
| `NEKO_SERVER_METRICS` | `false` | otherwise `<prefix>/metrics` answers for any logged-in Berth user (verified: 404 when false) |

Berth never sets both of these: the manager records a UDP block before it creates the container, and the block's size decides which one is set.

Do **not** use the v2 names (`NEKO_EPR`, `NEKO_NAT1TO1`, `NEKO_PASSWORD`, `NEKO_PASSWORD_ADMIN`, `NEKO_SCREEN`,
`NEKO_PATH_PREFIX`, ...). They still parse but neko logs `ERR deprecated V2 configuration is in use`, and upstream
documents that any v2 variable switches neko to a legacy compatibility mode (not verified here). The v3 name wins
when both are set. Note that the project spec's `NEKO_EPR` is a v2 name: use `NEKO_WEBRTC_EPR`.

The WebRTC media path was tested with headless Chromium 141 (VP8 1280x720 playing in the SPA's iframe, input and
clipboard working): on the Docker host the connection works whatever `NEKO_WEBRTC_NAT1TO1` says, because the host can
route to the workspace's bridge address; from a client that cannot (Chromium in another network namespace) it connects
only with `NEKO_WEBRTC_NAT1TO1` set to a reachable address, through the published UDP port (iptables DNAT counter
confirmed), and never connects without it. That is why `BERTH_PUBLIC_IP` is required for every remote browser. The
browser only offers an mDNS host candidate, which neko cannot resolve; the connection works through a peer-reflexive
pair. Not tested: a real NAT or cloud firewall, IPv6, `userland-proxy=false` at scale, TURN, audio.

`NEKO_WEBRTC_UDPMUX=<port>` (Berth's default, one port) serves every viewer from a single UDP socket; `NEKO_WEBRTC_EPR=a-b`
gives each viewer a port of the range. EPR leaked sockets in tests (2 of 6 reloads that reused a stored session left
their port bound; after enough of them the 8-port block was full and no new viewer could connect until neko was
restarted) and a closed tab holds its port for 5-10 s; UDPMUX had neither problem (10 of 10 reloads, 3 concurrent
viewers). With an empty `NEKO_WEBRTC_NAT1TO1` neko calls `https://checkip.amazonaws.com` at every start. By default
the browser also contacts `stun:stun.l.google.com:19302` (an empty `NEKO_WEBRTC_ICESERVERS_*` falls back to it); set
both `NEKO_WEBRTC_ICESERVERS_FRONTEND`/`_BACKEND` to a non-empty JSON list to use your own (untested).

## Chromium profile (persistence)

The profile is `/home/user/.config/chromium`, i.e. on the data volume (neko hard-codes `/home/neko/...`; the
Dockerfile rewrites it). Two things had to be fixed for the data to survive a container removal:

1. **Cookies were session-only.** neko seeds `Preferences` with `"cookies": 4` (session only) and Chromium deletes
   such cookies when it stops. `rootfs/etc/chromium/policies/managed/berth.json` sets `DefaultCookiesSetting: 1`
   (a managed policy beats the stored preference, and also applies to volumes seeded earlier). A/B on the supervised
   stop/start path: pref 4 loses the cookie, pref 1 or the policy keeps it; `--bwsi` (kept) makes no difference.
2. **Stale profile lock.** Chromium writes `SingletonLock -> <hostname>-<pid>` into the profile. After `docker kill`
   or a crash the volume keeps it, the next container has another hostname, and Chromium refuses to open the profile
   ("in use by another computer", no window). `berth-chromium` (neko's `chromium` program runs it) deletes the three
   `Singleton*` links before `exec chromium`; it runs only at program start, when no Chromium of this container is
   alive. After a hard kill Chromium may show its "didn't shut down correctly" notice (not observed in a screenshot
   check, not ruled out).

neko's Chromium policies (`/etc/chromium/policies/managed/policies.json`, unchanged apart from the home path) still
apply: downloads and DevTools are disabled and only two force-installed extensions are allowed (they download from
Google at runtime; extension persistence could not be tested without internet). Override with another file in that
directory if the product needs otherwise.

## Chromium and the hardening flags

Chromium starts and renders under all the flags above because neko starts it with `--no-sandbox`. Chromium's own
sandbox cannot start in this container (verified: without `--no-sandbox` it dies with "The setuid sandbox is not
running as root ... PR_SET_NO_NEW_PRIVS" and "Failed to move to new namespace ... Operation not permitted";
`unshare -U` is refused by the default seccomp profile). The smallest relaxation that would allow the sandbox is a
seccomp profile that permits unprivileged user namespaces (not `--cap-add SYS_ADMIN`); not tested.

## Files

```
Dockerfile
rootfs/                                       copied to / (root-owned)
  etc/neko/supervisord/ttyd.conf              ttyd program
  etc/chromium/policies/managed/berth.json    cookie policy
  etc/berth/skel/.bashrc, .profile            dotfile skeleton
  usr/local/bin/berth-chromium                Chromium launcher (stale lock cleanup)
  usr/local/bin/berth-healthcheck             container HEALTHCHECK
```

## Testing notes

Built and tested in a sandbox whose Docker daemon uses the `vfs` storage driver (no layer sharing: the 23-layer
neko image does not fit on the disk when pulled) and whose `docker build` has no route to the Debian mirrors. The
tested image therefore used a flattened copy of `docker.io/m1k1o/neko:chromium` as `BASE` and a variant of this
Dockerfile without step 1 (apt) that adds the official static ttyd 1.7.7 binary instead; the CLI tools are missing in
that image. Steps 2 and 3 and everything after them are byte-identical. Step 1 itself (apt) has not been run.
