# wjetcd

`wjetcd` 是一个使用 Go 语言实现的分布式键值存储系统，基于 Raft 一致性协议，提供强一致性的键值存储服务。

## 核心特性

### 分布式一致性与高可用

- **Raft 协议**: 基于 Raft 协议实现日志复制和 Leader 选举，确保数据在集群中的强一致性
- **快照机制**: 支持 Snapshot 快照压缩日志，实现快速的节点崩溃恢复
- **WAL 预写日志**: 使用 Write-Ahead Log 保障故障恢复

### 高性能存储

- **MVCC 多版本并发控制**: 每个 key 保留历史版本，支持按 revision 查询
- **CAS 原子操作**: Compare-And-Swap 条件更新
- **按前缀操作**: 支持按前缀进行范围查询和删除

### 高级功能

- **Lease 租约机制**: 支持基于 TTL 的租约，实现键值对的自动过期
- **Watch 观察者模式**: 客户端可监视指定的键或前缀，实时获取数据变更通知
- **分布式锁**: 基于 Lease 实现分布式锁机制，支持续期和竞争检测

## 项目架构

```
                    客户端层
                  HTTP Client
                        │
                        ▼
┌─────────────────────────────────────────────────────┐
│                   server/server.go                   │
│  ┌───────────┐  ┌─────────────┐  ┌──────────────┐   │
│  │  HTTP     │  │   RPC       │  │   Watch      │   │
│  │  Handler  │  │   Handler   │  │   Manager    │   │
│  └───────────┘  └─────────────┘  └──────────────┘   │
│  ┌───────────┐  ┌─────────────┐  ┌──────────────┐   │
│  │  Lease    │  │   KVStore   │  │   Apply      │   │
│  │  Manager  │  │  (MVCC)     │  │   Loop       │   │
│  └───────────┘  └─────────────┘  └──────────────┘   │
│  ┌─────────────────────────────────────────────────┐ │
│  │            Lock Manager (分布式锁)              │ │
│  └─────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────┐
│                     raft/                            │
│  ┌────────────┐  ┌───────────┐  ┌─────────────────┐  │
│  │  Leader    │  │ Candidate │  │    Follower     │  │
│  │  Election  │  │  Election │  │ AppendEntries   │  │
│  └────────────┘  └───────────┘  └─────────────────┘  │
│  ┌────────────┐  ┌───────────┐  ┌─────────────────┐  │
│  │    Log    │  │  Snapshot │  │   Persister     │  │
│  │ Replication│ │  Install  │  │   (State)       │  │
│  └────────────┘  └───────────┘  └─────────────────┘  │
└─────────────────────────────────────────────────────┘
                        │
                        ▼
┌─────────────────────────────────────────────────────┐
│                   storage/                           │
│  ┌─────────────────┐  ┌─────────────────────────┐   │
│  │   WAL           │  │     Snapshot           │   │
│  │ (预写日志)       │  │   (状态快照)            │   │
│  └─────────────────┘  └─────────────────────────┘   │
└─────────────────────────────────────────────────────┘
```

## 目录结构

| 目录 | 说明 |
|------|------|
| `raft/` | Raft 一致性算法核心实现 |
| `kv/` | MVCC 键值存储核心 |
| `server/` | 服务器层，包含 HTTP Handler、LeaseManager、LockManager、WatchManager |
| `storage/` | 存储层（WAL 和 Snapshot） |
| `labrpc/` | RPC 通信层 |
| `labgob/` | 序列化工具 |
| `cmd/server/` | 服务器启动程序 |
| `cmd/client/` | 客户端命令行工具 |

## 快速开始

### 环境要求

- Go >= 1.22

### 构建

```bash
go build -o bin/server ./cmd/server
go build -o bin/client ./cmd/client
```

### 运行服务器节点

```bash
./bin/server -id=1 -raft=:8001 -http=:9001 -rpc=:7001 -peers=:8001,:8002,:8003
```

### 客户端操作

```bash
# 写入键值
./bin/client -addrs=:9001 -op=put -key=foo -value=bar

# 读取键值
./bin/client -addrs=:9001 -op=get -key=foo

# 健康检查
./bin/client -addrs=:9001 -op=health
```

## API 接口

### 键值操作

| 接口 | 方法 | 说明 |
|------|------|------|
| `/put` | POST | 写入键值对（支持 lease_id） |
| `/get` | GET | 读取键值 |
| `/delete` | DELETE | 删除键值 |

### Watch 监视

| 接口 | 方法 | 说明 |
|------|------|------|
| `/watch` | GET | 监视键值变化 |
| `/watch/prefix` | GET | 监视指定前缀的所有键变化 |
| `/wait` | GET | 等待键值变化事件 |

### Prefix 前缀操作

| 接口 | 方法 | 说明 |
|------|------|------|
| `/get/prefix` | GET | 获取指定前缀的所有键值对 |
| `/delete/prefix` | DELETE | 删除指定前缀的所有键值对 |

### Lease 租约

| 接口 | 方法 | 说明 |
|------|------|------|
| `/lease/grant` | POST | 授予租约 |
| `/lease/keepalive` | PUT | 租约续期 |
| `/lease/revoke` | DELETE | 撤销租约 |
| `/lease/attach` | POST | 绑定 key 到租约 |

### 分布式锁

| 接口 | 方法 | 说明 |
|------|------|------|
| `/lock/acquire` | POST | 获取分布式锁 |
| `/lock/release` | DELETE | 释放分布式锁 |
| `/lock/keepalive` | PUT | 锁续期 |
| `/lock/status` | GET | 查询锁状态 |

## 分布式锁使用示例

### 加锁

```bash
# 获取锁，返回 lease_id
curl -X POST "http://localhost:9001/lock/acquire?key=mylock&ttl=30&owner_id=client-1"

# 响应: {"ok":true,"key":"mylock","lease_id":1,"owner_id":"client-1"}
```

### 续期

```bash
# 续期锁的 TTL
curl -X PUT "http://localhost:9001/lock/keepalive?key=mylock&lease_id=1&owner_id=client-1&ttl=20"
```

### 释放

```bash
# 释放锁
curl -X DELETE "http://localhost:9001/lock/release?key=mylock&lease_id=1&owner_id=client-1"
```

### 查询状态

```bash
# 查询锁状态
curl "http://localhost:9001/lock/status?key=mylock"

# 响应: {"ok":true,"locked":true,"lease_id":1}
```

## License

MIT
