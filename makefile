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
	cluster-up cluster-down cluster-status

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
# 测试（简单 KV 测试）
# ========================

test-put:
	@echo "Testing PUT..."
	./$(CLIENT_BIN) -op put -key foo -value bar

test-get:
	@echo "Testing GET..."
	./$(CLIENT_BIN) -op get -key foo

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
	@echo "  make test-put         # 测试写入"
	@echo "  make test-get         # 测试读取"
	@echo "  make clean            # 清理所有文件"