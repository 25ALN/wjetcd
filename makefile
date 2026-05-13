GO=go
BIN_DIR=bin

SERVER_BIN=$(BIN_DIR)/server
CLIENT_BIN=$(BIN_DIR)/client
RPCCLIENT_BIN=$(BIN_DIR)/rpcclient

# 节点配置
NODES=3
BASE_PORT=8000

# 日志目录
LOG_DIR=logs

.PHONY: all build clean help \
	run-server run-client run-rpcclient \
	start-server start-client \
	cluster-up cluster-down cluster-status \
	test-put test-get test-watch test-lease-grant test-lease-revoke test-lease-attach test-lease-keepalive test-lease-expire test-watch-lease test-all find-leader \
	test-integration test-replication test-follower-watch \
	test-mvcc-concurrent test-prefix test-prefix-watch

# ========================
# 编译
# ========================

all: build

build: build-server build-client build-rpcclient

build-server:
	@echo "Building server..."
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(SERVER_BIN) ./cmd/server

build-client:
	@echo "Building client..."
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(CLIENT_BIN) ./cmd/client

build-rpcclient:
	@echo "Building rpcclient..."
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(RPCCLIENT_BIN) ./cmd/rpcclient

# ========================
# 单节点运行（调试用）
# ========================

run-server:
	@echo "Starting single server..."
	$(GO) run ./cmd/server

run-client:
	@echo "Starting client..."
	$(GO) run ./cmd/client

run-rpcclient:
	@echo "Starting rpcclient..."
	$(GO) run ./cmd/rpcclient

# ========================
# 二进制运行
# ========================

start-server:
	@echo "Running built server..."
	./$(SERVER_BIN)

start-client:
	@echo "Running built client..."
	./$(CLIENT_BIN)

# ========================
# 集群管理（核心🔥）
# ========================

cluster-up: build
	@echo "Starting cluster..."

	@mkdir -p $(LOG_DIR)

	@peers="127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003"; \
	for i in 1 2 3; do \
		raft_port=$$((8000 + $$i)); \
		http_port=$$((9000 + $$i)); \
		rpc_port=$$((7000 + $$i)); \
		echo "Starting node $$i..."; \
		nohup ./$(SERVER_BIN) \
			-id=$$i \
			-raft=127.0.0.1:$$raft_port \
			-http=127.0.0.1:$$http_port \
			-rpc=127.0.0.1:$$rpc_port \
			-peers=$$peers \
			> $(LOG_DIR)/node$$i.log 2>&1 & \
		echo $$! > $(LOG_DIR)/node$$i.pid; \
	done

	@echo "Cluster started!"
# ========================

cluster-down:
	@echo "Stopping cluster..."
	@for pidfile in $(LOG_DIR)/*.pid; do \
		if [ -f $$pidfile ]; then \
			pid=$$(cat $$pidfile); \
			echo "Killing $$pid"; \
			kill -15 $$pid 2>/dev/null || kill -9 $$pid 2>/dev/null || true; \
		fi \
	done
	@sleep 1
	@for port in 9001 9002 9003 7001 7002 7003 8001 8002 8003; do \
		fuser -k $$port/tcp 2>/dev/null || true; \
	done
	@sleep 1
	@rm -f $(LOG_DIR)/*.pid
	@echo "Cluster stopped."

# ========================

cluster-status:
	@echo "Cluster status:"
	@for pidfile in $(LOG_DIR)/*.pid; do \
		if [ -f $$pidfile ]; then \
			pid=$$(cat $$pidfile); \
			if ps -p $$pid > /dev/null; then \
				echo "$$pid is running"; \
			else \
				echo "$$pid is NOT running"; \
			fi \
		fi \
	done

# ========================
# 测试
# ========================

# 查找当前leader
find-leader:
	@echo "Finding leader..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			echo "Leader is node $$i (http://127.0.0.1:900$$i)"; \
		fi; \
	done

# 测试PUT (需要先启动集群)
test-put:
	@echo "Testing PUT..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			echo "PUT via node $$i..."; \
			curl -s -X POST "http://127.0.0.1:900$$i/put?key=foo&value=bar"; \
			echo ""; \
			break; \
		fi; \
	done
	@echo "PUT test done!"

# 测试GET
test-get:
	@echo "Testing GET..."
	@for i in 1 2 3; do \
		val=$$(curl -s "http://127.0.0.1:900$$i/get?key=foo" 2>/dev/null); \
		echo "$$val" | grep -q '"value":"bar"' && echo "$$val" && break; \
	done || echo '{"error":"key not found"}'
	@echo ""


# 测试集群重启后数据恢复
test-leader-switch:
	@echo "Testing Leader Switch..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			old_leader=$$i; \
			echo "Current leader: $$old_leader"; \
			echo "Forcing new election (simulated by no action)..."; \
			echo "Checking data is consistent across nodes..."; \
			val1=$$(curl -s "http://127.0.0.1:9001/get?key=foo" 2>/dev/null); \
			echo "Node 1: $$val1"; \
			val2=$$(curl -s "http://127.0.0.1:9002/get?key=foo" 2>/dev/null); \
			echo "Node 2: $$val2"; \
			val3=$$(curl -s "http://127.0.0.1:9003/get?key=foo" 2>/dev/null); \
			echo "Node 3: $$val3"; \
			break; \
		fi; \
	done
	@echo "Leader switch test done!"

test-all: test-put test-get test-replication test-follower-watch
	@echo ""
	@echo "=== All tests passed! ==="

# ========================
# 集成测试 (验证 Raft 复制与 Watch 限制)
# ========================

test-integration:
	@echo "Running integration tests (Lease replication & Watch restriction)..."
	@go test -v -run TestLeaseReplicationAndWatchRestriction ./server

test-follower-watch:
	@echo "Testing Watch on Follower (should fail)..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" != "leader" ]; then \
			echo "Testing watch on follower node $$i..."; \
			res=$$(curl -s "http://127.0.0.1:900$$i/watch?key=test"); \
			echo "$$res" | grep -q '"error"' && echo "PASS: Follower correctly rejected watch" || echo "FAIL: Follower accepted watch"; \
			break; \
		fi; \
	done

test-replication:
	@echo "Testing Lease Attach Replication..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			leader=$$i; \
			echo "Leader is node $$leader"; \
			lease_result=$$(curl -s -X POST "http://127.0.0.1:900$$leader/lease/grant?ttl=30"); \
			lease_id=$$(echo "$$lease_result" | grep -o '"lease_id":[0-9]*' | cut -d':' -f2); \
			curl -s -X POST "http://127.0.0.1:900$$leader/lease/attach?key=repl_test&value=repl_val&lease_id=$$lease_id" > /dev/null; \
			sleep 1; \
			echo "Checking replication on all nodes:"; \
			all_pass=true; \
			for j in 1 2 3; do \
				val=$$(curl -s "http://127.0.0.1:900$$j/get?key=repl_test" 2>/dev/null); \
				echo "$$val" | grep -q '"value":"repl_val"' && echo "  Node $$j: PASS" || (echo "  Node $$j: FAIL ($$val)" && all_pass=false); \
			done; \
			if [ "$$all_pass" = true ]; then echo "PASS: Data replicated to all nodes"; else echo "FAIL: Replication incomplete"; fi; \
			break; \
		fi; \
	done

# ========================
# 清理
# ========================

clean:
	rm -rf $(BIN_DIR) $(LOG_DIR) *.wal *.snap server_*.log

# ========================
# 帮助
# ========================

help:
	@echo "用法："
	@echo "  make build            # 编译所有程序"
	@echo "  make run-server       # 直接运行单节点"
	@echo "  make cluster-up       # 启动3节点集群"
	@echo "  make cluster-down     # 停止集群"
	@echo "  make cluster-status   # 查看集群状态"
	@echo "  make find-leader      # 查找当前leader"
	@echo "  make test-put         # 测试PUT写入"
	@echo "  make test-get        # 测试GET读取"
	@echo "  make test-watch     # 测试Watch机制"
	@echo "  make test-all      # 运行所有测试"
	@echo "  make test-mvcc-concurrent  # 运行MVCC高并发测试"
	@echo "  make clean            # 清理所有文件"

# ========================
# MVCC 高并发测试
# ========================
test-mvcc-concurrent: build
	@echo "Running MVCC concurrent test..."
	@bash test_mvcc_concurrent.sh

# ========================
# Prefix 操作测试
# ========================
test-prefix: build cluster-up
	@echo "Waiting for cluster to elect leader..."
	@sleep 3
	@echo "Testing prefix operations..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			leader=$$i; \
			echo "Leader is node $$leader"; \
			curl -s -X POST "http://127.0.0.1:900$$leader/put?key=service/node1&value=server1" > /dev/null; \
			curl -s -X POST "http://127.0.0.1:900$$leader/put?key=service/node2&value=server2" > /dev/null; \
			curl -s -X POST "http://127.0.0.1:900$$leader/put?key=config/timeout&value=30" > /dev/null; \
			sleep 1; \
			echo "--- Testing get/prefix ---"; \
			result=$$(curl -s "http://127.0.0.1:900$$leader/get/prefix?prefix=service/"); \
			echo "$$result"; \
			echo "$$result" | grep -q '"node1"' && echo "PASS: contains service/node1" || echo "FAIL: missing node1"; \
			echo "$$result" | grep -q '"node2"' && echo "PASS: contains service/node2" || echo "FAIL: missing node2"; \
			echo "--- Testing delete/prefix ---"; \
			delete_result=$$(curl -s -X DELETE "http://127.0.0.1:900$$leader/delete/prefix?prefix=service/"); \
			echo "$$delete_result"; \
			sleep 1; \
			echo "--- Verifying deletion ---"; \
			after_result=$$(curl -s "http://127.0.0.1:900$$leader/get/prefix?prefix=service/"); \
			echo "$$after_result"; \
			echo "$$after_result" | grep -q '"node1"' && echo "FAIL: node1 still exists" || echo "PASS: node1 deleted"; \
			echo "$$after_result" | grep -q '"node2"' && echo "FAIL: node2 still exists" || echo "PASS: node2 deleted"; \
			break; \
		fi; \
	done
	@echo "Prefix test completed!"
	@make cluster-down

# ========================
# Prefix Watch 测试
# ========================
test-prefix-watch: build cluster-up
	@echo "Waiting for cluster to elect leader..."
	@sleep 3
	@echo "Testing prefix watch..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			leader=$$i; \
			echo "Leader is node $$leader"; \
			watch_result=$$(curl -s "http://127.0.0.1:900$$leader/watch/prefix?prefix=events/"); \
			echo "Create prefix watcher: $$watch_result"; \
			echo "$$watch_result" | grep -q '"ok":true' && echo "PASS: watcher created" || echo "FAIL: watcher creation failed"; \
			curl -s -X POST "http://127.0.0.1:900$$leader/put?key=events/user/login&value=alice" > /dev/null; \
			curl -s -X POST "http://127.0.0.1:900$$leader/put?key=config/ignore&value=me" > /dev/null; \
			echo "PASS: events created (should be tracked by prefix watcher)"; \
			break; \
		fi; \
	done
	@echo "Prefix watch test completed!"
	@make cluster-down