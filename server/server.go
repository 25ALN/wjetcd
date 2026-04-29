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
	leaseMgr *LeaseManager
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
	persistent bool  // 是否持续监听
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

type Lease struct {
	ID       int64
	TTL      int64
	expireAt int64
	Keys     map[string]string
}

type leaseItem struct {
	ID       int64
	expireAt int64
}

type LeaseManager struct {
	mu      sync.Mutex
	leases  map[int64]*Lease
	leaseID int64
	heap    []*leaseItem
}

func NewLeaseManager() *LeaseManager {
	return &LeaseManager{
		leases: make(map[int64]*Lease),
		heap:   make([]*leaseItem, 0),
	}
}

func (le *LeaseManager) Grant(ttl int64) int64 {
	le.mu.Lock()
	defer le.mu.Unlock()

	le.leaseID++
	id := le.leaseID
	expireAt := time.Now().UnixNano() + ttl*1e9

	l := &Lease{
		ID:       id,
		TTL:      ttl,
		expireAt: expireAt,
		Keys:     make(map[string]string),
	}
	le.leases[id] = l

	item := &leaseItem{ID: id, expireAt: expireAt}
	le.heap = append(le.heap, item)
	le.bubbleUp(len(le.heap) - 1)

	return id
}

func (le *LeaseManager) Attach(leaseId int64, key string, value string) {
	le.mu.Lock()
	defer le.mu.Unlock()

	lease := le.leases[leaseId]
	if lease == nil {
		return
	}
	lease.Keys[key] = value
}

func (le *LeaseManager) Revoke(key string) int64 {
	le.mu.Lock()
	defer le.mu.Unlock()

	for id, lease := range le.leases {
		if _, ok := lease.Keys[key]; ok {
			delete(lease.Keys, key)
			return id
		}
	}
	return 0
}

func (le *LeaseManager) GetLease(leaseId int64) *Lease {
	le.mu.Lock()
	defer le.mu.Unlock()
	return le.leases[leaseId]
}

func (le *LeaseManager) KeepAlive(leaseId int64, ttl int64) bool {
	le.mu.Lock()
	defer le.mu.Unlock()

	lease := le.leases[leaseId]
	if lease == nil {
		return false
	}

	lease.TTL = ttl
	lease.expireAt = time.Now().UnixNano() + ttl*1e9

	for i, item := range le.heap {
		if item.ID == leaseId {
			item.expireAt = lease.expireAt
			le.bubbleDown(i)
			le.bubbleUp(i)
			break
		}
	}
	return true
}

func (le *LeaseManager) RevokeLease(leaseId int64) []string {
	le.mu.Lock()
	defer le.mu.Unlock()

	lease := le.leases[leaseId]
	if lease == nil {
		return nil
	}

	keys := make([]string, 0, len(lease.Keys))
	for key := range lease.Keys {
		keys = append(keys, key)
	}

	delete(le.leases, leaseId)

	for i, item := range le.heap {
		if item.ID == leaseId {
			le.heap[i] = le.heap[len(le.heap)-1]
			le.heap = le.heap[:len(le.heap)-1]
			if i < len(le.heap) {
				le.bubbleDown(i)
				le.bubbleUp(i)
			}
			break
		}
	}

	return keys
}

func (le *LeaseManager) GetExpiringLease() *leaseItem {
	le.mu.Lock()
	defer le.mu.Unlock()

	if len(le.heap) == 0 {
		log.Printf("Lease heap is empty")
		return nil
	}
	return le.heap[0]
}

func (le *LeaseManager) RemoveExpiredLease() ([]string, int64) {
	le.mu.Lock()
	defer le.mu.Unlock()

	if len(le.heap) == 0 {
		return nil, 0
	}

	item := le.heap[0]
	now := time.Now().UnixNano()
	if item.expireAt > now {
		return nil, 0
	}

	lease := le.leases[item.ID]
	if lease == nil {
		return nil, 0
	}

	keys := make([]string, 0, len(lease.Keys))
	for key := range lease.Keys {
		keys = append(keys, key)
	}

	delete(le.leases, item.ID)

	le.heap[0] = le.heap[len(le.heap)-1]
	le.heap = le.heap[:len(le.heap)-1]
	if len(le.heap) > 0 {
		le.bubbleDown(0)
	}

	return keys, item.ID
}

// 插入新元素并保证最小的元素在堆顶
func (le *LeaseManager) bubbleUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if le.heap[i].expireAt >= le.heap[parent].expireAt {
			break
		}
		le.heap[i], le.heap[parent] = le.heap[parent], le.heap[i]
		i = parent
	}
}

// 删除某个元素后调整堆使得堆最小元素在顶
func (le *LeaseManager) bubbleDown(i int) {
	n := len(le.heap)
	for {
		left := 2*i + 1
		right := 2*i + 2
		smallest := i

		if left < n && le.heap[left].expireAt < le.heap[smallest].expireAt {
			smallest = left
		}
		if right < n && le.heap[right].expireAt < le.heap[smallest].expireAt {
			smallest = right
		}
		if smallest == i {
			break
		}
		le.heap[i], le.heap[smallest] = le.heap[smallest], le.heap[i]
		i = smallest
	}
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
		leaseMgr: NewLeaseManager(),
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
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		s.cleanupExpiredLeases()
		s.watchers.CleanupExpired()
	}
}

func (s *Server) cleanupExpiredLeases() {
	for {
		keys, leaseId := s.leaseMgr.RemoveExpiredLease()
		if leaseId == 0 {
			return
		}
		leaseIdStr := fmt.Sprintf("%d", leaseId)
		log.Printf("Lease %s expired, deleting keys: %v", leaseIdStr, keys)

		s.mu.Lock()
		for _, key := range keys {
			cmd := kv.Command{Type: kv.CmdDelete, Key: key}
			s.store.Apply(cmd)
			s.notifyWatchers(cmd)
		}
		s.mu.Unlock()
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
			if m, isMap := msg.Command.(map[string]interface{}); isMap {
				cmd = kv.Command{
					Key:     toString(m["Key"]),
					Value:   toString(m["Value"]),
					LeaseID: toInt64(m["LeaseID"]),
					TTL:     toInt64(m["TTL"]),
					Type:    kv.CommandType(toInt(m["Type"])),
				}
				ok = true
			}
		}
		if !ok {
			s.mu.Unlock()
			continue
		}
		if s.wal != nil {
			s.wal.WriteEntry(cmd)
		}

		switch cmd.Type {
		case kv.CmdLeaseGrant:
			ttl := cmd.TTL
			if ttl <= 0 {
				ttl = 10
			}
			leaseId := s.leaseMgr.Grant(ttl)
			result = fmt.Sprintf("%d", leaseId)

		case kv.CmdLeaseRevoke:
			leaseId := cmd.LeaseID
			if leaseId > 0 {
				keys := s.leaseMgr.RevokeLease(leaseId)
				for _, key := range keys {
					deleteCmd := kv.Command{Type: kv.CmdDelete, Key: key}
					s.store.Apply(deleteCmd)
					s.notifyWatchers(deleteCmd)
				}
			}
			result = "OK"

		case kv.CmdLeaseKeepAlive:
			leaseId := cmd.LeaseID
			ttl := cmd.TTL
			if ttl <= 0 {
				ttl = 10
			}
			ok := s.leaseMgr.KeepAlive(leaseId, ttl)
			if ok {
				result = "OK"
			} else {
				result = "ERROR"
			}

		case kv.CmdLeaseAttach:
			if cmd.LeaseID > 0 {
				s.leaseMgr.Attach(cmd.LeaseID, cmd.Key, cmd.Value)
				s.store.SetKeyLease(cmd.Key, cmd.LeaseID)
				s.store.Put(cmd.Key, cmd.Value)
			}
			result = "OK"

		default:
			result, _ = s.store.Apply(cmd)
		}

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

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toInt64(v interface{}) int64 {
	switch val := v.(type) {
	case int64:
		return val
	case float64: // JSON numbers decode as float64
		return int64(val)
	case int:
		return int64(val)
	}
	return 0
}

func toInt(v interface{}) int {
	switch val := v.(type) {
	case int:
		return val
	case float64:
		return int(val)
	case int64:
		return int(val)
	}
	return 0
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
	mux := http.NewServeMux()
	handler := NewHTTPHandler(s)
	mux.HandleFunc("/put", handler.Put)
	mux.HandleFunc("/get", handler.Get)
	mux.HandleFunc("/delete", handler.Delete)
	mux.HandleFunc("/watch", handler.Watch)
	mux.HandleFunc("/wait", handler.Wait)
	mux.HandleFunc("/health", handler.Health)
	mux.HandleFunc("/stats", handler.Stats)
	mux.HandleFunc("/lease/grant", handler.LeaseGrant)
	mux.HandleFunc("/lease/revoke", handler.LeaseRevoke)
	mux.HandleFunc("/lease/keepalive", handler.LeaseKeepAlive)
	mux.HandleFunc("/lease/attach", handler.LeaseAttach)
	return http.ListenAndServe(s.httpAddr, mux)
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
