# Berth per-user workspace image: neko (WebRTC remote Chromium) + ttyd (web terminal)
# + CLI tools, everything running as the non-root user "user" (uid 1000, home /home/user).
# Contract (ports, env vars, volume, health) is documented in README.md next to this file.
#
#   docker build -t berth/workspace:latest images/workspace
#   docker build --build-arg BASE=docker.io/m1k1o/neko:chromium -t berth/workspace:latest images/workspace
#
# BASE: the neko v3 Chromium image. The default is the GHCR image the project spec names,
# pinned to a release tag (3.2.0 is published there, 2026-10). Bump it deliberately: this
# Dockerfile patches neko's own supervisord/Chromium config (step 3) and fails the build
# if that config no longer looks the way the patches expect.
ARG BASE=ghcr.io/m1k1o/neko/chromium:3.2.0
FROM ${BASE}

LABEL org.opencontainers.image.title="berth-workspace" \
      org.opencontainers.image.description="Berth per-user workspace: ttyd terminal + neko/Chromium remote browser"

# 1. Distro packages (Debian trixie, same as the neko base): ttyd from the distro,
#    plus the CLI tools users expect. One layer, apt lists removed.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        bash-completion ca-certificates curl git htop iputils-ping jq less nano \
        openssh-client procps python3 tmux ttyd unzip vim wget; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# 2. Our files: ttyd's supervisord program, the Chromium launcher, the healthcheck,
#    and the dotfile skeleton (kept in /etc/berth/skel, installed into the home in step 3).
COPY rootfs/ /

# 3. Turn neko's user "neko" into "user" and point neko's config at the new name.
#    neko hard-codes "neko" in a few places (see the patches); each patch is asserted so
#    that a base image that changed shape breaks the build instead of the running container.
RUN set -eux; \
    # 3a. rename user + group in place: same uid/gid 1000, same audio/video/pulse groups,
    #     home moves to /home/user together with neko's seed content (Chromium Preferences, ...)
    usermod --login user --home /home/user --move-home --shell /bin/bash neko; \
    groupmod --new-name user neko; \
    mv /tmp/runtime-neko /tmp/runtime-user; \
    # 3b. neko's config: Chromium profile dir (-> the data volume), download allow-list,
    #     group of the supervisord control socket
    sed -i 's#/home/neko#/home/user#g' \
        /etc/neko/supervisord/chromium.conf /etc/chromium/policies/managed/policies.json; \
    sed -i 's#^chown=root:neko$#chown=root:user#' /etc/neko/supervisord.conf; \
    # 3c. start Chromium through our launcher, which removes the stale profile lock a
    #     killed container leaves on the volume (README: "Chromium profile")
    sed -i 's#^command=/usr/bin/chromium$#command=/usr/local/bin/berth-chromium#' \
        /etc/neko/supervisord/chromium.conf; \
    # 3d. supervisord echoes every line its programs print into `docker logs` at loglevel
    #     debug (each healthcheck probe would add some); programs keep their own log files
    #     in /var/log/neko, supervisord's own events (spawn/exit/FATAL) stay in `docker logs`
    sed -i 's#^loglevel=debug$#loglevel=info#' /etc/neko/supervisord.conf; \
    # 3e. dotfiles for new volumes (Docker copies /home/user into an empty named volume)
    install -o user -g user -m 0644 /etc/berth/skel/.bashrc  /home/user/.bashrc; \
    install -o user -g user -m 0644 /etc/berth/skel/.profile /home/user/.profile; \
    chmod 0755 /usr/local/bin/berth-chromium /usr/local/bin/berth-healthcheck; \
    # 3f. assertions
    grep -q '^command=/usr/local/bin/berth-chromium$' /etc/neko/supervisord/chromium.conf; \
    grep -q -- '--user-data-dir=/home/user/.config/chromium$' /etc/neko/supervisord/chromium.conf; \
    grep -q '^chown=root:user$' /etc/neko/supervisord.conf; \
    grep -q '^loglevel=info$' /etc/neko/supervisord.conf; \
    if grep -rIl --exclude='*-' -e '/home/neko' -e 'root:neko' /etc; then echo 'leftover neko references' >&2; exit 1; fi; \
    test "$(id -u user)" = 1000; \
    test "$(getent passwd user | cut -d: -f6)" = /home/user; \
    command -v ttyd

# neko runs its programs as $USER (supervisord.conf: user=%(ENV_USER)s)
ENV USER=user \
    XDG_RUNTIME_DIR=/tmp/runtime-user

EXPOSE 7681 8080

# Healthy only when ttyd (:7681) AND neko (:8080, under NEKO_SERVER_PATH_PREFIX) answer HTTP.
# A dead service is noticed after 3 failed probes (~15 s); a fresh container has 30 s
# during which failures do not count, and turns healthy at its first good probe.
HEALTHCHECK --interval=5s --timeout=3s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/berth-healthcheck"]

# CMD is inherited from the base image: supervisord -c /etc/neko/supervisord.conf
