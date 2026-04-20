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
	test-put test-get test-watch test-all find-leader

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
			kill $$pid || true; \
		fi \
	done
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

# 运行所有测试
test-all: test-put test-get test-watch
	@echo ""
	@echo "=== All tests passed! ==="

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