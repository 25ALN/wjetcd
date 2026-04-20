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
	watchMu  sync.Mutex
	watchers map[string]map[uint64]*watcherInfo // key -> watcherID -> watcherInfo
	nextID   uint64
}

type watcherInfo struct {
	id         uint64
	ch         chan Event
	prevValue  string
	persistent bool // 是否持续监听
	expireAt   int64 // 超时时间戳 (unix nano)
}

type Watcher struct {
	Key    string
	ID     uint64
	Ch     chan Event
	Cancel chan struct{}
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
		watchers: NewWatchManager(),
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

	// 启动过期watcher清理goroutine
	go s.cleanupLoop()

	return s
}

func NewWatchManager() *WatchManager {
	return &WatchManager{
		watchers: make(map[string]map[uint64]*watcherInfo),
	}
}

// 添加watcher
// persistent: 是否持续监听 (触发后不删除)
// timeout: 超时时间 (0 表示不过期)
func (wa *WatchManager) AddWatcher(key string, persistent bool, timeout time.Duration) *Watcher {
	wa.watchMu.Lock()
	defer wa.watchMu.Unlock()

	wa.nextID++
	id := wa.nextID

	ch := make(chan Event, 10)
	cancelCh := make(chan struct{})

	expireAt := int64(0)
	if timeout > 0 {
		expireAt = time.Now().Add(timeout).UnixNano()
	}

	info := &watcherInfo{
		id:         id,
		ch:         ch,
		persistent: persistent,
		expireAt:   expireAt,
	}

	if wa.watchers[key] == nil {
		wa.watchers[key] = make(map[uint64]*watcherInfo)
	}
	wa.watchers[key][id] = info

	return &Watcher{
		Key:    key,
		ID:     id,
		Ch:     ch,
		Cancel: cancelCh,
	}
}

// 取消watcher
func (wa *WatchManager) RemoveWatcher(key string, id uint64) {
	wa.watchMu.Lock()
	defer wa.watchMu.Unlock()

	if watchers, ok := wa.watchers[key]; ok {
		if info, ok := watchers[id]; ok {
			close(info.ch)
			delete(watchers, id)
		}
	}
}

// 清理过期的watcher
func (wa *WatchManager) CleanupExpired() {
	wa.watchMu.Lock()
	defer wa.watchMu.Unlock()

	now := time.Now().UnixNano()
	for key, watchers := range wa.watchers {
		for id, info := range watchers {
			if info.expireAt > 0 && now > info.expireAt {
				close(info.ch)
				delete(watchers, id)
			}
		}
		if len(watchers) == 0 {
			delete(wa.watchers, key)
		}
	}
}

func (s *Server) notifyWatchers(cmd kv.Command) {
	var eventType string
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

	s.watchers.watchMu.Lock()
	watchers, ok := s.watchers.watchers[cmd.Key]
	if !ok {
		s.watchers.watchMu.Unlock()
		return
	}

	for id, info := range watchers {
		select {
		case info.ch <- event:
			if !info.persistent {
				close(info.ch)
				delete(watchers, id)
			}
		default:
		}
	}

	if len(watchers) == 0 {
		delete(s.watchers.watchers, cmd.Key)
	}
	s.watchers.watchMu.Unlock()
}

func (s *Server) CancelWatcher(key string, id uint64) {
	s.watchers.RemoveWatcher(key, id)
}

func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.watchers.CleanupExpired()
	}
}

// 启动应用日志循环
func (s *Server) applyLoop() {
	for msg := range s.applyCh {
		if !msg.CommandValid {
			continue
		}

		var result string
		var cmdIdx int

		s.mu.Lock()
		cmd, ok := msg.Command.(kv.Command)
		if !ok {
			s.mu.Unlock()
			continue
		}
		if s.wal != nil {
			s.wal.WriteEntry(cmd)
		}
		result, _ = s.store.Apply(cmd)
		cmdIdx = msg.CommandIndex
		s.notifyWatchers(cmd)
		s.mu.Unlock()

		if cmdIdx >= 0 {
			s.notifyReply(cmdIdx, result)
		}

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
	http.HandleFunc("/watch", handler.Watch)
	http.HandleFunc("/wait", handler.Wait)
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
