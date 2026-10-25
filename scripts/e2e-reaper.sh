#!/usr/bin/env bash
# Real-stack test of the reaper (Milestone 4): Traefik + the backend + Docker + a REAL
# workspace container, with short timeouts (idle 1m, reclaim 1m, a look every 10 s).
#
#   scripts/e2e-reaper.sh            leaves nothing behind
#   KEEP=1 scripts/e2e-reaper.sh     leaves the stack up (for web/e2e/reaper.mjs, for example)
#
# Needs: docker with Compose, the images berth-backend:local and berth/workspace:latest
# (`docker compose build`), jq, curl and python3. It writes a throw-away .env next to
# docker-compose.yml (git-ignored) and removes it at the end, and it DELETES the stack's
# state (`docker compose down -v`, every berth-ws-* container, berth-net-* network and
# berth-data-* volume): run it on a test host. It takes about 12 minutes.
#
# The workspace state is read from a copy of the backend's SQLite database, not from the
# API: GET /api/workspace reconciles by itself, and the point is what the reaper does when
# nobody asks. Programs are run in the workspace with `docker exec`, so no terminal
# connection (and no ttyd behaviour on close) is involved.
set -u
REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK=${WORK:-$(mktemp -d)}
PORT=${PORT:-18090}
B=http://127.0.0.1:$PORT
J=$WORK/jar
H='Content-Type: application/json'
DOCKER_GID=$(stat -c %g /var/run/docker.sock)
export DOCKER_GID BERTH_HTTP_PORT=$PORT

pass=0; fail=0
ok()    { pass=$((pass+1)); printf 'PASS %s\n' "$1"; }
bad()   { fail=$((fail+1)); printf 'FAIL %s\n' "$1"; }
check() { if [ "$2" -eq 0 ]; then ok "$1"; else bad "$1"; fi; }
now()   { date +%s; }

# dbq SQL: run a query on a consistent copy of the backend's database.
dbq() {
  rm -rf "$WORK/db"; mkdir -p "$WORK/db"
  docker cp berth-berth-1:/data/. "$WORK/db/" >/dev/null 2>&1
  python3 -I - "$WORK/db/berth.db" "$1" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
for r in c.execute(sys.argv[2]):
    print('|'.join('' if x is None else str(x) for x in r))
PY
}
dbstate() { dbq "select state from workspaces w join users u on u.id=w.user_id where u.username='alice'"; }
dbfield() { dbq "select coalesce($1,'') from workspaces w join users u on u.id=w.user_id where u.username='alice'"; }
# waitdb STATE SECONDS: wait until the database says alice's workspace is in STATE; sets ELAPSED.
waitdb() {
  local want=$1 max=$2 t0 s; t0=$(now)
  while [ $(( $(now) - t0 )) -lt "$max" ]; do
    s=$(dbstate); [ "$s" = "$want" ] && { ELAPSED=$(( $(now) - t0 )); return 0; }
    sleep 2
  done
  ELAPSED=$(( $(now) - t0 )); return 1
}
apistate() { curl -sS -b "$J" $B/api/workspace | jq -r '.workspace.state'; }
wait_api() { local i; for i in $(seq 1 "$2"); do [ "$(apistate)" = "$1" ] && return 0; sleep 1; done; return 1; }
beat() { curl -sS -o /dev/null -b "$J" -X POST -H "$H" $B/api/workspace/heartbeat; }
ctr_running() { [ "$(docker inspect berth-ws-alice --format '{{.State.Running}}' 2>/dev/null)" = true ]; }
inws() { docker exec -u 1000 berth-ws-alice "$@"; }

cleanup() {
  rm -f "$REPO/.env"
  if [ -z "${KEEP:-}" ]; then
    (cd "$REPO" && docker compose down -v >/dev/null 2>&1)
    docker ps -aq --filter name=berth-ws- | xargs -r docker rm -f >/dev/null 2>&1
    docker network ls -q --filter name=berth-net- | xargs -r docker network rm >/dev/null 2>&1
    docker volume ls -q --filter name=berth-data- | xargs -r docker volume rm >/dev/null 2>&1
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== setup: a fresh stack with idle 1m, reclaim 1m and a look every 10 s"
(cd "$REPO" && docker compose down -v >/dev/null 2>&1)
docker ps -aq --filter name=berth-ws- | xargs -r docker rm -f >/dev/null 2>&1
docker network ls -q --filter name=berth-net- | xargs -r docker network rm >/dev/null 2>&1
docker volume ls -q --filter name=berth-data- | xargs -r docker volume rm >/dev/null 2>&1
cat > "$REPO/.env" <<EOF
DOCKER_GID=$DOCKER_GID
BERTH_HTTP_PORT=$PORT
BERTH_LOG_LEVEL=debug
BERTH_BCRYPT_COST=10
BERTH_IDLE_TIMEOUT=1m
BERTH_RECLAIM_AFTER=1m
BERTH_REAPER_INTERVAL=10s
BERTH_IDLE_CPU_PERCENT=5
EOF
# --no-deps: the one-shot workspace-image service would build or copy the image again.
(cd "$REPO" && docker compose up -d --no-build --no-deps traefik berth 2>&1 | grep -iE "error|fail")
for i in $(seq 1 90); do [ "$(docker inspect berth-berth-1 --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ] && break; sleep 1; done
check "stack is up" $([ "$(docker inspect berth-berth-1 --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ]; echo $?)
docker logs berth-berth-1 2>&1 | grep -q 'BERTH_IDLE_TIMEOUT is 1m0s'; check "the backend warns about the short idle timeout" $?
docker logs berth-berth-1 2>&1 | grep -q 'reaper started'; check "the reaper announces its settings at start" $?
curl -sS -o /dev/null $B/api/auth/register -H "$H" -d '{"username":"alice","password":"correct horse"}'
curl -sS -o /dev/null -c "$J" $B/api/auth/login -H "$H" -d '{"username":"alice","password":"correct horse"}'

echo "== A. power on alice, write a file"
curl -sS -o /dev/null -b "$J" -X POST -H "$H" $B/api/workspace/start
wait_api running 150; check "alice reaches running" $?
ID1=$(docker inspect berth-ws-alice --format '{{.Id}}')
PW1=$(curl -sS -b "$J" $B/api/workspace | jq -r '.workspace.neko_password')
inws sh -c 'echo persisted-data > ~/persist.txt' ; check "wrote a file on the data volume" $?

echo "== B. heartbeats keep it alive well past the 1m timeout (140 s, one beat every 15 s)"
t0=$(now)
while [ $(( $(now) - t0 )) -lt 140 ]; do beat; sleep 15; done
ctr_running; check "still running after 140 s of heartbeats" $?
[ "$(dbstate)" = running ]; check "database says running" $?
la=$(dbq "select strftime('%s',last_active_at) from workspaces"); age=$(( $(now) - la ))
[ "$age" -le 25 ]; check "last_active_at follows the heartbeats (${age}s old)" $?
echo "   idle CPU sample: $(docker stats --no-stream --format '{{.CPUPerc}}' berth-ws-alice)"

echo "== C. the heartbeats stop: idle stop"
waitdb stopped 150; r=$?; check "the reaper stops the unused workspace (after ${ELAPSED}s)" $r
[ "$ELAPSED" -ge 35 ] && [ "$ELAPSED" -le 100 ]; check "...about the 60 s timeout plus up to one 10 s look (took ${ELAPSED}s)" $?
[ "$(docker inspect berth-ws-alice --format '{{.Id}} {{.State.Status}}')" = "$ID1 exited" ]; check "its container is kept (exited)" $?
[ -n "$(dbfield last_stopped_at)" ]; check "last_stopped_at is recorded" $?
docker logs berth-berth-1 2>&1 | grep -q 'stopped an idle workspace'; check "the stop is logged at info" $?
docker volume ls -q | grep -qx berth-data-alice; check "the data volume exists" $?

echo "== D. stopped for the reclaim age (1m): the container is removed, the volume is not"
waitdb offline 120; r=$?; check "the reaper removes the stopped workspace's container (${ELAPSED}s after the stop was seen)" $r
docker inspect berth-ws-alice >/dev/null 2>&1; [ $? -ne 0 ]; check "the container is gone from Docker" $?
docker volume ls -q | grep -qx berth-data-alice; check "the data volume survived" $?
[ -z "$(dbfield container_id)" ] && [ -z "$(dbfield udp_port_start)" ]; check "container_id and the UDP block are cleared in the database" $?
[ "$(dbfield neko_password)" = "$PW1" ]; check "the browser password is kept" $?
docker logs berth-berth-1 2>&1 | grep -q 'reclaimed a stopped workspace'; check "the removal is logged" $?
docker logs berth-berth-1 2>&1 | grep -q 'is not recorded in the database'; [ $? -ne 0 ]; check "the reaper does not report its own removal as an unrecorded container" $?

echo "== E. power on again: a new container, same volume and password"
curl -sS -o /dev/null -b "$J" -X POST -H "$H" $B/api/workspace/start
wait_api running 150; check "alice is running again" $?
[ "$(docker inspect berth-ws-alice --format '{{.Id}}')" != "$ID1" ]; check "it is a new container" $?
[ "$(curl -sS -b "$J" $B/api/workspace | jq -r '.workspace.neko_password')" = "$PW1" ]; check "the browser password is unchanged" $?
[ "$(inws cat /home/user/persist.txt)" = persisted-data ]; check "the file survived the stop AND the removal" $?
[ "$(dbfield udp_port_start)" = 30000 ]; check "the UDP block was allocated again" $?

echo "== F. a busy workspace is not stopped; an idle one is"
docker exec -d -u 1000 berth-ws-alice sh -c 'exec yes >/dev/null'
sleep 3
[ "$(inws pgrep -c yes)" -ge 1 ]; check "a CPU-bound job is running in the workspace" $?
t0=$(now); stayed=0
while [ $(( $(now) - t0 )) -lt 150 ]; do
  [ "$(dbstate)" = running ] || { stayed=1; break; }
  sleep 10
done
[ $stayed -eq 0 ]; check "no heartbeats for 150 s, yet still running because the CPU is busy" $?
la=$(dbq "select strftime('%s',last_active_at) from workspaces"); age=$(( $(now) - la ))
[ "$age" -ge 140 ]; check "busy is not user activity: last_active_at was not rewritten (${age}s old)" $?
[ "$(docker logs berth-berth-1 2>&1 | grep -c 'workspace is busy')" = 1 ]; check "the busy verdict is logged once, on the change" $?
echo "   busy CPU sample: $(docker stats --no-stream --format '{{.CPUPerc}}' berth-ws-alice)"
inws pkill yes
waitdb stopped 150; r=$?; check "after the job ends the workspace is stopped (${ELAPSED}s)" $r
[ "$ELAPSED" -ge 35 ] && [ "$ELAPSED" -le 110 ]; check "...about the 60 s timeout after the last busy look (took ${ELAPSED}s)" $?

echo "== G. the reaper reconciles Docker with the database on its own"
curl -sS -o /dev/null -b "$J" -X POST -H "$H" $B/api/workspace/start
wait_api running 150; check "alice is running (container kept, docker start)" $?
docker kill berth-ws-alice >/dev/null
waitdb stopped 40; r=$?; check "a crashed container is recorded as stopped by the reaper alone (${ELAPSED}s)" $r
docker rm -f berth-ws-alice >/dev/null
waitdb offline 40; r=$?; check "a container removed by hand is recorded as offline by the reaper alone (${ELAPSED}s)" $r
[ -z "$(dbfield udp_port_start)" ]; check "...and its UDP block is released" $?

echo "== H. a restarted backend gives everyone a full timeout before it stops anyone"
curl -sS -o /dev/null -b "$J" -X POST -H "$H" $B/api/workspace/start
wait_api running 150; check "alice is running" $?
beat
ts=$(now); docker stop berth-berth-1 >/dev/null; took=$(( $(now) - ts ))
[ "$took" -le 10 ]; check "the backend stops within Docker's 10 s grace (${took}s)" $?
docker logs berth-berth-1 2>&1 | tail -3 | grep -q 'shutting down'; check "...and logged a clean shutdown" $?
echo "   backend down for 80 s while the heartbeats would fail..."
sleep 80
docker start berth-berth-1 >/dev/null
for i in $(seq 1 60); do [ "$(docker inspect berth-berth-1 --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ] && break; sleep 1; done
t0=$(now)
la=$(dbq "select strftime('%s',last_active_at) from workspaces"); echo "   last_active_at is $(( $(now) - la ))s old at the restart (timeout 60 s)"
sleep 35
[ "$(dbstate)" = running ]; check "35 s after the restart it is still running although its last activity is older than the timeout" $?
waitdb stopped 120; r=$?; check "and it is stopped once a full timeout has passed without a heartbeat" $r
tot=$(( $(now) - t0 )); [ "$tot" -ge 50 ] && [ "$tot" -le 110 ]; check "...${tot}s after the restart (timeout 60 s + a look)" $?

echo "== the backend's log"
! docker logs berth-berth-1 2>&1 | grep -qi 'panic'; check "no panic in the backend log" $?
n=$(docker logs berth-berth-1 2>&1 | grep -c 'could not look after every workspace'); echo "   'could not look after every workspace' appeared $n time(s)"
docker logs berth-berth-1 2>&1 | grep -E 'stopped an idle|reclaimed a stopped|reaper started|workspace is busy|no longer busy' | sed 's/^/   /' | cut -c1-220 | head -20
echo "== RESULT: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
