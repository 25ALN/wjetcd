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
	mu       sync.Mutex
	store    *kv.KVStore
	rf       *raft.Raft
	applyCh  chan raft.ApplyMsg
	id       int
	addr     string
	peers    []string
	wal      *storage.WAL
	snapshot *storage.Snapshot

	replyCh  map[int]chan interface{}
	rpcAddr  string
	httpAddr string
	watchers *WatchManager
}

type WatchManager struct {
	mu      sync.Mutex
	watcher map[string][]chan Event //监测哪些key发生了变化
}

type Watcher struct {
	Key string
	Ch  chan Event
}

type Event struct {
	Key   string
	Value string
	Type  string
}

func NewServer(
	id int,
	raftAddr string,
	peers []string,
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

// 添加watcher
func (wa *WatchManager) AddWatcher(key string) chan Event {
	wa.mu.Lock()
	defer wa.mu.Unlock()
	ch := make(chan Event, 10)
	wa.watcher[key] = append(wa.watcher[key], ch)
	return ch
}

func (s *Server) notifyWatchers(cmd kv.Command) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var eventType string
	//只需要处理put和delete，get没有必要通知watcher
	switch cmd.Type {
	case kv.CmdPut:
		eventType = "PUT"
	case kv.CmdDelete:
		eventType = "DELETE"
	default:
		return
	}

	event := Event{
		Key:   cmd.Key,
		Value: cmd.Value,
		Type:  eventType,
	}

	for _, ch := range s.watchers.watcher[cmd.Key] {
		select {
		case ch <- event:
		default:
		}
	}
}

// 启动应用日志循环
func (s *Server) applyLoop() {
	for msg := range s.applyCh {
		if !msg.CommandValid {
			continue
		}
		log.Printf("APPLY idx=%d", msg.CommandIndex)
		s.mu.Lock()
		var result string
		var cmdIdx int
		if cmd, ok := msg.Command.(kv.Command); ok {
			if s.wal != nil {
				if err := s.wal.WriteEntry(cmd); err != nil {
					log.Printf("[Server %d] WAL write error: %v", s.id, err)
				} else {
					log.Printf("success save datas")
				}
			}
			result, _ = s.store.Apply(cmd)
			cmdIdx = msg.CommandIndex
		}
		s.mu.Unlock()

		if cmdIdx >= 0 {
			s.notifyReply(cmdIdx, result)
		}

		// 更新LastApplied
		s.mu.Lock()
		if msg.CommandIndex > s.rf.LastApplied {
			s.rf.LastApplied = msg.CommandIndex
		}
		s.mu.Unlock()
	}
}

func (s *Server) notifyReply(cmdIdx int, result interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.replyCh[cmdIdx]; ok {
		ch <- result
		delete(s.replyCh, cmdIdx)
		log.Printf("NOTIFY idx=%d exist=%v", cmdIdx, ok)

	} else {
		log.Printf(" replyCh not found for idx=%d (RESULT LOST!)", cmdIdx)
	}
}

// 向Raft提交命令
func (s *Server) Submit(cmd kv.Command) (string, error) {
	s.mu.Lock()
	if !s.IsLeader() {
		s.mu.Unlock()
		return "", fmt.Errorf("not leader")
	}
	replyCh := make(chan interface{}, 1)

	idx, term, ok := s.rf.Start(cmd)
	if !ok {
		s.mu.Unlock()
		return "", fmt.Errorf("submit failed")
	}

	log.Printf("[Server %d] Submit command at index %d, term %d", s.id, idx, term)

	s.replyCh[idx] = replyCh
	s.mu.Unlock()

	select {
	case result := <-replyCh:
		return result.(string), nil
	case <-time.After(5 * time.Second):
		s.mu.Lock()
		delete(s.replyCh, idx)
		s.mu.Unlock()
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
