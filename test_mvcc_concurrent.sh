#!/bin/bash

# 高并发 MVCC 测试脚本
BASE_PORT=19000
HTTP_PORT=19010
RPC_PORT=17000
BIN_DIR=bin
SERVER_BIN=$BIN_DIR/server
CLUSTER_SIZE=3

LOG_DIR=logs_mvcc_test
mkdir -p $LOG_DIR

echo "Building..."
go build -o $SERVER_BIN ./cmd/server || { echo "Build failed"; exit 1; }

echo "Starting cluster..."
PEERS=""
for i in $(seq 1 $CLUSTER_SIZE); do
    port=$((BASE_PORT + i))
    if [ -z "$PEERS" ]; then
        PEERS="127.0.0.1:$port"
    else
        PEERS="$PEERS,127.0.0.1:$port"
    fi
done

for i in $(seq 1 $CLUSTER_SIZE); do
    raft_port=$((BASE_PORT + i))
    http_port=$((HTTP_PORT + i))
    rpc_port=$((RPC_PORT + i))
    echo "Starting node $i..."
    nohup $SERVER_BIN \
        -id=$i \
        -raft=127.0.0.1:$raft_port \
        -http=127.0.0.1:$http_port \
        -rpc=127.0.0.1:$rpc_port \
        -peers="$PEERS" \
        > $LOG_DIR/node$i.log 2>&1 &
    echo $! > $LOG_DIR/node$i.pid
done

sleep 5

# 找到 leader
LEADER_HTTP=""
for attempt in $(seq 1 10); do
    for i in $(seq 1 $CLUSTER_SIZE); do
        http_port=$((HTTP_PORT + i))
        status=$(curl -s "http://127.0.0.1:$http_port/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4)
        if [ "$status" = "leader" ]; then
            LEADER_HTTP="http://127.0.0.1:$http_port"
            echo "Leader found at node $i: $LEADER_HTTP"
            break 2
        fi
    done
    echo "Waiting for leader... attempt $attempt"
    sleep 1
done

if [ -z "$LEADER_HTTP" ]; then
    echo "No leader found!"
    exit 1
fi

echo ""
echo "=== Test 1: Basic PUT/GET ==="
for i in $(seq 1 10); do
    curl -s -X POST "$LEADER_HTTP/put?key=test$i&value=val$i" > /dev/null
done
sleep 1

for i in $(seq 1 10); do
    resp=$(curl -s "$LEADER_HTTP/get?key=test$i")
    echo "GET test$i: $resp"
done

echo ""
echo "=== Test 2: Serial PUT (20 requests) ==="
echo "Launching 20 PUTs..."
for i in $(seq 1 20); do
    curl -s --connect-timeout 3 --max-time 5 -X POST "$LEADER_HTTP/put?key=conc$i&value=val$i" > /dev/null
    if [ $((i % 5)) -eq 0 ]; then
        echo "  Completed $i PUTs..."
    fi
done
sleep 1
resp=$(curl -s "$LEADER_HTTP/get?key=conc20")
echo "After PUTs: $resp"

echo ""
echo "=== Test 3: Concurrent PUT (50 requests) ==="
echo "Launching 50 concurrent PUTs..."
mkdir -p $LOG_DIR/results
pids=""
start_time=$(date +%s%N)
for i in $(seq 1 50); do
    curl -s --connect-timeout 5 --max-time 10 -X POST "$LEADER_HTTP/put?key=cp50_$i&value=val$i" > $LOG_DIR/results/cp50_$i 2>&1 &
    pids="$pids $!"
done
wait $pids
end_time=$(date +%s%N)
elapsed=$(( (end_time - start_time) / 1000000 ))
success=0
fail=0
for i in $(seq 1 50); do
    if grep -q '"ok":true' $LOG_DIR/results/cp50_$i 2>/dev/null; then
        success=$((success+1))
    else
        fail=$((fail+1))
    fi
done
echo "50 concurrent PUTs: ${success} success, ${fail} failed (took ${elapsed}ms)"
if [ $fail -gt 0 ]; then
    echo "FAILED: Not all concurrent PUTs succeeded!"
fi

echo ""
echo "=== Test 4: Concurrent PUT (200 requests) ==="
echo "Launching 200 concurrent PUTs..."
pids=""
start_time=$(date +%s%N)
for i in $(seq 1 200); do
    curl -s --connect-timeout 5 --max-time 15 -X POST "$LEADER_HTTP/put?key=cp200_$i&value=val$i" > $LOG_DIR/results/cp200_$i 2>&1 &
    pids="$pids $!"
done
wait $pids
end_time=$(date +%s%N)
elapsed=$(( (end_time - start_time) / 1000000 ))
success=0
fail=0
for i in $(seq 1 200); do
    if grep -q '"ok":true' $LOG_DIR/results/cp200_$i 2>/dev/null; then
        success=$((success+1))
    else
        fail=$((fail+1))
    fi
done
echo "200 concurrent PUTs: ${success} success, ${fail} failed (took ${elapsed}ms)"
if [ $fail -gt 0 ]; then
    echo "FAILED: Not all concurrent PUTs succeeded!"
fi

# 验证并发写入数据一致性
echo ""
echo "=== Test 5: Verify concurrent data ==="
verify_ok=true
for key in cp50_1 cp50_50 cp200_1 cp200_100 cp200_200; do
    resp=$(curl -s "$LEADER_HTTP/get?key=$key")
    echo "GET $key: $resp"
    if ! echo "$resp" | grep -q '"ok":true'; then
        verify_ok=false
    fi
done
if [ "$verify_ok" = true ]; then
    echo "Data verification: PASS"
else
    echo "Data verification: FAIL"
fi

echo ""
echo "=== Test 6: Delete with Tombstone ==="
curl -s -X DELETE "$LEADER_HTTP/delete?key=test1" > /dev/null
sleep 1
resp=$(curl -s "$LEADER_HTTP/get?key=test1")
echo "After delete test1: $resp"

echo ""
echo "=== Test 4: Stats ==="
stats=$(curl -s "$LEADER_HTTP/stats")
echo "Stats: $stats"

echo ""
echo "=== All tests completed ==="

# 清理
echo "Cleaning up..."
for i in $(seq 1 $CLUSTER_SIZE); do
    if [ -f $LOG_DIR/node$i.pid ]; then
        pid=$(cat $LOG_DIR/node$i.pid)
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

echo "Test finished."
