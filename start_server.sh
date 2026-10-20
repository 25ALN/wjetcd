#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

NODES=${1:-3}
BASE_RAFT_PORT=8000
BASE_HTTP_PORT=9000
BASE_RPC_PORT=7000
LOG_DIR=logs

echo "Building server..."
go build -o bin/server ./cmd/server

mkdir -p "$LOG_DIR"

peers=""
for i in $(seq 1 $NODES); do
    [ -n "$peers" ] && peers="$peers,"
    peers="${peers}127.0.0.1:$((BASE_RAFT_PORT + i))"
done

for i in $(seq 1 $NODES); do
    raft_port=$((BASE_RAFT_PORT + i))
    http_port=$((BASE_HTTP_PORT + i))
    rpc_port=$((BASE_RPC_PORT + i))
    echo "Starting node $i (raft=:${raft_port} http=:${http_port} rpc=:${rpc_port})..."
    nohup ./bin/server \
        -id=$i \
        -raft=127.0.0.1:$raft_port \
        -http=127.0.0.1:$http_port \
        -rpc=127.0.0.1:$rpc_port \
        -peers="$peers" \
        > "$LOG_DIR/node${i}.log" 2>&1 &
    echo $! > "$LOG_DIR/node${i}.pid"
done

echo ""
echo "Cluster started! ($NODES nodes)"
echo "Logs: $LOG_DIR/node*.log"
echo "Stop: ./stop_server.sh"
