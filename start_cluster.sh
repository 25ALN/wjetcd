#!/bin/bash
# 一键启动3个raft节点，每个节点独立端口和数据文件

set -e

# 杀掉已占用端口的进程（可选，安全起见可注释）
fuser -k 5000/tcp 5001/tcp 5002/tcp 8000/tcp 8001/tcp 8002/tcp 6000/tcp 6001/tcp 6002/tcp || true

# 清理旧数据
rm -f node0.wal node1.wal node2.wal node0.snap node1.snap node2.snap

# 后台启动3个server节点
for i in 0 1 2; do
  echo "启动server节点 $i ..."
  ./bin/server -id $i > server_$i.log 2>&1 &
  sleep 0.5
  # 可用 tail -f server_$i.log 实时查看日志
  # 也可用 ps aux|grep bin/server 检查进程
  # 也可用 lsof -i:800$i 检查端口
  # 也可用 curl http://127.0.0.1:800$i/health 检查健康
  # 也可用 curl http://127.0.0.1:800$i/stats 检查状态
  # 也可用 kill %1 %2 %3 结束进程
  # 也可用 jobs 查看后台任务
  # 也可用 fg/bg 控制任务
  # 也可用 pkill -f bin/server 一键杀死
  # 也可用 killall server
  # 也可用 kill $(jobs -p)
done

echo "等待3秒让集群选主..."
sleep 3

echo "可用如下命令测试："
echo "./bin/client -addr 127.0.0.1:8000 -op put -key foo -value bar"
echo "./bin/client -addr 127.0.0.1:8001 -op get -key foo"
echo "./bin/client -addr 127.0.0.1:8002 -op health"
