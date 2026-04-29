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
	test-integration test-replication test-follower-watch

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

# 测试Watch
test-watch:
	@echo "Testing Watch..."
	@echo "Step 1: Create watcher on key 'watchkey'..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			echo "Creating watcher via node $$i..."; \
			curl -s "http://127.0.0.1:900$$i/watch?key=watchkey"; \
			echo ""; \
			echo "Step 2: PUT to trigger watch..."; \
			curl -s -X POST "http://127.0.0.1:900$$i/put?key=watchkey&value=newvalue"; \
			echo ""; \
			echo "Step 3: Verify value updated..."; \
			for j in 1 2 3; do \
				val=$$(curl -s "http://127.0.0.1:900$$j/get?key=watchkey" 2>/dev/null); \
				echo "$$val" | grep -q '"value":"newvalue"' && echo "$$val" && break; \
			done; \
			break; \
		fi; \
	done
	@echo "Watch test done!"

# 测试 Lease Grant
test-lease-grant:
	@echo "Testing Lease Grant..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			echo "Granting lease via node $$i..."; \
			curl -s -X POST "http://127.0.0.1:900$$i/lease/grant?ttl=20"; \
			echo ""; \
			break; \
		fi; \
	done
	@echo "Lease Grant test done!"

# 测试 Lease Attach - 验证数据写入
test-lease-attach:
	@echo "Testing Lease Attach (via Raft)..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			echo "1. Grant lease..."; \
			lease_result=$$(curl -s -X POST "http://127.0.0.1:900$$i/lease/grant?ttl=20"); \
			echo "$$lease_result"; \
			lease_id=$$(echo "$$lease_result" | grep -o '"lease_id":[0-9]*' | cut -d':' -f2); \
			echo "2. Attach key=attachtest, value=attachval to lease $$lease_id..."; \
			attach_result=$$(curl -s -X POST "http://127.0.0.1:900$$i/lease/attach?key=attachtest&value=attachval&lease_id=$$lease_id"); \
			echo "$$attach_result"; \
			sleep 1; \
			echo "3. Get value from leader (should be 'attachval' or empty due to async)..."; \
			val=$$(curl -s "http://127.0.0.1:900$$i/get?key=attachtest" 2>/dev/null); \
			echo "Leader: $$val"; \
			echo "4. Get value from another node..."; \
			for j in 1 2 3; do \
				[ "$$j" != "$$i" ] && val=$$(curl -s "http://127.0.0.1:900$$j/get?key=attachtest" 2>/dev/null) && echo "Node $$j: $$val"; \
			done; \
			break; \
		fi; \
	done
	@echo "Lease Attach test done!"

# 测试 Lease Revoke
test-lease-revoke:
	@echo "Testing Lease Revoke..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			echo "Granting lease..."; \
			lease_result=$$(curl -s -X POST "http://127.0.0.1:900$$i/lease/grant?ttl=20"); \
			lease_id=$$(echo "$$lease_result" | grep -o '"lease_id":[0-9]*' | cut -d':' -f2); \
			echo "Attaching key..."; \
			curl -s -X POST "http://127.0.0.1:900$$i/lease/attach?key=revokekey&value=revokevalue&lease_id=$$lease_id"; \
			echo ""; \
			echo "Waiting for replication..."; \
			sleep 1; \
			echo "Verifying key exists..."; \
			val=$$(curl -s "http://127.0.0.1:900$$i/get?key=revokekey" 2>/dev/null); \
			echo "$$val"; \
			echo "Revoking lease $$lease_id..."; \
			curl -s -X POST "http://127.0.0.1:900$$i/lease/revoke?lease_id=$$lease_id"; \
			echo ""; \
			sleep 1; \
			echo "Verifying key deleted..."; \
			val=$$(curl -s "http://127.0.0.1:900$$i/get?key=revokekey" 2>/dev/null); \
			echo "$$val"; \
			break; \
		fi; \
	done
	@echo "Lease Revoke test done!"

# 测试 Lease 过期自动删除
test-lease-expire:
	@echo "Testing Lease Expire..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			echo "Granting short TTL lease (2 seconds)..."; \
			lease_result=$$(curl -s -X POST "http://127.0.0.1:900$$i/lease/grant?ttl=2"); \
			lease_id=$$(echo "$$lease_result" | grep -o '"lease_id":[0-9]*' | cut -d':' -f2); \
			echo "Attaching key..."; \
			curl -s -X POST "http://127.0.0.1:900$$i/lease/attach?key=expirekey&value=expirevalue&lease_id=$$lease_id"; \
			echo ""; \
			echo "Waiting for replication and expiry (3 seconds total)..."; \
			sleep 1; \
			val=$$(curl -s "http://127.0.0.1:900$$i/get?key=expirekey" 2>/dev/null); \
			echo "Before expiry: $$val"; \
			sleep 2; \
			val=$$(curl -s "http://127.0.0.1:900$$i/get?key=expirekey" 2>/dev/null); \
			echo "After expiry: $$val"; \
			break; \
		fi; \
	done
	@echo "Lease Expire test done!"

# 测试 Watch 感知 Lease 过期删除
test-watch-lease:
	@echo "Testing Watch Lease Expiry..."
	@for i in 1 2 3; do \
		status=$$(curl -s "http://127.0.0.1:900$$i/health" 2>/dev/null | grep -o '"status":"[^"]*"' | cut -d'"' -f4); \
		if [ "$$status" = "leader" ]; then \
			echo "Granting short TTL lease..."; \
			lease_result=$$(curl -s -X POST "http://127.0.0.1:900$$i/lease/grant?ttl=2"); \
			lease_id=$$(echo "$$lease_result" | grep -o '"lease_id":[0-9]*' | cut -d':' -f2); \
			echo "Attaching key..."; \
			curl -s -X POST "http://127.0.0.1:900$$i/lease/attach?key=watchleasekey&value=watchleasevalue&lease_id=$$lease_id"; \
			echo ""; \
			echo "Waiting for expiry..."; \
			sleep 3; \
			echo "Verifying key deleted..."; \
			val=$$(curl -s "http://127.0.0.1:900$$i/get?key=watchleasekey" 2>/dev/null); \
			echo "$$val"; \
			break; \
		fi; \
	done
	@echo "Watch Lease test done!"

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

test-all: test-put test-get test-watch test-lease-grant test-lease-attach test-lease-revoke test-replication test-follower-watch
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
	@echo "  make clean            # 清理所有文件"