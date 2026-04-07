package server

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"sync"
	"testetcd/kv"
	"testetcd/raft"
	"testetcd/storage"
	"time"
)

type Server struct {
	mu      sync.Mutex
	store   *kv.KVStore
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg
	id      int
	addr    string
	peers   []string
	//peers    []*labrpc.ClientEnd
	wal      *storage.WAL
	snapshot *storage.Snapshot

	replyCh  map[int]chan interface{}
	rpcAddr  string
	httpAddr string
	watcher  map[string]int //监测哪些key发生了变化
}

func NewServer(
	id int,
	raftAddr string,
	peers []string,
	//peers []*labrpc.ClientEnd,
	wal *storage.WAL,
	snapshot *storage.Snapshot,
	rpcAddr string,
	httpAddr string,
) *Server {
	s := &Server{
		store:    kv.NewKVStore(),
		id:       id,
		addr:     raftAddr,
		peers:    peers,
		applyCh:  make(chan raft.ApplyMsg, 100),
		wal:      wal,
		snapshot: snapshot,
		replyCh:  make(map[int]chan interface{}),
		rpcAddr:  rpcAddr,
		httpAddr: httpAddr,
	}

	// 创建Raft实例，mock Persister和peers
	persister := raft.MakePersister()
	//s.rf = raft.Make(nil, id, persister, s.applyCh)
	// var temperrs []*labrpc.ClientEnd
	// temperrs = make([]*labrpc.ClientEnd, len(peers))
	// for i := 0; i < len(peers); i++ {
	// 	ename := fmt.Sprintf("peer%d", i)
	// 	temperrs[i] = labrpc.MakeNetwork().MakeEnd(ename)
	// }
	s.rf = raft.Make(peers, id, persister, s.applyCh)
	// 启动Raft HTTP服务，监听Raft节点间通信端口
	go func() {
		err := s.rf.StartHTTPServer(raftAddr)
		if err != nil {
			log.Fatalf("Raft HTTP server failed: %v", err)
		}
	}()

	// 启动应用日志循环
	go s.applyLoop()

	return s
}

// 启动应用日志循环
func (s *Server) applyLoop() {
	for msg := range s.applyCh {
		if !msg.CommandValid {
			continue
		}

		s.mu.Lock()
		if cmd, ok := msg.Command.(kv.Command); ok {
			result, err := s.store.Apply(cmd)
			if err != nil {
				log.Printf("[Server %d] Apply error: %v", s.id, err)
			}
			s.notifyReply(msg.CommandIndex, result)
		}
		s.mu.Unlock()

		// 更新LastApplied
		s.mu.Lock()
		if msg.CommandIndex > s.rf.LastApplied {
			s.rf.LastApplied = msg.CommandIndex
		}
		s.mu.Unlock()
	}
}

// 通知对应请求的响应通道
func (s *Server) notifyReply(cmdIdx int, result interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ch, ok := s.replyCh[cmdIdx]; ok {
		select {
		case ch <- result:
		default:
		}
		delete(s.replyCh, cmdIdx)
	}
}

// 向Raft提交命令
func (s *Server) Submit(cmd kv.Command) (string, error) {
	s.mu.Lock()
	// 检查是否是Leader
	isLeader := s.IsLeader()
	s.mu.Unlock()

	if !isLeader {
		fmt.Printf("Node %d is not the leader\n", s.id)
		return "", fmt.Errorf("not leader")
	}

	// 提交到Raft
	idx, term, ok := s.rf.Start(cmd)
	if !ok {
		return "", fmt.Errorf("submit failed")
	}

	log.Printf("[Server %d] Submit command at index %d, term %d", s.id, idx, term)

	// 创建响应通道
	s.mu.Lock()
	replyCh := make(chan interface{}, 1)
	s.replyCh[idx] = replyCh
	s.mu.Unlock()

	// 等待应用完成（有超时）
	select {
	case result := <-replyCh:
		return result.(string), nil
	case <-time.After(5 * time.Second):
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.replyCh, idx)
		return "", fmt.Errorf("timeout")
	}
}

// 从本地存储读取（无需经过Raft）
func (s *Server) Get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Get(key)
}

// Leader检测
func (s *Server) IsLeader() bool {
	return s.rf.State == raft.Leader
}

// 启动RPC服务器
func (s *Server) StartRPCServer() {
	rpc.Register(NewHandler(s))
	listener, err := net.Listen("tcp", s.rpcAddr)
	if err != nil {
		log.Fatal("RPC listen error:", err)
	}
	go rpc.Accept(listener)
}

// 启动HTTP服务器
func (s *Server) StartHTTPServer() error {
	handler := NewHTTPHandler(s)
	http.HandleFunc("/put", handler.Put)
	http.HandleFunc("/get", handler.Get)
	http.HandleFunc("/delete", handler.Delete)
	http.HandleFunc("/health", handler.Health)
	http.HandleFunc("/stats", handler.Stats)
	return http.ListenAndServe(s.httpAddr, nil)
}

// 获取服务器统计信息
func (s *Server) GetStats() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	return map[string]interface{}{
		"id":           s.id,
		"is_leader":    s.IsLeader(),
		"store_size":   s.store.Size(),
		"commit_index": s.rf.CommitIndex,
		"last_applied": s.rf.LastApplied,
		"current_term": s.rf.CurrentTerm,
	}
}

// 关闭服务器
func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	close(s.applyCh)
	s.wal.Close()
	s.snapshot.Close()
}
