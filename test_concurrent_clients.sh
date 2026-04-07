
#!/bin/bash

# 自动编译所有二进制
echo "[INFO] 编译所有二进制..."
make all || { echo "[ERROR] make all 失败，退出"; exit 1; }

# 检查二进制是否存在
for bin in ./bin/rpcclient ./bin/client; do
    if [ ! -f "$bin" ]; then
        echo "[ERROR] 缺少 $bin，请检查 makefile 或源码"
        exit 1
    fi
done

# 测试并发客户端连接

echo "=== 并发RPC客户端测试 ==="

# 启动多个RPC客户端，在后台运行
for i in {1..5}; do
    (
        echo "客户端 $i: 写入数据"
        ./bin/rpcclient -rpcaddr 127.0.0.1:6000 -op put -key "key$i" -value "value$i"
        sleep 1
        
        echo "客户端 $i: 读取数据"
        ./bin/rpcclient -rpcaddr 127.0.0.1:6000 -op get -key "key$i"
    ) &
done

# 等待所有后台进程完成
wait

echo ""
echo "=== 并发HTTP客户端测试 ==="

# 使用curl进行HTTP并发测试
for i in {1..5}; do
    (
        echo "HTTP客户端 $i: 写入数据"
        curl -s "http://127.0.0.1:8000/put?key=httpkey$i&value=httpvalue$i" | head -c 50
        echo ""
        sleep 1
        
        echo "HTTP客户端 $i: 读取数据"
        curl -s "http://127.0.0.1:8000/get?key=httpkey$i"
        echo ""
    ) &
done

wait

echo ""
echo "=== 测试完成 ==="
echo "获取服务器统计:"
curl -s "http://127.0.0.1:8000/stats"
echo ""
