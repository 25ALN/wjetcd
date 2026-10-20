#!/bin/bash

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

go build -o bin/client ./cmd/client

find_leader() {
    for i in 1 2 3; do
        status=$(curl -s "http://127.0.0.1:$((9000 + i))/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4)
        if [ "$status" = "leader" ]; then
            echo "http://127.0.0.1:$((9000 + i))"
            return 0
        fi
    done
    return 1
}

run_bench() {
    concurrency=${1:-10}
    total=${2:-100}
    tmpdir=$(mktemp -d)

    leader=$(find_leader)
    if [ -z "$leader" ]; then
        echo "Error: No leader found. Is the cluster running?"
        rm -rf "$tmpdir"
        return 1
    fi
    echo "Leader: $leader"
    echo "Concurrency: $concurrency | Total requests: $total"
    echo ""

    start_time=$(date +%s%N)
    running=0
    sent=0

    for i in $(seq 1 "$total"); do
        curl -s --connect-timeout 5 --max-time 10 \
            -X POST "$leader/put?key=bench_$i&value=val_$i" \
            > "$tmpdir/$i" 2>&1 &
        running=$((running + 1))
        sent=$((sent + 1))

        if [ "$running" -ge "$concurrency" ]; then
            wait -n 2>/dev/null || wait
            running=$((running - 1))
        fi
    done
    wait

    end_time=$(date +%s%N)
    elapsed_ms=$(( (end_time - start_time) / 1000000 ))

    success=0
    fail=0
    for f in "$tmpdir"/*; do
        if grep -q '"ok":true' "$f" 2>/dev/null; then
            success=$((success + 1))
        else
            fail=$((fail + 1))
        fi
    done

    echo "========== Results =========="
    echo "  Total:      $total"
    echo "  Success:    $success"
    echo "  Failed:     $fail"
    echo "  Time:       ${elapsed_ms}ms"
    if [ "$elapsed_ms" -gt 0 ]; then
        qps=$((success * 1000 / elapsed_ms))
        echo "  Throughput: ${qps} req/s"
    fi
    echo "============================="

    rm -rf "$tmpdir"
}

run_cmd() {
    OP=$1
    shift
    case "$OP" in
        put)
            [ $# -lt 2 ] && echo "Usage: put <key> <value>" && return
            ./bin/client -op=put -key="$1" -value="$2"
            ;;
        get)
            [ $# -lt 1 ] && echo "Usage: get <key>" && return
            ./bin/client -op=get -key="$1"
            ;;
        health)
            ./bin/client -op=health
            ;;
        bench)
            run_bench "$1" "$2"
            ;;
        help)
            echo "Commands:"
            echo "  put <key> <value>       - Write key-value"
            echo "  get <key>               - Read value"
            echo "  health                  - Health check"
            echo "  bench [concurrency] [total] - Concurrent write benchmark"
            echo "  quit / exit             - Exit"
            ;;
        quit|exit)
            exit 0
            ;;
        "")
            ;;
        *)
            echo "Unknown command: $OP (type 'help' for usage)"
            ;;
    esac
}

if [ $# -gt 0 ]; then
    run_cmd "$@"
    exit $?
fi

echo "=== wjetcd client (type 'help' for commands, 'quit' to exit) ==="
while true; do
    read -r -p "> " line
    eval "set -- $line"
    run_cmd "$@"
done
