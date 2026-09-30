#!/usr/bin/env bash
# run_drill.sh — repeatable Task 5.3.29 validation drill.
# Prereqs: docker, python3, socat, curl, openssl (for cert regen if expired).
# Usage:  ./run_drill.sh            — full drill (stubs + haproxy + tests)
#         ./run_drill.sh stop       — tear down container and stubs
set -u
cd "$(dirname "$0")"
TESTDIR=$PWD
IMAGE=haproxy:2.9
NAME=haproxy-5329-test

stop_all() {
    docker rm -f "$NAME" 2>/dev/null
    pkill -f "stub_server.py" 2>/dev/null
    rm -f run/*.pid run/admin.sock
    echo "stopped"
}

[ "${1:-}" = "stop" ] && { stop_all; exit 0; }

# --- stubs: blue primary/backup, green primary/backup -------------------
pkill -f "stub_server.py" 2>/dev/null
python3 stub_server.py BLUE-1 18081 2>run/blue1.log  & echo $! > run/blue1.pid
python3 stub_server.py BLUE-2 18082 2>run/blue2.log  & echo $! > run/blue2.pid
python3 stub_server.py GREEN-1 18083 2>run/green1.log & echo $! > run/green1.pid
python3 stub_server.py GREEN-2 18084 2>run/green2.log & echo $! > run/green2.pid
sleep 0.5

# --- config check --------------------------------------------------------
echo "=== haproxy -c ==="
docker run --rm -v "$TESTDIR/haproxy.test.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro" \
    -v "$TESTDIR:/etc/haproxy:ro" \
    --add-host=host.docker.internal:host-gateway \
    "$IMAGE" haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg || exit 1

# --- run -----------------------------------------------------------------
docker rm -f "$NAME" 2>/dev/null
docker run -d --name "$NAME" \
    -v "$TESTDIR/haproxy.test.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro" \
    -v "$TESTDIR:/etc/haproxy:ro" \
    -v "$TESTDIR/run:/run/haproxy" \
    --add-host=host.docker.internal:host-gateway \
    -p 8080:80 -p 8443:443 \
    "$IMAGE"
sleep 1
# admin.sock lands on the host at ./run/admin.sock (bind mount) with the
# cfg's prod `mode 660` owned by the in-container haproxy uid — loosen it
# via the container's root so unprivileged host socat can drive it
# (drill-only; the cfg keeps 660).
docker exec -u root "$NAME" chmod 666 /run/haproxy/admin.sock
echo "proxy:  http://localhost:8080  https://localhost:8443 (-k)"
echo "socket: $TESTDIR/run/admin.sock"
