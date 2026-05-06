#!/bin/bash
# 停止 MVCC 测试集群
LOG_DIR=logs_mvcc_test
BASE_PORT=19000
HTTP_PORT=19010
RPC_PORT=17000
CLUSTER_SIZE=3

echo "Stopping cluster..."
for i in $(seq 1 $CLUSTER_SIZE); do
    if [ -f $LOG_DIR/node$i.pid ]; then
        pid=$(cat $LOG_DIR/node$i.pid)
        echo "Killing node $i (PID $pid)..."
        kill -15 $pid 2>/dev/null || kill -9 $pid 2>/dev/null || true
    fi
done
sleep 1
for port in $(seq $HTTP_PORT $((HTTP_PORT + CLUSTER_SIZE))); do
    fuser -k $port/tcp 2>/dev/null || true
done
for port in $(seq $RPC_PORT $((RPC_PORT + CLUSTER_SIZE))); do
    fuser -k $port/tcp 2>/dev/null || true
done
for port in $(seq $BASE_PORT $((BASE_PORT + CLUSTER_SIZE))); do
    fuser -k $port/tcp 2>/dev/null || true
done
rm -rf $LOG_DIR
echo "Cluster stopped."
