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

	replyCh    map[int]chan interface{}
	rpcAddr    string
	httpAddr   string
	watchers   *WatchManager
	leaseMgr   *LeaseManager
	lockMgr    *LockManager
	currentRev int64
}

type WatchManager struct {
	watchMu      sync.Mutex
	watchers     map[string]map[uint64]*watcherInfo    // exact key -> watcherID -> watcherInfo
	prefixWatchers map[string]map[uint64]*watcherInfo // prefix -> watcherID -> watcherInfo
	nextID       uint64
	eventLog     []kv.WatchEvent // 历史事件日志（简单版）
}

type watcherInfo struct {
	id         uint64
	ch         chan Event
	prevValue  string
	persistent bool  // 是否持续监听
	expireAt   int64 // 超时时间戳 (unix nano)
	fromRev    int64 // 从哪个 revision 开始监听
}

type Watcher struct {
	Key    string
	ID     uint64
	Ch     chan Event
	Cancel chan struct{}
}

type Event struct {
	Key      string
	Value    string
	Type     string
	Revision int64
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
		store:      kv.NewKVStore(),
		id:         id,
		addr:       raftAddr,
		peers:      peers,
		applyCh:    make(chan raft.ApplyMsg, 100),
		wal:        wal,
		snapshot:   snapshot,
		replyCh:    make(map[int]chan interface{}),
		rpcAddr:    rpcAddr,
		httpAddr:   httpAddr,
		watchers:   NewWatchManager(),
		leaseMgr:   NewLeaseManager(),
		currentRev: 0,
	}

	// 初始化分布式锁管理器
	s.lockMgr = NewLockManager(s.leaseMgr, s.store)

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
		watchers:       make(map[string]map[uint64]*watcherInfo),
		prefixWatchers: make(map[string]map[uint64]*watcherInfo),
	}
}

// 添加普通 watcher
func (wa *WatchManager) AddWatcher(key string, persistent bool, timeout time.Duration, fromRev int64) *Watcher {
	return wa.addWatcherInternal(key, "", persistent, timeout, fromRev)
}

// 添加 prefix watcher
func (wa *WatchManager) AddPrefixWatcher(prefix string, persistent bool, timeout time.Duration, fromRev int64) *Watcher {
	return wa.addWatcherInternal("", prefix, persistent, timeout, fromRev)
}

func (wa *WatchManager) addWatcherInternal(key string, prefix string, persistent bool, timeout time.Duration, fromRev int64) *Watcher {
	wa.watchMu.Lock()
	defer wa.watchMu.Unlock()

	wa.nextID++
	id := wa.nextID

	ch := make(chan Event, 100)
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
		fromRev:    fromRev,
	}

	if prefix != "" {
		if wa.prefixWatchers[prefix] == nil {
			wa.prefixWatchers[prefix] = make(map[uint64]*watcherInfo)
		}
		wa.prefixWatchers[prefix][id] = info
	} else {
		if wa.watchers[key] == nil {
			wa.watchers[key] = make(map[uint64]*watcherInfo)
		}
		wa.watchers[key][id] = info
	}

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

// 取消 prefix watcher
func (wa *WatchManager) RemovePrefixWatcher(prefix string, id uint64) {
	wa.watchMu.Lock()
	defer wa.watchMu.Unlock()
	if watchers, ok := wa.prefixWatchers[prefix]; ok {
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
	case kv.CmdDelete, kv.CmdDeletePrefix:
		eventType = "DELETE"
	default:
		return
	}

	event := Event{
		Key:      cmd.Key,
		Value:    cmd.Value,
		Type:     eventType,
		Revision: cmd.Revision,
	}

	watchEvent := kv.WatchEvent{
		Key:      cmd.Key,
		Value:    cmd.Value,
		Type:     eventType,
		Revision: cmd.Revision,
	}
	s.watchers.watchMu.Lock()
	s.watchers.eventLog = append(s.watchers.eventLog, watchEvent)

	s.notifyExactWatchers(cmd.Key, event)
	s.notifyPrefixWatchers(cmd.Key, event)

	s.watchers.watchMu.Unlock()
}

func (s *Server) notifyExactWatchers(key string, event Event) {
	watchers, ok := s.watchers.watchers[key]
	if !ok {
		return
	}

	for id, info := range watchers {
		if info.fromRev > 0 {
			for _, ev := range s.watchers.eventLog {
				if ev.Key == key && ev.Revision >= info.fromRev && ev.Revision < event.Revision {
					histEvent := Event{
						Key:   ev.Key,
						Value: ev.Value,
						Type:  ev.Type,
					}
					select {
					case info.ch <- histEvent:
					default:
					}
				}
			}
		}

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
		delete(s.watchers.watchers, key)
	}
}

func (s *Server) notifyPrefixWatchers(key string, event Event) {
	for prefix, watchers := range s.watchers.prefixWatchers {
		if len(prefix) == 0 || (len(key) >= len(prefix) && key[:len(prefix)] == prefix) {
			for id, info := range watchers {
				if info.fromRev > 0 {
					for _, ev := range s.watchers.eventLog {
						if ev.Revision >= info.fromRev && ev.Revision < event.Revision {
							prefixMatch := len(prefix) == 0 || (len(ev.Key) >= len(prefix) && ev.Key[:len(prefix)] == prefix)
							if prefixMatch {
								histEvent := Event{
									Key:   ev.Key,
									Value: ev.Value,
									Type:  ev.Type,
								}
								select {
								case info.ch <- histEvent:
								default:
								}
							}
						}
					}
				}

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
				delete(s.watchers.prefixWatchers, prefix)
			}
		}
	}
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
		// 同步更新本地 currentRev
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
					Key:       toString(m["Key"]),
					Value:     toString(m["Value"]),
					LeaseID:   toInt64(m["LeaseID"]),
					TTL:       toInt64(m["TTL"]),
					Type:      kv.CommandType(toInt(m["Type"])),
					Revision:  toInt64(m["Revision"]),
					Tombstone: toBool(m["Tombstone"]),
				}
				ok = true
			}
		}
		if !ok {
			s.mu.Unlock()
			continue
		}

		// 增加全局 revision 并设置到命令中（写操作）
		if cmd.Type == kv.CmdPut || cmd.Type == kv.CmdDelete || cmd.Type == kv.CmdCAS || cmd.Type == kv.CmdLeaseAttach {
			s.currentRev++
			cmd.Revision = s.currentRev
		}

		// CmdDeletePrefix 需要单独处理 revision
		if cmd.Type == kv.CmdDeletePrefix {
			s.currentRev++
			cmd.Revision = s.currentRev
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
					s.currentRev++
					deleteCmd := kv.Command{Type: kv.CmdDelete, Key: key, Revision: s.currentRev}
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
				// 使用 Apply 来确保 MVCC 正确记录版本
				cmd.Type = kv.CmdPut
				result, _ = s.store.Apply(cmd)
			}
			result = "OK"
			//
		case kv.CmdCAS:
			result, _ = s.store.Apply(cmd)
			if result == "OK" {
				cmd.Type = kv.CmdPut
				s.notifyWatchers(cmd)
			}

		case kv.CmdDeletePrefix:
			deletedKeys := s.store.DeletePrefix(cmd.Prefix)
			for _, key := range deletedKeys {
				deleteCmd := kv.Command{Type: kv.CmdDelete, Key: key, Revision: s.currentRev}
				s.notifyWatchers(deleteCmd)
			}
			result = fmt.Sprintf("%d", len(deletedKeys))
			cmdIdx = msg.CommandIndex
			s.notifyReply(cmdIdx, result)
			s.mu.Unlock()
			s.mu.Lock()
			if msg.CommandIndex > s.rf.LastApplied {
				s.rf.LastApplied = msg.CommandIndex
			}
			s.mu.Unlock()
			continue

		case kv.CmdGetPrefix:
			resultMap := s.store.GetPrefix(cmd.Prefix)
			result = fmt.Sprintf("%v", resultMap)
			cmdIdx = msg.CommandIndex
			s.notifyReply(cmdIdx, result)
			s.mu.Unlock()
			s.mu.Lock()
			if msg.CommandIndex > s.rf.LastApplied {
				s.rf.LastApplied = msg.CommandIndex
			}
			s.mu.Unlock()
			continue

		default:
			result, _ = s.store.Apply(cmd)
			// 自动关联 LeaseID
			if cmd.LeaseID > 0 && cmd.Type == kv.CmdPut {
				s.leaseMgr.Attach(cmd.LeaseID, cmd.Key, cmd.Value)
				s.store.SetKeyLease(cmd.Key, cmd.LeaseID)
			}
			s.notifyWatchers(cmd)
		}

		cmdIdx = msg.CommandIndex
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

func toBool(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case float64:
		return val != 0
	case int:
		return val != 0
	case int64:
		return val != 0
	}
	return false
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

// GetPrefix 本地读取（无需经过 Raft）
func (s *Server) GetPrefix(prefix string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.GetPrefix(prefix)
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
	mux.HandleFunc("/lock/acquire", handler.LockAcquire)
	mux.HandleFunc("/lock/release", handler.LockRelease)
	mux.HandleFunc("/lock/keepalive", handler.LockKeepAlive)
	mux.HandleFunc("/lock/status", handler.LockStatus)
	mux.HandleFunc("/watch/prefix", handler.WatchPrefix)
	mux.HandleFunc("/delete/prefix", handler.DeletePrefix)
	mux.HandleFunc("/get/prefix", handler.GetPrefix)
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
