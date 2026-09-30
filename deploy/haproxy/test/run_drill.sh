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
    pkill -f "stub_go" 2>/dev/null
    rm -f run/*.pid run/admin.sock
    echo "stopped"
}

mkdir -p run results
# admin.sock lands here from inside the container (haproxy uid 99) —
# the bind-mounted dir must be writable by it.
chmod 0777 run

[ "${1:-}" = "stop" ] && { stop_all; exit 0; }

# --- stubs: blue primary/backup, green primary/backup -------------------
# Backends run the Go stub when it builds (same runtime class as the order
# gateway it stands in for — proves blue/green for Go services); fall back
# to the Python stub when no Go toolchain is present.
pkill -f "stub_server.py" 2>/dev/null
pkill -f "stub_go" 2>/dev/null
STUB="./stub_go"
if command -v go >/dev/null 2>&1; then
    go build -o ./stub_go ./stub_server.go 2>/dev/null || STUB=""
else
    STUB=""
fi
if [ -z "$STUB" ]; then
    STUB="python3 stub_server.py"
fi
echo "stub backend: $STUB"
# stdout must be redirected too — a surviving stub holding the inherited
# stdout pipe blocks any caller that captures output (go exec
# CombinedOutput, `| tail`) forever after the script exits.
$STUB BLUE-1 18081 >run/blue1.log 2>&1  & echo $! > run/blue1.pid
$STUB BLUE-2 18082 >run/blue2.log 2>&1  & echo $! > run/blue2.pid
$STUB GREEN-1 18083 >run/green1.log 2>&1 & echo $! > run/green1.pid
$STUB GREEN-2 18084 >run/green2.log 2>&1 & echo $! > run/green2.pid
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
