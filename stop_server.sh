#!/bin/bash

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

LOG_DIR=logs

if [ ! -d "$LOG_DIR" ]; then
    echo "No logs directory found, cluster may not be running."
    exit 0
fi

echo "Stopping cluster..."
for pidfile in "$LOG_DIR"/*.pid; do
    [ -f "$pidfile" ] || continue
    pid=$(cat "$pidfile")
    if kill -0 "$pid" 2>/dev/null; then
        echo "Killing node (PID $pid)..."
        kill -15 "$pid" 2>/dev/null || kill -9 "$pid" 2>/dev/null
    fi
done

sleep 1
for port in 9001 9002 9003 7001 7002 7003 8001 8002 8003; do
    fuser -k "$port/tcp" 2>/dev/null || true
done

rm -f "$LOG_DIR"/*.pid
echo "Cluster stopped."
