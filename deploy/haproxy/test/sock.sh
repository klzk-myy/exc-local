#!/usr/bin/env bash
# sock.sh "<admin socket command>" — convenience wrapper for the stats socket.
cd "$(dirname "$0")"
echo "$1" | socat ./run/admin.sock -
